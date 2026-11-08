package hypervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/gorilla/websocket"
)

func TestToWebSocketURL(t *testing.T) {
	cases := map[string]string{
		"http://bus:9000":  "ws://bus:9000",
		"https://bus:9000": "wss://bus:9000",
		"ws://bus:9000":    "ws://bus:9000",
		"ws://bus:9000/":   "ws://bus:9000",
		"wss://bus":        "wss://bus",
	}
	for in, want := range cases {
		if got := toWebSocketURL(in); got != want {
			t.Errorf("toWebSocketURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTunnelDoLocal(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/files/agent-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Test"); got != "yes" {
			t.Errorf("expected X-Test header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent_id":"agent-1","entries":[]}`))
	}))
	defer bridge.Close()

	tun := &Tunnel{
		localBase:  bridge.URL,
		httpClient: bridge.Client(),
	}
	resp := tun.doLocal(nanoprotocol.TunnelRequest{
		RequestID: "r1",
		Method:    http.MethodGet,
		Path:      "/api/v1/files/agent-1",
		Headers:   map[string][]string{"X-Test": {"yes"}},
	})

	if resp.RequestID != "r1" {
		t.Errorf("request id = %q, want r1", resp.RequestID)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "agent-1") {
		t.Errorf("unexpected body: %s", resp.Body)
	}
}

func TestTunnelRunOnce(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer bridge.Close()

	var gotRegister bool
	responseCh := make(chan string, 1)
	busWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var m nanoprotocol.Message
		if err := conn.ReadJSON(&m); err != nil {
			return
		}
		gotRegister = m.Type == nanoprotocol.TypeTunnelRegister

		registered, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegistered, nanoprotocol.TunnelRegistered{})
		_ = conn.WriteJSON(registered)

		req, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequest, nanoprotocol.TunnelRequest{
			RequestID: "r1",
			Method:    http.MethodGet,
			Path:      "/",
		})
		if err := conn.WriteJSON(req); err != nil {
			return
		}

		if err := conn.ReadJSON(&m); err != nil {
			return
		}
		var resp nanoprotocol.TunnelResponse
		if err := m.DecodePayload(&resp); err != nil {
			return
		}
		responseCh <- string(resp.Body)
		_ = conn.Close()
	}))
	defer busWS.Close()

	bridgeAddr := strings.TrimPrefix(bridge.URL, "http://")
	tun := NewTunnel("user-1", busWS.URL, bridgeAddr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// runOnce returns an error when the bus closes the conn; that is expected,
	// but the tunnel must have registered and serviced a request first.
	registeredFlag, _ := tun.runOnce(ctx)
	if !registeredFlag {
		t.Error("expected runOnce to report a successful registration")
	}

	if !gotRegister {
		t.Error("expected tunnel_register message")
	}
	select {
	case body := <-responseCh:
		if body != `{"ok":true}` {
			t.Errorf("response body = %q, want {\"ok\":true}", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tunnel response")
	}
}

// TestTunnelConnWriteBounded verifies a tunnel write to a stalled peer returns
// an error within the bounded write deadline instead of hanging forever. This
// guards against the keepalive/response-write deadlock that broke uploads and
// downloads.
func TestTunnelConnWriteBounded(t *testing.T) {
	busWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Accept the connection but never read, simulating a stalled bus.
		<-r.Context().Done()
	}))
	defer busWS.Close()

	wsURL := "ws" + strings.TrimPrefix(busWS.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Payload larger than typical socket buffers so the write cannot complete
	// against a peer that never reads.
	blob := `{"blob":"` + strings.Repeat("a", 8<<20) + `"}`
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, struct {
		Body string `json:"body"`
	}{Body: blob})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = writeJSONScoped(conn, 300*time.Millisecond, msg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected write to fail against a stalled peer")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("write was not bounded; took %v", elapsed)
	}
}

// The bus authenticates its internal endpoints, and the tunnel handshake is
// built from a URL — so the token travels in the Authorization header of the
// dial request.
func TestTunnelDialCarriesTheInternalToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")

	headers := make(chan string, 1)
	busWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case headers <- r.Header.Get("Authorization"):
		default:
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		conn.Close()
	}))
	defer busWS.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun := &Tunnel{wsURL: "ws" + strings.TrimPrefix(busWS.URL, "http")}
	go tun.runOnce(ctx)

	select {
	case got := <-headers:
		if got != "Bearer s3cret" {
			t.Errorf("Authorization = %q, want the configured token", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel never dialed the bus")
	}
}
