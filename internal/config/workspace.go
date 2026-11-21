package config

import (
	"os"
	"strings"
)

// DefaultWorkspaceURL is where the workspace module listens inside the compose
// network.
const DefaultWorkspaceURL = "http://nanoworkspace:8080"

// WorkspaceURL returns the workspace module's base URL.
//
// The agent needs it for two things: creating a goal from the /goal command and
// posting a round report. Both carry the platform internal token, because a
// container has no browser session.
func WorkspaceURL() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_WORKSPACE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultWorkspaceURL
}
