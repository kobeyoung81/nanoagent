package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// dispatchFor records the calls and answers each with a labelled result.
func dispatchFor(seen *[]string) ParallelDispatch {
	return ParallelDispatch{
		Run: func(ctx context.Context, name, argumentsJSON string) (string, error) {
			*seen = append(*seen, name+" "+argumentsJSON)
			return "ok:" + name, nil
		},
	}
}

// TestParallelTool_RunsCallsConcurrently proves the point of the tool: three
// calls are in flight at the same time, which is what a plain turn never does.
func TestParallelTool_RunsCallsConcurrently(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	tool := NewParallelTool()
	tool.SetParallelDispatch(ParallelDispatch{
		Run: func(ctx context.Context, name, argumentsJSON string) (string, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-time.After(5 * time.Second):
				return "", fmt.Errorf("test never released the call")
			}
			return "ok:" + name, nil
		},
	})

	done := make(chan string, 1)
	go func() {
		out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
			map[string]any{"tool": "read", "arguments": map[string]any{"path": "a"}},
			map[string]any{"tool": "grep", "arguments": map[string]any{"pattern": "b"}},
			map[string]any{"tool": "readonly_bash", "arguments": map[string]any{"command": "ls"}},
		}})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- out
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of 3 calls were in flight at once", i)
		}
	}
	close(release)
	out := <-done

	if !strings.HasPrefix(out, `<parallel count="3" ok="3">`) {
		t.Errorf("unexpected header: %q", tailOf(out, 120))
	}
	for i, name := range []string{"read", "grep", "readonly_bash"} {
		if !strings.Contains(out, fmt.Sprintf(`<call index="%d" tool=%q state="ok"`, i+1, name)) {
			t.Errorf("call %d (%s) missing from the block: %q", i+1, name, out)
		}
	}
}

// TestParallelTool_ResultsAreInRequestOrder pins the ordering contract: results
// come back in request order even though the calls finish in another order.
func TestParallelTool_ResultsAreInRequestOrder(t *testing.T) {
	tool := NewParallelTool()
	tool.SetParallelDispatch(ParallelDispatch{
		Run: func(ctx context.Context, name, argumentsJSON string) (string, error) {
			if name == "slow" {
				time.Sleep(60 * time.Millisecond)
			}
			return "done:" + name, nil
		},
	})

	out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "slow"},
		map[string]any{"tool": "fast"},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Index(out, "done:slow") > strings.Index(out, "done:fast") {
		t.Errorf("results must follow request order, got: %q", out)
	}
	if !strings.Contains(out, `<call index="1" tool="slow"`) || !strings.Contains(out, `<call index="2" tool="fast"`) {
		t.Errorf("unexpected call numbering: %q", out)
	}
}

// TestParallelTool_OneFailureDoesNotStopSiblings pins the isolation rule.
func TestParallelTool_OneFailureDoesNotStopSiblings(t *testing.T) {
	tool := NewParallelTool()
	tool.SetParallelDispatch(ParallelDispatch{
		Run: func(ctx context.Context, name, argumentsJSON string) (string, error) {
			if name == "boom" {
				return "", fmt.Errorf("kaboom")
			}
			return "fine:" + name, nil
		},
	})

	out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "read"},
		map[string]any{"tool": "boom"},
		map[string]any{"tool": "grep"},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "fine:read") || !strings.Contains(out, "fine:grep") {
		t.Errorf("siblings must still run: %q", out)
	}
	if !strings.Contains(out, `<call index="2" tool="boom" state="error"`) || !strings.Contains(out, "kaboom") {
		t.Errorf("the failed call should be marked: %q", out)
	}
	if !strings.Contains(out, `ok="2"`) {
		t.Errorf("the header should count successes: %q", out)
	}
}

