package agent

// This file is the measurement rig for the request layout's prompt-cache effect.
//
// It exists because the effect it measures is invisible from the outside: a hit
// and a miss look identical in a transcript, and the "real" proof (the
// cache_hit_ratio log of a deployed agent) needs a container restart, a live
// conversation and a reader. So the rig replays a *real* session through the
// production assembly and asks the real upstream how much of the prompt it served
// from its prefix cache.
//
// Two layers:
//
//   - TestCacheBenchStructural runs everywhere, needs no credentials and no
//     network: it pins the layout invariants the cache rests on (a byte-stable
//     head and tool block, tail-only changes, an idempotent tail).
//   - TestCacheBenchLive is env-gated behind CACHEBENCH=1 plus credentials and a
//     real session directory. It sends the assembled requests to the actual
//     upstream and reports prompt/cached tokens per turn, side by side with the
//     layouts the platform used *before* this change. Those control arms are
//     reconstructed here (that code no longer exists) and serve only as the
//     denominator; every measurement arm goes through the production functions.

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/skill"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// benchSharedPrompt stands in for the shared head prompt the bus folds into every
// session-agent definition: both modes carry the same bytes.
const benchSharedPrompt = "# Agent\n\nYou are a coding agent working in a shared repository. Follow the plan.\n"

// benchHarness is the smallest assembly that is still the production one: the head
// reader over a home directory, the session modes' definitions, and a tool
// registry of the real builtin tools.
type benchHarness struct {
	cfg   Config
	agent *Agent
	cb    *ContextBuilder
	reg   *tool.Registry
	build ResolvedAgent
	plan  ResolvedAgent
}

// benchModes builds the build/plan pair a session agent runs with. Both modes
// share one prompt (the bus folds the same head prompt into both) and differ only
// by permissions — exactly the pair unionByRules has to reconcile.
func benchModes(prompt string) (Config, ResolvedAgent, ResolvedAgent) {
	readOnly := []nanoprotocol.PermissionRule{
		{Pattern: "*", Action: nanoprotocol.PermissionDeny},
		{Pattern: "read", Action: nanoprotocol.PermissionAllow},
		{Pattern: "list", Action: nanoprotocol.PermissionAllow},
		{Pattern: "glob", Action: nanoprotocol.PermissionAllow},
		{Pattern: "grep", Action: nanoprotocol.PermissionAllow},
		{Pattern: "readonly_bash", Action: nanoprotocol.PermissionAllow},
		{Pattern: "plan_write", Action: nanoprotocol.PermissionAllow},
		{Pattern: "plan_execute", Action: nanoprotocol.PermissionAllow},
		{Pattern: "todowrite", Action: nanoprotocol.PermissionAllow},
	}
	build := ResolvedAgent{ID: "build", Prompt: prompt, Output: nanoprotocol.OutputStream,
		SessionBinding: nanoprotocol.SessionBindingShared}
	plan := ResolvedAgent{ID: "plan", Prompt: prompt, Output: nanoprotocol.OutputStream,
		SessionBinding: nanoprotocol.SessionBindingShared, Permissions: readOnly}
	cfg := Config{
		SessionAgentIDs: []string{"build", "plan"},
		Agents:          map[string]ResolvedAgent{"build": build, "plan": plan},
	}
	return cfg, build, plan
}

// benchRegistry registers the builtin tools that need no live service. It mirrors
// cmd/nanosec's buildToolsWith; the tools that need a bus, MCP or a subagent
// manager (task, cron, question, skill helpers, media) are absent, which is why
// the block is smaller than a real agent's — the layout under test does not depend
// on the list's length.
func benchRegistry(workspace, shared, memoryDir string) *tool.Registry {
	reg := tool.NewRegistry()
	reg.Register(tool.NewShellTool(2*time.Minute, workspace))
	reg.Register(tool.NewReadOnlyShellTool(2*time.Minute, workspace))
	reg.Register(&tool.ReadFileTool{Workspace: workspace, SharedWorkspace: shared})
	reg.Register(&tool.WriteFileTool{Workspace: workspace, SharedWorkspace: shared})
	reg.Register(&tool.EditFileTool{Workspace: workspace, SharedWorkspace: shared})
	reg.Register(&tool.ListDirTool{Workspace: workspace, SharedWorkspace: shared})
	reg.Register(&tool.AppendNoteTool{MemoryDir: memoryDir})
	reg.Register(&tool.WebFetchTool{})
	reg.Register(tool.NewGrepTool(workspace, shared))
	reg.Register(tool.NewGlobTool(workspace, shared))
	reg.Register(tool.NewTodoWriteTool())
	reg.Register(tool.NewPlanWriteTool(workspace, shared))
	reg.Register(tool.NewPlanExecuteTool(workspace, shared))
	return reg
}

