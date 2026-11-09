package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// DefaultAdminURL is where the admin module listens inside the compose network.
const DefaultAdminURL = "http://nanoadmin:8080"

// AdminURL returns the admin module's base URL.
//
// Configuration belongs to the admin module: the agent asks it over HTTP, the
// same way the gateway and the provisioner do. The bus routes messages only.
func AdminURL() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_ADMIN_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultAdminURL
}

// FetchItems returns one component's configuration from the admin module.
//
// A non-200 is an error rather than an empty list: "admin said no" must not look
// like "this component has no settings", or a broken deployment would silently
// run on defaults.
func FetchItems(component string) ([]nanoprotocol.ConfigItem, error) {
	return fetchItemsFrom(AdminURL(), component, 5*time.Second)
}

func fetchItemsFrom(baseURL, component string, timeout time.Duration) ([]nanoprotocol.ConfigItem, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/config/"+url.PathEscape(component), nil)
	if err != nil {
		return nil, err
	}
	if token := InternalToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ask admin for %s config: %w", component, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin answered %d for %s config", resp.StatusCode, component)
	}
	var payload struct {
		Items []nanoprotocol.ConfigItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s config: %w", component, err)
	}
	return payload.Items, nil
}
