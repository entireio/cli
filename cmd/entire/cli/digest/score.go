package digest

import (
	"strings"
)

// scorePrompt ranks a prompt by "interestingness" using heuristics.
// Higher scores = more likely to be fun, emotional, or notable.
func scorePrompt(text string) int {
	score := 0
	words := strings.Fields(text)
	lower := strings.ToLower(text)

	// Short prompts show personality ("ls", "Word!", "full")
	if len(words) <= 5 && len(words) > 0 {
		score += 3
	}

	// Single-word disbelief/reaction ("Really?", "Seriously?", "No.", "What?")
	if len(words) == 1 && strings.ContainsAny(text, "!?.") {
		score += 4
	}

	// Emotional punctuation
	if strings.ContainsAny(text, "!?") {
		score += 2
	}

	// Frustration signals
	for _, word := range frustrationWords {
		if strings.Contains(lower, word) {
			score += 3
			break
		}
	}

	// Sarcasm and wit signals
	for _, phrase := range sarcasmPhrases {
		if strings.Contains(lower, phrase) {
			score += 3
			break
		}
	}

	// Context-amnesia complaints (calling out AI memory failures)
	for _, phrase := range amnesiaPhrases {
		if strings.Contains(lower, phrase) {
			score += 3
			break
		}
	}

	// AI self-reference questions ("are you stuck", "did you just", "why did you")
	for _, phrase := range aiSelfReferencePhrases {
		if strings.Contains(lower, phrase) {
			score += 2
			break
		}
	}

	// Celebration signals
	for _, word := range celebrationWords {
		if strings.Contains(lower, word) {
			score += 3
			break
		}
	}

	// Penalize long copy-paste prompts (likely specs or context dumps)
	if len(text) > 500 {
		score -= 2
	}

	// Heavily penalize auto-generated session continuations
	if strings.Contains(text, "This session is being continued") {
		score -= 5
	}

	// Penalize structured instructions (markdown headers)
	if strings.HasPrefix(text, "#") || strings.HasPrefix(text, "## ") {
		score--
	}

	// Penalize skill/command invocations
	if strings.HasPrefix(text, "/") && len(words) <= 3 {
		score--
	}

	return score
}

var frustrationWords = []string{
	"broken", "wrong", "why", "stuck", "hate", "ugh",
	"wtf", "terrible", "awful", "annoying", "confused",
	"doesn't work", "not working", "failing",
}

var sarcasmPhrases = []string{
	"captain obvious", "thanks for nothing", "you just said that",
	"literally what i just said", "i just told you", "i already said",
	"that's what i said", "did you even read", "are you serious",
	"thanks for the lecture",
}

var amnesiaPhrases = []string{
	"have you forgotten", "you just told me", "you said earlier",
	"you already", "we just discussed", "remember when",
	"forgotten everything", "you literally just",
}

var aiSelfReferencePhrases = []string{
	"are you stuck", "are you ok", "did you just",
	"why did you", "can you explain why you", "what are you doing",
	"is bash stuck", "you seem to be",
}

var celebrationWords = []string{
	"worked", "perfect", "awesome", "finally", "nice",
	"great", "love", "beautiful", "nailed", "ship it",
}
