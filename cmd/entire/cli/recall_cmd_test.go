package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/experimental"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/stretchr/testify/require"
)

func TestRecallCommandIsExperimentalAndListedInLabs(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"recall"})
	require.NoError(t, err)
	require.Equal(t, "recall", cmd.Name())
	require.Equal(t, experimental.GroupID, cmd.GroupID, "recall must be gated as experimental")
	require.Contains(t, labsOverview(), "entire recall")

	ingest, _, err := root.Find([]string{"recall", "ingest"})
	require.NoError(t, err)
	require.Equal(t, "ingest", ingest.Name())
}

func TestRecallTurnsFromEntries_KeepsOnlyUserAndAssistantText(t *testing.T) {
	t.Parallel()

	entries := []summarize.Entry{
		{Type: summarize.EntryTypeUser, Content: "  Add a retry\n  wrapper  "},
		{Type: summarize.EntryTypeTool, ToolName: "Read", ToolDetail: "x.go"},
		{Type: summarize.EntryTypeAssistant, Content: strings.Repeat("a", 50)},
		{Type: summarize.EntryTypeAssistant, Content: "   "},
	}
	turns := recallTurnsFromEntries(entries, 20)

	require.Equal(t, []recallTurn{
		{Role: "user", Text: "Add a retry wrapper"},
		{Role: "assistant", Text: strings.Repeat("a", 17) + "..."},
	}, turns, "tool entries and blank turns are dropped; assistant text is truncated, user text is not")
}

func TestRecallDiffLines_KeepsHunksAndChangesDropsNoise(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"diff --git a/internal/x.go b/internal/x.go",
		"index 0000000..1111111 100644",
		"--- a/internal/x.go",
		"+++ b/internal/x.go",
		"@@ -0,0 +1,2 @@",
		"+func withRetry() {}",
		"-old := 1",
		"\\ No newline at end of file",
	}, "\n")
	got := recallDiffLines(raw, 100)

	require.Equal(t, []string{
		"+++ b/internal/x.go",
		"@@ -0,0 +1,2 @@",
		"+func withRetry() {}",
		"-old := 1",
	}, got)
}

func TestRecallDiffLines_IsCapped(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	for range 50 {
		sb.WriteString("+line\n")
	}
	require.Len(t, recallDiffLines(sb.String(), 10), 10)
}

func TestRecallBinary_PrefersEnvThenPathThenCargoTarget(t *testing.T) {
	t.Parallel()

	exists := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, want := range paths {
				if p == want {
					return true
				}
			}
			return false
		}
	}
	noPath := func(string) (string, error) { return "", errors.New("not found") }

	got, err := recallBinary("/repo", "/opt/recall", noPath, exists("/opt/recall"))
	require.NoError(t, err)
	require.Equal(t, "/opt/recall", got, "ENTIRE_RECALL_BIN wins")

	got, err = recallBinary("/repo", "", func(string) (string, error) { return "/usr/bin/recall", nil }, exists())
	require.NoError(t, err)
	require.Equal(t, "/usr/bin/recall", got, "then $PATH")

	got, err = recallBinary("/repo", "", noPath, exists("/repo/recall/target/debug/recall"))
	require.NoError(t, err)
	require.Equal(t, "/repo/recall/target/debug/recall", got, "then the cargo build in the repo")

	_, err = recallBinary("/repo", "", noPath, exists())
	require.Error(t, err)
	require.Contains(t, err.Error(), "cargo build", "the error tells the user how to get a binary")
}

