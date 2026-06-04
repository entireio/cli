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
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

// setupExportRepo creates a git repo with an initial commit (required for
// HEAD-resolving operations). The caller is responsible for chdir; this helper
// does NOT call t.Parallel because tests using t.Chdir cannot parallelize.
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
		[]byte(`{"enabled": true}`),
		0o600,
	))

	return repo
}

func writeCheckpointForExport(t *testing.T, repo *git.Repository, cpID id.CheckpointID, opts checkpoint.WriteCommittedOptions) {
	t.Helper()
	if opts.CheckpointID.IsEmpty() {
		opts.CheckpointID = cpID
	}
	if opts.Strategy == "" {
		opts.Strategy = strategy.StrategyNameManualCommit
	}
	if opts.AuthorName == "" {
		opts.AuthorName = exportTestAuthorName
	}
	if opts.AuthorEmail == "" {
		opts.AuthorEmail = exportTestAuthorEmail
	}
	store := checkpoint.NewGitStore(repo)
	require.NoError(t, store.WriteCommitted(context.Background(), opts))
}

func TestRunExplainExport_JSONSingleCheckpoint(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("aaaa11112222")
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-json",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
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

func TestRunExplainExport_JSONFetchesRemoteV1Metadata(t *testing.T) {
	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "origin.git")
	producerDir := filepath.Join(tmpDir, "producer")
	localDir := filepath.Join(tmpDir, "local")

	runGit(t, tmpDir, "init", "--bare", bareDir)

	testutil.InitRepo(t, producerDir)
	testutil.WriteFile(t, producerDir, "README.md", "init")
	testutil.GitAdd(t, producerDir, "README.md")
	testutil.GitCommit(t, producerDir, "init")
	runGit(t, producerDir, "remote", "add", "origin", bareDir)

	producerRepo, err := git.PlainOpen(producerDir)
	require.NoError(t, err)
	runGit(t, producerDir, "push", "origin", "HEAD:refs/heads/main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGit(t, tmpDir, "clone", "--branch", "main", bareDir, localDir)

	targetID := id.MustCheckpointID("aaaa99998888")
	writeCheckpointForExport(t, producerRepo, targetID, checkpoint.WriteCommittedOptions{
		SessionID:  "remote-v1-session",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"remote"}]}}` + "\n")),
	})
	runGit(t, producerDir, "push", "origin", paths.MetadataBranchName+":"+paths.MetadataBranchName)

	require.NoError(t, os.MkdirAll(filepath.Join(localDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true}`),
		0o600,
	))
	t.Chdir(localDir)

	var stdout, stderr bytes.Buffer
	err = runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "aaaa9999",
		json:         true,
		sessionIndex: -1,
	})
	require.NoError(t, err, "stderr: %s", stderr.String())

	var envelope checkpointExportJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope), "output: %s", stdout.String())
	require.Equal(t, targetID.String(), envelope.CheckpointID)
	require.Len(t, envelope.Sessions, 1)
	require.Equal(t, "remote-v1-session", envelope.Sessions[0].SessionID)
}

// TestRunExplainExport_JSONUsesMetadataOnlyReader verifies the codex finding 3:
// --json must read metadata.json directly, not via ReadSessionContent (which
// depends on transcript availability). We exercise this by writing a v1
// checkpoint, then asserting the envelope has populated per-session fields
// (not a stub entry).
func TestRunExplainExport_JSONUsesMetadataOnlyReader(t *testing.T) {
	repo := setupExportRepo(t)

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
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-no-leak",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"SECRET-RAW"}]}}` + "\n")),
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
}

