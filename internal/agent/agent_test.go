package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// MockTool for agent tests
type MockTool struct {
	NameVal     string
	ExecuteFunc func(ctx context.Context, params map[string]any) (string, error)
}

func (m *MockTool) Name() string                         { return m.NameVal }
func (m *MockTool) Description() string                  { return "mock tool" }
func (m *MockTool) Parameters() provider.ParameterSchema { return provider.ParameterSchema{} }
func (m *MockTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	if m.ExecuteFunc != nil {
		return m.ExecuteFunc(ctx, params)
	}
	return "mock result", nil
}

// MockBusClient implements BusClient for testing
type MockBusClient struct {
	PollFunc         func(userID string, bindingID int64, agentID string) (*nanoprotocol.InboundMessage, error)
	PostOutboundFunc func(msg nanoprotocol.OutboundMessage) error
	PostChunkFunc    func(chunk nanoprotocol.OutboundChunk) error
	PostCompleteFunc func(complete nanoprotocol.OutboundComplete) error
	CloseFunc        func() error
	ListAgentsFunc   func(userID string) ([]nanoprotocol.AgentRecord, error)
	GetConfigFunc    func(componentType string) ([]nanoprotocol.ConfigItem, error)
	ConfigChangesCh  chan nanoprotocol.ConfigChange
	SaveSessionFunc  func(nanoprotocol.SaveSessionRequest) error
}

func (m *MockBusClient) PollInbound(userID string, bindingID int64, agentID string) (*nanoprotocol.InboundMessage, error) {
	if m.PollFunc != nil {
		return m.PollFunc(userID, bindingID, agentID)
	}
	return nil, nil
}

func (m *MockBusClient) PostOutbound(msg nanoprotocol.OutboundMessage) error {
	if m.PostOutboundFunc != nil {
		return m.PostOutboundFunc(msg)
	}
	return nil
}

func (m *MockBusClient) PostOutboundChunk(chunk nanoprotocol.OutboundChunk) error {
	if m.PostChunkFunc != nil {
		return m.PostChunkFunc(chunk)
	}
	return nil
}

func (m *MockBusClient) PostOutboundComplete(complete nanoprotocol.OutboundComplete) error {
	if m.PostCompleteFunc != nil {
		return m.PostCompleteFunc(complete)
	}
	return nil
}

func (m *MockBusClient) Close() error {
	if m.CloseFunc != nil {
		return m.CloseFunc()
	}
	return nil
}

func (m *MockBusClient) ListAgents(userID string) ([]nanoprotocol.AgentRecord, error) {
	if m.ListAgentsFunc != nil {
		return m.ListAgentsFunc(userID)
	}
	return nil, nil
}

func (m *MockBusClient) GetConfigItems(componentType string) ([]nanoprotocol.ConfigItem, error) {
	if m.GetConfigFunc != nil {
		return m.GetConfigFunc(componentType)
	}
	return nil, nil
}

func (m *MockBusClient) ConfigChanges() <-chan nanoprotocol.ConfigChange {
	if m.ConfigChangesCh != nil {
		return m.ConfigChangesCh
	}
	return make(chan nanoprotocol.ConfigChange)
}

func (m *MockBusClient) SaveSession(req nanoprotocol.SaveSessionRequest) error {
	if m.SaveSessionFunc != nil {
		return m.SaveSessionFunc(req)
	}
	return nil
}

func (m *MockBusClient) SetAgentPermission(req nanoprotocol.SetAgentPermissionRequest) error {
	return nil
}

