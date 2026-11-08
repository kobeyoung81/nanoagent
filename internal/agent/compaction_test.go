package agent

import (
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

func TestCompactionFloor(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "1", Name: "bash"}}},
		{Role: "tool", ToolCallID: "1", Content: "result"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "a3"},
		{Role: "user", Content: "u4"},
		{Role: "assistant", Content: "a4"},
	}
	// 8 text messages; keeping 6 lands on u2 (index 4), which is already a user
	// boundary and retains exactly the last 6 text messages.
	if got := compactionFloor(msgs, 6); got != 4 {
		t.Fatalf("compactionFloor = %d, want 4", got)
	}

	// When the 6th-from-last text message is an assistant message, the floor is
	// backed up to the nearest preceding user so no tool call is orphaned.
	// Text: u1,a1,u2,a2,u3,a3,u4,a4,u5 -> 6th from end is a2 (index 3), backed up
	// to u2 (index 2).
	short := []provider.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "a3"},
		{Role: "user", Content: "u4"},
		{Role: "assistant", Content: "a4"},
		{Role: "user", Content: "u5"},
	}
	if got := compactionFloor(short, 6); got != 2 {
		t.Fatalf("compactionFloor = %d, want 2", got)
	}

	// Fewer than `keep` text messages -> floor 0 (no drop).
	if got := compactionFloor(short[:3], 6); got != 0 {
		t.Fatalf("compactionFloor = %d, want 0", got)
	}
}
