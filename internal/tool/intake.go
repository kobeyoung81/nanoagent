package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The intake binding (design §13, step 2).
//
// An intake turn is an ordinary conversation turn — nobody is away, so the
// blocking question tool works — that fills one server-side goal draft. Two
// tools write into it: the question tool (a question that names a check is
// written the moment the user answers) and goal_intake_answer (free-form
// answers, one round per call). Both go through one writer so a round is one
// request and one verdict.

// IntakeContext is the draft this turn is filling. The zero value means "not an
// intake turn", and every write refuses rather than guessing a draft.
type IntakeContext struct {
	DraftID string
	// SessionKey is the conversation the draft belongs to; it is what a
	// mistaken draft id is compared against when the workspace says "no such
	// goal".
	SessionKey string
}

// IntakeContextTool is implemented by tools that act on an intake turn.
type IntakeContextTool interface {
	SetIntakeContext(c IntakeContext)
}

// DraftAnswer is one answer on its way to the workspace. Value is `any` because
// a proof may be a list and a budget may be {"rounds":3,"minutes":10}; the
// workspace decides what each shape means.
type DraftAnswer struct {
	Check string `json:"check"`
	Value any    `json:"value"`
}

// DraftWriter posts answer rounds into the workspace.
type DraftWriter struct {
	workspaceURL string
	userID       string
	token        string
	http         *http.Client
}

// NewDraftWriter builds the writer for one account.
func NewDraftWriter(workspaceURL, userID, token string) *DraftWriter {
	return &DraftWriter{
		workspaceURL: strings.TrimRight(workspaceURL, "/"),
		userID:       userID,
		token:        token,
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

// Write posts one round of answers and returns the line the model sees back.
//
// The line carries what actually landed (`stored`) and what is still missing:
// "saved" alone let a model believe an unreadable budget had been accepted, and
// it asked the same question again.
func (w *DraftWriter) Write(ctx context.Context, draftID string, answers []DraftAnswer) (string, error) {
	if w == nil || w.workspaceURL == "" {
		return "", fmt.Errorf("the workspace url is not configured")
	}
	if draftID == "" {
		return "", fmt.Errorf("no goal draft is bound to this turn")
	}
	body, err := json.Marshal(map[string]any{"answers": answers, "user_id": w.userID})
	if err != nil {
		return "", err
	}
	endpoint := w.workspaceURL + "/api/v1/workspace/goals/drafts/" + url.PathEscape(draftID) + "/answers"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		var view struct {
			Verdict struct {
				Filled   []string `json:"filled"`
				Missing  []string `json:"missing"`
				Blockers []string `json:"blockers"`
			} `json:"verdict"`
			Stored map[string]string `json:"stored"`
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			return "", fmt.Errorf("workspace answered 200 with an unreadable body: %w", err)
		}
		return DescribeStored(view.Stored, view.Verdict.Filled, view.Verdict.Missing), nil
	case http.StatusNotFound:
		return "", fmt.Errorf("draft %s does not exist (it may have been deleted or already started); do not retry — ask the user to reopen 「设定长程目标」", draftID)
	case http.StatusConflict:
		return "", fmt.Errorf("draft %s has already been started, so it cannot be edited", draftID)
	default:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return "", fmt.Errorf("workspace answered %d: %s", resp.StatusCode, e.Error)
	}
}

// DescribeStored is the one-line result both tools return: what landed, and what
// the draft still misses.
func DescribeStored(stored map[string]string, filled, missing []string) string {
	var b strings.Builder
	if len(stored) == 0 {
		b.WriteString("没有写入任何一项")
	} else {
		b.WriteString("已写入：")
		keys := make([]string, 0, len(stored))
		for k := range stored {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteString("；")
			}
			b.WriteString(k + "=" + stored[k])
		}
	}
	fmt.Fprintf(&b, "。目标补全 %d/6", len(filled))
	if len(missing) > 0 {
		b.WriteString("，还差：" + strings.Join(missing, "、"))
	} else {
		b.WriteString("，六项齐全，可以让用户确认了")
	}
	return b.String()
}

func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// intakeBinder is the per-turn draft id, shared by the two tools that write.
//
// It lives here rather than on each tool because a round may be filled by both:
// the question tool writes what its marked questions collected, and the tool
// writes whatever the model could not attach to a question.
type intakeBinder struct {
	mu    sync.Mutex
	draft IntakeContext
}

// NewIntakeBinder builds the per-turn binding both intake tools share.
func NewIntakeBinder() *intakeBinder {
	return &intakeBinder{}
}

func (b *intakeBinder) set(c IntakeContext) {
	b.mu.Lock()
	b.draft = c
	b.mu.Unlock()
}

func (b *intakeBinder) get() IntakeContext {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.draft
}
