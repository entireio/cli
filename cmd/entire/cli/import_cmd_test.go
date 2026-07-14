package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
