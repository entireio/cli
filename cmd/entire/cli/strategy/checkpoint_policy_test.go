package strategy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestCondenseSessionRejectsUnsupportedPolicy(t *testing.T) {
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})

	strategy := NewManualCommitStrategy()
	sessionID := "policy-fallback-condense"
	setupSessionWithCheckpoint(t, strategy, repo, workDir, sessionID)
	state, err := strategy.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	writeUnsupportedCheckpointPolicy(t, repo)

	result, err := strategy.CondenseSession(
		context.Background(),
		repo,
		testTrailerCheckpointID,
		state,
		nil,
	)
	require.ErrorContains(t, err, "checkpoint policy cannot be satisfied by this Entire CLI")
	require.Nil(t, result)
}

func TestCondenseSessionRejectsUnreadablePolicy(t *testing.T) {
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})

	strategy := NewManualCommitStrategy()
	sessionID := "policy-unreadable-condense"
	setupSessionWithCheckpoint(t, strategy, repo, workDir, sessionID)
	state, err := strategy.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	writeMalformedCheckpointPolicy(t, repo)

	result, err := strategy.CondenseSession(
		context.Background(),
		repo,
		testTrailerCheckpointID,
		state,
		nil,
	)
	require.ErrorContains(t, err, "checkpoint policy could not be read")
	require.ErrorContains(t, err, "parse policy.json")
	require.Nil(t, result)
}

func TestCondenseAndMarkFullyCondensedSkipsUnsupportedPolicy(t *testing.T) {
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})

	strategy := NewManualCommitStrategy()
	sessionID := "policy-block-stop"
	setupSessionWithCheckpoint(t, strategy, repo, workDir, sessionID)
	require.NoError(t, MutateSessionState(context.Background(), sessionID, func(state *SessionState) error {
		state.Phase = session.PhaseEnded
		state.FilesTouched = nil
		return nil
	}))
	writeUnsupportedCheckpointPolicy(t, repo)

	require.NoError(t, strategy.CondenseAndMarkFullyCondensed(context.Background(), sessionID))

	state, err := strategy.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.False(t, state.FullyCondensed)
}

func TestFinalizeAllTurnCheckpointsSkipsUnsupportedPolicy(t *testing.T) {
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})
	writeUnsupportedCheckpointPolicy(t, repo)

	sessionID := "policy-block-turn-finalize"
	store := cpkg.NewGitStore(repo, cpkg.DefaultV1Refs())
	require.NoError(t, store.Write(context.Background(), cpkg.Session{
		CheckpointID: testTrailerCheckpointID,
		SessionID:    sessionID,
		Strategy:     StrategyNameManualCommit,
		Transcript:   redact.AlreadyRedacted([]byte("old transcript\n")),
		Prompts:      []string{"old prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
		Agent:        "Claude Code",
	}))

	metadataDir := filepath.Join(workDir, ".entire", "metadata", sessionID)
	require.NoError(t, os.MkdirAll(metadataDir, 0o755))
	transcriptPath := filepath.Join(metadataDir, paths.TranscriptFileName)
	require.NoError(t, os.WriteFile(transcriptPath, []byte(testTranscriptPromptResponse), 0o644))

	state := &SessionState{
		SessionID:         sessionID,
		AgentType:         "Claude Code",
		TranscriptPath:    transcriptPath,
		TurnCheckpointIDs: []string{testTrailerCheckpointID.String()},
	}

	errCount := NewManualCommitStrategy().finalizeAllTurnCheckpoints(context.Background(), state)
	require.Equal(t, 1, errCount)
	require.Empty(t, state.TurnCheckpointIDs)
}

