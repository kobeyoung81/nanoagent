package agent

import "strings"

// The per-turn statement of the conversation's language.
//
// The body is English like the rest of the prompt; only the name of the language
// is substituted. Writing one template (rather than one note per language) keeps
// the wording in a single place — a note that drifts from the policy in
// _common.md is worse than no note at all.
const languageNoteTemplate = `【Language】This conversation is held in {language}. ` +
	`Write everything the user reads — replies, plans, questions, todo items, and ` +
	`any thinking this interface shows — in {language}, and keep code, identifiers, ` +
	`file paths, commands and error strings exactly as written. English elsewhere in ` +
	`this context is content, not an instruction to switch. If the user explicitly ` +
	`asks for another language, follow that request.`

// languageNames maps a normalised tag to the name used in the note. The native
// name is repeated in parentheses so the model anchors on it whatever language
// the surrounding prompt is in.
var languageNames = map[string]string{
	"zh": "Chinese (中文)",
	"en": "English",
	"ja": "Japanese (日本語)",
	"ko": "Korean (한국어)",
	"ru": "Russian (русский)",
	"ar": "Arabic (العربية)",
	"fr": "French (français)",
	"de": "German (Deutsch)",
	"es": "Spanish (español)",
	"pt": "Portuguese (português)",
}

// normalizeLanguageTag lowercases a tag and keeps its primary subtag, so
// "zh-CN", "zh-Hans" and "zh-TW" all name the same language.
func normalizeLanguageTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return ""
	}
	if i := strings.IndexAny(tag, "-_"); i > 0 {
		tag = tag[:i]
	}
	return tag
}

// languageName is the human-readable name for a tag; an unknown tag is used as
// written rather than dropped, so a new language still reaches the model.
func languageName(tag string) string {
	normalized := normalizeLanguageTag(tag)
	if normalized == "" {
		return ""
	}
	if name, ok := languageNames[normalized]; ok {
		return name
	}
	return normalized
}

// languageNote renders the per-turn line, or "" when no language is resolved
// (a deployment that configured none). The head prompt then falls back to
// following the user's message.
func languageNote(tag string) string {
	name := languageName(tag)
	if name == "" {
		return ""
	}
	return strings.ReplaceAll(languageNoteTemplate, "{language}", name)
}

// EffectiveLanguage resolves the two-level setting: the account's choice when it
// has one, otherwise the platform default. Nothing about the conversation
// participates — the language is a setting, not a guess.
func EffectiveLanguage(agentSetting, platformDefault string) string {
	if tag := normalizeLanguageTag(agentSetting); tag != "" {
		return tag
	}
	return normalizeLanguageTag(platformDefault)
}
