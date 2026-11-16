// Package portalclient calls the portal's internal endpoints.
//
// The portal owns the agent rows and the conversation projection. The runtime
// keeps using the bus for messages (its inbound queue, outbound chunks, config
// changes), but the two things it does to the portal's data — saving a session
// snapshot and reporting a container status — go straight there.
package portalclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// Client is the runtime's door to the portal.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client. An empty base URL leaves it disabled, and every call
// then fails loudly rather than calling "http://:80".
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			// A full snapshot of a long conversation can be a few MiB; the
			// default timeout would abort it mid-write.
			Timeout: 60 * time.Second,
		},
	}
}

// Enabled reports whether a base URL was configured.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// SaveSession applies one snapshot to the conversation projection.
//
// A 409 means the delta followed a prefix the store does not hold; it is surfaced
// as nanoprotocol.ErrSessionResync so the sender resends the whole conversation
// instead of leaving a hole in the history.
func (c *Client) SaveSession(req nanoprotocol.SaveSessionRequest) error {
	err := c.post(context.Background(), "/internal/v1/sessions", req, nil)
	if isConflict(err) {
		return nanoprotocol.ErrSessionResync
	}
	return err
}

// Agent is the part of an agent row the runtime reads: identity, the display
// name the home directory is derived from, and the status that decides whether a
// process should exist.
type Agent struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

// ListAgents lists the agents belonging to a user.
func (c *Client) ListAgents(userID string) ([]Agent, error) {
	var out struct {
		Agents []Agent `json:"agents"`
	}
	// include_deleted: the supervisor must see a deleted agent (it skips it and
	// must not resurrect it), unlike the browser's own list.
	path := "/internal/v1/agents?include_deleted=1&user_id=" + strings.TrimSpace(urlEscape(userID))
	if err := c.get(context.Background(), path, &out); err != nil {
		return nil, err
	}
	if out.Agents == nil {
		out.Agents = []Agent{}
	}
	return out.Agents, nil
}

// SetAgentStatus records a runtime status report for one agent.
func (c *Client) SetAgentStatus(agentID, status string) error {
	body := map[string]string{"status": status}
	return c.post(context.Background(), "/internal/v1/agents/"+urlEscape(agentID)+"/status", body, nil)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	if !c.Enabled() {
		return fmt.Errorf("portal client is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	if !c.Enabled() {
		return fmt.Errorf("portal client is not configured")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(detail))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// statusError keeps the status code so a caller can recognise the one answer it
// treats specially (409 → resend the full conversation).
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("portal answered %d: %s", e.code, e.body)
}

func isConflict(err error) bool {
	se, ok := err.(*statusError)
	return ok && se.code == http.StatusConflict
}

// urlEscape escapes one path or query segment.
func urlEscape(s string) string {
	return strings.NewReplacer("/", "%2F", " ", "%20", "?", "%3F", "#", "%23", "&", "%26").Replace(s)
}
