package tool

import (
	"encoding/json"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/question"
)

// The question card is a form the user has to fill in, so the guidance the model
// gets (schema caps, description) and the parse of what it actually sent are
// pinned here: the prompt tells the model the caps, and the tool keeps an
// oversized call readable instead of silently trimming it.

func TestParseQuestions_KeepsFlagsAndTrimsHeader(t *testing.T) {
	req, ok := ParseQuestions("q-1", `{"questions":[{"question":"Pick one","header":"  Choice  ",`+
		`"options":[{"label":"A","description":"first"}],"multiple":true,"custom":true}]}`)
	if !ok {
		t.Fatal("ParseQuestions reported failure")
	}
	if req.ID != "q-1" {
		t.Errorf("id = %q, want q-1", req.ID)
	}
	if len(req.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(req.Questions))
	}
	q := req.Questions[0]
	if q.Header != "Choice" {
		t.Errorf("header = %q, want the trimmed label", q.Header)
	}
	if !q.Multiple || !q.Custom {
		t.Errorf("multiple/custom = %v/%v, want both true", q.Multiple, q.Custom)
	}
	if len(q.Options) != 1 || q.Options[0].Description != "first" {
		t.Errorf("options = %+v, want the label and its description", q.Options)
	}
}

func TestParseQuestions_RejectsWhatItCannotPresent(t *testing.T) {
	for _, args := range []string{`{}`, `{"questions":[]}`, `{"questions":"nope"}`, `not json`} {
		if _, ok := ParseQuestions("q-1", args); ok {
			t.Errorf("ParseQuestions(%s) = ok, want failure", args)
		}
	}
}

func TestRawParameters_CarriesTheGuidanceCaps(t *testing.T) {
	var doc struct {
		Properties struct {
			Questions struct {
				MaxItems int `json:"maxItems"`
				Items    struct {
					Properties struct {
						Options struct {
							MaxItems int `json:"maxItems"`
						} `json:"options"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"questions"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(new(QuestionTool).RawParameters(), &doc); err != nil {
		t.Fatalf("raw parameters are not valid JSON: %v", err)
	}
	if doc.Properties.Questions.MaxItems != MaxQuestionsPerCall {
		t.Errorf("questions maxItems = %d, want %d", doc.Properties.Questions.MaxItems, MaxQuestionsPerCall)
	}
	if got := doc.Properties.Questions.Items.Properties.Options.MaxItems; got != MaxOptionsPerQuestion {
		t.Errorf("options maxItems = %d, want %d", got, MaxOptionsPerQuestion)
	}
}

func TestParameterSchemasByNameTheCaps(t *testing.T) {
	// The provider-facing schema and the verbatim one are separate literals; a
	// test that reads both is what catches an edit applied to only one of them.
	description := new(QuestionTool).Parameters().Properties["questions"].Description
	if !strings.Contains(description, "at most 5") || !strings.Contains(description, "8 options") {
		t.Errorf("questions description = %q, want the caps named", description)
	}
	toolDescription := new(QuestionTool).Description()
	if !strings.Contains(toolDescription, "at most 5 questions") || !strings.Contains(toolDescription, "8 options") {
		t.Errorf("tool description = %q, want the caps named", toolDescription)
	}
}

func TestExceedsGuidanceCaps(t *testing.T) {
	answer := func(n int) question.Request {
		req := question.Request{ID: "q"}
		for i := 0; i < n; i++ {
			req.Questions = append(req.Questions, question.Question{Question: "q", Options: []question.Option{{Label: "A"}}})
		}
		return req
	}
	if ExceedsGuidanceCaps(answer(MaxQuestionsPerCall)) {
		t.Error("a call at the cap must not be reported")
	}
	if !ExceedsGuidanceCaps(answer(MaxQuestionsPerCall + 1)) {
		t.Error("too many questions must be reported")
	}

	wide := answer(1)
	wide.Questions[0].Options = make([]question.Option, MaxOptionsPerQuestion+1)
	if !ExceedsGuidanceCaps(wide) {
		t.Error("too many options must be reported")
	}
}
