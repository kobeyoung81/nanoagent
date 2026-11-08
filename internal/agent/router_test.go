package agent

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

type mockBusClient struct {
	agents        []nanoprotocol.AgentRecord
	configItems   []nanoprotocol.ConfigItem
	configChanges chan nanoprotocol.ConfigChange
	outbound      []nanoprotocol.OutboundMessage
	completions   []nanoprotocol.OutboundComplete
}

func newMockBusClient(agents ...nanoprotocol.AgentRecord) *mockBusClient {
	return &mockBusClient{
		agents:        agents,
		configChanges: make(chan nanoprotocol.ConfigChange, 1),
	}
}

func (m *mockBusClient) PollInbound(userID string, bindingID int64, agentID string) (*nanoprotocol.InboundMessage, error) {
	return nil, nil
}

func (m *mockBusClient) PostOutbound(msg nanoprotocol.OutboundMessage) error {
	m.outbound = append(m.outbound, msg)
	return nil
}

func (m *mockBusClient) PostOutboundChunk(chunk nanoprotocol.OutboundChunk) error {
	return nil
}

func (m *mockBusClient) PostOutboundComplete(complete nanoprotocol.OutboundComplete) error {
	m.completions = append(m.completions, complete)
	return nil
}

func (m *mockBusClient) ListAgents(userID string) ([]nanoprotocol.AgentRecord, error) {
	return m.agents, nil
}

func (m *mockBusClient) GetConfigItems(componentType string) ([]nanoprotocol.ConfigItem, error) {
	return m.configItems, nil
}

func (m *mockBusClient) ConfigChanges() <-chan nanoprotocol.ConfigChange {
	return m.configChanges
}

func (m *mockBusClient) SaveSession(req nanoprotocol.SaveSessionRequest) error {
	return nil
}

func (m *mockBusClient) SetAgentPermission(req nanoprotocol.SetAgentPermissionRequest) error {
	return nil
}

func (m *mockBusClient) Close() error {
	return nil
}

func makeAgent(id, status string) nanoprotocol.AgentRecord {
	return nanoprotocol.AgentRecord{
		ID:     id,
		Status: status,
	}
}

// testAgentBuilder builds a minimal Agent per record for router tests.
func testAgentBuilder(t *testing.T, mock BusClient) AgentBuilder {
	t.Helper()
	return func(record nanoprotocol.AgentRecord) (*Agent, error) {
		cfg := Config{
			UserID:        "user-1",
			AgentID:       record.ID,
			Model:         "test-model",
			MaxIterations: 3,
			SessionDir:    t.TempDir(),
		}
		return New(cfg, mock, provider.NewMock(), tool.NewRegistry())
	}
}

func TestNewAgentRouter_LoadsAgents(t *testing.T) {
	mock := newMockBusClient(
		makeAgent("agent-1", "active"),
		makeAgent("agent-2", "active"),
	)

	r, err := NewAgentRouter("user-1", mock, testAgentBuilder(t, mock))
	if err != nil {
		t.Fatalf("NewAgentRouter: %v", err)
	}
	if r.defaultID != "agent-1" {
		t.Fatalf("expected default agent-1, got %s", r.defaultID)
	}
	if len(r.agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(r.agents))
	}
}

func TestNewAgentRouter_NoAgents(t *testing.T) {
	mock := newMockBusClient()
	_, err := NewAgentRouter("user-1", mock, testAgentBuilder(t, mock))
	if err == nil {
		t.Fatal("expected error for no agents")
	}
}

func TestNewAgentRouter_SkipsSuspended(t *testing.T) {
	mock := newMockBusClient(
		makeAgent("agent-1", "active"),
		makeAgent("agent-2", "suspended"),
	)

	r, err := NewAgentRouter("user-1", mock, testAgentBuilder(t, mock))
	if err != nil {
		t.Fatalf("NewAgentRouter: %v", err)
	}
	if len(r.agents) != 1 {
		t.Fatalf("expected 1 active agent, got %d", len(r.agents))
	}
	if _, ok := r.agents["agent-2"]; ok {
		t.Fatal("suspended agent should not be loaded")
	}
}

func TestResolveAgent_ExplicitID(t *testing.T) {
	mock := newMockBusClient(
		makeAgent("agent-1", "active"),
		makeAgent("agent-2", "active"),
	)

	r, _ := NewAgentRouter("user-1", mock, testAgentBuilder(t, mock))
	h := r.resolveAgent("agent-2")
	if h == nil {
		t.Fatal("expected handler for agent-2")
	}
	if h.ID != "agent-2" {
		t.Fatalf("expected agent-2, got %s", h.ID)
	}
}

func TestResolveAgent_DefaultFallback(t *testing.T) {
	mock := newMockBusClient(
		makeAgent("agent-1", "active"),
	)

	r, _ := NewAgentRouter("user-1", mock, testAgentBuilder(t, mock))
	h := r.resolveAgent("")
	if h == nil {
		t.Fatal("expected default handler")
	}
	if h.ID != "agent-1" {
		t.Fatalf("expected default agent-1, got %s", h.ID)
	}
}

func TestResolveAgent_UnknownFallsBackToDefault(t *testing.T) {
	mock := newMockBusClient(
		makeAgent("agent-1", "active"),
	)

	r, _ := NewAgentRouter("user-1", mock, testAgentBuilder(t, mock))
	h := r.resolveAgent("nonexistent")
	if h == nil {
		t.Fatal("expected fallback handler")
	}
	if h.ID != "agent-1" {
		t.Fatalf("expected fallback to agent-1, got %s", h.ID)
	}
}