func TestAgent_ProcessMessage(t *testing.T) {
	// Setup dependencies
	tmpDir, _ := os.MkdirTemp("", "agent-test")
	defer os.RemoveAll(tmpDir)

	sessStore, _ := session.NewStore(tmpDir)
	toolReg := tool.NewRegistry()
	mockProvider := provider.NewMock()

	// Capture outbound completions
	var completions []nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completions = append(completions, c)
			return nil
		},
	}

	cfg := Config{
		UserID:        "test-user",
		Model:         "test-model",
		MaxIterations: 5,
		SessionDir:    tmpDir,
	}

	agent, _ := New(cfg, mockBus, mockProvider, toolReg)
	agent.sessions = sessStore // Override with test store

	// Test Case 1: Simple Q&A
	t.Run("SimpleResponse", func(t *testing.T) {
		// Mock LLM response
		mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "Hello there!"}, nil
		}

		inbound := &nanoprotocol.InboundMessage{
			UserID:    "test-user",
			SessionID: "test:1",
			Content:   "Hi",
		}

		err := agent.processMessage(context.Background(), inbound)
		if err != nil {
			t.Fatalf("processMessage failed: %v", err)
		}

		if len(completions) != 1 {
			t.Fatalf("Expected 1 outbound complete, got %d", len(completions))
		}
		if completions[0].FullContent != "Hello there!" {
			t.Errorf("Expected content 'Hello there!', got '%s'", completions[0].FullContent)
		}
		if completions[0].MessageID == "" {
			t.Error("Expected outbound MessageID to be generated")
		}

		// Verify session: the user message, the mode note injected for the turn,
		// and the assistant reply.
		sess := sessStore.GetOrCreate("test:1")
		msgs := sess.GetMessages()
		if len(msgs) != 3 { // User + mode note + Assistant
			t.Errorf("Expected 3 messages in session, got %d", len(msgs))
		}
		notes := 0
		for _, m := range msgs {
			if m.Role == "system" && strings.Contains(m.Content, "【模式状态】") {
				notes++
			}
		}
		if notes != 1 {
			t.Errorf("expected one mode note row in the turn, got %d", notes)
		}
	})
}

// withoutModeNotes drops the per-turn mode notes a real turn injects, so tests
// that assert the conversation's shape keep their indices now that the note is
// wired in. A test that *wants* the note asserts on it directly.
func withoutModeNotes(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, modeNotePrefix) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func TestAgent_ProcessMessage_ToolUse(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "agent-tool-test")
	defer os.RemoveAll(tmpDir)

	sessStore, _ := session.NewStore(tmpDir)
	toolReg := tool.NewRegistry()
	toolReg.Register(&MockTool{NameVal: "test-tool", ExecuteFunc: func(ctx context.Context, params map[string]any) (string, error) {
		return "tool result", nil
	}})

	mockProvider := provider.NewMock()
	var completions []nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completions = append(completions, c)
			return nil
		},
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, toolReg)
	agent.sessions = sessStore

	// Mock LLM sequence: Tool Call -> Final Answer
	step := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		step++
		if step == 1 {
			return &provider.ChatResponse{
				ToolCalls: []provider.ToolCall{
					{ID: "call-1", Name: "test-tool", Arguments: "{}"},
				},
			}, nil
		}
		return &provider.ChatResponse{Content: "Final answer based on tool result"}, nil
	}

	inbound := &nanoprotocol.InboundMessage{UserID: "test-user", SessionID: "test:2", Content: "Run tool"}
	err := agent.processMessage(context.Background(), inbound)
	if err != nil {
		t.Fatalf("processMessage failed: %v", err)
	}

	if len(completions) != 1 {
		t.Fatalf("Expected 1 outbound complete, got %d", len(completions))
	}
	if completions[0].FullContent != "Final answer based on tool result" {
		t.Errorf("Unexpected content: %s", completions[0].FullContent)
	}

	// Verify session history: User -> Assistant(ToolCall) -> Tool(Result) -> Assistant(Final)
	sess := sessStore.GetOrCreate("test:2")
	msgs := withoutModeNotes(sess.GetMessages())
	if len(msgs) != 4 {
		t.Errorf("Expected 4 messages, got %d", len(msgs))
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) == 0 {
		t.Error("Expected tool call at index 1")
	}
	if msgs[2].Role != "tool" || msgs[2].Content != "tool result" {
		t.Error("Expected tool result at index 2")
	}
}

