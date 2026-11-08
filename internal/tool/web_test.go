package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebFetchTool(t *testing.T) {
	// Create a mock HTTP server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>Hello World</body></html>"))
	}))
	defer ts.Close()

	wt := &WebFetchTool{}

	// Test valid fetch
	res, err := wt.Execute(context.Background(), map[string]any{"url": ts.URL})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res != "<html><body>Hello World</body></html>" {
		t.Errorf("Unexpected result: %s", res)
	}

	// Test invalid URL
	_, err = wt.Execute(context.Background(), map[string]any{"url": "http://invalid-url"})
	if err == nil {
		t.Error("Expected error for invalid URL")
	}

	// Test missing URL
	_, err = wt.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("Expected error for missing URL")
	}
}
