package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	goGit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestAgentCheckCmd_ConstructsSubcommands(t *testing.T) {
	cmd := newAgentCheckCmd()
	require.Equal(t, "agentcheck", cmd.Name())
	require.NotNil(t, cmd.Commands())

	children := map[string]bool{}
	for _, child := range cmd.Commands() {
		children[child.Name()] = true
	}
	require.True(t, children["status"], "agentcheck should register status")
	require.True(t, children["open"], "agentcheck should register open")
}

func TestAgentCheckCmd_RegisteredOnRoot(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"agentcheck"})
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.Equal(t, "agentcheck", cmd.Name())
	require.Equal(t, groupSessions, cmd.GroupID)
}

func TestAgentCheckStatus_MissingBundle(t *testing.T) {
	cpID := setupAgentCheckCheckpoint(t)

	var stdout, stderr bytes.Buffer
	cmd := newAgentCheckStatusCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{cpID.String()[:8]})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	out := stdout.String()
	require.Contains(t, out, "Checkpoint: "+cpID.String())
	require.Contains(t, out, "AgentCheck evidence bundle: missing")
	require.Contains(t, out, filepath.Join(agentCheckDirName, agentCheckReportsName, cpID.String()))
	requireNoFabricatedAgentCheckResult(t, out)
}

func TestAgentCheckStatus_ExistingBundle(t *testing.T) {
	cpID := setupAgentCheckCheckpoint(t)
	repoRoot, err := agentCheckTestRepoRoot()
	require.NoError(t, err)
	wantBundle := filepath.Join(repoRoot, agentCheckDirName, agentCheckReportsName, cpID.String())
	require.NoError(t, os.MkdirAll(wantBundle, 0o755))

	var stdout, stderr bytes.Buffer
	cmd := newAgentCheckStatusCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{cpID.String()})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	out := stdout.String()
	require.Contains(t, out, "AgentCheck evidence bundle: found")
	require.Contains(t, out, wantBundle)
	require.Contains(t, out, "HTML report: missing")
	requireNoFabricatedAgentCheckResult(t, out)
}

func TestAgentCheckOpen_MissingReport(t *testing.T) {
	cpID := setupAgentCheckCheckpoint(t)
	repoRoot, err := agentCheckTestRepoRoot()
	require.NoError(t, err)
	bundleDir := filepath.Join(repoRoot, agentCheckDirName, agentCheckReportsName, cpID.String())
	require.NoError(t, os.MkdirAll(bundleDir, 0o755))

	var stdout, stderr bytes.Buffer
	cmd := newAgentCheckOpenCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{cpID.String()})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	out := stdout.String()
	require.Contains(t, out, "AgentCheck HTML report missing")
	require.Contains(t, out, filepath.Join(bundleDir, agentCheckReportName))
	requireNoFabricatedAgentCheckResult(t, out)
}

func TestAgentCheckStatus_NoTargetWithoutHeadCheckpointIsActionable(t *testing.T) {
	repo := setupAgentCheckRepo(t)
	require.NoError(t, repo.Close())

	var stdout, stderr bytes.Buffer
	cmd := newAgentCheckStatusCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	out := stdout.String()
	require.Contains(t, out, "AgentCheck evidence status unavailable.")
	require.Contains(t, out, "HEAD does not reference an Entire checkpoint")
	requireNoFabricatedAgentCheckResult(t, out)
}

func TestAgentCheckOpen_ExistingReportPrintsPath(t *testing.T) {
	cpID := setupAgentCheckCheckpoint(t)
	repoRoot, err := agentCheckTestRepoRoot()
	require.NoError(t, err)
	bundleDir := filepath.Join(repoRoot, agentCheckDirName, agentCheckReportsName, cpID.String())
	reportPath := filepath.Join(bundleDir, agentCheckReportName)
	require.NoError(t, os.MkdirAll(bundleDir, 0o755))
	require.NoError(t, os.WriteFile(reportPath, []byte("<!doctype html><title>AgentCheck</title>"), 0o600))

	var stdout, stderr bytes.Buffer
	cmd := newAgentCheckOpenCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{cpID.String()})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	out := stdout.String()
	require.Contains(t, out, "AgentCheck HTML report: "+reportPath)
	requireNoFabricatedAgentCheckResult(t, out)
}

func TestAgentCheckBase_EvaluatesRendersAndPersistsVerification(t *testing.T) {
	cpID := setupAgentCheckCheckpoint(t)

	var stdout, stderr bytes.Buffer
	cmd := newAgentCheckCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{cpID.String()})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	out := stdout.String()
	require.Contains(t, out, "AgentCheck")
	require.Contains(t, out, "Checkpoint: "+cpID.String())
	require.Contains(t, out, "Verdict:    TRUSTED")
	require.Contains(t, out, "Summary:")
	require.Contains(t, out, "Verification:")

	repoRoot, err := agentCheckTestRepoRoot()
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(repoRoot, agentCheckDirName, agentCheckReportsName, cpID.String(), agentCheckVerificationEvidenceName))
}

func setupAgentCheckCheckpoint(t *testing.T) id.CheckpointID {
	t.Helper()
	repo := setupAgentCheckRepo(t)
	cpID := id.MustCheckpointID("acac11112222")
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteOptions{
		SessionID:  "agentcheck-session",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"agentcheck"}]}}` + "\n")),
	})
	require.NoError(t, repo.Close())
	return cpID
}

func setupAgentCheckRepo(t *testing.T) *goGit.Repository {
	t.Helper()
	originalWD, err := os.Getwd()
	require.NoError(t, err)

	tmpDir, err := os.MkdirTemp("", t.Name())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.Chdir(originalWD)
		paths.ClearWorktreeRootCache()
		runtime.GC()
		_ = os.RemoveAll(tmpDir)
	})

	testutil.InitRepo(t, tmpDir)
	require.NoError(t, os.Chdir(tmpDir))

	repo, err := goGit.PlainOpen(tmpDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "f.txt"), []byte("init"), 0o600))
	_, err = wt.Add("f.txt")
	require.NoError(t, err)
	_, err = wt.Commit("init", &goGit.CommitOptions{
		Author: &object.Signature{Name: exportTestAuthorName, Email: exportTestAuthorEmail, When: time.Now()},
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"), []byte(`{"enabled": true}`), 0o600))
	return repo
}

func agentCheckTestRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return resolved, nil
	}
	return cwd, nil
}

func requireNoFabricatedAgentCheckResult(t *testing.T, out string) {
	t.Helper()
	for _, forbidden := range []string{"TRUSTED", "REVIEW REQUIRED", "FAIL", "trust score", "findings[]"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("AgentCheck output fabricated result field %q:\n%s", forbidden, out)
		}
	}
}
