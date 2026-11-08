package agent

import (
	"os"
	"strings"
	"testing"
)

// TestPlaceholderAgentMatchesTemplate keeps the two copies of the identity stub
// in step: the image template seeds new agent homes, and the Go placeholder is
// the fallback when the template is missing. Divergence is invisible until an
// image ships without the template, which is exactly when it hurts.
func TestPlaceholderAgentMatchesTemplate(t *testing.T) {
	data, err := os.ReadFile("../../template/AGENT.md")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	got := strings.TrimRight(placeholderAGENT, "\n")
	want := strings.TrimRight(string(data), "\n")
	if got != want {
		t.Errorf("placeholderAGENT differs from template/AGENT.md:\n--- placeholder ---\n%s\n--- template ---\n%s", got, want)
	}
}