func TestAgent_MaxIterations(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "agent-iter-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	mockProvider := provider.NewMock()
	// Always return tool call to cause infinite loop
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{
			ToolCalls: []provider.ToolCall{{ID: "loop", Name: "test-tool", Arguments: "{}"}},
		}, nil
	}

	agent, _ := New(Config{MaxIterations: 2, SessionDir: tmpDir}, &MockBusClient{}, mockProvider, tool.NewRegistry())
	agent.sessions = sessStore

	// Add mock tool to avoid execution error masking the loop error
	agent.tools.Register(&MockTool{NameVal: "test-tool"})

	inbound := &nanoprotocol.InboundMessage{SessionID: "test:3"}
	err := agent.processMessage(context.Background(), inbound)

	if err == nil {
		t.Error("Expected error for max iterations, got nil")
	} else if !strings.Contains(err.Error(), "max iterations (2) reached") ||
		!strings.Contains(err.Error(), "category=max_iterations") {
		t.Errorf("Expected wrapped max iterations error, got: %v", err)
	}
}

func TestAgent_ProcessMessage_StreamsChunks(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)

	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 6)
		go func() {
			ch <- provider.ChatStreamEvent{Kind: "thought", Delta: "Let me think about this."}
			ch <- provider.ChatStreamEvent{Kind: "thought", Delta: " Done."}
			// Give the reasoning a measurable wall-clock span so ThoughtElapsedMs
			// is non-zero (the model "thinks" before answering).
			time.Sleep(30 * time.Millisecond)
			for _, d := range []string{"Hello", " there", "!"} {
				ch <- provider.ChatStreamEvent{Kind: "content", Delta: d}
			}
			ch <- provider.ChatStreamEvent{Done: true, FullContent: "Hello there!"}
			close(ch)
		}()
		return ch, nil
	}

	var chunks []nanoprotocol.OutboundChunk
	var completions []nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostChunkFunc: func(chunk nanoprotocol.OutboundChunk) error {
			chunks = append(chunks, chunk)
			return nil
		},
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completions = append(completions, c)
			return nil
		},
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	agent.sessions = sessStore

	inbound := &nanoprotocol.InboundMessage{UserID: "test-user", SessionID: "test:stream", Content: "hi"}
	if err := agent.processMessage(context.Background(), inbound); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if len(chunks) != 5 {
		t.Fatalf("expected 5 chunks (2 thought + 3 content), got %d", len(chunks))
	}
	var thought, content string
	var contentKind int
	for _, c := range chunks {
		switch c.Kind {
		case nanoprotocol.ChunkKindThought:
			thought += c.Content
		case nanoprotocol.ChunkKindContent:
			content += c.Content
			contentKind++
		default:
			t.Errorf("unexpected chunk kind %q", c.Kind)
		}
	}
	if thought != "Let me think about this. Done." {
		t.Errorf("unexpected thought text %q", thought)
	}
	if content != "Hello there!" {
		t.Errorf("expected streamed content %q, got %q", "Hello there!", content)
	}
	if contentKind != 3 {
		t.Errorf("expected 3 content chunks, got %d", contentKind)
	}
	if len(completions) != 1 || completions[0].FullContent != "Hello there!" {
		t.Fatalf("expected 1 complete with full content, got %+v", completions)
	}
	if len(completions[0].MessageID) == 0 {
		t.Error("expected complete message id to match the chunk message id")
	}
	if completions[0].MessageID != chunks[0].MessageID {
		t.Errorf("chunk and complete should share the message id: chunk=%q complete=%q", chunks[0].MessageID, completions[0].MessageID)
	}

	sess := sessStore.GetOrCreate("test:stream")
	msgs := withoutModeNotes(sess.GetMessages())
	if len(msgs) != 3 {
		t.Fatalf("expected 3 persisted messages (user + thought + assistant), got %d", len(msgs))
	}
	if msgs[1].Role != "thought" || msgs[1].Content != "Let me think about this. Done." {
		t.Errorf("expected the streamed thought persisted, got role=%q content=%q", msgs[1].Role, msgs[1].Content)
	}
	if msgs[1].ThoughtElapsedMs < 20 {
		t.Errorf("expected a measured thought duration, got %dms", msgs[1].ThoughtElapsedMs)
	}
	if msgs[2].Content != "Hello there!" {
		t.Errorf("expected the assembled reply in the session, got %q", msgs[2].Content)
	}
	if msgs[2].Reasoning != "Let me think about this. Done." {
		t.Errorf("expected reasoning attached to the assistant row for replay, got %q", msgs[2].Reasoning)
	}
	// The LLM-facing sequence must exclude thoughts.
	if conv := withoutModeNotes(sess.ConversationMessages()); len(conv) != 2 {
		t.Errorf("expected 2 conversation messages (thought excluded), got %d", len(conv))
	}
	if conv := sess.ConversationMessages(); conv[len(conv)-1].Reasoning != "Let me think about this. Done." {
		t.Errorf("expected reasoning to survive into the LLM-facing messages, got %q", conv[len(conv)-1].Reasoning)
	}
}

