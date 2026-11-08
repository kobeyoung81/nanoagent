package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// TestLoadAgentConfig_PresentsInternalToken pins the regression that took the
// agents offline: the bus refuses its internal endpoints to anonymous callers,
// and LoadAgentConfig is the agent's first bus call. If this client forgets the
// shared token, every agent container exits on startup with 401 and never
// registers its hypervisor tunnel.
func TestLoadAgentConfig_PresentsInternalToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")

	var (
		mu      sync.Mutex
		gotAuth []string
	)
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()

		resp, err := nanoprotocol.NewMessage(nanoprotocol.TypeConfigItems, nanoprotocol.GetConfigItemsResponse{})
		if err != nil {
			t.Errorf("build response: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer bus.Close()

	if _, err := LoadAgentConfig(bus.URL); err != nil {
		t.Fatalf("LoadAgentConfig: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotAuth) == 0 {
		t.Fatal("LoadAgentConfig made no bus request")
	}
	for i, auth := range gotAuth {
		if auth != "Bearer s3cret" {
			t.Errorf("request %d Authorization = %q, want %q", i, auth, "Bearer s3cret")
		}
	}
}
