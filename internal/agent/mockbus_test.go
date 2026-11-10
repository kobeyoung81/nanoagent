package agent

import (
	"gitee.com/kobeyoung81/nanoprotocol"
)

// mockBusClient is the message plane the tests stand in for. The interface no
// longer carries the portal's data (agents, sessions): those moved to
// internal/portalclient, so the mock only has to be the bus.
type mockBusClient struct {
	configItems   []nanoprotocol.ConfigItem
	configChanges chan nanoprotocol.ConfigChange
	outbound      []nanoprotocol.OutboundMessage
	completions   []nanoprotocol.OutboundComplete
}

func newMockBusClient(_ ...nanoprotocol.AgentRecord) *mockBusClient {
	return &mockBusClient{configChanges: make(chan nanoprotocol.ConfigChange, 1)}
}

func (m *mockBusClient) PollInbound(string, int64, string) (*nanoprotocol.InboundMessage, error) {
	return nil, nil
}

func (m *mockBusClient) PostOutbound(msg nanoprotocol.OutboundMessage) error {
	m.outbound = append(m.outbound, msg)
	return nil
}

func (m *mockBusClient) PostOutboundChunk(nanoprotocol.OutboundChunk) error { return nil }

func (m *mockBusClient) PostOutboundComplete(complete nanoprotocol.OutboundComplete) error {
	m.completions = append(m.completions, complete)
	return nil
}

func (m *mockBusClient) GetConfigItems(string) ([]nanoprotocol.ConfigItem, error) {
	return m.configItems, nil
}

func (m *mockBusClient) ConfigChanges() <-chan nanoprotocol.ConfigChange {
	return m.configChanges
}

func (m *mockBusClient) SetAgentPermission(nanoprotocol.SetAgentPermissionRequest) error { return nil }

func (m *mockBusClient) Close() error { return nil }

// mockSessions is the conversation projection: it records what the syncer sent.
type mockSessions struct {
	saved []nanoprotocol.SaveSessionRequest
	err   error
}

func (m *mockSessions) SaveSession(req nanoprotocol.SaveSessionRequest) error {
	if m.err != nil {
		return m.err
	}
	m.saved = append(m.saved, req)
	return nil
}
