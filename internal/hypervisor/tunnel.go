// Package hypervisor provides the in-container control plane: it supervises the
// per-agent processes and exposes an external HTTP bridge. The tunnel connects
// this bridge back to the bus over WebSocket so the portal can reach a
// per-user container regardless of the deployment backend (docker/swarm/k8s).
package hypervisor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"github.com/gorilla/websocket"
)

const (
	tunnelPingInterval = 45 * time.Second
	tunnelPongWait     = 75 * time.Second
)

// tunnelWriteTimeout bounds a single tunnel frame write (e.g. a tunnel
// response carrying a workspace file) so a stalled peer cannot hold the
// connection indefinitely. The deadline is scoped to the write and cleared
// afterwards so it never leaks into other operations. It is a variable (not a
// const) so tests can shrink it.
var tunnelWriteTimeout = 10 * time.Second

// Tunnel maintains a persistent WebSocket to the bus and services
// tunnel_request messages by proxying them to the local hypervisor HTTP bridge,
// and tunnel_stream_* messages by driving the terminal session manager.
type Tunnel struct {
	userID     string
	wsURL      string
	localBase  string
	httpClient *http.Client
	terminals  *TerminalManager
}

// NewTunnel builds a Tunnel for the given user. busURL may be ws://, wss://,
// http:// or https://; it is normalized to the bus's WebSocket endpoint.
// localAddr is the hypervisor HTTP bridge address (e.g. ":9090"). terminals
// receives stream frames and provides PTY output; it may be nil.
func NewTunnel(userID, busURL, localAddr string, terminals *TerminalManager) *Tunnel {
	ws := toWebSocketURL(busURL) + "/ws/hypervisor"

	base := localAddr
	if base == "" {
		base = ":9090"
	}
	if strings.HasPrefix(base, ":") {
		base = "localhost" + base
	}
	base = strings.TrimRight("http://"+base, "/")

	return &Tunnel{
		userID:     userID,
		wsURL:      ws,
		localBase:  base,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		terminals:  terminals,
	}
}

// toWebSocketURL converts an http(s):// or ws(s):// base URL into its ws(s)
// form with no trailing slash.
func toWebSocketURL(raw string) string {
	raw = strings.TrimRight(raw, "/")
	switch {
	case strings.HasPrefix(raw, "http://"):
		return "ws://" + raw[len("http://"):]
	case strings.HasPrefix(raw, "https://"):
		return "wss://" + raw[len("https://"):]
	default:
		return raw
	}
}

