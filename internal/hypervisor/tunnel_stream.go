package hypervisor

// Streaming on the container side. A request whose body arrives as chunks is
// piped into the local HTTP request, and a response larger than one frame is
// read out as chunks: the file never has to fit in memory, which is what made a
// 5 MB download kill the tunnel.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// tunnelStream is one in-flight request. A streamed request also has a pipe for
// the body that arrives as chunks; an inline request has only the cancel, because
// its answer can still be a whole file and the caller can still walk away.
type tunnelStream struct {
	writer *io.PipeWriter
	cancel context.CancelFunc
}

func (s *tunnelStream) closePipe(err error) {
	if s.writer != nil {
		_ = s.writer.CloseWithError(err)
	}
}

// track registers a request so a later cancel can find it.
func (t *Tunnel) track(requestID string, st *tunnelStream) {
	t.streamsMu.Lock()
	t.streams[requestID] = st
	t.streamsMu.Unlock()
}

// startStreamedRequest opens the pipe the request body will arrive on and hands
// the local request to a serving goroutine.
func (t *Tunnel) startStreamedRequest(parent context.Context, conn *tunnelConn, req nanoprotocol.TunnelRequest) {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(parent)
	st := &tunnelStream{writer: writer, cancel: cancel}

	t.track(req.RequestID, st)
	go t.serveLocal(ctx, conn, req, reader, st)
}

// deliverRequestBody routes one chunk to the request it belongs to. A chunk for
// a request that was cancelled is dropped: the caller is gone.
func (t *Tunnel) deliverRequestBody(chunk nanoprotocol.TunnelBodyChunk) {
	t.streamsMu.Lock()
	st, ok := t.streams[chunk.RequestID]
	t.streamsMu.Unlock()
	if !ok {
		return
	}
	if st.writer == nil {
		return
	}
	if chunk.Error != "" {
		st.closePipe(errors.New(chunk.Error))
		return
	}
	if len(chunk.Data) > 0 {
		if _, err := st.writer.Write(chunk.Data); err != nil {
			slog.Debug("tunnel request body write", "request_id", chunk.RequestID, "error", err)
			return
		}
	}
	if chunk.Final {
		_ = st.writer.Close()
	}
}

// cancelStream stops a request the caller walked away from: the local HTTP call
// is cancelled and its pipe closed, so a file read stops with it.
func (t *Tunnel) cancelStream(requestID string) {
	t.streamsMu.Lock()
	st, ok := t.streams[requestID]
	if ok {
		delete(t.streams, requestID)
	}
	t.streamsMu.Unlock()
	if ok {
		st.cancel()
		st.closePipe(context.Canceled)
	}
}

func (t *Tunnel) forgetStream(requestID string, st *tunnelStream) {
	t.streamsMu.Lock()
	if t.streams[requestID] == st {
		delete(t.streams, requestID)
	}
	t.streamsMu.Unlock()
	st.cancel()
}

// serveLocal executes the request against the local bridge and writes the
// answer back, chunking it when it is larger than one frame. Both the inline and
// the streamed request paths end here: whether the answer is streamed depends on
// the answer, not on the request.
func (t *Tunnel) serveLocal(ctx context.Context, conn *tunnelConn, req nanoprotocol.TunnelRequest, body io.Reader, st *tunnelStream) {
	if st != nil {
		defer t.forgetStream(req.RequestID, st)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, t.localBase+req.Path, body)
	if err != nil {
		t.writeStreamedFailure(conn, req.RequestID, http.StatusBadRequest, err.Error())
		return
	}
	for key, values := range req.Headers {
		if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	if req.Streamed {
		// The body is a pipe, so the transport cannot infer a length; carry the
		// declared one when the relay knew it.
		if req.BodyLength >= 0 {
			httpReq.ContentLength = req.BodyLength
		}
	} else {
		httpReq.ContentLength = int64(len(req.Body))
	}

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return // the caller cancelled; nothing to report
		}
		t.writeStreamedFailure(conn, req.RequestID, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// A cancel has to stop the file read itself, not just the next frame: close
	// the local response so a blocking Read returns instead of finishing a file
	// nobody waits for.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = resp.Body.Close()
		case <-stopWatch:
		}
	}()

	t.writeLocalResponse(ctx, conn, req.RequestID, resp)
}

