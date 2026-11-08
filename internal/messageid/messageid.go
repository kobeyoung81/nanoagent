package messageid

import "github.com/google/uuid"

// New returns a unique message ID for outbound bus messages.
func New() string {
	return uuid.New().String()
}
