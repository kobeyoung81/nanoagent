package hypervisor

// The container half of the chunked tunnel: a large local response is read out
// in blocks, and a chunked request body is piped into the local bridge. Both are
// driven over a real WebSocket, the same way the provisioner drives them.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/gorilla/websocket"
)

func dialFakeRelay(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handler(conn)
	}))
}

// TestServeStreamedReadsALargeAnswerInChunks is the container-side regression:
// a body bigger than one frame must come back as head + chunks, not as one
// message the relay's read limit would reject.
func TestServeStreamedReadsALargeAnswerInChunks(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), (900<<10)/8)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer bridge.Close()

	got := make(chan []byte, 1)
	relay := dialFakeRelay(t, func(conn *websocket.Conn) {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			t.Errorf("read register: %v", err)
			return
		}
		var register nanoprotocol.TunnelRegister
		if err := msg.DecodePayload(&register); err != nil {
			t.Errorf("decode register: %v", err)
			return
		}
		if len(register.Features) == 0 || register.Features[0] != nanoprotocol.TunnelFeatureBodyStream {
			t.Errorf("features = %v, want body-stream", register.Features)
		}
		registered, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegistered, nanoprotocol.TunnelRegistered{})
		_ = conn.WriteJSON(registered)

		req, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequest, nanoprotocol.TunnelRequest{
			// A download has no body, so the request is inline; the answer is what
			// must be chunked.
			RequestID: "r-big", Method: http.MethodGet, Path: "/big",
		})
		if err := conn.WriteJSON(req); err != nil {
			t.Errorf("write request: %v", err)
			return
		}

		var body bytes.Buffer
		for {
			if err := conn.ReadJSON(&msg); err != nil {
				t.Errorf("read frame: %v", err)
				return
			}
			if msg.Type != nanoprotocol.TypeTunnelResponseBody {
				var head nanoprotocol.TunnelResponse
				if err := msg.DecodePayload(&head); err == nil && !head.Streamed {
					t.Errorf("answer was not streamed")
					return
				}
				continue
			}
			var chunk nanoprotocol.TunnelBodyChunk
			if err := msg.DecodePayload(&chunk); err != nil {
				t.Errorf("decode chunk: %v", err)
				return
			}
			body.Write(chunk.Data)
			if chunk.Final {
				got <- body.Bytes()
				return
			}
		}
	})
	defer relay.Close()

	tun := NewTunnel("user-1", relay.URL, strings.TrimPrefix(bridge.URL, "http://"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = tun.runOnce(ctx); close(done) }()

	select {
	case body := <-got:
		if len(body) != len(payload) {
			t.Fatalf("streamed %d bytes, want %d", len(body), len(payload))
		}
		if !bytes.Equal(body, payload) {
			t.Fatal("the streamed body differs from the local answer")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the container never streamed the answer")
	}
	cancel()
	<-done
}

// TestServeStreamedPipesTheUpload is the request direction: chunks must reach
// the local bridge in order and in full.
func TestServeStreamedPipesTheUpload(t *testing.T) {
	payload := bytes.Repeat([]byte("upload!"), (1<<20)/7)
	received := make(chan []byte, 1)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		if _, err := body.ReadFrom(r.Body); err != nil {
			t.Errorf("read upload: %v", err)
		}
		received <- body.Bytes()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer bridge.Close()

	relay := dialFakeRelay(t, func(conn *websocket.Conn) {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		registered, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegistered, nanoprotocol.TunnelRegistered{})
		_ = conn.WriteJSON(registered)

		req, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequest, nanoprotocol.TunnelRequest{
			RequestID: "r-up", Method: http.MethodPut, Path: "/upload",
			Streamed: true, BodyLength: int64(len(payload)),
		})
		_ = conn.WriteJSON(req)

		seq := 0
		for offset := 0; offset < len(payload); offset += nanoprotocol.TunnelChunkBytes {
			end := offset + nanoprotocol.TunnelChunkBytes
			if end > len(payload) {
				end = len(payload)
			}
			chunk, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequestBody, nanoprotocol.TunnelBodyChunk{
				RequestID: "r-up", Seq: seq, Data: payload[offset:end], Final: end == len(payload),
			})
			if err := conn.WriteJSON(chunk); err != nil {
				return
			}
			seq++
		}
		// The answer is small, so it must come back inline.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := conn.ReadJSON(&msg); err != nil {
			t.Errorf("read answer: %v", err)
		}
	})
	defer relay.Close()

	tun := NewTunnel("user-2", relay.URL, strings.TrimPrefix(bridge.URL, "http://"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = tun.runOnce(ctx); close(done) }()

	select {
	case body := <-received:
		if !bytes.Equal(body, payload) {
			t.Fatalf("bridge received %d bytes, want %d", len(body), len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge never saw the upload")
	}
	cancel()
	<-done
}

// TestCancelStopsTheStream: after the relay cancels, the container must stop
// sending chunks. The container is reading a file nobody waits for otherwise.
func TestCancelStopsTheStream(t *testing.T) {
	started := make(chan struct{}, 1)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		block := make([]byte, 64<<10)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write(block); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer bridge.Close()

	stopped := make(chan struct{}, 1)
	relay := dialFakeRelay(t, func(conn *websocket.Conn) {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		registered, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegistered, nanoprotocol.TunnelRegistered{})
		_ = conn.WriteJSON(registered)

		req, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequest, nanoprotocol.TunnelRequest{
			RequestID: "r-cancel", Method: http.MethodGet, Path: "/stall",
		})
		_ = conn.WriteJSON(req)

		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Errorf("the local request never started")
			return
		}

		// Wait for the stream to be under way, then cancel it.
		chunks := 0
		for chunks < 2 {
			if err := conn.ReadJSON(&msg); err != nil {
				t.Errorf("read chunk: %v", err)
				return
			}
			if msg.Type == nanoprotocol.TypeTunnelResponseBody {
				chunks++
			}
		}
		cancelMsg, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelCancel, nanoprotocol.TunnelCancel{RequestID: "r-cancel"})
		_ = conn.WriteJSON(cancelMsg)

		// Nothing more may arrive: a deadline is how the absence is observed.
		_ = conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		for {
			if err := conn.ReadJSON(&msg); err != nil {
				stopped <- struct{}{}
				return
			}
			if msg.Type == nanoprotocol.TypeTunnelResponseBody {
				var chunk nanoprotocol.TunnelBodyChunk
				_ = msg.DecodePayload(&chunk)
				if len(chunk.Data) > 0 {
					t.Errorf("the container kept streaming after the cancel")
					return
				}
			}
		}
	})
	defer relay.Close()

	tun := NewTunnel("user-3", relay.URL, strings.TrimPrefix(bridge.URL, "http://"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = tun.runOnce(ctx); close(done) }()

	select {
	case <-stopped:
	case <-time.After(8 * time.Second):
		t.Fatal("the stream did not stop after the cancel")
	}
	cancel()
	<-done
}