// writeLocalResponse sends a small answer as one frame and a large one as a head
// plus chunks.
func (t *Tunnel) writeLocalResponse(ctx context.Context, conn *tunnelConn, requestID string, resp *http.Response) {
	if resp.ContentLength >= 0 && resp.ContentLength <= nanoprotocol.TunnelInlineBodyBytes {
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, nanoprotocol.TunnelInlineBodyBytes+1))
		if readErr != nil {
			t.writeStreamedFailure(conn, requestID, http.StatusBadGateway, readErr.Error())
			return
		}
		if len(payload) <= nanoprotocol.TunnelInlineBodyBytes {
			msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, nanoprotocol.TunnelResponse{
				RequestID: requestID,
				Status:    resp.StatusCode,
				Headers:   resp.Header,
				Body:      payload,
			})
			if err == nil {
				_ = conn.writeJSON(msg)
			}
			return
		}
		// The bridge under-declared its length; continue as a stream with what
		// was already read.
		t.writeStreamedHead(conn, requestID, resp, -1)
		if err := t.writeTunnelChunk(conn, nanoprotocol.TunnelBodyChunk{RequestID: requestID, Seq: 0, Data: payload}); err != nil {
			return
		}
		t.streamBody(ctx, conn, requestID, resp.Body, 1)
		return
	}

	t.writeStreamedHead(conn, requestID, resp, resp.ContentLength)
	t.streamBody(ctx, conn, requestID, resp.Body, 0)
}

// writeStreamedHead announces a chunked body; the status and headers are the
// container's, so the browser sees the real content type and length.
func (t *Tunnel) writeStreamedHead(conn *tunnelConn, requestID string, resp *http.Response, length int64) {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, nanoprotocol.TunnelResponse{
		RequestID:  requestID,
		Status:     resp.StatusCode,
		Headers:    resp.Header,
		Streamed:   true,
		BodyLength: length,
	})
	if err != nil {
		return
	}
	_ = conn.writeJSON(msg)
}

// streamBody reads the local answer out in frame-sized blocks.
func (t *Tunnel) streamBody(ctx context.Context, conn *tunnelConn, requestID string, body io.Reader, seq int) {
	buf := make([]byte, nanoprotocol.TunnelChunkBytes)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := body.Read(buf)
		if n > 0 {
			if werr := t.writeTunnelChunk(conn, nanoprotocol.TunnelBodyChunk{
				RequestID: requestID,
				Seq:       seq,
				Data:      buf[:n],
			}); werr != nil {
				return
			}
			seq++
		}
		if errors.Is(err, io.EOF) {
			_ = t.writeTunnelChunk(conn, nanoprotocol.TunnelBodyChunk{RequestID: requestID, Seq: seq, Final: true})
			return
		}
		if err != nil {
			_ = t.writeTunnelChunk(conn, nanoprotocol.TunnelBodyChunk{RequestID: requestID, Seq: seq, Error: err.Error()})
			return
		}
	}
}

func (t *Tunnel) writeTunnelChunk(conn *tunnelConn, chunk nanoprotocol.TunnelBodyChunk) error {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponseBody, chunk)
	if err != nil {
		return err
	}
	return conn.writeJSON(msg)
}

// writeStreamedFailure answers with an inline error body: a failure is small and
// the relay turns it into the caller's status.
func (t *Tunnel) writeStreamedFailure(conn *tunnelConn, requestID string, status int, detail string) {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, nanoprotocol.TunnelResponse{
		RequestID: requestID,
		Status:    status,
		Body:      []byte(detail),
	})
	if err != nil {
		return
	}
	_ = conn.writeJSON(msg)
}