func TestAgent_ToolUseEmitsChunks(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)
	toolReg := tool.NewRegistry()
	toolReg.Register(&MockTool{NameVal: "test-tool", ExecuteFunc: func(ctx context.Context, params map[string]any) (string, error) {
		return "tool output", nil
	}})

	mockProvider := provider.NewMock()
	step := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		step++
		if step == 1 {
			return &provider.ChatResponse{
				ToolCalls: []provider.ToolCall{{ID: "call-1", Name: "test-tool", Arguments: `{"a":1}`}},
			}, nil
		}
		return &provider.ChatResponse{Content: "final"}, nil
	}

	var chunks []nanoprotocol.OutboundChunk
	mockBus := &MockBusClient{
		PostChunkFunc: func(chunk nanoprotocol.OutboundChunk) error {
			chunks = append(chunks, chunk)
			return nil
		},
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, toolReg)
	agent.sessions = sessStore

	if err := agent.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "test-user", SessionID: "test:tools", Content: "use a tool",
	}); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	var toolUse, toolResult bool
	for _, c := range chunks {
		switch c.Kind {
		case nanoprotocol.ChunkKindToolUse:
			toolUse = true
			if c.Content != `{"name":"test-tool","arguments":"{\"a\":1}"}` {
				t.Errorf("unexpected tool_use payload %q", c.Content)
			}
		case nanoprotocol.ChunkKindToolResult:
			toolResult = true
			if c.Content != "tool output" {
				t.Errorf("unexpected tool_result payload %q", c.Content)
			}
		}
	}
	if !toolUse {
		t.Error("expected a tool_use chunk before tool execution")
	}
	if !toolResult {
		t.Error("expected a tool_result chunk after tool execution")
	}
}

// TestAgent_ToolCallPersistsReasoning verifies that the reasoning text is
// attached to the assistant rows (both the tool-call turn and the final
// answer) so thinking models can replay reasoning_content on later requests.
func TestAgent_ToolCallPersistsReasoning(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)
	toolReg := tool.NewRegistry()
	toolReg.Register(&MockTool{NameVal: "test-tool", ExecuteFunc: func(ctx context.Context, params map[string]any) (string, error) {
		return "tool output", nil
	}})

	mockProvider := provider.NewMock()
	step := 0
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		step++
		ch := make(chan provider.ChatStreamEvent, 4)
		go func() {
			if step == 1 {
				ch <- provider.ChatStreamEvent{Kind: "thought", Delta: "need a tool"}
				ch <- provider.ChatStreamEvent{Done: true, ToolCalls: []provider.ToolCall{{ID: "call-1", Name: "test-tool", Arguments: `{"a":1}`}}}
			} else {
				ch <- provider.ChatStreamEvent{Kind: "thought", Delta: "summarize"}
				ch <- provider.ChatStreamEvent{Kind: "content", Delta: "final"}
				ch <- provider.ChatStreamEvent{Done: true, FullContent: "final"}
			}
			close(ch)
		}()
		return ch, nil
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, &MockBusClient{}, mockProvider, toolReg)
	agent.sessions = sessStore

	if err := agent.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "test-user", SessionID: "test:reasoning", Content: "use a tool",
	}); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	msgs := sessStore.GetOrCreate("test:reasoning").GetMessages()
	var toolAssistant, finalAssistant *provider.Message
	for i := range msgs {
		m := msgs[i]
		if m.Role != "assistant" {
			continue
		}
		if len(m.ToolCalls) > 0 {
			toolAssistant = &m
		} else if m.Content == "final" {
			finalAssistant = &m
		}
	}
	if toolAssistant == nil || toolAssistant.Reasoning != "need a tool" {
		t.Fatalf("tool-call assistant reasoning not persisted: %+v", toolAssistant)
	}
	if finalAssistant == nil || finalAssistant.Reasoning != "summarize" {
		t.Fatalf("final assistant reasoning not persisted: %+v", finalAssistant)
	}
}

