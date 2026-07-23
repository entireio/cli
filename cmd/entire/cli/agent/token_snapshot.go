package agent

// CumulativeTokenSnapshot caches an agent's running cumulative token totals at a
// known transcript position. Agents that report CUMULATIVE counts (e.g. Codex,
// whose rollout emits token_count events carrying total_token_usage since
// session start) can use it to compute a checkpoint delta by scanning only the
// transcript lines added since this snapshot, instead of re-parsing the whole
// rollout on every turn-end hook.
//
// It is persisted in session state and round-trips as JSON.
type CumulativeTokenSnapshot struct {
	// LineCount is the transcript line count when this snapshot was captured
	// (same counting convention as TranscriptAnalyzer.GetTranscriptPosition).
	// The next incremental calc scans from here and uses it as the fast-path
	// guard: the snapshot is a valid baseline only when LineCount <= the next
	// fromOffset (otherwise the transcript was reset/rewound and we full-scan).
	LineCount int `json:"line_count"`
	// InputTokens is the cumulative total input tokens reported at LineCount.
	InputTokens int `json:"input_tokens"`
	// CachedInputTokens is the cumulative cached-input tokens reported at LineCount.
	CachedInputTokens int `json:"cached_input_tokens"`
	// OutputTokens is the cumulative output tokens reported at LineCount.
	OutputTokens int `json:"output_tokens"`
}

// IncrementalTokenCalculator is a TokenCalculator whose transcript reports
// CUMULATIVE token counts. It computes the checkpoint delta from a persisted
// baseline snapshot, scanning only the lines added since that snapshot rather
// than the whole transcript — the whole-file rescan is O(session) per hook and
// O(session^2) across a session.
type IncrementalTokenCalculator interface {
	TokenCalculator

	// CalculateTokenUsageIncremental returns the checkpoint token delta for
	// transcript lines after fromOffset, plus a fresh snapshot to persist for the
	// next checkpoint. prior is the snapshot persisted at the previous checkpoint,
	// or nil on cold start (first checkpoint, resumed/imported session, or after a
	// transcript reset); when prior is nil or stale the implementation falls back
	// to a full scan for that one call.
	//
	// CONTRACT: for any given inputs, the returned usage MUST equal
	// CalculateTokenUsage(transcriptData, fromOffset) — only the amount of parsing
	// differs. next is nil only when the transcript carries no cumulative token
	// data at all.
	CalculateTokenUsageIncremental(transcriptData []byte, fromOffset int, prior *CumulativeTokenSnapshot) (usage *TokenUsage, next *CumulativeTokenSnapshot, err error)
}
