package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestParseExplainRepoFlag(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, in, owner, repo, wantErr string
	}{
		{name: "owner/name", in: "acme/widgets", owner: "acme", repo: "widgets"},
		{name: "gh prefixed", in: "gh/acme/widgets", owner: "acme", repo: "widgets"},
		{name: "leading slash", in: "/gh/acme/widgets", owner: "acme", repo: "widgets"},
		{name: "lowercased", in: "ACME/Widgets", owner: "acme", repo: "widgets"},
		{name: "surrounding space", in: "  acme/widgets  ", owner: "acme", repo: "widgets"},
		{name: "empty", in: "", wantErr: "--repo requires a value"},
		{name: "bare word", in: "widgets", wantErr: "expected owner/name"},
		// A repo ID is rejected rather than guessed at: the control plane and
		// the search index expose different identifiers for a repository.
		{name: "bare ulid", in: "01KVBJCWYA4YW6J5M9GP655HZ9", wantErr: "expected owner/name"},
		{name: "clone url", in: "https://github.com/acme/widgets", wantErr: "invalid --repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := parseExplainRepoFlag(tc.in)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.owner, owner)
			assert.Equal(t, tc.repo, repo)
		})
	}
}

// TestExplainRepoIsCurrent checks same-repo detection against the origin URL.
// Not parallel: uses t.Chdir.
func TestExplainRepoIsCurrent(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	setOrigin := func(t *testing.T, url string) {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "remote", "remove", "origin").CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such remote") {
			t.Logf("remove origin: %s", out)
		}
		out, err = exec.CommandContext(t.Context(), "git", "-C", dir, "remote", "add", "origin", url).CombinedOutput()
		require.NoError(t, err, "add origin: %s", out)
	}

	ctx := context.Background()

	setOrigin(t, "git@github.com:acme/widgets.git")
	assert.True(t, explainRepoIsCurrent(ctx, "acme", "widgets"))
	assert.True(t, explainRepoIsCurrent(ctx, "ACME", "Widgets"), "comparison is case-insensitive")
	assert.False(t, explainRepoIsCurrent(ctx, "acme", "other"))

	setOrigin(t, "https://github.com/acme/widgets")
	assert.True(t, explainRepoIsCurrent(ctx, "acme", "widgets"))

	// entire:// mirror URLs carry the forge in the path.
	setOrigin(t, "entire://aws-us-east-2.entire.io/gh/acme/widgets")
	assert.True(t, explainRepoIsCurrent(ctx, "acme", "widgets"))

	// --repo is GitHub-scoped, so a non-GitHub origin with a coincidentally
	// matching owner/name must not count as the current repo.
	setOrigin(t, "git@gitlab.com:acme/widgets.git")
	assert.False(t, explainRepoIsCurrent(ctx, "acme", "widgets"))
}

// --- render paths -----------------------------------------------------

// stubCrossRepoReader serves a fixed checkpoint so the render paths can be
// exercised without a cell.
type stubCrossRepoReader struct {
	transcript []byte
	metaErr    error
	sessions   int
}

func (s *stubCrossRepoReader) Read(context.Context, id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	n := s.sessions
	if n == 0 {
		n = 1
	}
	return &checkpoint.CheckpointSummary{
		CheckpointID:     testAPICheckpointID,
		Strategy:         "manual-commit",
		CheckpointsCount: 4,
		FilesTouched:     []string{"README.md"},
		Sessions:         make([]checkpoint.SessionFilePaths, n),
		TokenUsage:       &types.TokenUsage{InputTokens: 10, OutputTokens: 20},
	}, nil
}

func (s *stubCrossRepoReader) List(context.Context) ([]checkpoint.CheckpointInfo, error) {
	return nil, errors.New("not supported")
}

func (s *stubCrossRepoReader) ReadSessionMetadata(_ context.Context, cid id.CheckpointID, _ int) (*checkpoint.Metadata, error) {
	if s.metaErr != nil {
		return nil, s.metaErr
	}
	return &checkpoint.Metadata{
		CheckpointID: cid,
		SessionID:    "stub-session",
		Strategy:     "manual-commit",
		CreatedAt:    time.Date(2026, 7, 14, 17, 30, 22, 0, time.UTC),
		Agent:        types.AgentType("Claude Code"),
	}, nil
}

func (s *stubCrossRepoReader) ReadSessionPrompts(context.Context, id.CheckpointID, int) (string, error) {
	return "do the foreign thing", nil
}

func (s *stubCrossRepoReader) ReadSessionMetadataAndPrompts(ctx context.Context, cid id.CheckpointID, idx int) (*checkpoint.Metadata, string, error) {
	meta, err := s.ReadSessionMetadata(ctx, cid, idx)
	if err != nil {
		return nil, "", err
	}
	return meta, "do the foreign thing", nil
}

func (s *stubCrossRepoReader) ReadTaskRecords(context.Context, id.CheckpointID) ([]checkpoint.StoredTaskRecord, error) {
	return nil, nil
}

