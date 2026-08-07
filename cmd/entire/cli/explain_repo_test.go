package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/redact"
)

func TestParseExplainRepoFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    explainRepoRef
		wantErr string
	}{
		{"owner/name lowercased", "EntireIO/CLI", explainRepoRef{owner: "entireio", repo: "cli"}, ""},
		{"gh prefix stripped", "gh/Owner/Repo", explainRepoRef{owner: "owner", repo: "repo"}, ""},
		{"leading slash gh prefix", "/gh/owner/repo", explainRepoRef{owner: "owner", repo: "repo"}, ""},
		{"dotted repo name", "entireio/entire.io", explainRepoRef{owner: "entireio", repo: "entire.io"}, ""},
		{"bare ULID", "01KVBJCWYA4YW6J5M9GP655HZ9", explainRepoRef{repoID: "01KVBJCWYA4YW6J5M9GP655HZ9"}, ""},
		{"empty", "", explainRepoRef{}, "--repo requires a value"},
		{"garbage without slash", "not-a-ulid", explainRepoRef{}, "expected owner/name, gh/owner/name, or a repo ID"},
		{"extra path segment", "owner/repo/extra", explainRepoRef{}, "expected owner/name"},
		{"missing repo segment", "owner/", explainRepoRef{}, "expected owner/name"},
		{"metacharacters rejected", "own$er/repo", explainRepoRef{}, "expected owner/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseExplainRepoFlag(tt.value)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestResolveExplainRepoID resolves a raw repo ULID to owner/name through the
// control plane's by-id repo lookup (the `repo get` endpoint). Not parallel:
// swaps the process-global activeCoreClient seam.
func TestResolveExplainRepoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/01KVBJCWYA4YW6J5M9GP655HZ9" {
			writeNotFoundProblem(t, w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, printJSON(w, &coreapi.Repo{
			ID:       "01KVBJCWYA4YW6J5M9GP655HZ9",
			Name:     "entire.io",
			FullName: coreapi.NewOptString("EntireIO/Entire.io"),
		}))
	}))
	t.Cleanup(srv.Close)
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	owner, repo, err := resolveExplainRepoID(cmd, "01KVBJCWYA4YW6J5M9GP655HZ9")
	require.NoError(t, err)
	assert.Equal(t, "entireio", owner, "full name must come back lowercased")
	assert.Equal(t, "entire.io", repo)

	_, _, err = resolveExplainRepoID(cmd, "01KVBJCWYA4YW6J5M9GP655HZZ")
	require.ErrorContains(t, err, "no repository with ID")
}

// TestExplainRepoIsCurrent checks same-repo detection against the origin URL
// forms a clone can carry. Not parallel: uses t.Chdir.
func TestExplainRepoIsCurrent(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	ctx := context.Background()
	assert.False(t, explainRepoIsCurrent(ctx, "owner", "repo"), "no origin remote must not match")

	gitRun(t, dir, "remote", "add", "origin", "git@github.com:EntireIO/CLI.git")
	assert.True(t, explainRepoIsCurrent(ctx, "entireio", "cli"), "scp-style ssh origin")
	assert.False(t, explainRepoIsCurrent(ctx, "entireio", "other"))
	assert.False(t, explainRepoIsCurrent(ctx, "other", "cli"))

	gitRun(t, dir, "remote", "set-url", "origin", "https://github.com/EntireIO/CLI.git")
	assert.True(t, explainRepoIsCurrent(ctx, "entireio", "cli"), "https origin")

	gitRun(t, dir, "remote", "set-url", "origin", "entire://aws-us-east-2.entire.io/gh/EntireIO/CLI")
	assert.True(t, explainRepoIsCurrent(ctx, "entireio", "cli"), "entire:// mirror origin")
}