// unusableParallelCall is the shape from production: the model writes the batch
// correctly at first and then breaks the escaping of a nested command, so the
// arguments are not valid JSON while finish_reason says "tool_calls".
const unusableParallelCall = "{\"calls\": [{\"tool\": \"bash\", \"arguments\": {\"command\": \"echo one\necho two\"}}]}"

// TestAgent_UnusableToolCallIsAnsweredAndTheTurnContinues verifies the recovery
// that replaced "fail the turn on a malformed tool call": the call is never
// executed and never persisted as the model wrote it, but it is answered with an
// error result — the same shape as a tool that failed — so the model sees what
// it wrote and can re-emit it. The turn continues instead of dying.
func TestAgent_UnusableToolCallIsAnsweredAndTheTurnContinues(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)
	toolReg := tool.NewRegistry()
	toolReg.Register(&MockTool{NameVal: "test-tool", ExecuteFunc: func(ctx context.Context, params map[string]any) (string, error) {
		return "tool output", nil
	}})

	mockProvider := provider.NewMock()
	var requests []provider.ChatRequest
	step := 0
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		requests = append(requests, req)
		step++
		ch := make(chan provider.ChatStreamEvent, 2)
		go func() {
			switch step {
			case 1:
				ch <- provider.ChatStreamEvent{
					Done:         true,
					FinishReason: "tool_calls",
					ToolCalls: []provider.ToolCall{
						{ID: "bad-1", Name: "parallel_execution", Arguments: unusableParallelCall},
					},
				}
			case 2:
				ch <- provider.ChatStreamEvent{
					Done:         true,
					FinishReason: "tool_calls",
					ToolCalls: []provider.ToolCall{
						{ID: "ok-1", Name: "test-tool", Arguments: `{"a":1}`},
					},
				}
			default:
				ch <- provider.ChatStreamEvent{Done: true, FullContent: "final"}
			}
			close(ch)
		}()
		return ch, nil
	}

	var completions []nanoprotocol.OutboundComplete
	var chunks []nanoprotocol.OutboundChunk
	mockBus := &MockBusClient{
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completions = append(completions, c)
			return nil
		},
		PostChunkFunc: func(chunk nanoprotocol.OutboundChunk) error {
			chunks = append(chunks, chunk)
			return nil
		},
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, toolReg)
	agent.sessions = sessStore

	sid := "test:unusable-args"
	if err := agent.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "test-user", SessionID: sid, Content: "hi",
	}); err != nil {
		t.Fatalf("the turn should continue after an unusable tool call, got %v", err)
	}
	if len(completions) != 1 || completions[0].Error != "" {
		t.Fatalf("expected one successful completion, got %+v", completions)
	}

	// The model was told what it wrote: the next request carries the answer to
	// the dropped call, addressed to its id.
	if len(requests) < 2 {
		t.Fatalf("expected a follow-up request, got %d", len(requests))
	}
	var notice, rawPersisted bool
	for _, m := range requests[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "bad-1" {
			notice = strings.Contains(m.Content, "not valid JSON") && strings.Contains(m.Content, "nothing ran")
		}
	}
	if !notice {
		t.Errorf("the dropped call was not answered in the next request: %+v", requests[1].Messages)
	}

	// The transcript keeps a legal placeholder instead of the model's bytes, and
	// every call in it stays acceptable to the provider.
	sess := sessStore.GetOrCreate(sid)
	for _, m := range sess.ConversationMessages() {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if !json.Valid([]byte(tc.Arguments)) {
					t.Fatalf("unusable arguments reached the transcript: %+v", tc)
				}
				if tc.ID == "bad-1" {
					rawPersisted = true
					if tc.Arguments != "{}" {
						t.Errorf("dropped call should be persisted as {}, got %q", tc.Arguments)
					}
				}
			}
		}
	}
	if !rawPersisted {
		t.Error("expected the dropped call to be recorded (as a legal placeholder) so it can be answered")
	}

	// The batch ran: the good call executed after the bad one was answered.
	var executed, showedRaw bool
	for _, c := range chunks {
		if c.Kind == nanoprotocol.ChunkKindToolResult && c.Content == "tool output" {
			executed = true
		}
		if c.Kind == nanoprotocol.ChunkKindToolUse && strings.Contains(c.Content, "echo one") {
			showedRaw = true
		}
	}
	if !executed {
		t.Error("the following tool call never executed")
	}
	if !showedRaw {
		t.Error("the tool_use card should show what the model actually wrote")
	}
}

