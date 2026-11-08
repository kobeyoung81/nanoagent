package provider

import (
	"encoding/json"
	"testing"
)

func TestMultiContentMapping(t *testing.T) {
	parts := []ContentPart{
		{Type: "text", Text: "describe this"},
		{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
	}
	got := multiContent(parts)
	if len(got) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(got))
	}
	if got[0].Type != "text" || got[0].Text != "describe this" {
		t.Errorf("text part wrong: %+v", got[0])
	}
	if got[1].Type != "image_url" || got[1].ImageURL == nil || got[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Errorf("image part wrong: %+v", got[1])
	}
	if multiContent(nil) != nil {
		t.Error("nil parts should map to nil")
	}
	if multiContent([]ContentPart{{Type: "image_url"}}) != nil {
		t.Error("empty image_url should map to nil")
	}
}

func TestBuildChatRequest_VisionParts(t *testing.T) {
	req := ChatRequest{
		Model: "vision-model",
		Messages: []Message{
			{Role: "user", Content: "what is this?", Parts: []ContentPart{
				{Type: "text", Text: "what is this?"},
				{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
			}},
			{Role: "user", Content: "plain text"},
		},
	}
	out, err := buildChatRequest(req, "default", false)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	raw, err := json.Marshal(out.Messages[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The content field must be the multimodal array, not a string.
	if len(decoded.Content) == 0 || decoded.Content[0] != '[' {
		t.Fatalf("expected multimodal content array, got %s", decoded.Content)
	}
	// A message without parts keeps a plain string content.
	if out.Messages[1].Content != "plain text" || out.Messages[1].MultiContent != nil {
		t.Errorf("plain message should keep text content: %+v", out.Messages[1])
	}
}