func newBenchHarness(t *testing.T, home string) *benchHarness {
	t.Helper()
	skills := skill.NewRegistry(skill.NewLoader(filepath.Join(home, "skills")))
	cb := NewContextBuilder(home, filepath.Join(home, "AGENT.md"),
		filepath.Join(home, "bootstrap"), filepath.Join(home, "memory"), skills, 3)
	cfg, build, plan := benchModes(benchSharedPrompt)
	return &benchHarness{
		cfg:   cfg,
		agent: &Agent{ctxBuilder: cb, config: cfg},
		cb:    cb,
		reg:   benchRegistry(home, "", filepath.Join(home, "memory")),
		build: build,
		plan:  plan,
	}
}

// requestTools mirrors loop.go's tool block: the union over the session's modes, so
// the block is byte-identical whichever mode is active.
func (h *benchHarness) requestTools(spec ResolvedAgent) []provider.ToolDef {
	specs := h.cfg.sessionUnionSpecs(spec)
	defs := unionByDefinition(h.reg.Definitions(), nil, nil, specs)
	return unionByRules(defs, specs)
}

// request assembles one turn the way loop.go does: the head (head 1 plus the
// frozen per-file segments), the pinned prefix, then the conversation as far as the
// budget allows. Tail injections are already persisted in the session.
func (h *benchHarness) request(sess *session.Session, spec ResolvedAgent) []provider.Message {
	return h.requestWithHead(sess, h.agent.headMessages(sess, "web", "bench", spec.Prompt))
}

func (h *benchHarness) requestWithHead(sess *session.Session, head []provider.Message) []provider.Message {
	msgs, _ := buildRequestMessages(head, pinnedPrefix(sess),
		provider.SanitizeMessages(h.agent.contextMessages(sess)),
		budgetAvailable(1_000_000, 0))
	return msgs
}

// freshHead rebuilds the head from the files as they are right now — what the
// agent did before the head became a frozen per-segment snapshot, and the control
// arm for "the head follows the files".
func (h *benchHarness) freshHead(prompt string) []provider.Message {
	var head []provider.Message
	if h1 := h.cb.BuildSystemPrompt("web", "bench", prompt); h1 != "" {
		head = append(head, provider.Message{Role: "system", Content: h1})
	}
	for _, s := range h.cb.HeadSegments() {
		head = append(head, provider.Message{Role: "system", Content: HeadMarker(s.Name, s.Hash) + "\n" + s.Text})
	}
	return head
}

// benchWireMessage is the part of a message the provider's request builder puts on
// the wire. Timestamps, thought timings and error detail never reach the upstream,
// and the prefix cache is keyed on what does.
type benchWireMessage struct {
	Role       string              `json:"role"`
	Content    string              `json:"content,omitempty"`
	ToolCalls  []provider.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	Reasoning  string              `json:"reasoning_content,omitempty"`
}

func wireShape(msgs []provider.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		b, err := json.Marshal(benchWireMessage{
			Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls,
			ToolCallID: m.ToolCallID, Reasoning: m.Reasoning,
		})
		if err != nil {
			out = append(out, "!"+m.Role)
			continue
		}
		out = append(out, string(b))
	}
	return out
}

// commonPrefixRows counts how many leading messages two requests share. A cached
// prefix can never be longer than this, so it is the ceiling the upstream can
// report as cached tokens.
func commonPrefixRows(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// benchFixtureHome is a throwaway home with the same file layout a real agent home
// has, so the structural checks never touch (or depend on) the running agent's
// files.
func benchFixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{"bootstrap", "memory", "skills"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(home, "AGENT.md"), "# Identity\n\nI am the bench agent.\n")
	writeFile(t, filepath.Join(home, "bootstrap", "SOUL.md"), "Direct answers only.\n")
	writeFile(t, filepath.Join(home, "bootstrap", "USER.md"), "Speaks Chinese.\n")
	writeFile(t, filepath.Join(home, "bootstrap", "TOOLS.md"), "Notes about tools.\n")
	writeFile(t, filepath.Join(home, "memory", "MEMORY.md"), "- the repo uses Go\n- delivery goes through Jenkins\n")
	return home
}

