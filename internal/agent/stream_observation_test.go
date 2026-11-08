package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// captureLogs redirects the default logger so a test can read what a stream
// reported about itself.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// shrinkObservationThresholds makes the reporting thresholds reachable inside a
// test instead of after two seconds of real waiting.
func shrinkObservationThresholds(t *testing.T, post, gap time.Duration) {
	t.Helper()
	prevPost, prevGap := chunkPostWarnThreshold, streamGapWarnThreshold
	chunkPostWarnThreshold, streamGapWarnThreshold = post, gap
	t.Cleanup(func() { chunkPostWarnThreshold, streamGapWarnThreshold = prevPost, prevGap })
}

func newObservationAgent(t *testing.T, bus *MockBusClient, mock *provider.MockProvider) *Agent {
	t.Helper()
	ag, err := New(Config{
		UserID:        "test-user",
		MaxIterations: 5,
		SessionDir:    t.TempDir(),
	}, bus, mock, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

// TestChatStreamTurn_ReportsSlowChunkPosting pins the measurement that decides
// whether a cut can be blamed on this loop: a delta is posted to the bus
// synchronously, so a slow post stops the provider being read.
func TestChatStreamTurn_ReportsSlowChunkPosting(t *testing.T) {
	logs := captureLogs(t)
	shrinkObservationThresholds(t, 10*time.Millisecond, time.Hour)

	mock := provider.NewMock()
	mock.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 3)
		ch <- provider.ChatStreamEvent{Kind: "content", Delta: "hello "}
		ch <- provider.ChatStreamEvent{Kind: "content", Delta: "world"}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "hello world", FinishReason: "stop"}
		close(ch)
		return ch, nil
	}
	bus := &MockBusClient{PostChunkFunc: func(nanoprotocol.OutboundChunk) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}}

	ag := newObservationAgent(t, bus, mock)
	if _, err := ag.chatStreamTurn(context.Background(), provider.ChatRequest{Model: "basic"}, inboundMsg(), "m1"); err != nil {
		t.Fatalf("chatStreamTurn: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "llm stream held up or cut") {
		t.Fatalf("a 30ms chunk post under a 10ms threshold must be reported; logs:\n%s", out)
	}
	if strings.Contains(out, "max_post_ms=0") {
		t.Fatalf("max_post_ms must carry the measured post time; logs:\n%s", out)
	}
	if !strings.Contains(out, "chunks=2") || !strings.Contains(out, "bytes=11") {
		t.Fatalf("the report must carry what streamed; logs:\n%s", out)
	}
	if !strings.Contains(out, "outcome=completed") {
		t.Fatalf("a slow-but-complete stream is not an interruption; logs:\n%s", out)
	}
}

// TestChatStreamTurn_ReportsLongEventGap pins the other half: a stream that goes
// quiet long enough is the shape the proxies in front of the gateway cut.
func TestChatStreamTurn_ReportsLongEventGap(t *testing.T) {
	logs := captureLogs(t)
	shrinkObservationThresholds(t, time.Hour, 10*time.Millisecond)

	mock := provider.NewMock()
	mock.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 3)
		go func() {
			ch <- provider.ChatStreamEvent{Kind: "content", Delta: "think"}
			time.Sleep(40 * time.Millisecond)
			ch <- provider.ChatStreamEvent{Kind: "content", Delta: "answer"}
			ch <- provider.ChatStreamEvent{Done: true, FullContent: "thinkanswer", FinishReason: "stop"}
			close(ch)
		}()
		return ch, nil
	}

	ag := newObservationAgent(t, &MockBusClient{}, mock)
	if _, err := ag.chatStreamTurn(context.Background(), provider.ChatRequest{Model: "basic"}, inboundMsg(), "m1"); err != nil {
		t.Fatalf("chatStreamTurn: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "llm stream held up or cut") {
		t.Fatalf("a 40ms quiet stretch under a 10ms threshold must be reported; logs:\n%s", out)
	}
	if strings.Contains(out, "max_gap_ms=0") {
		t.Fatalf("max_gap_ms must carry the measured gap; logs:\n%s", out)
	}
}

// TestChatStreamTurn_ReportsCutStream is the attribution case: the stream ends
// without a terminal event, so the report has to say what had streamed by then.
func TestChatStreamTurn_ReportsCutStream(t *testing.T) {
	logs := captureLogs(t)

	mock := provider.NewMock()
	mock.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 1)
		ch <- provider.ChatStreamEvent{Kind: "content", Delta: "半截"}
		close(ch) // no Done event: the connection died mid-stream
		return ch, nil
	}

	ag := newObservationAgent(t, &MockBusClient{}, mock)
	resp, err := ag.chatStreamTurn(context.Background(), provider.ChatRequest{Model: "basic"}, inboundMsg(), "m1")
	if err == nil {
		t.Fatal("a stream without a terminal event must fail the call")
	}
	if resp == nil || resp.Content != "半截" {
		t.Fatalf("the partial content must survive for the caller; got %+v", resp)
	}

	out := logs.String()
	if !strings.Contains(out, "outcome=interrupted") {
		t.Fatalf("a cut stream must be reported as interrupted; logs:\n%s", out)
	}
	if !strings.Contains(out, "bytes=6") {
		t.Fatalf("the report must carry the bytes received before the cut; logs:\n%s", out)
	}
}

// TestChatStreamTurn_QuietStreamLogsNothing is the negative control: a fast,
// quiet, successful stream must not add a line, or the signal is worthless.
func TestChatStreamTurn_QuietStreamLogsNothing(t *testing.T) {
	logs := captureLogs(t)

	mock := provider.NewMock()
	mock.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 3)
		ch <- provider.ChatStreamEvent{Kind: "content", Delta: "a"}
		ch <- provider.ChatStreamEvent{Kind: "content", Delta: "b"}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "ab", FinishReason: "stop"}
		close(ch)
		return ch, nil
	}

	ag := newObservationAgent(t, &MockBusClient{}, mock)
	if _, err := ag.chatStreamTurn(context.Background(), provider.ChatRequest{Model: "basic"}, inboundMsg(), "m1"); err != nil {
		t.Fatalf("chatStreamTurn: %v", err)
	}
	if out := logs.String(); strings.Contains(out, "held up or cut") {
		t.Fatalf("a quiet stream must log nothing; logs:\n%s", out)
	}
}
