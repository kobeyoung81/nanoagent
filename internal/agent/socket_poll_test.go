package agent

// The poll cadence is the visible half of the agent socket: with a connection the
// bus says when work arrives and the poll only has to catch a lost notice; with
// none the poll is the only source of work and keeps the old half-second.

import (
	"testing"
)

// socketBus is a mock bus that reports a socket and can signal work.
type socketBus struct {
	*mockBusClient
	connected bool
	ready     chan struct{}
}

func (s *socketBus) SocketConnected() bool         { return s.connected }
func (s *socketBus) InboundReady() <-chan struct{} { return s.ready }

func TestPollIntervalIsASafetynetOnlyWithASocket(t *testing.T) {
	a := &Agent{}

	a.busClient = newMockBusClient()
	if got := a.pollInterval(); got != fallbackPollInterval {
		t.Fatalf("without a socket the poll is the only source of work: got %s", got)
	}

	disconnected := &socketBus{mockBusClient: newMockBusClient(), connected: false}
	a.busClient = disconnected
	if got := a.pollInterval(); got != fallbackPollInterval {
		t.Fatalf("a dropped socket must fall back to polling: got %s", got)
	}

	connected := &socketBus{mockBusClient: newMockBusClient(), connected: true}
	a.busClient = connected
	if got := a.pollInterval(); got != socketPollInterval {
		t.Fatalf("with a socket the poll is a safety net: got %s", got)
	}
	if socketPollInterval < 10*fallbackPollInterval {
		t.Fatalf("the safety net is supposed to be much slower: %s vs %s", socketPollInterval, fallbackPollInterval)
	}
}

func TestInboundReadyIsNilWithoutASocket(t *testing.T) {
	a := &Agent{busClient: newMockBusClient()}
	if got := a.inboundReady(); got != nil {
		t.Fatalf("a client without a socket must not offer a wake-up channel, got %v", got)
	}

	ready := make(chan struct{}, 1)
	a.busClient = &socketBus{mockBusClient: newMockBusClient(), ready: ready}
	if got := a.inboundReady(); got == nil {
		t.Fatal("expected the socket's wake-up channel")
	}
	select {
	case <-a.inboundReady():
		t.Fatal("nothing was pushed")
	default:
	}
}