// Run connects to the bus and keeps the tunnel alive until ctx is cancelled.
func (t *Tunnel) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		registered, err := t.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("hypervisor tunnel disconnected, reconnecting", "user_id", t.userID, "error", err, "delay", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if registered {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// runOnce establishes one tunnel connection and services requests until it
// drops or ctx is cancelled. The bool reports whether the tunnel successfully
// registered and serviced requests before the connection ended; it is used to
// reset the reconnect backoff after a healthy session.
func (t *Tunnel) runOnce(ctx context.Context) (bool, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	// The bus authenticates its internal endpoints with a shared token; the
	// tunnel is one of them, so the handshake carries it.
	var header http.Header
	if token := config.InternalToken(); token != "" {
		header = http.Header{"Authorization": {"Bearer " + token}}
	}
	conn, _, err := dialer.DialContext(ctx, t.wsURL, header)
	if err != nil {
		return false, fmt.Errorf("dial bus: %w", err)
	}

	wrapped := &tunnelConn{conn: conn}
	defer wrapped.close()

	// Route PTY output frames through this connection for as long as it lives.
	// Terminal sessions survive disconnects (output is buffered in the ring);
	// the next tunnel re-registers the sender.
	if t.terminals != nil {
		t.terminals.SetSender(func(msg nanoprotocol.Message) {
			_ = wrapped.writeJSON(msg)
		})
	}
	defer func() {
		if t.terminals != nil {
			t.terminals.SetSender(nil)
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(tunnelPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(tunnelPongWait))
	})

	register, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegister, nanoprotocol.TunnelRegister{
		UserID: t.userID,
	})
	if err != nil {
		return false, err
	}
	if err := wrapped.writeJSON(register); err != nil {
		return false, fmt.Errorf("register tunnel: %w", err)
	}
	slog.Info("hypervisor tunnel registered", "user_id", t.userID)

	done := make(chan struct{})
	defer close(done)
	go t.keepalive(conn, done)

	for {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return true, fmt.Errorf("read: %w", err)
		}
		switch msg.Type {
		case nanoprotocol.TypeTunnelRequest:
			var req nanoprotocol.TunnelRequest
			if err := msg.DecodePayload(&req); err != nil {
				slog.Warn("tunnel request decode error", "error", err)
				continue
			}
			go t.serveRequest(wrapped, req)
		case nanoprotocol.TypeTunnelStreamInput:
			var p nanoprotocol.TunnelStreamInput
			if err := msg.DecodePayload(&p); err != nil {
				continue
			}
			if t.terminals != nil {
				if err := t.terminals.Input(p.StreamID, p.Data); err != nil {
					slog.Debug("terminal input rejected", "terminal_id", p.StreamID, "error", err)
				}
			}
		case nanoprotocol.TypeTunnelStreamResize:
			var p nanoprotocol.TunnelStreamResize
			if err := msg.DecodePayload(&p); err != nil {
				continue
			}
			if t.terminals != nil {
				_ = t.terminals.Resize(p.StreamID, p.Cols, p.Rows)
			}
		case nanoprotocol.TypeTunnelStreamClose:
			var p nanoprotocol.TunnelStreamClose
			if err := msg.DecodePayload(&p); err != nil {
				continue
			}
			if t.terminals != nil {
				_ = t.terminals.Kill(p.StreamID)
			}
		}
	}
}

// keepalive pings the bus so intermediate proxies do not drop the idle tunnel.
func (t *Tunnel) keepalive(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(tunnelPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// WriteControl bounds its own frame; do NOT set a global write
			// deadline here — it would leak into concurrent/successive data
			// writes and kill slow tunnel responses with "i/o timeout".
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// serveRequest proxies a tunnel_request to the local HTTP bridge and replies
// with a tunnel_response.
func (t *Tunnel) serveRequest(conn *tunnelConn, req nanoprotocol.TunnelRequest) {
	resp := t.doLocal(req)
	out, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, resp)
	if err != nil {
		return
	}
	if err := conn.writeJSON(out); err != nil {
		slog.Warn("tunnel response write failed", "request_id", req.RequestID, "error", err)
	}
}

// doLocal executes the request against the local hypervisor HTTP bridge.
func (t *Tunnel) doLocal(req nanoprotocol.TunnelRequest) nanoprotocol.TunnelResponse {
	httpReq, err := http.NewRequest(req.Method, t.localBase+req.Path, bytes.NewReader(req.Body))
	if err != nil {
		return nanoprotocol.TunnelResponse{RequestID: req.RequestID, Status: http.StatusBadRequest}
	}
	for key, values := range req.Headers {
		if strings.EqualFold(key, "Host") {
			continue
		}
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nanoprotocol.TunnelResponse{
			RequestID: req.RequestID,
			Status:    http.StatusBadGateway,
			Body:      []byte(err.Error()),
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nanoprotocol.TunnelResponse{RequestID: req.RequestID, Status: http.StatusBadGateway}
	}
	return nanoprotocol.TunnelResponse{
		RequestID: req.RequestID,
		Status:    resp.StatusCode,
		Headers:   resp.Header,
		Body:      body,
	}
}

// writeJSONScoped writes v to conn with a bounded, self-clearing write
// deadline. This is shared by all tunnel data writes so slow responses cannot
// hang the connection nor leak a deadline into later operations.
func writeJSONScoped(conn *websocket.Conn, timeout time.Duration, v any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	err := conn.WriteJSON(v)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// tunnelConn serializes writes to a tunnel WebSocket connection.
type tunnelConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *tunnelConn) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeJSONScoped(c.conn, tunnelWriteTimeout, v)
}

func (c *tunnelConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.Close()
}
