package agent

import (
	"strings"
	"testing"
)

// The language note is the only place the model is told which language this
// conversation is in. It is one English template with the language name
// substituted — never a translated note — so these tests pin the substitution,
// the fallbacks and the two-level precedence.

func TestNormalizeLanguageTag(t *testing.T) {
	cases := map[string]string{
		"zh":      "zh",
		"zh-CN":   "zh",
		"zh-Hans": "zh",
		"ZH-TW":   "zh",
		"en_GB":   "en",
		"  ja  ":  "ja",
		"":        "",
		"pt-BR":   "pt",
	}
	for in, want := range cases {
		if got := normalizeLanguageTag(in); got != want {
			t.Errorf("normalizeLanguageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLanguageNote_IsEnglishWithTheLanguageNamed(t *testing.T) {
	note := languageNote("zh-CN")
	if !strings.Contains(note, "Chinese (中文)") {
		t.Fatalf("the note does not name the language: %q", note)
	}
	// The body is English; the only Chinese in it is the native name.
	if strings.Contains(note, "本次对话") || strings.Contains(note, "回复") {
		t.Fatalf("the note was translated: %q", note)
	}
	if !strings.Contains(note, "This conversation is held in") {
		t.Fatalf("the template was not rendered: %q", note)
	}

	en := languageNote("en")
	if !strings.Contains(en, "English") || strings.Contains(en, "中文") {
		t.Fatalf("english note = %q", en)
	}

	// An unknown tag is still stated, rather than silently dropped.
	unknown := languageNote("pt-BR")
	if !strings.Contains(unknown, "Portuguese (português)") {
		t.Fatalf("pt-BR note = %q", unknown)
	}
	other := languageNote("xx")
	if !strings.Contains(other, "xx") {
		t.Fatalf("unknown tag note = %q", other)
	}

	// No resolved language means no line at all; the head prompt then falls back
	// to following the user's message.
	if got := languageNote(""); got != "" {
		t.Fatalf("empty tag produced a note: %q", got)
	}
}

func TestEffectiveLanguage(t *testing.T) {
	cases := []struct {
		name            string
		agent, platform string
		want            string
	}{
		{"account overrides the platform", "en", "zh", "en"},
		{"no choice inherits the platform", "", "zh", "zh"},
		{"nothing configured", "", "", ""},
		{"regional tag is normalised", "EN-gb", "zh", "en"},
		{"platform tag is normalised", "", "JA", "ja"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveLanguage(c.agent, c.platform); got != c.want {
				t.Fatalf("EffectiveLanguage(%q, %q) = %q, want %q", c.agent, c.platform, got, c.want)
			}
		})
	}
}
