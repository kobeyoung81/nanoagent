package tool

import (
	"context"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"strings"
	"testing"
	"time"
)

// MockTool implements Tool for testing
type MockTool struct {
	NameVal     string
	DescVal     string
	ExecuteFunc func(ctx context.Context, params map[string]any) (string, error)
}

func (m *MockTool) Name() string                         { return m.NameVal }
func (m *MockTool) Description() string                  { return m.DescVal }
func (m *MockTool) Parameters() provider.ParameterSchema { return provider.ParameterSchema{} }
func (m *MockTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	if m.ExecuteFunc != nil {
		return m.ExecuteFunc(ctx, params)
	}
	return "mock result", nil
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	mock := &MockTool{NameVal: "test-tool", DescVal: "A test tool"}

	reg.Register(mock)

	// Test Get
	got, ok := reg.Get("test-tool")
	if !ok {
		t.Error("Get failed to return registered tool")
	}
	if got.Name() != "test-tool" {
		t.Errorf("Expected name 'test-tool', got '%s'", got.Name())
	}

	// Test Execute
	res, err := reg.Execute(context.Background(), "test-tool", `{"arg":"val"}`)
	if err != nil {
		t.Errorf("Execute failed: %v", err)
	}
	if res != "mock result" {
		t.Errorf("Expected 'mock result', got '%s'", res)
	}

	// Test Execute with invalid JSON
	_, err = reg.Execute(context.Background(), "test-tool", `{invalid-json`)
	if err == nil {
		t.Error("Expected error for invalid JSON arguments")
	}

	// Test Definitions
	defs := reg.Definitions()
	if len(defs) != 1 {
		t.Errorf("Expected 1 definition, got %d", len(defs))
	}
	if defs[0].Name != "test-tool" {
		t.Errorf("Expected definition name 'test-tool', got '%s'", defs[0].Name)
	}
}

func TestShellTool(t *testing.T) {
	st := NewShellTool(1*time.Second, "")

	if st.Name() != "bash" {
		t.Errorf("Expected name 'bash', got '%s'", st.Name())
	}

	// Test valid command
	res, err := st.Execute(context.Background(), map[string]any{"command": "echo 'hello'"})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res != "hello\n" {
		t.Errorf("Expected 'hello\\n', got '%s'", res)
	}

	// Test timeout (sleep 2s > 1s timeout)
	// Note: sleep 2 might vary on systems but usually reliable enough for unit test
	_, err = st.Execute(context.Background(), map[string]any{"command": "sleep 2"})
	if err == nil {
		t.Error("Expected timeout error")
	}

	// Test missing command
	_, err = st.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("Expected error for missing command param")
	}
}

func TestShellTool_DenyList_RmRf(t *testing.T) {
	st := NewShellTool(1*time.Second, "")
	cmds := []string{"rm -rf /", "rm -r /home", "rm -fr /tmp"}
	for _, cmd := range cmds {
		_, err := st.Execute(context.Background(), map[string]any{"command": cmd})
		if err == nil {
			t.Errorf("expected '%s' to be blocked", cmd)
		}
		if err != nil && !strings.Contains(err.Error(), "command blocked") {
			t.Errorf("expected 'command blocked' error for '%s', got: %v", cmd, err)
		}
	}
}

func TestShellTool_DenyList_Mkfs(t *testing.T) {
	st := NewShellTool(1*time.Second, "")
	_, err := st.Execute(context.Background(), map[string]any{"command": "mkfs.ext4 /dev/sda1"})
	if err == nil {
		t.Fatal("expected mkfs to be blocked")
	}
	if !strings.Contains(err.Error(), "command blocked") {
		t.Errorf("expected 'command blocked' error, got: %v", err)
	}
}

func TestShellTool_DenyList_ForkBomb(t *testing.T) {
	st := NewShellTool(1*time.Second, "")
	_, err := st.Execute(context.Background(), map[string]any{"command": ":(){ :|:& };:"})
	if err == nil {
		t.Fatal("expected fork bomb to be blocked")
	}
	if !strings.Contains(err.Error(), "command blocked") {
		t.Errorf("expected 'command blocked' error, got: %v", err)
	}
}

func TestShellTool_DenyList_DD(t *testing.T) {
	st := NewShellTool(1*time.Second, "")
	_, err := st.Execute(context.Background(), map[string]any{"command": "dd if=/dev/zero of=/dev/sda"})
	if err == nil {
		t.Fatal("expected dd to be blocked")
	}
	if !strings.Contains(err.Error(), "command blocked") {
		t.Errorf("expected 'command blocked' error, got: %v", err)
	}
}

func TestShellTool_DenyList_SafeRm(t *testing.T) {
	st := NewShellTool(1*time.Second, "")
	// "rm file.txt" should NOT be blocked (no -rf flags)
	_, err := st.Execute(context.Background(), map[string]any{"command": "rm file.txt"})
	// err may be non-nil because the file doesn't exist, but it should NOT be "command blocked"
	if err != nil && strings.Contains(err.Error(), "command blocked") {
		t.Errorf("'rm file.txt' should not be blocked, got: %v", err)
	}
}

func TestShellTool_DenyList_SafeCommand(t *testing.T) {
	st := NewShellTool(1*time.Second, "")
	cmds := []string{"ls -la", "echo hello", "pwd", "cat /dev/null"}
	for _, cmd := range cmds {
		_, err := st.Execute(context.Background(), map[string]any{"command": cmd})
		if err != nil && strings.Contains(err.Error(), "command blocked") {
			t.Errorf("'%s' should not be blocked", cmd)
		}
	}
}

// TestShellTool_TimeoutKillsProcessGroup is a regression test for a hang where
// a long-running child (spawned by the shell) survived the direct child's kill
// and kept the output pipe open, blocking Wait() forever. The command runs a
// background sleep that would outlive a 300ms timeout; the tool must return at
// the deadline.
func TestShellTool_TimeoutKillsProcessGroup(t *testing.T) {
	st := NewShellTool(300*time.Millisecond, "")
	start := time.Now()
	_, err := st.Execute(context.Background(), map[string]any{
		// Spawn a background child that holds the pipe open well past the
		// timeout, then block the foreground shell on a long sleep.
		"command": "sleep 30 & sleep 30",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("tool hung: took %v for a 300ms timeout", elapsed)
	}
}