func TestPrePushWarnsAndSkipsCheckpointPushWhenPolicyUnsupported(t *testing.T) {
	workDir := setupRepoWithCheckpointBranch(t)
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "remote", "add", "origin", bareDir)

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})
	_, err = checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "branch-v1",
	})
	require.NoError(t, err)

	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	t.Setenv(interactive.EnvTestTTY, "1")
	oldWriter := stderrWriter
	var stderr bytes.Buffer
	stderrWriter = &stderr
	t.Cleanup(func() { stderrWriter = oldWriter })

	err = NewManualCommitStrategy().PrePush(context.Background(), "origin")
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "requires checkpoint support newer than this Entire CLI")

	out := runCheckpointPolicyGit(t, workDir, "ls-remote", bareDir, "refs/heads/"+paths.MetadataBranchName)
	require.Empty(t, strings.TrimSpace(out))
}

func TestPrePushWarnsAndPushesWhenPolicyDiverged(t *testing.T) {
	workDir := setupRepoWithCheckpointBranch(t)
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "remote", "add", "origin", bareDir)

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})
	baseHash, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "push", bareDir, checkpointpolicy.RefName.String()+":"+checkpointpolicy.RefName.String())

	localHash, err := checkpointpolicy.WriteLocal(t.Context(), repo, baseHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	_, err = checkpointpolicy.WriteLocal(t.Context(), repo, baseHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "branch-v1",
	})
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "push", bareDir, checkpointpolicy.RefName.String()+":"+checkpointpolicy.RefName.String())
	require.NoError(t, checkpointpolicy.SetRef(repo, checkpointpolicy.RefName, localHash))

	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	t.Setenv(interactive.EnvTestTTY, "1")
	oldWriter := stderrWriter
	var stderr bytes.Buffer
	stderrWriter = &stderr
	t.Cleanup(func() { stderrWriter = oldWriter })

	err = NewManualCommitStrategy().PrePush(context.Background(), "origin")
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "Could not reconcile checkpoint policy")

	out := runCheckpointPolicyGit(t, workDir, "ls-remote", bareDir, "refs/heads/"+paths.MetadataBranchName)
	require.NotEmpty(t, strings.TrimSpace(out))
}

func TestSyncCheckpointPolicyForPrePushUsesPushTarget(t *testing.T) {
	workDir := setupGitRepo(t)
	originBareDir := filepath.Join(t.TempDir(), "origin.git")
	_, err := git.PlainInit(originBareDir, true)
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "remote", "add", "origin", originBareDir)

	pushTargetDir := filepath.Join(t.TempDir(), "push-target.git")
	_, err = git.PlainInit(pushTargetDir, true)
	require.NoError(t, err)

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Close()
	})

	_, err = checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "branch-v1",
	})
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "push", originBareDir, checkpointpolicy.RefName.String()+":"+checkpointpolicy.RefName.String())

	targetHash, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	runCheckpointPolicyGit(t, workDir, "push", pushTargetDir, checkpointpolicy.RefName.String()+":"+checkpointpolicy.RefName.String())
	require.NoError(t, repo.Storer.RemoveReference(checkpointpolicy.RefName))

	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	syncCheckpointPolicyForPrePush(context.Background(), repo, pushSettings{
		remote:        "origin",
		checkpointURL: pushTargetDir,
	})
	state, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, targetHash, state.Hash)
}

func writeUnsupportedCheckpointPolicy(t *testing.T, repo *git.Repository) {
	t.Helper()
	_, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "branch-v1",
	})
	require.NoError(t, err)
}

func writeMalformedCheckpointPolicy(t *testing.T, repo *git.Repository) {
	t.Helper()
	blobHash, err := cpkg.CreateBlobFromContent(repo, []byte(`{"checkpoint_version":`))
	require.NoError(t, err)
	treeHash, err := cpkg.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		checkpointpolicy.PolicyFileName: {Name: checkpointpolicy.PolicyFileName, Mode: filemode.Regular, Hash: blobHash},
	})
	require.NoError(t, err)
	commitHash, err := cpkg.CreateCommit(context.Background(), repo, treeHash, plumbing.ZeroHash, "malformed checkpoint policy", "Test", "test@example.com")
	require.NoError(t, err)
	require.NoError(t, checkpointpolicy.SetRef(repo, checkpointpolicy.RefName, commitHash))
}

func runCheckpointPolicyGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}
