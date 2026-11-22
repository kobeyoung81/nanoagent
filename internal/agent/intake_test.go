package agent

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// TestIntakeContextOf pins where the draft binding comes from. The intake turn
// is an ordinary turn, so it cannot use the goal binding (which needs
// source=goal and would make the question tool headless).
func TestIntakeContextOf(t *testing.T) {
	cases := []struct {
		name string
		msg  *nanoprotocol.InboundMessage
		want string
	}{
		{name: "nil", msg: nil},
		{name: "no metadata", msg: &nanoprotocol.InboundMessage{SessionID: "web:a"}},
		{name: "other metadata", msg: &nanoprotocol.InboundMessage{Metadata: map[string]any{"agent_mode": "build"}}},
		{name: "empty draft", msg: &nanoprotocol.InboundMessage{Metadata: map[string]any{intakeDraftMetadataKey: "  "}}},
		{
			name: "bound",
			msg: &nanoprotocol.InboundMessage{
				SessionID: "web:chat-1:v2",
				Metadata:  map[string]any{intakeDraftMetadataKey: "g_abc"},
			},
			want: "g_abc",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := intakeContextOf(c.msg)
			if got.DraftID != c.want {
				t.Fatalf("draft = %q, want %q", got.DraftID, c.want)
			}
			if c.want != "" && got.SessionKey == "" {
				t.Error("the session key is what a mistaken draft id is checked against")
			}
		})
	}
}