func TestRunExplainExport_TranscriptStreamsStoredBytes(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("cccc11112222")
	raw := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"stored line"}]}}` + "\n")
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-stored",
		Transcript: redact.AlreadyRedacted(raw),
	})

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       "cccc1111",
		transcript:   true,
		sessionIndex: -1,
	})
	require.NoError(t, err)
	require.Equal(t, raw, stdout.Bytes())
}

func TestRunExplainExport_RawTranscriptStreamsRawBytes(t *testing.T) {
	repo := setupExportRepo(t)

	cpID := id.MustCheckpointID("dddd11112222")
	raw := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello raw"}]}}` + "\n")
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-raw",
		Transcript: redact.AlreadyRedacted(raw),
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
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
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

	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-zero",
		Transcript: redact.AlreadyRedacted(rawSession0),
	})
	// Second fixture write with the same checkpoint ID appends session 1.
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
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
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-only",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
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
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteCommittedOptions{
		SessionID:  "session-via-commit",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
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
// H3 partial-failure path end-to-end against the envelope builder. The mock
// reader hits the same default branch in readSessionMetadataForExport that a
// future store without metadata-only reads would hit, which is the public
// surface this contract guarantees.
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
			{Index: 1, Error: "read session metadata: blob 0xdead missing"},
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
	require.Equal(t, "read session metadata: blob 0xdead missing", failed["error"])
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

// TestExplainExport_HasInvestigation pins the JSON wire format for the
// has_investigation umbrella flag in the export envelope. omitempty: true
// must marshal as "has_investigation":true; a freshly-zeroed envelope must
// drop the field entirely (so older checkpoints don't look investigated).
func TestExplainExport_HasInvestigation(t *testing.T) {
	t.Parallel()

	bTrue, err := json.Marshal(checkpointExportJSON{
		CheckpointID:     "abcdef011111",
		HasInvestigation: true,
	})
	require.NoError(t, err)

	var rawTrue map[string]any
	require.NoError(t, json.Unmarshal(bTrue, &rawTrue))
	got, ok := rawTrue["has_investigation"].(bool)
	require.True(t, ok, "expected has_investigation key, raw: %s", string(bTrue))
	require.True(t, got, "expected has_investigation:true, raw: %s", string(bTrue))

	bZero, err := json.Marshal(checkpointExportJSON{CheckpointID: "abcdef011111"})
	require.NoError(t, err)
	require.NotContains(t, string(bZero), "has_investigation",
		"zero-value envelope must omit has_investigation key")
}

// TestExplainExport_PerSessionInvestigateFields pins the JSON wire format
// for the per-session investigate fields. The fields are populated when
// the session metadata carries them, and omitted when they are zero-valued.
func TestExplainExport_PerSessionInvestigateFields(t *testing.T) {
	t.Parallel()

	bPopulated, err := json.Marshal(checkpointSessionJSON{
		Index:            0,
		SessionID:        "investigate-session",
		InvestigateRunID: "0123456789ab",
		InvestigateTopic: "the perf regression in foo()",
	})
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(bPopulated, &raw))
	require.Equal(t, "0123456789ab", raw["investigate_run_id"])
	require.Equal(t, "the perf regression in foo()", raw["investigate_topic"])

	bZero, err := json.Marshal(checkpointSessionJSON{Index: 0, SessionID: "no-investigation"})
	require.NoError(t, err)
	for _, k := range []string{"investigate_run_id", "investigate_topic"} {
		require.NotContains(t, string(bZero), k,
			"zero-value session must omit %q", k)
	}
}

// TestSessionMetadataToJSON_CopiesInvestigateFields pins that
// sessionMetadataToJSON copies the investigate fields from CommittedMetadata
// into the per-session JSON struct.
func TestSessionMetadataToJSON_CopiesInvestigateFields(t *testing.T) {
	t.Parallel()

	meta := &checkpoint.CommittedMetadata{
		SessionID:        "investigate-session",
		Kind:             "agent_investigate",
		InvestigateRunID: "0123456789ab",
		InvestigateTopic: "topic from metadata.json",
	}

	got := sessionMetadataToJSON(0, meta)
	require.Equal(t, "0123456789ab", got.InvestigateRunID)
	require.Equal(t, "topic from metadata.json", got.InvestigateTopic)
}

// TestBuildCheckpointJSONEnvelope_PropagatesHasInvestigation verifies the
// summary-level has_investigation flag propagates from CheckpointSummary to
// the export envelope. Mirrors how HasReview is sourced.
func TestBuildCheckpointJSONEnvelope_PropagatesHasInvestigation(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("aaaa11112222")
	summary := &checkpoint.CheckpointSummary{
		Strategy:         "manual-commit",
		CheckpointsCount: 1,
		HasInvestigation: true,
		Sessions: []checkpoint.SessionFilePaths{
			{Metadata: "aa/aa11112222/0/metadata.json"},
		},
	}
	reader := &stubCommittedReader{
		summary: summary,
		contents: map[int]*checkpoint.SessionContent{
			0: {Metadata: checkpoint.CommittedMetadata{
				SessionID:        "investigate-session",
				Kind:             "agent_investigate",
				InvestigateRunID: "0123456789ab",
				InvestigateTopic: "summary-level topic",
			}},
		},
	}

	envelope, failed := buildCheckpointJSONEnvelope(context.Background(), reader, summary, cpID)
	require.Empty(t, failed)
	require.True(t, envelope.HasInvestigation,
		"envelope must mirror CheckpointSummary.HasInvestigation")
	require.Len(t, envelope.Sessions, 1)
	require.Equal(t, "0123456789ab", envelope.Sessions[0].InvestigateRunID)
	require.Equal(t, "summary-level topic", envelope.Sessions[0].InvestigateTopic)
}
