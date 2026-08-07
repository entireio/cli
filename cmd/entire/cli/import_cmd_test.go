package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

func TestImportClaudeCode_DryRunReportsCounts(t *testing.T) {
	// Not parallel: uses t.Chdir for CWD-based repo/worktree resolution.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)

	claudeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"claude-code", "--path", claudeDir, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v (out=%q)", err, out.String())
	}
	if !strings.Contains(out.String(), "Would import 1") {
		t.Fatalf("dry-run summary missing count: %q", out.String())
	}
}

func TestImportClaudeCodeDryRunBlocksWhenPolicyWriteUnsupported(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)

	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	writeUnsupportedCheckpointPolicyForCLITest(t, repo)

	claudeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644))

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"claude-code", "--path", claudeDir, "--dry-run"})

	err = cmd.Execute()
	require.ErrorContains(t, err, "checkpoint policy cannot be satisfied by this Entire CLI")
	require.NotContains(t, out.String(), "Would import")
}

func TestImportClaudeCodeDryRunBlocksWhenPolicyUnreadable(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)

	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	writeMalformedCheckpointPolicyForCLITest(t, repo)

	claudeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644))

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"claude-code", "--path", claudeDir, "--dry-run"})

	err = cmd.Execute()
	require.ErrorContains(t, err, "checkpoint policy could not be read")
	require.ErrorContains(t, err, "parse policy.json")
	require.NotContains(t, out.String(), "Would import")
}

func TestImportClaudeCodeHelpDocumentsCheckpointPolicy(t *testing.T) {
	t.Parallel()

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"claude-code", "--help"})
	cmd.SetContext(context.Background())

	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), "Import honors checkpoint policy before scanning transcripts.")
	require.Contains(t, out.String(), "fails even with --dry-run")
}

// TestImportAgentCmd_ReconcileFlagsRegistered pins the flag surface on every
// importer subcommand — adding an agent must not silently ship one without the
// reconcile flags — and the documented defaults.
func TestImportAgentCmd_ReconcileFlagsRegistered(t *testing.T) {
	t.Parallel()

	for _, imp := range agentimport.All() {
		t.Run(imp.Name(), func(t *testing.T) {
			t.Parallel()
			cmd := newImportAgentCmd(imp)
			for _, name := range []string{"reconcile", "accept-heuristics", "json"} {
				flag := cmd.Flags().Lookup(name)
				require.NotNil(t, flag, "--%s must be registered", name)
				require.Equal(t, "false", flag.DefValue, "--%s must default off", name)
			}
			lookback := cmd.Flags().Lookup("lookback")
			require.NotNil(t, lookback)
			require.Equal(t, strconv.Itoa(agentimport.DefaultLookbackDays), lookback.DefValue)
		})
	}
}

// TestImportClaudeCode_JSONOutput proves --json emits a parseable document on
// stdout with no progress output mixed in, and that --accept-heuristics turns
// reconciliation on by itself (accepting matches from a scan that never ran
// would silently do nothing).
func TestImportClaudeCode_JSONOutput(t *testing.T) {
	// Not parallel: uses t.Chdir for CWD-based repo/worktree resolution.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "unlinked work")
	t.Chdir(repoDir)
	headSHA := testutil.GetHeadHash(t, repoDir)

	claudeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644))

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// --accept-heuristics only; --reconcile is implied.
	cmd.SetArgs([]string{"claude-code", "--path", claudeDir, "--dry-run", "--accept-heuristics", "--json"})
	require.NoError(t, cmd.Execute())

	var got struct {
		CommitsScanned   int `json:"commits_scanned"`
		UnmatchedCommits []struct {
			CommitSHA string `json:"commit_sha"`
			Subject   string `json:"subject"`
		} `json:"unmatched_commits"`
		Summary struct {
			Agent         string `json:"agent"`
			DryRun        bool   `json:"dry_run"`
			Reconciled    bool   `json:"reconciled"`
			TurnsImported int    `json:"turns_imported"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got), "output must be a single JSON document: %q", out.String())
	require.True(t, got.Summary.Reconciled, "--accept-heuristics must imply --reconcile")
	require.True(t, got.Summary.DryRun)
	require.Equal(t, "claude-code", got.Summary.Agent)
	require.Equal(t, 1, got.Summary.TurnsImported)
	require.Equal(t, 1, got.CommitsScanned, "the repo's single commit has no session data")
	require.Len(t, got.UnmatchedCommits, 1)
	// Full SHAs, never abbreviated: consumers match on them.
	require.Equal(t, headSHA, got.UnmatchedCommits[0].CommitSHA)
	require.Equal(t, "unlinked work", got.UnmatchedCommits[0].Subject)
}

// TestImportClaudeCode_ReconcileTextReport proves the non-JSON path prints the
// same information as plain text, so an agent reading a pipe (no TTY) still
// sees which commits are without session data.
func TestImportClaudeCode_ReconcileTextReport(t *testing.T) {
	// Not parallel: uses t.Chdir for CWD-based repo/worktree resolution.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "unlinked work")
	t.Chdir(repoDir)
	headSHA := testutil.GetHeadHash(t, repoDir)

	claudeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644))

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"claude-code", "--path", claudeDir, "--dry-run", "--reconcile"})
	require.NoError(t, cmd.Execute())

	require.Contains(t, out.String(), "unmatched "+headSHA[:8]+" unlinked work")
	require.Contains(t, out.String(), "Scanned 1 commit(s) without session data")
}
