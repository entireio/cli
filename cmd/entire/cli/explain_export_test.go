package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
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
