package agent

import (
	"math"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// estimateTokens approximates the token count of text. CJK ideographs, kana,
// Hangul and fullwidth forms are close to one token per rune, while
// Latin/ASCII text is roughly one token per four characters. The old single
// chars/2.5 ratio underestimated CJK by ~2.5x, which let requests far exceed
// the configured context window (e.g. a 128k window accepting ~196k tokens).
func estimateTokens(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}

	var tokens float64
	ascii := 0
	flushASCII := func() {
		if ascii > 0 {
			tokens += float64(ascii) / 4.0
			ascii = 0
		}
	}

	for _, r := range text {
		if r < 0x80 {
			ascii++
			continue
		}
		flushASCII()
		if isWideRune(r) {
			tokens++
		} else {
			// Accented Latin, Cyrillic, Greek, Arabic, ...: roughly half a
			// token per rune.
			tokens += 0.5
		}
	}
	flushASCII()

	if tokens < 1 {
		return 1
	}
	return int(math.Ceil(tokens))
}

// isWideRune reports whether a rune belongs to a script that is tokenized near
// one-token-per-rune (CJK, kana, Hangul, fullwidth forms).
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x11FF: // Hangul Jamo
		return true
	case r >= 0x2E80 && r <= 0x9FFF: // CJK Radicals .. CJK Unified Ideographs
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // Hangul Syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK Compatibility Ideographs
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // Halfwidth/Fullwidth Forms
		return true
	case r >= 0x20000 && r <= 0x3FFFF: // CJK Unified Ideographs Extension B+
		return true
	}
	return false
}

// imageTokenEstimate is a conservative per-image token cost for budget
// accounting; inline images are expanded only at request time, so the exact
// pixels are unknown here.
const imageTokenEstimate = 1000

// tokenEstimateHeuristic estimates tokens from a message slice.
func tokenEstimateHeuristic(msgs []provider.Message) int {
	tokens := 0
	for _, m := range msgs {
		tokens += estimateTokens(m.Content)
		// Replayed reasoning_content is concatenated into the provider context,
		// so it must count against the budget.
		tokens += estimateTokens(m.Reasoning)
		tokens += len(m.Images) * imageTokenEstimate
		for _, tc := range m.ToolCalls {
			tokens += estimateTokens(tc.Name) + estimateTokens(tc.Arguments) + 4 // overhead per tool call
		}
		tokens += 4 // per-message overhead (role, separators)
	}
	return tokens
}

// maxOutputReserve caps how much of the context window is reserved for the
// model's own output when budgeting prompt space. A catalog max_output_tokens
// describes the model's capability, not a per-request reservation: reserving it
// verbatim (e.g. 384k of a 1M window) shrank the prompt budget so far that
// auto-compaction fired at a fraction of the window the user sees. The request's
// max_tokens is unaffected; under-reserving is safe because an over-long request
// is caught by the context-length handler, which compacts and retries.
const maxOutputReserve = 32768

// defaultTriggerPercent is where auto-compaction fires when the configured
// value is absent (0 or negative).
const defaultTriggerPercent = 75

// minTailBudget is the floor for the conversation tail once the pinned prefix
// has been paid for: the most recent turn must survive even when the system
// prompt and the summary together eat the whole budget.
const minTailBudget = 1024

// normalizeTriggerPercent clamps a configured trigger percentage into the range
// the gate can actually reach, reporting whether it had to change the value so
// the caller can warn instead of silently rewriting policy. The gate measures a
// budget-windowed request, which is bounded by the budget, so a trigger above
// 100% of it can never fire — and a context guard that silently never fires is
// the worst failure mode it has. Zero or negative means "unset".
func normalizeTriggerPercent(percent int) (int, bool) {
	switch {
	case percent <= 0:
		return defaultTriggerPercent, false
	case percent > 100:
		return 100, true
	default:
		return percent, false
	}
}

// compactionPlan resolves the prompt budget and the token count at which
// auto-compaction fires. It is the single source of truth for both: the turn's
// request builder budgets against it, and the usage event the client draws its
// threshold marker from reports it, so the marker can never disagree with the
// gate (the client used to hard-code 75% while the gate read a configurable
// value, which made every real trigger point look like a missed one).
func compactionPlan(contextWindow, maxOutputTokens, triggerPercent int) (budget, threshold int) {
	budget = budgetAvailable(contextWindow, maxOutputTokens)
	percent, _ := normalizeTriggerPercent(triggerPercent)
	threshold = budget * percent / 100
	if threshold < 1 {
		threshold = 1
	}
	return budget, threshold
}

// budgetAvailable returns the number of tokens available for prompt content,
// reserving space for max output tokens.
func budgetAvailable(contextWindow, maxOutputTokens int) int {
	if maxOutputTokens > maxOutputReserve {
		maxOutputTokens = maxOutputReserve
	}
	budget := contextWindow - maxOutputTokens
	if budget < 1024 {
		budget = 1024
	}
	return budget
}

// turnBoundaries returns indices where each "turn" starts in the message slice.
// A turn begins with a user message and includes all subsequent assistant/tool messages
// until the next user message.
func turnBoundaries(msgs []provider.Message) []int {
	var boundaries []int
	for i, m := range msgs {
		if m.Role == "user" {
			boundaries = append(boundaries, i)
		}
	}
	return boundaries
}

// truncateToBudget removes the oldest turns (keeping the most recent ones)
// until the estimated token count fits within budget.
// Returns the truncated messages and whether any truncation occurred.
func truncateToBudget(msgs []provider.Message, budget int) ([]provider.Message, bool) {
	if tokenEstimateHeuristic(msgs) <= budget {
		return msgs, false
	}

	boundaries := turnBoundaries(msgs)
	if len(boundaries) <= 1 {
		// Only one turn or no turns — can't truncate further
		return msgs, false
	}

	// Remove oldest turns one at a time until we fit
	for startTurn := 1; startTurn < len(boundaries); startTurn++ {
		cutIdx := boundaries[startTurn]
		truncated := msgs[cutIdx:]
		if tokenEstimateHeuristic(truncated) <= budget {
			return truncated, true
		}
	}

	// Keep at least the last turn
	lastTurn := boundaries[len(boundaries)-1]
	return msgs[lastTurn:], true
}

// buildRequestMessages assembles the LLM-facing message list: the head (head 1 —
// the static system prompt — followed by one frozen system message per session
// file), the pinned prefix, then as much of the conversation tail as the
// remaining budget allows. It returns the messages and how many conversation
// messages were dropped, so the caller can log a truncation that is otherwise
// invisible.
//
// The pinned prefix (the running summary, the ephemeral mode note) is kept out
// of truncation on purpose. It used to be prepended into the same slice the
// truncator walked, and since the truncator drops the OLDEST turns first the
// summary was the first thing to go: a compaction was applied, immediately
// truncated away, and the model kept reading the raw tail with no summary — the
// expensive half of compaction was paid for nothing.
func buildRequestMessages(head, pinned, conversation []provider.Message, budget int) ([]provider.Message, int) {
	headTokens := tokenEstimateHeuristic(head)
	pinnedTokens := tokenEstimateHeuristic(pinned)
	tailBudget := budget - headTokens - pinnedTokens
	if tailBudget < minTailBudget {
		tailBudget = minTailBudget
	}
	tail, _ := truncateToBudget(conversation, tailBudget)
	out := make([]provider.Message, 0, len(head)+len(pinned)+len(tail))
	out = append(out, head...)
	out = append(out, pinned...)
	out = append(out, tail...)
	return out, len(conversation) - len(tail)
}