// TestCacheBenchStructural pins the layout invariants the prompt cache depends on.
// Nothing here talks to the network: it is the cheap half of the rig, and it fails
// the moment a change makes the head, the tool block or the tail stop behaving.
func TestCacheBenchStructural(t *testing.T) {
	home := benchFixtureHome(t)
	h := newBenchHarness(t, home)

	// The tool block must be stable across calls and across modes: it is rendered
	// ahead of the messages, so a single reordering throws away the whole prefix.
	if names := h.reg.Names(); !sort.StringsAreSorted(names) {
		t.Errorf("tool definitions must be sorted by name, got %v", names)
	}
	buildTools := mustJSON(t, h.requestTools(h.build))
	if again := mustJSON(t, h.requestTools(h.build)); again != buildTools {
		t.Error("the tool block must be byte-stable between calls")
	}
	if planTools := mustJSON(t, h.requestTools(h.plan)); planTools != buildTools {
		t.Error("the tool block must be byte-identical across session modes")
	}

	sess := session.New("web:bench")
	h.agent.injectSessionState(sess, h.build)
	sess.AddMessage(provider.Message{Role: "user", Content: "第一轮：看一下仓库结构"})
	turn1 := h.request(sess, h.build)
	if len(turn1) < 3 {
		t.Fatalf("expected a head of at least three messages, got %d", len(turn1))
	}

	// Steady state: the whole of turn 1 stays a prefix of turn 2. This is what the
	// upstream rewards, and what the previous layout broke every single turn.
	sess.AddMessage(provider.Message{Role: "assistant", Content: "好的，我先读架构文档。"})
	h.agent.injectSessionState(sess, h.build)
	sess.AddMessage(provider.Message{Role: "user", Content: "第二轮：继续"})
	turn2 := h.request(sess, h.build)
	if got := commonPrefixRows(wireShape(turn2), wireShape(turn1)); got != len(turn1) {
		t.Errorf("steady state: turn 2 shares %d of turn 1's %d messages, want all of them", got, len(turn1))
	}
	if again := wireShape(h.request(sess, h.build)); !equalRows(again, wireShape(turn2)) {
		t.Error("assembling the same turn twice must produce the same bytes")
	}

	// Mode switch: the head and the tool block are the same bytes, and the note for
	// the new mode lands *after* everything the previous turn had already sent.
	if a := h.agent.headMessages(sess, "web", "bench", h.build.Prompt); mustJSON(t, a) != mustJSON(t, h.agent.headMessages(sess, "web", "bench", h.plan.Prompt)) {
		t.Error("the head must be byte-identical across session modes")
	}
	sess.AddMessage(provider.Message{Role: "assistant", Content: "接下来切到 Plan。"})
	h.agent.injectSessionState(sess, h.plan)
	sess.AddMessage(provider.Message{Role: "user", Content: "切到 Plan 模式，先出计划"})
	turn3 := h.request(sess, h.plan)
	if got := commonPrefixRows(wireShape(turn3), wireShape(turn2)); got != len(turn2) {
		t.Errorf("mode switch: the new request shares %d of the previous %d messages, want all of them", got, len(turn2))
	}

	// A file that grew after the snapshot is reported once, at the tail, as its
	// appended suffix — and it does not touch the frozen head.
	headBefore := mustJSON(t, h.agent.headMessages(sess, "web", "bench", h.build.Prompt))
	memPath := filepath.Join(home, "memory", "MEMORY.md")
	before, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, memPath, string(before)+"- the bench appends one line\n")

	sess.AddMessage(provider.Message{Role: "assistant", Content: "写完计划。"})
	h.agent.injectSessionState(sess, h.build)
	sess.AddMessage(provider.Message{Role: "user", Content: "第三轮：继续"})
	turn4 := h.request(sess, h.build)

	if after := mustJSON(t, h.agent.headMessages(sess, "web", "bench", h.build.Prompt)); after != headBefore {
		t.Error("a file change must not rewrite the frozen head")
	}
	if n := countRows(turn4, "system", "[更新MEMORY.md]"); n != 1 {
		t.Errorf("expected exactly one MEMORY.md tail update, got %d", n)
	} else if update := lastRow(turn4, "system", "[更新MEMORY.md]"); !strings.Contains(update, "the bench appends one line") {
		t.Errorf("an append-only file must report just the appended suffix, got %q", update)
	}
	if got := commonPrefixRows(wireShape(turn4), wireShape(turn3)); got != len(turn3) {
		t.Errorf("file change: the new request shares %d of the previous %d messages, want all of them", got, len(turn3))
	}

	// The same unchanged turn must not report the file twice.
	if n := countRows(sess.Messages, "system", "[更新MEMORY.md]"); n != 1 {
		t.Errorf("the tail update must be reported once, got %d", n)
	}
}