// TestParallelTool_RefusesUnbatchableCalls pins the calls a batch may not carry,
// each in its own block so the rest of the batch still runs.
func TestParallelTool_RefusesUnbatchableCalls(t *testing.T) {
	seen := []string{}
	tool := NewParallelTool()
	tool.SetParallelDispatch(dispatchFor(&seen))

	out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "question", "arguments": map[string]any{"prompt": "which?"}},
		map[string]any{"tool": "todowrite", "arguments": map[string]any{"todos": []any{}}},
		map[string]any{"tool": "plan_write"},
		map[string]any{"tool": "cron"},
		map[string]any{"tool": "parallel_execution"},
		map[string]any{"tool": "read", "arguments": map[string]any{"path": "a"}},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "read ") {
		t.Fatalf("only the batchable call should run, saw %v", seen)
	}
	for _, refused := range []string{"question", "todowrite", "plan_write", "cron", "parallel_execution"} {
		if !strings.Contains(out, "cannot run inside a batch") {
			t.Fatalf("expected a refusal explanation, got: %q", out)
		}
		if !strings.Contains(out, refused) {
			t.Errorf("the refusal of %s should name it: %q", refused, out)
		}
	}
	if !strings.Contains(out, "ok:read") && !strings.Contains(out, "ok:read") {
		t.Errorf("the batchable call should have run: %q", out)
	}
}

// TestParallelTool_LeavesAskEntriesOut pins the approval rule: a call the mode
// would ask about cannot raise its card inside a batch, so it is left out with
// an instruction instead.
func TestParallelTool_LeavesAskEntriesOut(t *testing.T) {
	seen := []string{}
	tool := NewParallelTool()
	dispatch := dispatchFor(&seen)
	dispatch.Decision = func(name string) nanoprotocol.PermissionAction {
		if name == "bash" {
			return nanoprotocol.PermissionAsk
		}
		return nanoprotocol.PermissionAllow
	}
	tool.SetParallelDispatch(dispatch)

	out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "bash", "arguments": map[string]any{"command": "ls"}},
		map[string]any{"tool": "read", "arguments": map[string]any{"path": "a"}},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "read ") {
		t.Fatalf("an ask entry must not run inside a batch, saw %v", seen)
	}
	if !strings.Contains(out, "needs your approval") {
		t.Errorf("expected the approval explanation: %q", out)
	}
}

// TestParallelTool_RefusesSamePathWrites pins the contract: a batch is for
// independent calls, so a second write to a path another entry already claimed is
// refused before anything runs. Refusing beats both racing (a lost update) and
// silently serializing (which would make a concurrent tool order things and hide
// the caller's mistake).
func TestParallelTool_RefusesSamePathWrites(t *testing.T) {
	seen := []string{}
	tool := NewParallelTool()
	tool.SetParallelDispatch(dispatchFor(&seen))

	out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "edit", "arguments": map[string]any{"path": "a.go"}},
		map[string]any{"tool": "write", "arguments": map[string]any{"path": "./a.go"}},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("only the first write of a path may run, saw %v", seen)
	}
	if !strings.Contains(out, "depend on each other") || !strings.Contains(out, "call 1") {
		t.Errorf("expected the refusal to name the other call, got: %q", out)
	}
	if !strings.Contains(out, `ok="1"`) {
		t.Errorf("the batch should report one success: %q", out)
	}
	// "./a.go" and "a.go" are the same path: spelling must not defeat the check.

	// Different paths stay concurrent, which is the point of the tool.
	seen = nil
	out, err = tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "edit", "arguments": map[string]any{"path": "a.go"}},
		map[string]any{"tool": "edit", "arguments": map[string]any{"path": "b.go"}},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(seen) != 2 || !strings.Contains(out, `count="2" ok="2"`) {
		t.Errorf("edits of different paths must both run, got %v / %q", seen, out)
	}
}

