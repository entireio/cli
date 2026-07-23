package codex

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/stretchr/testify/require"
)

// lineCount counts non-empty JSONL lines the same way GetTranscriptPosition does
// for well-formed rollouts (no blank lines).
func lineCount(data []byte) int { return len(splitJSONL(data)) }

// TestIncremental_MatchesFullScan replays every turn-end hook of a growing
// session and asserts the incremental calc produces byte-identical numbers to
// the full-scan CalculateTokenUsage — carrying the persisted snapshot forward
// like the lifecycle does.
func TestIncremental_MatchesFullScan(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	const turns = 8
	var prior *agent.CumulativeTokenSnapshot
	fromOffset := 0 // turn 1 starts at line 0

	for turn := 1; turn <= turns; turn++ {
		data, _ := buildRollout(turn, false, 0)

		full, err := ag.CalculateTokenUsage(data, fromOffset)
		require.NoError(t, err)

		inc, next, err := ag.CalculateTokenUsageIncremental(data, fromOffset, prior)
		require.NoError(t, err)
		require.Equal(t, full, inc, "turn %d: incremental delta must equal full scan", turn)

		// Next turn starts where this transcript ended.
		prior = next
		fromOffset = lineCount(data)
	}
}

// TestIncremental_MatchesFullScan_WithCachedTokens uses the shipped fixture that
// includes cached-input tokens and a mid-session offset.
func TestIncremental_MatchesFullScan_WithCachedTokens(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	data := []byte(sampleRollout)

	for _, offset := range []int{0, 4, 8, 11} {
		full, err := ag.CalculateTokenUsage(data, offset)
		require.NoError(t, err)

		// Cold start (no prior): must fall back to a full scan and match exactly.
		inc, next, err := ag.CalculateTokenUsageIncremental(data, offset, nil)
		require.NoError(t, err)
		require.Equal(t, full, inc, "offset %d: cold-start incremental must equal full scan", offset)
		require.NotNil(t, next)
		require.Equal(t, lineCount(data), next.LineCount, "offset %d: snapshot line count", offset)
	}
}

