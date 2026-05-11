package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

const (
	exportTestAuthorName  = "Test"
	exportTestAuthorEmail = "export-test@entire.local"
)

// setupExportRepo creates a git repo with v2 checkpoints enabled and an
// initial commit (required for HEAD-resolving operations). The caller is
// responsible for chdir; this helper does NOT call t.Parallel because tests
// using t.Chdir cannot parallelize.
func setupExportRepo(t *testing.T) *git.Repository {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	testFile := filepath.Join(tmpDir, "f.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("init"), 0o600))
	_, err = wt.Add("f.txt")
	require.NoError(t, err)
	_, err = wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: exportTestAuthorName, Email: exportTestAuthorEmail, When: time.Now()},
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`),
		0o600,
	))

	return repo
}

func writeV2CheckpointForExport(t *testing.T, repo *git.Repository, cpID id.CheckpointID, opts checkpoint.WriteCommittedOptions) {
	t.Helper()
	store := checkpoint.NewV2GitStore(repo, "origin")
	opts.CheckpointID = cpID
	if opts.AuthorName == "" {
		opts.AuthorName = exportTestAuthorName
	}
	if opts.AuthorEmail == "" {
		opts.AuthorEmail = exportTestAuthorEmail
	}
	if opts.Strategy == "" {
		opts.Strategy = "manual-commit"
	}
	require.NoError(t, store.WriteCommitted(context.Background(), opts))
}

func TestRunExplainExport_JSONSingleCheckpoint(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("aaaa11112222")
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:         "session-json",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
		CompactTranscript: []byte(`{"v":1,"type":"user"}` + "\n"),
	})

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "aaaa1111",
		json:         true,
		sessionIndex: -1,
	})
	require.NoError(t, err)

	var envelope checkpointExportJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope), "output: %s", stdout.String())

	require.Equal(t, cpID.String(), envelope.CheckpointID)
	require.Equal(t, 1, envelope.SessionCount)
	require.Len(t, envelope.Sessions, 1)
	require.Equal(t, "session-json", envelope.Sessions[0].SessionID)
	require.Equal(t, 0, envelope.Sessions[0].Index)
}

// TestRunExplainExport_JSONUsesMetadataOnlyReader verifies the codex finding 3:
// the v1 fallback for --json must read metadata.json directly, not via
// ReadSessionContent (which depends on transcript availability). We exercise
// this by writing a v1 checkpoint with v2 disabled, then asserting the
// envelope has populated per-session fields (not a stub entry).
func TestRunExplainExport_JSONUsesMetadataOnlyReader(t *testing.T) {
	repo := setupExportRepo(t)

	// Disable v2 in settings to force the v1 path. setupExportRepo wrote
	// `checkpoints_v2: true`; overwrite it.
	require.NoError(t, os.WriteFile(".entire/settings.json", []byte(`{"enabled": true}`), 0o600))

	cpID := id.MustCheckpointID("777711112222")
	v1 := checkpoint.NewGitStore(repo)
	require.NoError(t, v1.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-v1-only",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"raw"}]}}` + "\n")),
		AuthorName:   exportTestAuthorName,
		AuthorEmail:  exportTestAuthorEmail,
	}))

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "777711",
		json:         true,
		sessionIndex: -1,
	})
	require.NoError(t, err)

	var envelope checkpointExportJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	require.Len(t, envelope.Sessions, 1)
	require.Equal(t, "session-v1-only", envelope.Sessions[0].SessionID,
		"v1 envelope must populate session_id from metadata-only reader (not stub entry)")
	require.Empty(t, envelope.Sessions[0].Error, "well-formed v1 read must not surface a per-session error")
}

func TestRunExplainExport_JSONNeverEmbedsTranscript(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("bbbb11112222")
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:         "session-no-leak",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"SECRET-RAW"}]}}` + "\n")),
		CompactTranscript: []byte(`{"v":1,"text":"SECRET-COMPACT"}` + "\n"),
	})

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "bbbb1111",
		json:         true,
		sessionIndex: -1,
	})
	require.NoError(t, err)

	out := stdout.String()
	require.NotContains(t, out, "SECRET-RAW", "JSON envelope must not embed raw transcript")
	require.NotContains(t, out, "SECRET-COMPACT", "JSON envelope must not embed compact transcript")
}

func TestRunExplainExport_TranscriptStreamsCompactBytes(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("cccc11112222")
	compact := []byte(`{"v":1,"type":"user","content":[{"text":"compact line 1"}]}` + "\n" + `{"v":1,"type":"assistant","content":[{"text":"compact line 2"}]}` + "\n")
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:         "session-compact",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"raw line"}]}}` + "\n")),
		CompactTranscript: compact,
	})

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "cccc1111",
		transcript:   true,
		sessionIndex: -1,
	})
	require.NoError(t, err)
	require.Equal(t, compact, stdout.Bytes())
}

