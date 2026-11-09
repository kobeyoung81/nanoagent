package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// adminStub serves one component's items and records the Authorization header.
func adminStub(t *testing.T, items map[string][]nanoprotocol.ConfigItem, wantToken string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()

		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var component string
		// /api/v1/config/{component}
		for _, part := range splitPath(r.URL.Path) {
			component = part
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"component_type": component,
			"items":          items[component],
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAuth
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// The admin module refuses anonymous callers exactly like the bus did. This
// pins the regression that once took every agent offline: forgetting the shared
// token makes the agent exit on startup with 401 and never register its tunnel.
func TestLoadAgentConfig_PresentsInternalToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")
	srv, gotAuth := adminStub(t, map[string][]nanoprotocol.ConfigItem{
		"agent":   {{Key: "agent.language.default", Value: "zh"}},
		"gateway": {},
	}, "s3cret")

	if _, err := LoadAgentConfig(srv.URL); err != nil {
		t.Fatalf("LoadAgentConfig: %v", err)
	}
	if len(*gotAuth) == 0 {
		t.Fatal("LoadAgentConfig made no admin request")
	}
	for _, auth := range *gotAuth {
		if auth != "Bearer s3cret" {
			t.Fatalf("Authorization = %q, want the shared token", auth)
		}
	}
}

// A refused read must surface as an error, not as an empty configuration: the
// agent would otherwise run on defaults with no trace of why.
func TestFetchItemsRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := fetchItemsFrom(srv.URL, "agent", 0); err == nil {
		t.Fatal("a 503 was accepted as an empty configuration")
	}
}