// TestIncremental_ColdStartProducesReusableBaseline verifies a cold-start call
// yields a snapshot that a subsequent incremental call can baseline off to get
// the correct per-checkpoint delta.
func TestIncremental_ColdStartProducesReusableBaseline(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	// Turn 1: 2 token_count lines, cold start.
	turn1, _ := buildRollout(2, false, 0)
	_, snap, err := ag.CalculateTokenUsageIncremental(turn1, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.Equal(t, lineCount(turn1), snap.LineCount)

	// Turn 2: one more token_count line appended.
	turn2, _ := buildRollout(3, false, 0)
	fromOffset := lineCount(turn1)

	inc, _, err := ag.CalculateTokenUsageIncremental(turn2, fromOffset, snap)
	require.NoError(t, err)
	full, err := ag.CalculateTokenUsage(turn2, fromOffset)
	require.NoError(t, err)
	require.Equal(t, full, inc, "warm incremental delta must equal full scan")
}

// TestIncremental_ResumedOffsetFallsBackToFullScan covers a stale baseline
// (LineCount past the current fromOffset, as after a transcript reset/resume):
// the calc must ignore it and full-scan, still producing correct numbers.
func TestIncremental_ResumedOffsetFallsBackToFullScan(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	data, _ := buildRollout(3, false, 0)
	fromOffset := 3

	stale := &agent.CumulativeTokenSnapshot{LineCount: 9999, InputTokens: 1, CachedInputTokens: 1, OutputTokens: 1}
	inc, _, err := ag.CalculateTokenUsageIncremental(data, fromOffset, stale)
	require.NoError(t, err)
	full, err := ag.CalculateTokenUsage(data, fromOffset)
	require.NoError(t, err)
	require.Equal(t, full, inc, "stale baseline must be ignored (full-scan fallback)")
}

// TestIncremental_DoesNotParseHistory is the acceptance criterion: parse work is
// bounded by lines added since the last checkpoint, not the whole session.
// We corrupt every pre-offset line with bogus token_counts. A full scan of the
// corrupted transcript changes the numbers (it reads the bogus baseline); the
// incremental call, given a valid prior baseline, slices the history off and is
// UNAFFECTED — proving it never parsed those lines.
func TestIncremental_DoesNotParseHistory(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	clean, _ := buildRollout(5, false, 0)
	fromOffset := lineCount(clean)

	// Correct baseline as of fromOffset = the last cumulative in `clean`.
	_, prior, err := ag.CalculateTokenUsageIncremental(clean, fromOffset, nil)
	require.NoError(t, err)
	require.NotNil(t, prior)

	// Append one more turn to `clean` (the new delta the next hook sees).
	cleanNext, _ := buildRollout(6, false, 0)
	tail := transcript.SliceFromLine(cleanNext, fromOffset)

	// Build a corrupted transcript: same line count for lines <= fromOffset, but
	// every pre-offset token_count carries absurd values, followed by the real tail.
	var corrupt strings.Builder
	corrupt.WriteString(`{"timestamp":"t","type":"session_meta","payload":{"id":"x"}}`)
	corrupt.WriteByte('\n')
	for i := 2; i <= fromOffset; i++ {
		corrupt.WriteString(tokenCountLine(9_000_000, 8_000_000, 900_000))
		corrupt.WriteByte('\n')
	}
	corrupt.Write(tail)
	corrupted := []byte(corrupt.String())
	require.Equal(t, fromOffset, lineCount(corrupted)-lineCount(tail),
		"corrupted history must have the same pre-offset line count")

	// A FULL scan of the corrupted transcript reads the bogus baseline → wrong.
	fullCorrupt, err := ag.CalculateTokenUsage(corrupted, fromOffset)
	require.NoError(t, err)

	// The INCREMENTAL call with the valid prior slices off history → correct,
	// identical to the clean full scan.
	incCorrupt, _, err := ag.CalculateTokenUsageIncremental(corrupted, fromOffset, prior)
	require.NoError(t, err)
	cleanFull, err := ag.CalculateTokenUsage(cleanNext, fromOffset)
	require.NoError(t, err)

	require.Equal(t, cleanFull, incCorrupt,
		"incremental result must ignore corrupted history (only delta lines parsed)")
	require.NotEqual(t, fullCorrupt, incCorrupt,
		"a full scan WOULD have been corrupted — proving incremental skipped history")
}

// TestExtractAllModifiedFiles_FromBytes verifies the bytes-based extractor
// returns the same files as the disk-based ExtractModifiedFilesFromOffset, so
// the turn-end pipeline avoids the second full-file disk read.
func TestExtractAllModifiedFiles_FromBytes(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	data := []byte(sampleRollout)
	path := writeSampleRollout(t)

	for _, offset := range []int{0, 9} {
		fromBytes, err := ag.ExtractAllModifiedFiles(data, offset, "")
		require.NoError(t, err)
		fromDisk, _, err := ag.ExtractModifiedFilesFromOffset(path, offset)
		require.NoError(t, err)
		require.ElementsMatch(t, fromDisk, fromBytes, "offset %d: bytes extractor must match disk extractor", offset)
	}
}

// TestCodex_DeclaresBytesCapabilities locks in that Codex now routes both token
// calc and file extraction through in-memory bytes (no disk reread, incremental
// tokens) — the opposite of the pre-fix validation guard H3.
func TestCodex_DeclaresBytesCapabilities(t *testing.T) {
	t.Parallel()
	ag := NewCodexAgent()

	_, incOK := agent.AsIncrementalTokenCalculator(ag)
	require.True(t, incOK, "Codex must be an IncrementalTokenCalculator")

	_, subOK := agent.AsSubagentAwareExtractor(ag)
	require.True(t, subOK, "Codex must expose a bytes-based (SubagentAware) file extractor")
}