func TestRunExplainExport_RawTranscriptStreamsRawBytes(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("dddd11112222")
	raw := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello raw"}]}}` + "\n")
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:         "session-raw",
		Transcript:        redact.AlreadyRedacted(raw),
		CompactTranscript: []byte(`{"v":1,"type":"user"}` + "\n"),
	})

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:        "dddd1111",
		rawTranscript: true,
		sessionIndex:  -1,
	})
	require.NoError(t, err)
	require.Equal(t, raw, stdout.Bytes())
}

// TestExplainCmd_RawTranscriptWithSessionIndexRoutesToExportPath guards the
// cobra-layer dispatch: --raw-transcript --session-index must reach the
// export path (which honors the index). Before the fix, the legacy
// raw-transcript path silently ignored --session-index because the dispatch
// only forked on --json or --transcript.
func TestExplainCmd_RawTranscriptWithSessionIndexRoutesToExportPath(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("ffff11112222")
	raw0 := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello session 0"}]}}` + "\n")
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-zero",
		Transcript: redact.AlreadyRedacted(raw0),
	})

	cmd := newExplainCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"ffff1111", "--raw-transcript", "--session-index", "0"})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	require.Equal(t, raw0, stdout.Bytes())
}

// TestExplainCmd_RawTranscriptMultiSessionDistinctContent guards the H2
// finding from the code review: the previous one-session test could not
// catch a regression where --session-index was silently ignored. This
// fixture has two sessions with byte-distinct transcripts; we assert
// that index 0 and index 1 return different content matching the
// per-session transcript that was written.
func TestExplainCmd_RawTranscriptMultiSessionDistinctContent(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("9999bbbb1111")
	rawSession0 := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"SESSION-ZERO-MARKER"}]}}` + "\n")
	rawSession1 := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"SESSION-ONE-DIFFERENT-MARKER"}]}}` + "\n")

	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-zero",
		Transcript: redact.AlreadyRedacted(rawSession0),
	})
	// Second WriteCommitted with the same checkpoint ID appends session 1.
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-one",
		Transcript: redact.AlreadyRedacted(rawSession1),
	})

	runIdx := func(idx string) []byte {
		t.Helper()
		cmd := newExplainCmd()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"9999bbbb", "--raw-transcript", "--session-index", idx})
		require.NoError(t, cmd.ExecuteContext(context.Background()))
		return stdout.Bytes()
	}

	got0 := runIdx("0")
	got1 := runIdx("1")

	require.NotEqual(t, got0, got1, "session 0 and session 1 must yield different bytes")
	require.Contains(t, string(got0), "SESSION-ZERO-MARKER")
	require.Contains(t, string(got1), "SESSION-ONE-DIFFERENT-MARKER")
	require.NotContains(t, string(got0), "SESSION-ONE-DIFFERENT-MARKER",
		"session 0 output must not leak session 1 content")
}

func TestRunExplainExport_TranscriptRequiresTarget(t *testing.T) {
	setupExportRepo(t)

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		transcript:   true,
		sessionIndex: -1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--transcript requires")
}

func TestRunExplainExport_TranscriptOutOfRangeSessionIndex(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("eeee11112222")
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:         "session-only",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
		CompactTranscript: []byte(`{"v":1}` + "\n"),
	})

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "eeee1111",
		transcript:   true,
		sessionIndex: 5,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")
}