// TestAgent_UnusableToolCallsExhaustTheirRecoveryBudget verifies the bound: a
// model that keeps producing unusable arguments gets a few chances, then the
// turn fails with the dedicated category instead of consuming the iteration
// budget forever.
func TestAgent_UnusableToolCallsExhaustTheirRecoveryBudget(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)

	mockProvider := provider.NewMock()
	attempts := 0
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		attempts++
		ch := make(chan provider.ChatStreamEvent, 2)
		go func() {
			ch <- provider.ChatStreamEvent{
				Done:         true,
				FinishReason: "tool_calls",
				ToolCalls: []provider.ToolCall{
					{ID: "bad", Name: "parallel_execution", Arguments: unusableParallelCall},
				},
			}
			close(ch)
		}()
		return ch, nil
	}

	var completions []nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completions = append(completions, c)
			return nil
		},
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 10, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	agent.sessions = sessStore

	sid := "test:unusable-budget"
	err := agent.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "test-user", SessionID: sid, Content: "hi",
	})
	if err == nil {
		t.Fatal("expected the turn to fail once the recovery budget is spent")
	}
	if attempts != maxToolCallRecoveries+1 {
		t.Errorf("attempts = %d, want %d (budget + the failing call)", attempts, maxToolCallRecoveries+1)
	}
	if len(completions) != 1 {
		t.Fatalf("expected exactly one (error) completion, got %d", len(completions))
	}
	if completions[0].Error != nanoprotocol.TurnErrorToolCallIncomplete {
		t.Errorf("completion error = %q, want %q", completions[0].Error, nanoprotocol.TurnErrorToolCallIncomplete)
	}
	if !strings.Contains(completions[0].ErrorDetail, `"parallel_execution"`) {
		t.Errorf("ErrorDetail should name the tool, got %q", completions[0].ErrorDetail)
	}
	// The malformed call never reaches the transcript.
	for _, m := range sessStore.GetOrCreate(sid).ConversationMessages() {
		for _, tc := range m.ToolCalls {
			if !json.Valid([]byte(tc.Arguments)) {
				t.Fatalf("unusable arguments reached the transcript: %+v", tc)
			}
		}
	}
}

