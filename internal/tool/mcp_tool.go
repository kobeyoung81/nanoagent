package tool

import (
	"context"
	"encoding/json"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// McpCaller invokes a tool on a connected MCP server. Implemented by the agent
// package's MCP bridge so the adapter stays free of transport concerns.
type McpCaller interface {
	CallTool(ctx context.Context, serverID, toolName string, argumentsJSON string) (string, error)
}

// McpTool adapts a remote MCP tool to the local Tool interface. The name is
// namespaced (mcp__<server>__<tool>) so collisions with builtin tools and
// other servers are impossible.
type McpTool struct {
	NameValue        string
	DescriptionValue string
	RawSchema        []byte
	Caller           McpCaller
	ServerID         string
	ToolName         string
}

func (t *McpTool) Name() string { return t.NameValue }

func (t *McpTool) Description() string { return t.DescriptionValue }

func (t *McpTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{Type: "object", Properties: map[string]provider.PropertySchema{}}
}

// RawParameters returns the MCP server's native input schema verbatim.
func (t *McpTool) RawParameters() []byte { return t.RawSchema }

func (t *McpTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	return t.Caller.CallTool(ctx, t.ServerID, t.ToolName, string(raw))
}
