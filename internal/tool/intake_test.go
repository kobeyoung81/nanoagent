package tool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

func TestIntakeAnswers(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		got, err := intakeAnswers(map[string]any{"answers": []any{
			map[string]any{"check": "proof", "value": "go build ./... 退出码 0"},
			map[string]any{"check": "预算", "value": map[string]any{"rounds": float64(3), "minutes": float64(10)}},
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].Check != "proof" || got[1].Check != "预算" {
			t.Fatalf("answers = %+v", got)
		}
	})

	t.Run("one answer, the old shape", func(t *testing.T) {
		got, err := intakeAnswers(map[string]any{"check": "approach", "value": "一次写入"})
		if err != nil || len(got) != 1 || got[0].Check != "approach" {
			t.Fatalf("got %+v err %v", got, err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		if _, err := intakeAnswers(map[string]any{"answers": []any{}}); err == nil {
			t.Error("an empty batch must be refused")
		}
		if _, err := intakeAnswers(map[string]any{"answers": make([]any, 7)}); err == nil {
			t.Error("more than six answers must be refused")
		}
		if _, err := intakeAnswers(map[string]any{"answers": []any{map[string]any{"check": "proof"}}}); err == nil {
			t.Error("an answer without a value must be refused")
		}
	})
}

func TestGoalIntakeTool_PostsTheWholeRound(t *testing.T) {
	var got struct {
		Answers []DraftAnswer `json:"answers"`
		UserID  string        `json:"user_id"`
	}
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":{"filled":["condition","proof","budget","permissions"],"missing":["approach","constraints"]},"stored":{"proof":"2 条","budget":"3 轮 / 10 分钟"}}`))
	}))
	defer srv.Close()

	writer := NewDraftWriter(srv.URL, "u1", "internal")
	tl := NewGoalIntakeTool(writer)
	ctx := turnCtx(GoalContext{}, IntakeContext{DraftID: "g_1"})

	out, err := tl.Execute(ctx, map[string]any{"answers": []any{
		map[string]any{"check": "proof", "value": "a\nb"},
		map[string]any{"check": "budget", "value": "3 轮 / 10 分钟"},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if path != "/api/v1/workspace/goals/drafts/g_1/answers" {
		t.Errorf("path = %q", path)
	}
	if len(got.Answers) != 2 || got.UserID != "u1" {
		t.Errorf("body = %+v", got)
	}
	if !strings.Contains(out, "已写入") || !strings.Contains(out, "还差：approach、constraints") {
		t.Errorf("the model needs what landed and what is missing, got %q", out)
	}
}

func TestGoalIntakeTool_RefusesWithoutABinding(t *testing.T) {
	tl := NewGoalIntakeTool(NewDraftWriter("http://example.invalid", "u1", ""))
	if _, err := tl.Execute(turnCtx(GoalContext{}, IntakeContext{}), map[string]any{
		"answers": []any{map[string]any{"check": "proof", "value": "x"}},
	}); err == nil {
		t.Fatal("a write without a draft must be refused")
	}
}

// TestQuestionToolWritesMarkedAnswers is the whole point of the check field: the
// answers to marked questions land in the draft as one write, and the model is
// told what is still missing instead of asking the same question again.
func TestQuestionToolWritesMarkedAnswers(t *testing.T) {
	var body struct {
		Answers []DraftAnswer `json:"answers"`
	}
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"verdict":{"filled":["condition","proof"],"missing":["budget"]},"stored":{"proof":"1 条"}}`))
	}))
	defer srv.Close()

	tl := NewQuestionToolWithIntake(nil, NewDraftWriter(srv.URL, "u1", ""))
	params := map[string]any{"questions": []any{
		map[string]any{"question": "证据怎么给？", "check": "proof"},
		map[string]any{"question": "叫什么名字？"},
	}}
	note := tl.writeMarkedAnswers(turnCtx(GoalContext{}, IntakeContext{DraftID: "g_7"}), "q1", params, []string{"cat a.txt", "随便"})
	if writes != 1 {
		t.Fatalf("writes = %d, want 1 (one round, one write)", writes)
	}
	if len(body.Answers) != 1 || body.Answers[0].Check != "proof" || body.Answers[0].Value != "cat a.txt" {
		t.Fatalf("only the marked question may be written: %+v", body.Answers)
	}
	if !strings.Contains(note, "还差：budget") {
		t.Errorf("note = %q", note)
	}
}

// TestQuestionToolWithoutIntakeDoesNotWrite keeps an ordinary conversation from
// touching a goal draft.
func TestQuestionToolWithoutIntakeDoesNotWrite(t *testing.T) {
	tl := NewQuestionTool(nil)
	params := map[string]any{"questions": []any{map[string]any{"question": "q", "check": "proof"}}}
	if note := tl.writeMarkedAnswers(turnCtx(GoalContext{}, IntakeContext{DraftID: "g_7"}), "q1", params, []string{"a"}); note != "" {
		t.Fatalf("note = %q, want none", note)
	}
}

// TestQuestionSchemaCarriesCheck keeps the check enum in the schema the model
// reads: an unlisted value would be dropped by the provider.
func TestQuestionSchemaCarriesCheck(t *testing.T) {
	schema := string(NewQuestionTool(nil).RawParameters())
	for _, want := range []string{`"check"`, "condition", "proof", "approach", "constraints", "budget"} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema is missing %q", want)
		}
	}
	params := NewQuestionTool(nil).Parameters()
	props := params.Properties["questions"]
	_ = props
	var _ provider.PropertySchema
}