func TestResolveSessionIndex(t *testing.T) {
	t.Parallel()

	threeSessions := &checkpoint.CheckpointSummary{
		Sessions: make([]checkpoint.SessionFilePaths, 3),
	}

	tests := []struct {
		name      string
		summary   *checkpoint.CheckpointSummary
		requested int
		want      int
		wantErr   string
	}{
		{name: "default picks latest", summary: threeSessions, requested: -1, want: 2},
		{name: "explicit 0", summary: threeSessions, requested: 0, want: 0},
		{name: "explicit middle", summary: threeSessions, requested: 1, want: 1},
		{name: "explicit last", summary: threeSessions, requested: 2, want: 2},
		{name: "out of range", summary: threeSessions, requested: 3, wantErr: "out of range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveSessionIndex(tc.summary, tc.requested)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestResolveSessionIndex_EmptyVsMissing distinguishes the two error sentinels
// after the Claude D fix: nil summary means "checkpoint not found", empty
// Sessions means "checkpoint exists but has no sessions".
func TestResolveSessionIndex_EmptyVsMissing(t *testing.T) {
	t.Parallel()

	_, errNil := resolveSessionIndex(nil, -1)
	require.ErrorIs(t, errNil, checkpoint.ErrCheckpointNotFound)

	_, errEmpty := resolveSessionIndex(&checkpoint.CheckpointSummary{}, -1)
	require.ErrorIs(t, errEmpty, errCheckpointHasNoSessions)
	require.NotErrorIs(t, errEmpty, checkpoint.ErrCheckpointNotFound,
		"empty-checkpoint case must not look like 'checkpoint not found'")
}

// TestRunExplainExport_RawTranscriptRequiresTarget guards the error message
// contract: when --raw-transcript reaches runExplainExport without a target,
// the error must reference --raw-transcript (not --transcript).
func TestRunExplainExport_RawTranscriptRequiresTarget(t *testing.T) {
	setupExportRepo(t)

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		rawTranscript: true,
		sessionIndex:  -1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--raw-transcript requires")
}

// TestRunExplainExport_PositionalCommitSHAFallback covers the codex finding:
// a positional that doesn't match a checkpoint prefix should be re-resolved
// as a commit ref (with Entire-Checkpoint trailer) before failing.
func TestRunExplainExport_PositionalCommitSHAFallback(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("aaaabbbb1234")
	writeV2CheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:         "session-via-commit",
		Transcript:        redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
		CompactTranscript: []byte(`{"v":1}` + "\n"),
	})

	cwd, err := os.Getwd()
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "trailing.txt"), []byte("trailing"), 0o600))
	_, err = wt.Add("trailing.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit("trailing\n\nEntire-Checkpoint: "+cpID.String()+"\n", &git.CommitOptions{
		Author: &object.Signature{Name: exportTestAuthorName, Email: exportTestAuthorEmail, When: time.Now()},
	})
	require.NoError(t, err)

	var stdout, stderr bytes.Buffer
	err = runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       commitHash.String(),
		json:         true,
		sessionIndex: -1,
	})
	require.NoError(t, err, "positional commit SHA should fall back to commit-ref resolution")

	var envelope checkpointExportJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	require.Equal(t, cpID.String(), envelope.CheckpointID)
}

func TestExplainCmd_SessionIndexRequiresTranscriptFlag(t *testing.T) {
	setupExportRepo(t)

	cmd := newExplainCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"some-checkpoint", "--session-index", "1"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	require.Contains(t,
		err.Error(), "--session-index only applies",
		"expected --session-index validation error, got: %v", err,
	)
}

// TestRunExplainExport_NoModeFlagFailsLoudly guards the bugbot finding that
// `opts.json` was never read: previously, calling runExplainExport with all
// three mode flags false would silently dispatch to JSON output. The
// explicit default branch now returns an internal error so future
// regressions don't silently produce JSON for unmoded callers.
func TestRunExplainExport_NoModeFlagFailsLoudly(t *testing.T) {
	setupExportRepo(t)

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "any",
		sessionIndex: -1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "without an output mode")
	require.Empty(t, stdout.String(), "must not emit JSON when no mode is set")
}

// stubCommittedReader is a minimal CommittedReader that returns canned
// metadata or errors per session index. Used to exercise the partial-failure
// path in buildCheckpointJSONEnvelope without corrupting a real git tree.
type stubCommittedReader struct {
	summary  *checkpoint.CheckpointSummary
	contents map[int]*checkpoint.SessionContent // idx -> content (nil ⇒ return error)
	err      error                              // err returned for indexes not in contents
}

func (s *stubCommittedReader) ReadCommitted(_ context.Context, _ id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	return s.summary, nil
}

