package tool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// WebFetchTool fetches content from a URL.
type WebFetchTool struct{}

func (t *WebFetchTool) Name() string { return "webfetch" }
func (t *WebFetchTool) Description() string {
	return "Fetch the content of a web page."
}

func (t *WebFetchTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"url": {Type: "string", Description: "The URL to fetch"},
		},
		Required: []string{"url"},
	}
}

func (t *WebFetchTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	u, ok := params["url"].(string)
	if !ok {
		return "", fmt.Errorf("missing url")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	// Mimic a browser to avoid some bot blocks
	req.Header.Set("User-Agent", "Nanosec/1.0")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch returned status %d", resp.StatusCode)
	}

	// Limit body size to avoid OOM
	const maxBytes = 100 * 1024 // 100KB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return "", err
	}

	// io.LimitReader may cut a multibyte UTF-8 sequence at the byte boundary.
	// Trim trailing incomplete bytes so the result is always valid UTF-8.
	for len(body) > 0 && !utf8.Valid(body) {
		body = body[:len(body)-1]
	}

	// TODO: strip HTML tags using a library or return raw text if content-type is text/plain
	// For now, return raw body (LLM can handle some HTML/markdown)
	return string(body), nil
}