func TestRecallCheckpointFromReader_MapsSummaryAndLatestSessionToWireShape(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abcd12345678")
	transcript := []byte(`{"type":"user","message":{"role":"user","content":"Add a retry wrapper around the client"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Adding retry.go now."}]},"uuid":"a1"}
`)
	reader := &stubCommittedReader{
		summary: &checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			FilesTouched: []string{"internal/dispatch/retry.go"},
			Sessions:     []checkpoint.SessionFilePaths{{Metadata: "ab/cd12345678/0/metadata.json"}},
		},
		contents: map[int]*checkpoint.SessionContent{
			0: {
				Metadata:   checkpoint.Metadata{SessionID: "s1", Agent: "Claude Code"},
				Transcript: transcript,
			},
		},
	}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: add retry wrapper\n\nEntire-Checkpoint: abcd12345678\n", CheckpointID: cpID}

	got := recallCheckpointFromReader(context.Background(), io.Discard, reader, commit, []string{"+func withRetry() {}"}, nil, false)
	require.Empty(t, got.Unavailable, "a fully readable checkpoint declares nothing missing")

	require.Equal(t, "abcd12345678", got.CheckpointID)
	require.Equal(t, "9f2c1ab", got.CommitSHA)
	require.Equal(t, "feat: add retry wrapper", got.CommitMessage, "subject only: the trailer must not leak into the ledger text")
	require.Equal(t, "Claude Code", got.Agent)
	require.Equal(t, []string{"internal/dispatch/retry.go"}, got.Files)
	require.Equal(t, []string{"+func withRetry() {}"}, got.Diff)
	require.Equal(t, []recallTurn{
		{Role: "user", Text: "Add a retry wrapper around the client"},
		{Role: "assistant", Text: "Adding retry.go now."},
	}, got.Session)
}

func TestRenderRecallHits_ShowsTierVerdictAndBackingCommit(t *testing.T) {
	t.Parallel()

	backed := "77b4102 chore: drop retry wrapper"
	hits := []recallHit{
		{Tier: "INTENT", Verdict: "corroborated", Scored: 9.73, Confidence: 0.98, Text: "Remove the retry wrapper.", Why: "3 terms match commit subject", BackedBy: &backed},
		{Tier: "chat", Verdict: "contradicted", Scored: 1.20, Confidence: 0.21, Text: "This is isolated.", Why: "claims isolation; Graph reach escapes to 2"},
	}
	var out bytes.Buffer
	renderRecallHits(&out, "why did we drop the retry wrapper", recallCoverage{Total: 3, Complete: 3, GraphAvailable: true}, hits)
	s := out.String()

	for _, want := range []string{
		"why did we drop the retry wrapper",
		"INTENT", "✓ corroborated", "Remove the retry wrapper.", "commit 77b4102 chore: drop retry wrapper",
		"chat", "✗ CONTRADICTED", "Graph reach escapes",
	} {
		require.Contains(t, s, want)
	}
	require.Less(t, strings.Index(s, "INTENT"), strings.Index(s, "chat"), "hits print in ranked order")
}

func TestRenderRecallHits_EmptySaysSo(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	renderRecallHits(&out, "anything", recallCoverage{Total: 1, Complete: 1, GraphAvailable: true}, nil)
	require.Contains(t, out.String(), "no memories")
}

// ── privacy boundary: redacted or unavailable checkpoint data ─────────────

// recallStubReader fails on demand and counts transcript reads, so a test can
// prove the transcript was never opened.
type recallStubReader struct {
	summary      *checkpoint.CheckpointSummary
	readErr      error
	content      *checkpoint.SessionContent
	sessionErr   error
	sessionReads int
}

func (r *recallStubReader) Read(context.Context, id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	return r.summary, nil
}

func (r *recallStubReader) ReadSessionContent(context.Context, id.CheckpointID, int) (*checkpoint.SessionContent, error) {
	r.sessionReads++
	if r.sessionErr != nil {
		return nil, r.sessionErr
	}
	return r.content, nil
}

func recallTestSummary(cpID id.CheckpointID) *checkpoint.CheckpointSummary {
	return &checkpoint.CheckpointSummary{
		CheckpointID: cpID,
		FilesTouched: []string{"internal/dispatch/retry.go"},
		Sessions:     []checkpoint.SessionFilePaths{{Metadata: "ab/cd12345678/0/metadata.json"}},
	}
}