func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lastRow(msgs []provider.Message, role, contains string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == role && strings.Contains(msgs[i].Content, contains) {
			return msgs[i].Content
		}
	}
	return ""
}

// benchLiveConfig is everything the live benchmark needs. It is read from the
// environment so the test can never run by accident: CACHEBENCH=1 is not something
// CI sets, and without it the test skips.
type benchLiveConfig struct {
	baseURL    string
	apiKey     string
	model      string
	home       string
	sessionDir string
	session    string
	window     int
}

func loadBenchLiveConfig(t *testing.T) benchLiveConfig {
	t.Helper()
	if os.Getenv("CACHEBENCH") == "" {
		t.Skip("set CACHEBENCH=1 (plus CACHEBENCH_BASE_URL/API_KEY/HOME/SESSION_DIR) to run the live prompt-cache benchmark")
	}
	c := benchLiveConfig{
		baseURL:    os.Getenv("CACHEBENCH_BASE_URL"),
		apiKey:     os.Getenv("CACHEBENCH_API_KEY"),
		model:      os.Getenv("CACHEBENCH_MODEL"),
		home:       os.Getenv("CACHEBENCH_HOME"),
		sessionDir: os.Getenv("CACHEBENCH_SESSION_DIR"),
		session:    os.Getenv("CACHEBENCH_SESSION"),
		window:     40000,
	}
	if c.model == "" {
		c.model = "basic"
	}
	if v := os.Getenv("CACHEBENCH_WINDOW_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			t.Fatalf("CACHEBENCH_WINDOW_TOKENS must be an integer >= 1000, got %q", v)
		}
		c.window = n
	}
	for _, req := range []struct{ name, value string }{
		{"CACHEBENCH_BASE_URL", c.baseURL},
		{"CACHEBENCH_API_KEY", c.apiKey},
		{"CACHEBENCH_HOME", c.home},
		{"CACHEBENCH_SESSION_DIR", c.sessionDir},
	} {
		if req.value == "" {
			t.Fatalf("%s is required when CACHEBENCH=1", req.name)
		}
	}
	return c
}

// benchHistory loads the newest turns of a real session that fit the window. The
// suffix is cut on a user boundary so the request never starts in the middle of a
// tool call, and the source file is copied first: the benchmark must not write to a
// live session directory.
func benchHistory(t *testing.T, c benchLiveConfig) []provider.Message {
	t.Helper()
	entries, err := os.ReadDir(c.sessionDir)
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	prefix := strings.ReplaceAll(c.session, ":", "_")
	src := ""
	newest := time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if c.session != "" && !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if src == "" || info.ModTime().After(newest) {
			src, newest = filepath.Join(c.sessionDir, e.Name()), info.ModTime()
		}
	}
	if src == "" {
		t.Fatalf("no session jsonl in %s matching %q", c.sessionDir, c.session)
	}
	id := strings.Replace(strings.TrimSuffix(filepath.Base(src), ".jsonl"), "_", ":", 1)
	t.Logf("bench: history from %s (session %s)", filepath.Base(src), id)

	// The live agent rewrites the whole file on save (os.Create truncates first), so
	// a copy taken mid-write can be short or end mid-line. Only copy a version two
	// consecutive reads agree on and whose tail is a complete line.
	tmp := t.TempDir()
	var body []byte
	for attempt := 0; attempt < 5 && body == nil; attempt++ {
		first, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read session: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
		second, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read session: %v", err)
		}
		if bytes.Equal(first, second) && json.Valid(lastLine(second)) {
			body = second
		}
	}
	if body == nil {
		t.Fatalf("session %s kept changing while being copied", filepath.Base(src))
	}
	if err := os.WriteFile(filepath.Join(tmp, filepath.Base(src)), body, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(tmp)
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	defer store.Close()

	msgs := provider.SanitizeMessages(store.GetOrCreate(id).ConversationMessages())
	if len(msgs) == 0 {
		t.Fatalf("session %s has no conversation messages", id)
	}
	if os.Getenv("CACHEBENCH_HISTORY") == "all" {
		t.Logf("bench: window = the whole history (%d messages, ~%d tokens by the agent's own estimate)",
			len(msgs), tokenEstimateHeuristic(msgs))
		return msgs
	}
	// The window is the largest suffix that fits, cut at a user turn so the request
	// never opens in the middle of a tool call (production's truncator cuts there
	// too). When even the newest turn is over the window that turn is sent as-is:
	// the alternative is a request with almost no conversation in it.
	var turns []int
	for i, m := range msgs {
		if m.Role == "user" {
			turns = append(turns, i)
		}
	}
	start := -1
	for _, b := range turns {
		if tokenEstimateHeuristic(msgs[b:]) <= c.window {
			start = b
			break
		}
	}
	if start < 0 && len(turns) > 0 {
		start = turns[len(turns)-1]
	}
	if start < 0 {
		start = 0
	}
	window := msgs[start:]
	t.Logf("bench: window = %d of %d messages from index %d, ~%d tokens by the agent's own estimate",
		len(window), len(msgs), start, tokenEstimateHeuristic(window))
	return window
}

