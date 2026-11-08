package agent

import (
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// TestIncompleteToolCallNotice_SplitsInlineWrites pins the recovery advice for
// the two tools whose arguments carry a payload inline. Re-emitting the same
// body after an upstream cut is what repeats the truncation, so the notice must
// name the smaller route instead.
func TestIncompleteToolCallNotice_SplitsInlineWrites(t *testing.T) {
	cases := []struct {
		tool string
		want []string
		deny []string
	}{
		{
			tool: "write",
			want: []string{"append=true", "16 KiB"},
		},
		{
			tool: "edit",
			want: []string{"shortest unique fragment", "append=true"},
		},
		{
			tool: "bash",
			want: []string{`"arguments" as a JSON object`},
			deny: []string{"append=true", "16 KiB"},
		},
	}

	for _, tc := range cases {
		notice := incompleteToolCallNotice(provider.IncompleteToolCall{
			Tool:  tc.tool,
			Kind:  provider.ToolCallKindTruncated,
			Bytes: 9000,
		}, "length")
		for _, w := range tc.want {
			if !strings.Contains(notice, w) {
				t.Errorf("%s notice should contain %q, got %q", tc.tool, w, notice)
			}
		}
		for _, d := range tc.deny {
			if strings.Contains(notice, d) {
				t.Errorf("%s notice should not contain %q, got %q", tc.tool, d, notice)
			}
		}
		if !strings.Contains(notice, "nothing ran") {
			t.Errorf("%s notice must say nothing executed, got %q", tc.tool, notice)
		}
	}
}