func TestRecallCheckpointFromReader_MissingTranscriptIsLedgerOnlyPartial(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abcd12345678")
	reader := &recallStubReader{summary: recallTestSummary(cpID), sessionErr: errors.New("transcript withheld")}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: add retry wrapper\n\nEntire-Checkpoint: abcd12345678\n", CheckpointID: cpID}

	got := recallCheckpointFromReader(context.Background(), io.Discard, reader, commit, []string{"+func withRetry() {}"}, nil, false)

	require.Equal(t, "9f2c1ab", got.CommitSHA, "the ledger record survives without its transcript")
	require.Equal(t, "feat: add retry wrapper", got.CommitMessage)
	require.Equal(t, []string{"internal/dispatch/retry.go"}, got.Files)
	require.Empty(t, got.Session)
	require.Equal(t, []string{"session"}, got.Unavailable, "the missing field is declared, not inferred")
}

func TestRecallCheckpointFromReader_UnreadableCheckpointFallsBackToGitFacts(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abcd12345678")
	reader := &recallStubReader{readErr: errors.New("fetch checkpoint ref: repository not found")}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: add retry wrapper", CheckpointID: cpID}
	diff := []string{"+++ b/internal/dispatch/retry.go", "@@ -0,0 +1 @@", "+func withRetry() {}", "+++ b/internal/dispatch/client.go", "-old := 1"}

	got := recallCheckpointFromReader(context.Background(), io.Discard, reader, commit, diff, nil, false)

	require.Equal(t, []string{"internal/dispatch/retry.go", "internal/dispatch/client.go"}, got.Files, "files come from the diff headers when the store cannot be read")
	require.Equal(t, diff, got.Diff)
	require.Equal(t, []string{"session"}, got.Unavailable)
}

func TestRecallCheckpointFromReader_NoTranscriptsNeverOpensTheSession(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abcd12345678")
	reader := &recallStubReader{summary: recallTestSummary(cpID), content: &checkpoint.SessionContent{Transcript: []byte("{}")}}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: add retry wrapper", CheckpointID: cpID}

	got := recallCheckpointFromReader(context.Background(), io.Discard, reader, commit, nil, nil, true)

	require.Zero(t, reader.sessionReads, "--no-transcripts must not read the transcript at all")
	require.Empty(t, got.Session)
	require.Equal(t, []string{"session"}, got.Unavailable)
}

func TestRecallCheckpointFromReader_DiffFailureIsDeclaredNotSilent(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abcd12345678")
	reader := &recallStubReader{summary: recallTestSummary(cpID), sessionErr: errors.New("withheld")}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: add retry wrapper", CheckpointID: cpID}

	got := recallCheckpointFromReader(context.Background(), io.Discard, reader, commit, nil, errors.New("git show: exit 128"), false)

	require.ElementsMatch(t, []string{"session", "diff"}, got.Unavailable)
	require.Equal(t, []string{"internal/dispatch/retry.go"}, got.Files, "the summary's file list still stands")
}

func TestRecallCheckpointFromReader_NoFilesAnywhereIsDeclared(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abcd12345678")
	reader := &recallStubReader{readErr: errors.New("unreadable")}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: add retry wrapper", CheckpointID: cpID}

	got := recallCheckpointFromReader(context.Background(), io.Discard, reader, commit, nil, errors.New("git show failed"), false)

	require.ElementsMatch(t, []string{"session", "diff", "files"}, got.Unavailable)
}

func TestRecallFilesFromDiff_ReadsHeadersInOrder(t *testing.T) {
	t.Parallel()

	diff := []string{"+++ b/a/x.go", "+line", "+++ b/b/y.go", "+++ b/a/x.go", "+++ /dev/null"}
	require.Equal(t, []string{"a/x.go", "b/y.go"}, recallFilesFromDiff(diff), "deduplicated, /dev/null dropped")
}

