package provider

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// TestUsageFromKeepsCachedTokens pins the prompt-cache telemetry: the SDK reports
// prompt_tokens_details.cached_tokens and it used to be dropped on the floor,
// which left the platform with no way to tell a prefix change from a cache miss.
func TestUsageFromKeepsCachedTokens(t *testing.T) {
	got := usageFrom(openai.Usage{
		PromptTokens: 4108, CompletionTokens: 12, TotalTokens: 4120,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 4096},
	})
	if got.CachedTokens != 4096 {
		t.Errorf("CachedTokens = %d, want 4096", got.CachedTokens)
	}
	if got.PromptTokens != 4108 || got.CompletionTokens != 12 || got.TotalTokens != 4120 {
		t.Errorf("token counts lost: %+v", got)
	}

	// A provider that reports no details must not invent a hit.
	plain := usageFrom(openai.Usage{PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101})
	if plain.CachedTokens != 0 {
		t.Errorf("CachedTokens = %d, want 0 when prompt_tokens_details is absent", plain.CachedTokens)
	}
}
