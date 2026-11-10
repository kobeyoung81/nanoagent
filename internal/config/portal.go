package config

import (
	"os"
	"strings"
)

// DefaultPortalURL is where the portal listens inside the compose network. The
// portal owns the agent rows and the conversation projection, so the runtime asks
// it directly instead of going through the bus.
const DefaultPortalURL = "http://nanoportal:80"

// PortalURL returns the portal's base URL (NANOSEC_PORTAL_URL).
func PortalURL() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_PORTAL_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultPortalURL
}
