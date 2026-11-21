package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// GoalIntakeTool writes one answer back into a goal draft.
//
// The intake turn asks at most three questions with the blocking question tool;
// the answers land here so the draft in the workspace is the single source of
// truth. It is a plain tool rather than a command because the model decides how
// to phrase the question, and the answer may arrive mid-turn.
type GoalIntakeTool struct {
	userID       string
	workspaceURL string
	http         *http.Client
}

// NewGoalIntakeTool creates the tool for one account.
func NewGoalIntakeTool(userID, workspaceURL string) *GoalIntakeTool {
	return &GoalIntakeTool{
		userID:       userID,
		workspaceURL: strings.TrimRight(workspaceURL, "/"),
		http:         &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *GoalIntakeTool) Name() string { return "goal_intake_answer" }

func (t *GoalIntakeTool) Description() string {
	return `Save one answer into the goal draft being created (the intake step).

The six checks are condition, proof, approach, constraints, budget and permissions. Call this once per answer the user gives; the draft is then re-checked and "目标补全 N/6" reflects it. Do not summarise or rewrite the user's answer: write what they said.
`
}

func (t *GoalIntakeTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"draft_id": {Type: "string", Description: "The draft id from the intake request"},
			"check": {
				Type:        "string",
				Description: "Which check the answer fills: condition | proof | approach | constraints | budget",
			},
			"value": {Type: "string", Description: "The answer, verbatim (a list is one item per line)"},
		},
		Required: []string{"draft_id", "check", "value"},
	}
}

func (t *GoalIntakeTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	draftID, _ := params["draft_id"].(string)
	check, _ := params["check"].(string)
	value, _ := params["value"].(string)
	draftID = strings.TrimSpace(draftID)
	check = strings.TrimSpace(check)
	if draftID == "" || check == "" {
		return "", fmt.Errorf("draft_id and check are required")
	}
	switch check {
	case "condition", "proof", "approach", "constraints", "budget":
	default:
		return "", fmt.Errorf("check %q is not one of condition|proof|approach|constraints|budget", check)
	}
	if t.workspaceURL == "" {
		return "", fmt.Errorf("the workspace url is not configured")
	}
	body, _ := json.Marshal(map[string]string{
		"check": check, "value": value, "user_id": t.userID,
	})
	endpoint := t.workspaceURL + "/api/v1/workspace/goals/drafts/" + url.PathEscape(draftID) + "/answer"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := config.InternalToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("workspace answered %d", resp.StatusCode)
	}
	var view struct {
		Verdict struct {
			Filled  []string `json:"filled"`
			Missing []string `json:"missing"`
		} `json:"verdict"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&view)
	// The progress line is what the intake turn says back to the user, so the
	// tool reports it instead of a bare "ok".
	return fmt.Sprintf("已记录 %s；目标补全 %d/6，还差：%s", check, len(view.Verdict.Filled), strings.Join(view.Verdict.Missing, "、")), nil
}
