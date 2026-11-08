package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

func newTitleTestAgent(t *testing.T, titleRefresh int) (*Agent, *provider.MockProvider, *session.Store) {
	t.Helper()
	tmpDir, _ := os.MkdirTemp("", "title-test")
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	sessStore, _ := session.NewStore(tmpDir)
	mockProvider := provider.NewMock()
	mockProvider.Response = &provider.ChatResponse{Content: "Model Title"}

	a, _ := New(Config{
		UserID:               "test-user",
		Model:                "test-model",
		TitleModel:           "test-model",
		TitleRefreshMessages: titleRefresh,
		MaxIterations:        5,
		SessionDir:           tmpDir,
	}, &MockBusClient{}, mockProvider, tool.NewRegistry())
	a.sessions = sessStore
	return a, mockProvider, sessStore
}

func TestMaybeTitle_GeneratesAndRecords(t *testing.T) {
	a, mock, store := newTitleTestAgent(t, 12)
	sess := store.GetOrCreate("t:1")
	sess.AddMessage(provider.Message{Role: "user", Content: "hello world"})
	sess.AddMessage(provider.Message{Role: "assistant", Content: "hi"})

	a.maybeTitle(context.Background(), sess)

	if got := sess.GetMeta(titleMetaKey); got != "Model Title" {
		t.Fatalf("title = %q, want %q", got, "Model Title")
	}
	if got := sess.GetMeta(titleCountMetaKey); got != "2" {
		t.Fatalf("title count = %q, want 2", got)
	}
	if len(mock.Calls) == 0 {
		t.Fatal("expected a title model call")
	}
}

func TestMaybeTitle_NoRefreshBelowWatermark(t *testing.T) {
	a, mock, store := newTitleTestAgent(t, 12)
	sess := store.GetOrCreate("t:2")
	sess.SetMeta(titleMetaKey, "Existing")
	sess.SetMeta(titleCountMetaKey, "2")
	sess.AddMessage(provider.Message{Role: "user", Content: "a"})
	sess.AddMessage(provider.Message{Role: "assistant", Content: "b"})
	sess.AddMessage(provider.Message{Role: "user", Content: "c"}) // +1 message only

	a.maybeTitle(context.Background(), sess)

	if len(mock.Calls) != 0 {
		t.Fatalf("expected no refresh below the watermark, got %d call(s)", len(mock.Calls))
	}
	if got := sess.GetMeta(titleMetaKey); got != "Existing" {
		t.Fatalf("title = %q, want Existing", got)
	}
}

func TestMaybeTitle_RefreshesAtWatermark(t *testing.T) {
	a, mock, store := newTitleTestAgent(t, 2)
	sess := store.GetOrCreate("t:3")
	sess.SetMeta(titleMetaKey, "Existing")
	sess.SetMeta(titleCountMetaKey, "2")
	for i := 0; i < 4; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}

	a.maybeTitle(context.Background(), sess)

	if len(mock.Calls) == 0 {
		t.Fatal("expected a refresh call at the watermark")
	}
	if got := sess.GetMeta(titleMetaKey); got != "Model Title" {
		t.Fatalf("title = %q, want Model Title", got)
	}
}

func TestMaybeTitle_FallsBackToMainModel(t *testing.T) {
	a, mock, store := newTitleTestAgent(t, 12)
	a.config.TitleModel = "" // no dedicated title model
	sess := store.GetOrCreate("t:4")
	sess.AddMessage(provider.Message{Role: "user", Content: "hello"})
	sess.AddMessage(provider.Message{Role: "assistant", Content: "hi"})

	a.maybeTitle(context.Background(), sess)

	if len(mock.Calls) == 0 {
		t.Fatal("expected title generation to fall back to the main model")
	}
	if got := sess.GetMeta(titleMetaKey); got != "Model Title" {
		t.Fatalf("title = %q, want Model Title", got)
	}
}

func TestProvisionalTitle_FirstUserMessage(t *testing.T) {
	sess := session.New("p:1")
	sess.AddMessage(provider.Message{Role: "user", Content: "  plan my trip  "})
	if got := provisionalTitle(sess); got != "plan my trip" {
		t.Fatalf("provisionalTitle = %q, want %q", got, "plan my trip")
	}
}

// TestMaybeTitle_RespectsRecentMessagesWindow verifies the role_title type's
// recent_messages setting bounds the title input window.
func TestMaybeTitle_RespectsRecentMessagesWindow(t *testing.T) {
	a, mock, store := newTitleTestAgent(t, 12)
	a.config.Agents = map[string]ResolvedAgent{
		"role_title": {ID: "role_title", Settings: map[string]string{"recent_messages": "2"}},
	}
	mock.Response = &provider.ChatResponse{Content: "T"}

	sess := store.GetOrCreate("t:window")
	for i := 0; i < 6; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: fmt.Sprintf("m%d", i)})
	}

	a.maybeTitle(context.Background(), sess)

	if len(mock.Calls) == 0 {
		t.Fatal("expected a title model call")
	}
	content := mock.Calls[0].Messages[1].Content
	if strings.Contains(content, "m0") || strings.Contains(content, "m3") {
		t.Errorf("title window not bounded to the last 2 messages: %q", content)
	}
	if !strings.Contains(content, "m4") || !strings.Contains(content, "m5") {
		t.Errorf("recent messages missing from the title prompt: %q", content)
	}
}

// TestProvisionalTitle_IsShortened pins the sidebar placeholder: the first user
// message can be long, and the session list is narrow, so the placeholder is cut
// well below the safety net that bounds a generated title.
func TestProvisionalTitle_IsShortened(t *testing.T) {
	sess := session.New("web:chat1")
	sess.AddMessage(provider.Message{Role: "user", Content: strings.Repeat("好", maxProvisionalTitleRunes+30)})
	if got := []rune(provisionalTitle(sess)); len(got) != maxProvisionalTitleRunes {
		t.Fatalf("provisional title = %d runes, want %d", len(got), maxProvisionalTitleRunes)
	}
	if maxProvisionalTitleRunes >= maxTitleRunes {
		t.Errorf("the placeholder bound (%d) must stay below the title safety net (%d)", maxProvisionalTitleRunes, maxTitleRunes)
	}
}

// TestMaybeTitle_DefaultWindowIsTen pins the widened default: with no
// `recent_messages` setting the title prompt sees the last ten messages, which
// is what makes a topic (rather than the first sentence) nameable.
func TestMaybeTitle_DefaultWindowIsTen(t *testing.T) {
	a, mock, store := newTitleTestAgent(t, 12)
	mock.Response = &provider.ChatResponse{Content: "T"}

	sess := store.GetOrCreate("t:window-default")
	for i := 0; i < 14; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: fmt.Sprintf("m%d", i)})
	}

	a.maybeTitle(context.Background(), sess)

	if len(mock.Calls) == 0 {
		t.Fatal("expected a title model call")
	}
	content := mock.Calls[0].Messages[1].Content
	if strings.Contains(content, "m3") {
		t.Errorf("default window is not 10: %q", content)
	}
	for _, want := range []string{"m4", "m13"} {
		if !strings.Contains(content, want) {
			t.Errorf("%s missing from the default window: %q", want, content)
		}
	}
}