func (s *stubCrossRepoReader) ReadSessionContent(ctx context.Context, cid id.CheckpointID, idx int) (*checkpoint.SessionContent, error) {
	meta, prompts, err := s.ReadSessionMetadataAndPrompts(ctx, cid, idx)
	if err != nil {
		return nil, err
	}
	return &checkpoint.SessionContent{Metadata: *meta, Transcript: s.transcript, Prompts: prompts}, nil
}

func (s *stubCrossRepoReader) GetCheckpointAuthor(context.Context, id.CheckpointID) (checkpoint.Author, error) {
	return checkpoint.Author{Name: "Foreign Author"}, nil
}

func (s *stubCrossRepoReader) checkpointCommit(context.Context, id.CheckpointID) ([]associatedCommit, error) {
	return []associatedCommit{{SHA: "13e379e4b", ShortSHA: "13e379e", Message: "foreign commit"}}, nil
}

// withStubCrossRepoReader points the cross-repo path at stub and records the
// coordinates it was asked to read. It swaps a package-level var, so its
// callers must not use t.Parallel() — concurrent tests would clobber each
// other's stub and read another test's checkpoint.
func withStubCrossRepoReader(t *testing.T, stub crossRepoReader) *string {
	t.Helper()
	var asked string
	prev := newCrossRepoReader
	newCrossRepoReader = func(_ context.Context, _ bool, owner, repo string) (crossRepoReader, error) {
		asked = owner + "/" + repo
		return stub, nil
	}
	t.Cleanup(func() { newCrossRepoReader = prev })
	return &asked
}

func TestRunCrossRepoExplain_Prose(t *testing.T) {
	stub := &stubCrossRepoReader{transcript: []byte(`{"type":"user","message":{"role":"user","content":"do the foreign thing"}}` + "\n")}
	asked := withStubCrossRepoReader(t, stub)

	var out, errOut bytes.Buffer
	require.NoError(t, runCrossRepoExplain(context.Background(), &out, &errOut, crossRepoExplainOptions{
		repoFlag:     "acme/widgets",
		target:       testAPICheckpointID.String(),
		sessionIndex: -1,
		verbose:      true,
		noPager:      true,
	}))

	assert.Equal(t, "acme/widgets", *asked)
	got := out.String()
	assert.Contains(t, got, testAPICheckpointID.String())
	assert.Contains(t, got, "Foreign Author", "the cell's author must render without a local commit")
	assert.Contains(t, got, "13e379e", "the checkpoint's own commit must render, not '(none on this branch)'")
	assert.NotContains(t, got, "none on this branch")
	assert.Contains(t, got, "do the foreign thing")
}