func TestRenderRecallHits_PartialContextIsLabelledAndNeverCalledComplete(t *testing.T) {
	t.Parallel()

	cov := recallCoverage{Total: 43, Complete: 3, Partial: 40, GraphAvailable: true, Unavailable: map[string]int{"session": 40}}
	backed := "9f2c1ab feat: add retry wrapper"
	hits := []recallHit{
		{Tier: "chat", Verdict: "unverifiable", Scored: 1.2, Confidence: 0.30, Text: "Added setMaxRetries.", Why: "names setMaxRetries; diff unavailable", BackedBy: &backed, Partial: []string{"diff"}},
		{Tier: "LEDGER", Verdict: "corroborated", Scored: 2.6, Confidence: 0.95, Text: "COMMIT 9f2c1ab", Why: "is the commit record", BackedBy: &backed, Partial: []string{"session"}},
	}
	var out bytes.Buffer
	renderRecallHits(&out, "what changed", cov, hits)
	s := out.String()

	for _, want := range []string{"PARTIAL", "3 of 43", "40 without transcript", "? UNVERIFIABLE", "diff unavailable", "◌ partial context: diff", "◌ partial context: transcript"} {
		require.Contains(t, s, want)
	}
	require.NotContains(t, strings.ToLower(s), "context: complete", "partial context is never presented as complete")
}

func TestRenderRecallHits_CompleteContextSaysSo(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	renderRecallHits(&out, "q", recallCoverage{Total: 3, Complete: 3, GraphAvailable: true}, []recallHit{{Tier: "chat", Verdict: "neutral", Text: "x", Why: "no overlap with commit subject"}})
	s := out.String()
	require.Contains(t, s, "context: complete · 3 checkpoints")
	require.NotContains(t, s, "◌ partial")
	require.Contains(t, s, "· neutral", "neutral is 'checked, nothing found' and is not spelled like unverifiable")
}

func TestRecallCoverageLine_TruncationAndMissingGraphAreIncomplete(t *testing.T) {
	t.Parallel()

	line := recallCoverageLine(recallCoverage{Total: 5, Complete: 5, Truncated: true, GraphAvailable: false})
	require.Contains(t, line, "PARTIAL")
	require.Contains(t, line, "older checkpoints not examined")
	require.Contains(t, line, "graph unavailable")
}

func TestRecallCheckpointFromReader_PartialCheckpointMarshalsArraysNotNull(t *testing.T) {
	t.Parallel()

	// Regression from the first real --no-transcripts run: a nil slice
	// marshals as JSON null, and the Rust side rejects null for a sequence.
	cpID := id.MustCheckpointID("abcd12345678")
	reader := &recallStubReader{readErr: errors.New("unreadable")}
	commit := recallCommit{SHA: "9f2c1ab", Message: "feat: x", CheckpointID: cpID}

	got := recallCheckpointFromReader(context.Background(), io.Discard, reader, commit, nil, errors.New("no diff"), false)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	for _, field := range []string{`"session":[]`, `"files":[]`, `"diff":[]`} {
		require.Contains(t, string(raw), field, "%s", raw)
	}
	require.NotContains(t, string(raw), "null")
}

func TestRenderRecallHits_RedactedClaimMarkerReadsAsASentence(t *testing.T) {
	t.Parallel()

	hits := []recallHit{{Tier: "INTENT", Verdict: "corroborated", Text: "the key was REDACTED", Why: "2 terms match commit subject", Partial: []string{"diff", "claim redacted"}}}
	var out bytes.Buffer
	renderRecallHits(&out, "q", recallCoverage{Total: 1, Complete: 1, GraphAvailable: true}, hits)
	require.Contains(t, out.String(), "◌ partial context: diff unavailable; claim redacted")
	require.NotContains(t, out.String(), "redacted unavailable")
}