// TestExplainCmd_RepoFlagValidation covers the flag-combination rules for
// --repo/--cluster. These all fail before any git or network access.
func TestExplainCmd_RepoFlagValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"--cluster without --repo", []string{"01KVBJCWYA4YW6J5M9GP655HZ9", "--cluster", "c.example.com"}, "--cluster requires --repo"},
		{"--repo without a target", []string{"--repo", "owner/name"}, "--repo requires a checkpoint ID"},
		{"--repo with --commit", []string{"--commit", "HEAD", "--repo", "owner/name"}, "repo"},
		{"--repo with --session", []string{"--session", "sess-1", "--repo", "owner/name"}, "repo"},
		{"--repo with --generate", []string{"01KVBJCWYA4YW6J5M9GP655HZ9", "--generate", "--repo", "owner/name"}, "repo"},
		{"--repo with invalid value", []string{"01KVBJCWYA4YW6J5M9GP655HZ9", "--repo", "not-a-ulid"}, "expected owner/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newExplainCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestExplainCmd_RepoFlagRequiresFullID: a cross-repo target must be a full
// checkpoint ID — a prefix cannot name a per-checkpoint ref. Not parallel:
// uses t.Chdir (same-repo detection reads the cwd origin).
func TestExplainCmd_RepoFlagRequiresFullID(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	cmd := newExplainCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"abc123", "--repo", "owner/name"})
	err := cmd.Execute()
	require.ErrorContains(t, err, "full checkpoint ID")
}

// crossRepoFixture builds a "foreign" repo holding a real git-refs checkpoint
// and a separate local work repo, and chdirs into the local repo. Returns the
// foreign repo path (usable directly as a fetch URL) and the checkpoint ID.
func crossRepoFixture(t *testing.T) (foreignDir string, cid id.CheckpointID) {
	t.Helper()
	const settingsBody = `{"enabled":true,"checkpoints":{"primary":{"type":"git-refs"}}}`

	foreignDir = t.TempDir()
	testutil.InitRepo(t, foreignDir)
	testutil.WriteFile(t, foreignDir, "f.txt", "foreign")
	testutil.GitAdd(t, foreignDir, "f.txt")
	testutil.GitCommit(t, foreignDir, "foreign init")
	testutil.WriteFile(t, foreignDir, ".entire/settings.json", settingsBody)

	// checkpoint.Open resolves settings relative to cwd, so write the foreign
	// checkpoint while chdir'd into the foreign repo.
	t.Chdir(foreignDir)
	foreignRepo, err := gitrepo.OpenPath(foreignDir)
	require.NoError(t, err)
	stores, err := checkpoint.Open(context.Background(), foreignRepo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	cid = id.CheckpointID("01KVBJCWYA4YW6J5M9GP655HZ9")
	require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
		CheckpointID: cid,
		SessionID:    "foreign-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"role":"user","content":"do the foreign thing"}}`)),
		Prompts:      []string{"do the foreign thing"},
		FilesTouched: []string{"a.go"},
		AuthorName:   "Foreign Author",
		AuthorEmail:  "foreign@example.com",
	}))

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "local.txt", "local")
	testutil.GitAdd(t, localDir, "local.txt")
	testutil.GitCommit(t, localDir, "local init")
	t.Chdir(localDir)
	return foreignDir, cid
}

// TestCrossRepoExplain_FetchAndRender is the end-to-end shape of
// `checkpoint explain <id> --repo other/repo` with URL resolution bypassed:
// fetch the foreign checkpoint's ref from an explicit path URL into the local
// repo, then let the unchanged explain flow resolve and render it. Not
// parallel: uses t.Chdir.
func TestCrossRepoExplain_FetchAndRender(t *testing.T) {
	foreignDir, cid := crossRepoFixture(t)

	require.NoError(t, fetchCrossRepoCheckpoint(context.Background(), io.Discard, foreignDir, "acme/widgets", cid))

	var out bytes.Buffer
	err := runExplain(context.Background(), &out, io.Discard, "", "", "", cid.String(), true, true, false, false, false, false, false, 0)
	require.NoError(t, err)
	assert.Contains(t, out.String(), cid.String(), "explain must render the fetched foreign checkpoint")
	assert.Contains(t, out.String(), "do the foreign thing", "foreign checkpoint's prompt must render")
}

// TestFetchCrossRepoCheckpoint_MissingRefNamesRepo: a mirror that does not
// hold the checkpoint ref (legacy branch-based store, or wrong repo) must
// produce the specific per-repo error, not the generic "no checkpoint or
// commit found". Not parallel: uses t.Chdir.
func TestFetchCrossRepoCheckpoint_MissingRefNamesRepo(t *testing.T) {
	foreignDir, _ := crossRepoFixture(t)
	missing := id.CheckpointID("01KVBJCWYA4YW6J5M9GP655HZN")

	err := fetchCrossRepoCheckpoint(context.Background(), io.Discard, foreignDir, "acme/widgets", missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing.String())
	assert.Contains(t, err.Error(), "acme/widgets's mirror")
	assert.NotContains(t, strings.ToLower(err.Error()), "no checkpoint or commit found")
}