// TestParallelTool_IsolatesMalformedEntries pins that one bad entry is answered
// on its own instead of failing the batch.
func TestParallelTool_IsolatesMalformedEntries(t *testing.T) {
	seen := []string{}
	tool := NewParallelTool()
	tool.SetParallelDispatch(dispatchFor(&seen))

	out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"arguments": map[string]any{"path": "a"}},         // no tool
		map[string]any{"tool": "read", "arguments": "{not json"},         // bad JSON string
		map[string]any{"tool": "read", "arguments": "{\"path\":\"ok\"}"}, // valid JSON string
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "has no tool name") || !strings.Contains(out, "not valid JSON") {
		t.Errorf("expected per-entry errors, got: %q", out)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], `"path":"ok"`) {
		t.Fatalf("the well-formed entry should still run with its JSON string arguments, saw %v", seen)
	}
}

// TestParallelTool_RequiresDispatch pins the safe default: without the loop's
// dispatch the tool refuses rather than reaching into a registry.
func TestParallelTool_RequiresDispatch(t *testing.T) {
	_, err := NewParallelTool().Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "read"},
	}})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected a refusal without a dispatch, got: %v", err)
	}
}

// TestParallelTool_BoundsTheBatch pins the two hard limits.
func TestParallelTool_BoundsTheBatch(t *testing.T) {
	seen := []string{}
	tool := NewParallelTool()
	tool.SetParallelDispatch(dispatchFor(&seen))

	if _, err := tool.Execute(context.Background(), map[string]any{}); err == nil {
		t.Error("expected an error for a missing calls[]")
	}

	many := make([]any, maxParallelCalls+1)
	for i := range many {
		many[i] = map[string]any{"tool": "read"}
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"calls": many}); err == nil {
		t.Errorf("expected the batch to be capped at %d calls", maxParallelCalls)
	}
}

// TestParallelTool_TruncatesEachCallToAShare pins the merged-result budget: the
// whole block stays inside it, and each call gets an equal share.
func TestParallelTool_TruncatesEachCallToAShare(t *testing.T) {
	tool := NewParallelTool()
	tool.MaxChars = 1200
	tool.SetParallelDispatch(ParallelDispatch{
		Run: func(ctx context.Context, name, argumentsJSON string) (string, error) {
			return strings.Repeat("x", 5000), nil
		},
	})

	out, err := tool.Execute(context.Background(), map[string]any{"calls": []any{
		map[string]any{"tool": "read"},
		map[string]any{"tool": "grep"},
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(out) > 2000 {
		t.Errorf("merged block is %d bytes, past its budget", len(out))
	}
	if strings.Count(out, "truncated") != 2 {
		t.Errorf("each call should say it was truncated: %q", tailOf(out, 200))
	}
}

// TestParallelSchema_AdvertisesObjectsOnly pins the shape the model is told to
// write. The schema used to allow a JSON string for each inner call's
// arguments, which asks the model to escape a whole JSON document inside a
// string — the most common way it produces invalid arguments for the batch.
// The parser still accepts a string (older callers), but nothing may advertise it.
func TestParallelSchema_AdvertisesObjectsOnly(t *testing.T) {
	var schema struct {
		Properties struct {
			Calls struct {
				Items struct {
					Properties map[string]struct {
						Type        any    `json:"type"`
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"calls"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(NewParallelTool().RawParameters(), &schema); err != nil {
		t.Fatalf("the advertised schema is not valid JSON: %v", err)
	}
	arg, ok := schema.Properties.Calls.Items.Properties["arguments"]
	if !ok {
		t.Fatal("the schema no longer describes each call's arguments")
	}
	if arg.Type != "object" {
		t.Errorf("arguments type = %v, want object", arg.Type)
	}
	if strings.Contains(arg.Description, "string") {
		t.Errorf("the description still offers a string: %q", arg.Description)
	}
	if desc := NewParallelTool().Description(); !strings.Contains(desc, "as a JSON object") {
		t.Errorf("the tool description should say the arguments are an object: %q", desc)
	} else if strings.Contains(desc, "accepted too") {
		t.Errorf("the tool description still offers a string: %q", desc)
	}
}