// TestAgent_EmptyCompletionRetriesOnceThenFails verifies the empty-completion
// guard: a provider completion with no content and no tool calls is retried
// once, and when the retry is also empty the turn fails with a visible error
// instead of persisting an empty assistant row (which would wedge the session
// with a provider "Invalid assistant message: content or tool_calls must be
// set" on every later request).
func TestAgent_EmptyCompletionRetriesOnceThenFails(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)

	mockProvider := provider.NewMock()
	callCount := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		callCount++
		return &provider.ChatResponse{Content: ""}, nil
	}

	var completions []nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completions = append(completions, c)
			return nil
		},
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	agent.sessions = sessStore

	sid := "test:empty"
	err := agent.processMessage(context.Background(), &nanoprotocol.InboundMessage{UserID: "test-user", SessionID: sid, Content: "hi"})
	if err == nil {
		t.Fatal("expected the turn to fail after an empty completion and an empty retry")
	}

	// Exactly one retry: two LLM calls total, then failure.
	if callCount != 2 {
		t.Fatalf("expected exactly 2 LLM calls (1 + retry), got %d", callCount)
	}

	// The completion carries a failure category and no success content.
	if len(completions) != 1 {
		t.Fatalf("expected exactly one (error) completion, got %d", len(completions))
	}
	if completions[0].Error != nanoprotocol.TurnErrorProviderUnavailable {
		t.Errorf("expected category %q, got %q", nanoprotocol.TurnErrorProviderUnavailable, completions[0].Error)
	}
	if completions[0].FullContent != "" {
		t.Errorf("expected empty full content on the error completion, got %q", completions[0].FullContent)
	}

	// The session must contain no empty assistant row and exactly one error row.
	sess := sessStore.GetOrCreate(sid)
	var sawEmptyAssistant, sawErrorRow bool
	for _, m := range sess.GetMessages() {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
			sawEmptyAssistant = true
		}
		if m.Role == "error" {
			sawErrorRow = true
		}
	}
	if sawEmptyAssistant {
		t.Fatal("empty assistant row was persisted to the session")
	}
	if !sawErrorRow {
		t.Fatal("expected a persisted error row in the session")
	}
}

// TestAgent_EmptyCompletionRetrySucceeds verifies that a single retry can
// recover a healthy turn after one empty completion.
func TestAgent_EmptyCompletionRetrySucceeds(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)

	mockProvider := provider.NewMock()
	callCount := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		callCount++
		if callCount == 1 {
			return &provider.ChatResponse{Content: ""}, nil
		}
		return &provider.ChatResponse{Content: "recovered"}, nil
	}

	var completions []nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completions = append(completions, c)
			return nil
		},
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	agent.sessions = sessStore

	sid := "test:empty-recover"
	err := agent.processMessage(context.Background(), &nanoprotocol.InboundMessage{UserID: "test-user", SessionID: sid, Content: "hi"})
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 LLM calls (1 empty + 1 retry), got %d", callCount)
	}
	if len(completions) != 1 || completions[0].FullContent != "recovered" {
		t.Fatalf("expected one success completion with %q, got %+v", "recovered", completions)
	}

	sess := sessStore.GetOrCreate(sid)
	msgs := withoutModeNotes(sess.GetMessages())
	if len(msgs) != 2 { // user + assistant
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "recovered" {
		t.Errorf("unexpected final assistant row: %+v", msgs[1])
	}
}

// TestAgent_WedgedSessionHealsOnSanitize verifies that a persisted session
// containing birth-empty assistant rows (written by older code) still yields a
// valid provider request: sanitization drops the poison rows at request time.
func TestAgent_WedgedSessionHealsOnSanitize(t *testing.T) {
	tmpDir := t.TempDir()
	sessStore, _ := session.NewStore(tmpDir)
	sess := sessStore.GetOrCreate("test:heal")
	sess.AddMessage(provider.Message{Role: "user", Content: "explore"})
	sess.AddMessage(provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{
		{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
	}})
	sess.AddMessage(provider.Message{Role: "tool", ToolCallID: "c1", Content: "file list"})
	sess.AddMessage(provider.Message{Role: "assistant"}) // poison from older code
	sess.AddMessage(provider.Message{Role: "user", Content: "please continue"})
	if err := sessStore.Save(sess); err != nil {
		t.Fatal(err)
	}

	mockProvider := provider.NewMock()
	var sent []provider.ChatRequest
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		sent = append(sent, req)
		return &provider.ChatResponse{Content: "here you go"}, nil
	}

	agent, _ := New(Config{UserID: "test-user", MaxIterations: 5, SessionDir: tmpDir}, &MockBusClient{}, mockProvider, tool.NewRegistry())
	agent.sessions = sessStore

	if err := agent.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "test-user", SessionID: "test:heal", Content: "please continue",
	}); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if len(sent) != 1 {
		t.Fatalf("expected 1 LLM request, got %d", len(sent))
	}
	for _, m := range sent[0].Messages {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("poisoned empty assistant row reached the provider: %+v", m)
		}
	}
}
