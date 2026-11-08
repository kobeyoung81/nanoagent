package tool

import (
	"context"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// BusClient is the subset of the bus client used by tools that send messages
// or media (interface for testability).
type BusClient interface {
	PostOutbound(msg nanoprotocol.OutboundMessage) error
}

// Tool defines the interface that all agent tools must implement.
type Tool interface {
	Name() string
	Description() string
	Parameters() provider.ParameterSchema
	Execute(ctx context.Context, params map[string]any) (string, error)
}