// lastLine returns the final line of a JSONL body, used to check that a copied file
// is not caught mid-write.
func lastLine(body []byte) []byte {
	trimmed := bytes.TrimRight(body, "\n")
	if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// benchCopyHome copies the file layout (not the sessions) of a real agent home so
// the arms that change a file can do it without touching the running agent's data.
func benchCopyHome(t *testing.T, home string) string {
	t.Helper()
	dst := t.TempDir()
	for _, rel := range []string{"AGENT.md", "bootstrap", "memory", "skills"} {
		src := filepath.Join(home, rel)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyPath(src, filepath.Join(dst, rel)); err != nil {
			t.Fatalf("copy %s: %v", rel, err)
		}
	}
	return dst
}

func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		body, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, body, 0o644)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// TestCacheBenchLive is the measurement itself: one warm request establishes the
// prefix, then each arm extends it and reports how much of the prompt the upstream
// served from cache.
//
// The arms are the claims this change rests on, each with the control that used to
// fail on the same content:
//
//	steady state            vs tool order shuffled
//	mode switch             vs the old per-mode prompt + tool block
//	a file that grew         vs a head rebuilt from the current files
func TestCacheBenchLive(t *testing.T) {
	c := loadBenchLiveConfig(t)
	h := newBenchHarness(t, benchCopyHome(t, c.home))
	history := benchHistory(t, c)

	client := provider.NewOpenAI(c.apiKey, c.baseURL, c.model, 5*time.Minute)

	send := func(name string, msgs []provider.Message, tools []provider.ToolDef) provider.Usage {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		start := time.Now()
		resp, err := client.Chat(ctx, provider.ChatRequest{
			Model:     c.model,
			Messages:  msgs,
			Tools:     tools,
			SessionID: "cachebench:" + c.session,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		elapsed := time.Since(start)
		ratio := 0.0
		if resp.Usage.PromptTokens > 0 {
			ratio = float64(resp.Usage.CachedTokens) / float64(resp.Usage.PromptTokens)
		}
		t.Logf("  %-34s prompt=%7d cached=%7d %6.1f%%  %5.1fs  finish=%s",
			name, resp.Usage.PromptTokens, resp.Usage.CachedTokens, ratio*100, elapsed.Seconds(), resp.FinishReason)
		return resp.Usage
	}
	ratio := func(u provider.Usage) float64 {
		if u.PromptTokens == 0 {
			return 0
		}
		return float64(u.CachedTokens) / float64(u.PromptTokens)
	}

	// The warm request: the real history plus this turn's tail, assembled the way a
	// turn is. Every arm below extends exactly these bytes.
	const question = "继续。请只回复两个字：好的。"
	sess := session.New(c.session)
	sess.Messages = append(sess.Messages, history...)
	h.agent.injectSessionState(sess, h.build)
	sess.AddMessage(provider.Message{Role: "user", Content: question})
	warmMsgs := h.request(sess, h.build)
	warmTools := h.requestTools(h.build)
	base := append([]provider.Message{}, sess.Messages...)

	t.Logf("bench: model=%s window=%d tokens, %d warm messages, %d tools, head=%d messages",
		c.model, c.window, len(warmMsgs), len(warmTools),
		len(h.agent.headMessages(sess, "web", "bench", h.build.Prompt)))

	warm := send("warm (cold by construction)", warmMsgs, warmTools)
	if warm.CachedTokens > 0 {
		t.Logf("  note: a warm request already hit %d tokens — an earlier run of this bench left the prefix cached", warm.CachedTokens)
	}
	// A repeat is the harness's own sanity check: if the upstream reports nothing
	// cached for identical bytes, the numbers below mean nothing.
	sanity := send("sanity: identical request repeated", warmMsgs, warmTools)

	// arm extends the warmed session with one assistant turn, this turn's tail and
	// the question, under the given definition and tool block. A non-nil head
	// overrides the frozen one (that is the "head follows the files" control).
	arm := func(spec ResolvedAgent, tools []provider.ToolDef, head []provider.Message) []provider.Message {
		s := session.New(c.session)
		s.Messages = append(s.Messages, base...)
		s.AddMessage(provider.Message{Role: "assistant", Content: "好的。"})
		h.agent.injectSessionState(s, spec)
		s.AddMessage(provider.Message{Role: "user", Content: question})
		if head != nil {
			return h.requestWithHead(s, head)
		}
		return h.request(s, spec)
	}

	steady := send("new: steady state", arm(h.build, warmTools, nil), warmTools)

	shuffled := append([]provider.ToolDef{}, warmTools...)
	rand.New(rand.NewSource(1)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	shuffledHit := send("control: tool order shuffled", arm(h.build, shuffled, nil), shuffled)

	switched := send("new: mode switch build→plan", arm(h.plan, h.requestTools(h.plan), nil), h.requestTools(h.plan))

	// The old mode switch: its own prompt and its own tool block, both gone from the
	// code — reconstructed here only to give the new layout a denominator.
	oldPlan := h.plan
	oldPlan.Prompt = "# Plan mode\n\nYou are in plan mode. Investigate and produce a plan; do not write files.\n"
	oldSpecs := []ResolvedAgent{oldPlan}
	oldTools := unionByRules(unionByDefinition(h.reg.Definitions(), nil, nil, oldSpecs), oldSpecs)
	oldSwitchHit := send("control: mode switch, per-mode head+tools", arm(oldPlan, oldTools, nil), oldTools)

	// A file that grew after the snapshot: the head stays frozen and the change
	// travels as a tail update.
	memPath := filepath.Join(h.cb.memoryDir, "MEMORY.md")
	before, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("read MEMORY.md: %v", err)
	}
	writeFile(t, memPath, string(before)+"- the bench appended this line during the live run\n")
	grown := send("new: file grew, frozen head + tail", arm(h.build, warmTools, nil), warmTools)

	// The same content, but the head is rebuilt from the current files.
	freshHit := send("control: head rebuilt from current files", arm(h.build, warmTools, h.freshHead(h.build.Prompt)), warmTools)

	t.Logf("bench summary: sanity=%.1f%% steady=%.1f%% shuffled=%.1f%% switch=%.1f%% switch(old)=%.1f%% grown=%.1f%% grown(old-head)=%.1f%%",
		ratio(sanity)*100, ratio(steady)*100, ratio(shuffledHit)*100, ratio(switched)*100,
		ratio(oldSwitchHit)*100, ratio(grown)*100, ratio(freshHit)*100)

	// The numbers above are the deliverable; these checks only pin the direction, so
	// a regression fails the run instead of quietly halving the hit rate.
	if got := ratio(sanity); got < 0.9 {
		t.Fatalf("harness: identical requests only hit %.1f%% — the upstream cache is not behaving as the rig assumes", got*100)
	}
	if got := ratio(steady); got < 0.95 {
		t.Errorf("steady state hit %.1f%%, want >= 95%%", got*100)
	}
	if got := ratio(switched); got < 0.95 {
		t.Errorf("mode switch hit %.1f%%, want >= 95%% (the switch must cost only the tail)", got*100)
	}
	if got := ratio(grown); got < 0.95 {
		t.Errorf("a grown file hit %.1f%%, want >= 95%% (the frozen head is what keeps it)", got*100)
	}
	if got := ratio(steady); got <= ratio(shuffledHit) {
		t.Errorf("shuffling the tool order hit %.1f%%, steady state %.1f%% — the tool block is supposed to be what makes the difference", ratio(shuffledHit)*100, got*100)
	}
	if got := ratio(switched); got <= ratio(oldSwitchHit) {
		t.Errorf("the new mode switch hit %.1f%%, the old one %.1f%% — the shared head and tool union are supposed to be what makes the difference", got*100, ratio(oldSwitchHit)*100)
	}
	if got := ratio(grown); got <= ratio(freshHit) {
		t.Errorf("a grown file hit %.1f%% with a frozen head, %.1f%% with the head rebuilt from the files", got*100, ratio(freshHit)*100)
	}
}
