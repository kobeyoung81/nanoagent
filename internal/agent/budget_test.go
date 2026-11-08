package agent

import (
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// TestEstimateTokens_CJKNotUnderestimated guards the context-budget fix: CJK
// text is close to one token per rune, whereas the previous chars/2.5 ratio
// under-counted it by ~2.5x and let prompts far exceed the context window.
func TestEstimateTokens_CJKNotUnderestimated(t *testing.T) {
	cjk := estimateTokens(strings.Repeat("中", 1000))
	if cjk < 900 || cjk > 1100 {
		t.Fatalf("estimateTokens(1000 CJK runes) = %d, want ~1000", cjk)
	}
}

func TestEstimateTokens_ASCII(t *testing.T) {
	ascii := estimateTokens(strings.Repeat("a", 400))
	if ascii < 90 || ascii > 110 {
		t.Fatalf("estimateTokens(400 ASCII chars) = %d, want ~100", ascii)
	}
}

func TestEstimateTokens_Empty(t *testing.T) {
	if got := estimateTokens("   "); got != 0 {
		t.Fatalf("estimateTokens(blank) = %d, want 0", got)
	}
}

// TestBuildRequestMessages_KeepsPinnedPrefix pins the fix for a compaction that
// paid for a summary nobody read. The summary and the head are kept out of the
// conversation slice, and the truncator drops the OLDEST messages first:
// while they shared one slice with the conversation, the summary was the first
// thing to go — exactly when the conversation was long enough to need it.
func TestBuildRequestMessages_KeepsPinnedPrefix(t *testing.T) {
	convo := make([]provider.Message, 0, 40)
	for i := 0; i < 40; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		convo = append(convo, provider.Message{Role: role, Content: strings.Repeat("x", 400)})
	}
	pinned := []provider.Message{
		{Role: "user", Content: "[Previous conversation summary]\n" + strings.Repeat("s", 200)},
	}
	head := []provider.Message{{Role: "system", Content: "SYSTEM"}}

	got, dropped := buildRequestMessages(head, pinned, convo, 1200)
	if dropped == 0 {
		t.Fatalf("expected the conversation to be truncated at this budget")
	}
	var summaries int
	for _, m := range got {
		if strings.HasPrefix(m.Content, "[Previous conversation summary]") {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("pinned summary survived %d time(s), want exactly once", summaries)
	}
	if got[0].Role != "system" || got[0].Content != "SYSTEM" {
		t.Errorf("system prompt must stay first, got role=%q content=%q", got[0].Role, got[0].Content)
	}
	if !strings.HasPrefix(got[1].Content, "[Previous conversation summary]") {
		t.Errorf("the pinned summary must stay ahead of the conversation, got %q", got[1].Content)
	}
	if last := got[len(got)-1]; last.Content != convo[len(convo)-1].Content {
		t.Errorf("the newest message must survive truncation")
	}
	if len(got) != 1+len(pinned)+len(convo)-dropped {
		t.Errorf("assembled %d messages, want system + pinned + (conversation - dropped)", len(got))
	}
}

// TestBuildRequestMessages_NoTailRoom pins the floor: even when the pinned
// prefix eats the whole budget, the most recent turn is what the model has to
// see, so the tail budget never collapses to zero.
func TestBuildRequestMessages_NoTailRoom(t *testing.T) {
	convo := []provider.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "current question"},
	}
	pinned := []provider.Message{{Role: "user", Content: strings.Repeat("s", 8000)}}

	got, _ := buildRequestMessages(nil, pinned, convo, 10)
	if len(got) < 2 {
		t.Fatalf("assembled %d messages, want the pinned prefix plus the last turn", len(got))
	}
	if last := got[len(got)-1]; last.Content != "current question" {
		t.Errorf("last message = %q, want the current question", last.Content)
	}
}