func (s *stubCommittedReader) ReadSessionContent(_ context.Context, _ id.CheckpointID, idx int) (*checkpoint.SessionContent, error) {
	if c, ok := s.contents[idx]; ok && c != nil {
		return c, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, errors.New("stub: session not configured")
}

// TestBuildCheckpointJSONEnvelope_PartialFailureFromMockReader exercises the
// H3 partial-failure path end-to-end against the envelope builder. A real
// v2-tree corruption test isn't feasible from the cli package (the splice
// helper is unexported); the mock reader hits the same default branch in
// readSessionMetadataForExport that a v3-or-future store would hit, which
// IS the public surface this contract guarantees.
func TestBuildCheckpointJSONEnvelope_PartialFailureFromMockReader(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("eeee99998888")
	summary := &checkpoint.CheckpointSummary{
		Strategy:         "manual-commit",
		CheckpointsCount: 2,
		Sessions: []checkpoint.SessionFilePaths{
			{Metadata: "ee/ee99998888/0/metadata.json"},
			{Metadata: "ee/ee99998888/1/metadata.json"},
		},
	}
	reader := &stubCommittedReader{
		summary: summary,
		contents: map[int]*checkpoint.SessionContent{
			0: {Metadata: checkpoint.CommittedMetadata{
				SessionID: "good-session",
				Agent:     "Claude Code",
			}},
			// idx 1 not configured ⇒ stub returns error, simulating an
			// unreadable session metadata blob.
		},
	}

	envelope, failed := buildCheckpointJSONEnvelope(context.Background(), reader, summary, cpID)

	require.True(t, envelope.Partial, "envelope.Partial must be true when any session metadata read fails")
	require.Equal(t, []int{1}, failed, "failed-sessions slice must list the broken indexes")
	require.Len(t, envelope.Sessions, 2)

	require.Equal(t, "good-session", envelope.Sessions[0].SessionID)
	require.Empty(t, envelope.Sessions[0].Error)

	require.Equal(t, 1, envelope.Sessions[1].Index)
	require.Empty(t, envelope.Sessions[1].SessionID, "stub entry must not carry data that looks real")
	require.Empty(t, envelope.Sessions[1].Agent)
	require.NotEmpty(t, envelope.Sessions[1].Error, "stub entry must surface the underlying read error")
}

// TestCheckpointExportJSON_PartialContract pins the JSON shape that signals
// a partial-failure export to consumers (codex finding 3): the top-level
// `partial` flag plus per-session `error` fields. Consumers branch on
// either; the command also exits non-zero so automation can't trust an
// envelope where partial=true.
func TestCheckpointExportJSON_PartialContract(t *testing.T) {
	t.Parallel()

	envelope := checkpointExportJSON{
		CheckpointID: "abcdef123456",
		SessionCount: 2,
		Sessions: []checkpointSessionJSON{
			{Index: 0, SessionID: "good", Agent: "Claude Code"},
			{Index: 1, Error: "read v2 session metadata: blob 0xdead missing"},
		},
		Partial: true,
	}

	buf, err := json.Marshal(envelope)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf, &got))

	require.Equal(t, true, got["partial"], "envelope must surface partial=true at top level")
	sessions, ok := got["sessions"].([]any)
	require.True(t, ok)
	require.Len(t, sessions, 2)

	failed, ok := sessions[1].(map[string]any)
	require.True(t, ok)
	idx, ok := failed["index"].(float64)
	require.True(t, ok)
	require.InEpsilon(t, float64(1), idx, 0.0001)
	require.Equal(t, "read v2 session metadata: blob 0xdead missing", failed["error"])
	// The unreadable session must NOT carry stub fields that look like real data.
	require.NotContains(t, failed, "session_id")
	require.NotContains(t, failed, "agent")
}

// TestCheckpointMatchesSessionFilter guards the codex high finding: when a
// caller asks for `entire checkpoint explain --json --session <prefix>`, the
// filter must match against ALL contributing sessions, not just the latest.
// Multi-session checkpoints expose archived contributors via SessionIDs.
func TestCheckpointMatchesSessionFilter(t *testing.T) {
	t.Parallel()

	multi := strategy.RewindPoint{
		SessionID:  "9f44f514-b012", // latest
		SessionIDs: []string{"older-session-aaaa", "9f44f514-b012"},
	}
	single := strategy.RewindPoint{
		SessionID: "lone-session-bbbb",
	}

	tests := []struct {
		name   string
		point  strategy.RewindPoint
		filter string
		want   bool
	}{
		{name: "matches latest session id", point: multi, filter: "9f44f514", want: true},
		{name: "matches archived session id", point: multi, filter: "older-session", want: true},
		{name: "no match", point: multi, filter: "deadbeef", want: false},
		{name: "single-session match", point: single, filter: "lone", want: true},
		{name: "single-session miss", point: single, filter: "older-session", want: false},
		{name: "empty filter not handled here", point: multi, filter: "", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkpointMatchesSessionFilter(tc.point, tc.filter)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestExplainCmd_TranscriptAndJSONMutuallyExclusive(t *testing.T) {
	setupExportRepo(t)

	cmd := newExplainCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"some-checkpoint", "--json", "--transcript"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
}