func TestRunCrossRepoExplain_JSON(t *testing.T) {
	asked := withStubCrossRepoReader(t, &stubCrossRepoReader{sessions: 2})

	var out, errOut bytes.Buffer
	require.NoError(t, runCrossRepoExplain(context.Background(), &out, &errOut, crossRepoExplainOptions{
		repoFlag:     "acme/widgets",
		target:       testAPICheckpointID.String(),
		sessionIndex: -1,
		json:         true,
	}))
	assert.Equal(t, "acme/widgets", *asked)

	var envelope struct {
		CheckpointID string `json:"checkpoint_id"`
		SessionCount int    `json:"session_count"`
		Partial      bool   `json:"partial"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &envelope), "stdout must be clean JSON")
	assert.Equal(t, testAPICheckpointID.String(), envelope.CheckpointID)
	assert.Equal(t, 2, envelope.SessionCount)
	assert.False(t, envelope.Partial, "a complete read must not be flagged partial")
}

// A session whose metadata can't be read must produce the same partial-export
// contract as the local path: envelope on stdout, diagnostic on stderr, non-nil
// error so automation can't mistake it for a clean export.
func TestRunCrossRepoExplain_JSONPartialFailsHard(t *testing.T) {
	withStubCrossRepoReader(t, &stubCrossRepoReader{metaErr: errors.New("boom")})

	var out, errOut bytes.Buffer
	err := runCrossRepoExplain(context.Background(), &out, &errOut, crossRepoExplainOptions{
		repoFlag:     "acme/widgets",
		target:       testAPICheckpointID.String(),
		sessionIndex: -1,
		json:         true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "export incomplete")
	assert.Contains(t, errOut.String(), "failed to read metadata")

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &envelope), "the envelope is still written to stdout")
	assert.Equal(t, true, envelope["partial"])
}

func TestRunCrossRepoExplain_TranscriptWritesBytesVerbatim(t *testing.T) {
	raw := []byte(`{"type":"user"}` + "\n" + `{"type":"assistant"}` + "\n")
	withStubCrossRepoReader(t, &stubCrossRepoReader{transcript: raw})

	var out, errOut bytes.Buffer
	require.NoError(t, runCrossRepoExplain(context.Background(), &out, &errOut, crossRepoExplainOptions{
		repoFlag:      "acme/widgets",
		target:        testAPICheckpointID.String(),
		sessionIndex:  -1,
		rawTranscript: true,
	}))
	assert.Equal(t, string(raw), out.String(), "transcript bytes must pass through unmodified")
}

func TestRunCrossRepoExplain_EmptyTranscriptIsAnError(t *testing.T) {
	withStubCrossRepoReader(t, &stubCrossRepoReader{})

	var out, errOut bytes.Buffer
	err := runCrossRepoExplain(context.Background(), &out, &errOut, crossRepoExplainOptions{
		repoFlag:     "acme/widgets",
		target:       testAPICheckpointID.String(),
		sessionIndex: -1,
		transcript:   true,
	})
	require.ErrorContains(t, err, "has no transcript")
	// Copilot (PR #1942): a failure must not be preceded by a success marker.
	assert.NotContains(t, errOut.String(), "✓", "an empty transcript must not report success first")
}

func TestRunCrossRepoExplain_RejectsPrefix(t *testing.T) {
	withStubCrossRepoReader(t, &stubCrossRepoReader{})

	err := runCrossRepoExplain(context.Background(), io.Discard, io.Discard, crossRepoExplainOptions{
		repoFlag:     "acme/widgets",
		target:       "01KXGT",
		sessionIndex: -1,
	})
	require.ErrorContains(t, err, "requires a full checkpoint ID")
}

func TestCrossRepoExplainSessionIndex(t *testing.T) {
	t.Parallel()

	assert.Equal(t, -1, crossRepoExplainSessionIndex(false, 0), "unset means latest session")
	assert.Equal(t, 0, crossRepoExplainSessionIndex(true, 0))
	assert.Equal(t, 3, crossRepoExplainSessionIndex(true, 3))
}

// --- flag layer -------------------------------------------------------

func TestExplainCmd_RepoFlagValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "repo without a checkpoint",
			args:    []string{"checkpoint", "explain", "--repo", "acme/widgets"},
			wantErr: "--repo requires a checkpoint ID",
		},
		{
			name:    "repo with commit",
			args:    []string{"checkpoint", "explain", "--repo", "acme/widgets", "--commit", "HEAD"},
			wantErr: "[repo commit]",
		},
		{
			name:    "repo with generate",
			args:    []string{"checkpoint", "explain", "abc", "--repo", "acme/widgets", "--generate"},
			wantErr: "[repo generate]",
		},
		{
			name:    "repo with session filter",
			args:    []string{"checkpoint", "explain", "abc", "--repo", "acme/widgets", "--session", "s1"},
			wantErr: "[repo session]",
		},
		{
			name:    "repo with search-all",
			args:    []string{"checkpoint", "explain", "abc", "--repo", "acme/widgets", "--search-all"},
			wantErr: "[repo search-all]",
		},
		{
			name:    "insecure without repo",
			args:    []string{"checkpoint", "explain", "abc", "--insecure-http-auth"},
			wantErr: "--insecure-http-auth only applies with --repo",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := NewRootCmd()
			root.SetArgs(tc.args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			err := root.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Bugbot (PR #1942): the default no-summary hint tells the reader to run
// `explain --generate`, which is rejected with --repo and impossible against a
// read-only reader. A foreign checkpoint must say why instead of naming a
// command that cannot succeed.
func TestRunCrossRepoExplain_NoDeadGenerateHint(t *testing.T) {
	withStubCrossRepoReader(t, &stubCrossRepoReader{
		transcript: []byte(`{"type":"user","message":{"role":"user","content":"do the foreign thing"}}` + "\n"),
	})

	var out, errOut bytes.Buffer
	require.NoError(t, runCrossRepoExplain(context.Background(), &out, &errOut, crossRepoExplainOptions{
		repoFlag:     "acme/widgets",
		target:       testAPICheckpointID.String(),
		sessionIndex: -1,
		verbose:      true,
		noPager:      true,
	}))

	got := out.String()
	assert.NotContains(t, got, "--generate", "must not advertise a flag that --repo rejects")
	assert.Contains(t, got, "repo that owns it (acme/widgets)")
}

// The local path keeps its hint: the fix must be scoped to cross-repo reads.
func TestFormatCheckpointOutput_LocalKeepsGenerateHint(t *testing.T) {
	t.Parallel()

	summary := &checkpoint.CheckpointSummary{CheckpointID: testAPICheckpointID}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.Metadata{CheckpointID: testAPICheckpointID, SessionID: "s"},
		Prompts:  "do the local thing",
	}
	got := formatCheckpointOutput(context.Background(), summary, content, testAPICheckpointID, nil, checkpoint.Author{}, true, false, &bytes.Buffer{})
	assert.Contains(t, got, "--generate", "the local hint is still actionable")
}

func TestCrossRepoReadSource(t *testing.T) {
	t.Parallel()

	_, ok := crossRepoReadSource(context.Background())
	assert.False(t, ok, "an unmarked context is a local read")

	src, ok := crossRepoReadSource(withCrossRepoRead(context.Background(), "acme/widgets"))
	assert.True(t, ok)
	assert.Equal(t, "acme/widgets", src)

	_, ok = crossRepoReadSource(withCrossRepoRead(context.Background(), ""))
	assert.False(t, ok, "an empty repo name must not count as a cross-repo read")
}
