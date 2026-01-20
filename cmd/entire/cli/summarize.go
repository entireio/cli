package cli

import "entire.io/cli/cmd/entire/cli/stringutil"

// Summary holds the extracted/generated summary for a checkpoint.
// Currently uses heuristic extraction from transcripts.
// Future: AI summarization can populate Learnings and FrictionPoints.
type Summary struct {
	Intent         string   // The user's original intent (first prompt)
	Outcome        string   // The final outcome (last assistant response)
	Learnings      []string // Key learnings from the session (requires AI)
	FrictionPoints []string // Friction points encountered (requires AI)
}

// maxSummaryFieldLength is the maximum length (in runes) for Intent and Outcome fields.
const maxSummaryFieldLength = 200

// GenerateSummary extracts a summary from a transcript.
// Currently uses heuristic extraction:
// - Intent: first user prompt (truncated to 200 runes)
// - Outcome: last assistant message (truncated to 200 runes)
// - Learnings: empty (requires AI)
// - FrictionPoints: empty (requires AI)
//
//nolint:unparam // error return kept for future AI summarization which may fail
func GenerateSummary(transcript []transcriptLine) (*Summary, error) {
	prompts := extractUserPrompts(transcript)

	var intent string
	if len(prompts) > 0 {
		intent = stringutil.TruncateRunes(prompts[0], maxSummaryFieldLength, "...")
	}

	outcome := extractLastAssistantMessage(transcript)
	outcome = stringutil.TruncateRunes(outcome, maxSummaryFieldLength, "...")

	return &Summary{
		Intent:         intent,
		Outcome:        outcome,
		Learnings:      nil, // Requires AI - empty for now
		FrictionPoints: nil, // Requires AI - empty for now
	}, nil
}
