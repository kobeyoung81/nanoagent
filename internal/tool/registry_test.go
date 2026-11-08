package tool

import (
	"context"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// nameOnly is a minimal tool used to pin the definition ordering.
type nameOnly string

func (n nameOnly) Name() string                          { return string(n) }
func (n nameOnly) Description() string                   { return "test tool" }
func (n nameOnly) Parameters() provider.ParameterSchema  { return provider.ParameterSchema{Type: "object"} }
func (n nameOnly) Execute(context.Context, map[string]any) (string, error) { return "", nil }

// TestDefinitionsOrderIsStable is the regression for the prompt-cache prefix:
// the tool definitions used to come out of a map iteration, so the order (and
// therefore the cached prefix) changed on every turn. Measured against the
// upstream: swapping two adjacent definitions dropped cached tokens from 99.7%
// to 0.
func TestDefinitionsOrderIsStable(t *testing.T) {
	reg := NewRegistry()
	// Registered in a deliberately non-alphabetical order.
	for _, n := range []string{"write", "glob", "bash", "read", "edit"} {
		reg.Register(nameOnly(n))
	}

	first := reg.Definitions()
	want := []string{"bash", "edit", "glob", "read", "write"}
	if len(first) != len(want) {
		t.Fatalf("expected %d definitions, got %d", len(want), len(first))
	}
	for i, name := range want {
		if first[i].Name != name {
			t.Fatalf("definition %d = %q, want %q (definitions must be sorted by name)", i, first[i].Name, name)
		}
	}

	for i := 0; i < 20; i++ {
		again := reg.Definitions()
		for j := range again {
			if again[j].Name != first[j].Name {
				t.Fatalf("call %d changed the order at %d: %q vs %q", i, j, again[j].Name, first[j].Name)
			}
		}
	}

	names := reg.Names()
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("Names()[%d] = %q, want %q", i, names[i], name)
		}
	}
}
