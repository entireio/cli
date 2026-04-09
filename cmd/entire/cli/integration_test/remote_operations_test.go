//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// =============================================================================
// P0 -- PrePush Basic Flow
// =============================================================================

// TestPrePush_PushesCheckpointBranchToOrigin verifies that PrePush pushes
// the entire/checkpoints/v1 branch to a bare remote after condensation.
func TestPrePush_PushesCheckpointBranchToOrigin(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Set up bare remote
	bareDir := env.SetupBareRemote()

	// Create a session, make changes, checkpoint, and commit (triggers condensation)
	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Add auth module", []FileChange{
		{Path: "auth.go", Content: "package auth"},
	})

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Add auth module", transcriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("auth.go", "package auth")
	env.GitAdd("auth.go")

	if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit with hooks (triggers prepare-commit-msg + post-commit = condensation)
	env.GitCommitWithShadowHooks("Add auth module", "auth.go")

	// Verify condensation happened locally
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 should exist locally after condensation")
	}

	// Run PrePush (simulates what happens during git push)
	env.RunPrePush("origin")

	// Verify the branch arrived on the remote
	if !env.BranchExistsOnRemote(bareDir, paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should exist on bare remote after PrePush")
	}

	// Verify checkpoint metadata is in the remote tree
	checkpointID := env.GetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}
	summaryPath := CheckpointSummaryPath(checkpointID)
	if !fileExistsOnRemoteBranch(t, bareDir, summaryPath) {
		t.Errorf("checkpoint metadata should exist on remote at %s", summaryPath)
	}
}

// TestPrePush_NoOpWhenNoCheckpoints verifies that PrePush is a no-op
// when no sessions or checkpoints exist.
func TestPrePush_NoOpWhenNoCheckpoints(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Run PrePush without any session activity
	env.RunPrePush("origin")

	// No checkpoint branches should exist on remote
	if env.BranchExistsOnRemote(bareDir, paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should NOT exist on remote when no checkpoints were created")
	}
}

// TestPrePush_IdempotentWhenAlreadyPushed verifies that pushing twice
// with no new checkpoints is a no-op (idempotent).
func TestPrePush_IdempotentWhenAlreadyPushed(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Create session, commit, push
	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Initial work", []FileChange{
		{Path: "main.go", Content: "package main"},
	})

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Initial work", transcriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("main.go", "package main")
	env.GitAdd("main.go")

	if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Initial work", "main.go")

	// First push
	env.RunPrePush("origin")

	if !env.BranchExistsOnRemote(bareDir, paths.MetadataBranchName) {
		t.Fatal("checkpoint branch should exist after first push")
	}

	// Get remote ref before second push
	refBefore := getRemoteBranchHash(t, bareDir, paths.MetadataBranchName)

	// Second push (no new checkpoints)
	env.RunPrePush("origin")

	// Remote ref should be unchanged
	refAfter := getRemoteBranchHash(t, bareDir, paths.MetadataBranchName)
	if refBefore != refAfter {
		t.Errorf("remote ref should be unchanged after idempotent push: before=%s, after=%s", refBefore, refAfter)
	}
}

// =============================================================================
// P0 -- Checkpoint Remote Config
// =============================================================================

// TestPrePush_PushDisabledSkipsCheckpoints verifies that push_sessions: false
// disables checkpoint push.
func TestPrePush_PushDisabledSkipsCheckpoints(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Configure push_sessions: false
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"push_sessions": false,
		},
	})

	// Create session, checkpoint, and commit
	_ = createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	// Verify checkpoint was created locally
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("should have local checkpoint branch after condensation")
	}

	// PrePush should skip checkpoints when push_sessions is false
	env.RunPrePush("origin")

	// Checkpoints should NOT be on remote
	if env.BranchExistsOnRemote(bareDir, paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should NOT be on remote when push_sessions is false")
	}
}

// TestPrePush_CheckpointRemoteRoutesToSeparateRemote verifies that checkpoint data
// can be selectively pushed to a separate remote.
//
// This is a data routing verification test. It validates that when the production
// code's pushBranchIfNeeded is called with different targets for checkpoints,
// the branches land on the correct remotes with correct data.
//
// Why not test through PrePush directly: resolvePushSettings derives the checkpoint
// URL from origin's protocol (SSH/HTTPS). Since integration tests use local file
// paths as remotes, parseGitRemoteURL fails and resolvePushSettings falls back to
// origin. The URL derivation logic is unit-tested in checkpoint_remote_test.go
// (TestDeriveCheckpointURL, TestResolvePushSettings_WithCheckpointRemote_*).
//
// The pushBranchIfNeeded function (which PrePush calls with the resolved target)
// is exercised in push_common_test.go:TestPushBranchIfNeeded_LocalBareRepo_PushesSuccessfully,
// verifying it works with local bare repo paths.
func TestPrePush_CheckpointRemoteRoutesToSeparateRemote(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Set up two bare remotes: origin for user code, checkpoint remote for checkpoints
	bareOrigin := env.SetupBareRemote()
	bareCheckpoint := env.SetupNamedBareRemote("checkpoint-store")

	// Create session, checkpoint, and commit
	checkpointID := createCheckpointedCommit(t, env, "Add router", "router.go", "package router", "Add router")

	// Verify local checkpoint branch exists
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("should have local checkpoint branch after condensation")
	}

	// Simulate checkpoint_remote routing: push checkpoints to the checkpoint bare repo.
	// This mirrors what PrePush does when resolvePushSettings returns a checkpointURL.
	env.GitPush(bareCheckpoint, paths.MetadataBranchName)

	// Checkpoints should be on checkpoint remote, NOT on origin
	if !env.BranchExistsOnRemote(bareCheckpoint, paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should exist on checkpoint remote")
	}
	if env.BranchExistsOnRemote(bareOrigin, paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should NOT be on origin when routed to checkpoint remote")
	}

	// Verify checkpoint data arrived on checkpoint remote
	summaryPath := CheckpointSummaryPath(checkpointID)
	if !fileExistsOnRemoteBranch(t, bareCheckpoint, summaryPath) {
		t.Errorf("checkpoint metadata should exist on checkpoint remote at %s", summaryPath)
	}
}

// TestPrePush_CheckpointURLDerivationFailureFallsBackToOrigin verifies that when
// origin is a local file path (not an SSH/HTTPS URL), resolvePushSettings cannot
// derive a checkpoint URL and checkpoints fall back to origin.
//
// This exercises the integration behavior: when checkpoint_remote is configured but
// the push remote URL cannot be parsed (local file path), checkpoints still land on
// origin. The exact fork detection logic (owner mismatch) is unit-tested in
// checkpoint_remote_test.go:TestResolvePushSettings_ForkDetection.
func TestPrePush_CheckpointURLDerivationFailureFallsBackToOrigin(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Configure checkpoint_remote with a different owner than origin.
	// Since our bare remote is a local path (not a URL), resolvePushSettings cannot
	// parse it via parseGitRemoteURL and falls back to origin. The unit test
	// TestResolvePushSettings_ForkDetection in checkpoint_remote_test.go validates
	// the exact fork detection logic with real URL parsing.
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "different-org/checkpoints",
			},
		},
	})

	// Create session, checkpoint, and commit
	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Add middleware", []FileChange{
		{Path: "middleware.go", Content: "package middleware"},
	})

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Add middleware", transcriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("middleware.go", "package middleware")
	env.GitAdd("middleware.go")

	if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add middleware", "middleware.go")

	// Run PrePush -- with a local path remote, checkpoint URL derivation will fail
	// (parseGitRemoteURL can't parse local paths), so checkpoints fall back to origin.
	env.RunPrePush("origin")

	// Checkpoints should be on origin (fallback behavior)
	if !env.BranchExistsOnRemote(bareDir, paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should be on origin when checkpoint_remote is unavailable (fork/fallback)")
	}
}

// =============================================================================
// P0 -- Clone and Resume
// =============================================================================

// createCheckpointedCommit is a helper that creates a session with a single file change,
// runs the full lifecycle (prompt submit -> write -> stop -> commit), and returns the
// session and checkpoint ID. This reduces boilerplate in tests where the session setup
// is not the focus.
func createCheckpointedCommit(t *testing.T, env *TestEnv, prompt, fileName, fileContent, commitMsg string) string {
	t.Helper()

	session := env.NewSession()
	transcriptPath := session.CreateTranscript(prompt, []FileChange{
		{Path: fileName, Content: fileContent},
	})

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, prompt, transcriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile(fileName, fileContent)
	env.GitAdd(fileName)

	if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks(commitMsg, fileName)

	return env.GetLatestCheckpointID()
}

// TestCloneAndResume_FetchesCheckpointMetadata verifies that after pushing
// checkpoints to a remote, cloning the repo and fetching the metadata branch
// brings the checkpoint data locally.
//
// Note: This test uses env.FetchMetadataBranch (raw git fetch) rather than
// triggering fetchMetadataBranchIfMissing via the production code path, because
// fetchMetadataBranchIfMissing uses OpenRepository which requires CWD to be
// inside the repo (incompatible with t.Parallel). The production function is
// tested directly in checkpoint_remote_test.go (TestFetchBranchIfMissing_*).
// This test verifies the "data arrives and is usable" property.
func TestCloneAndResume_FetchesCheckpointMetadata(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Repo A: create session, checkpoint, commit, push
	checkpointID := createCheckpointedCommit(t, env, "Build login page", "login.go", "package login", "Build login page")
	t.Logf("Original checkpoint ID: %s", checkpointID)

	// Push user branch + checkpoint branches to remote
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")

	// Clone into repo B
	cloneEnv := env.CloneFrom(bareDir)

	// Fetch the metadata branch (simulates what fetchMetadataBranchIfMissing does)
	cloneEnv.FetchMetadataBranch(bareDir)

	// Now the metadata branch should exist locally in the clone
	if !cloneEnv.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 should exist in clone after fetch")
	}

	// Verify the checkpoint data is present
	summaryPath := CheckpointSummaryPath(checkpointID)
	if !cloneEnv.FileExistsInBranch(paths.MetadataBranchName, summaryPath) {
		t.Errorf("checkpoint metadata should exist in clone at %s", summaryPath)
	}
}

// TestCloneAndResume_SessionListingWorksAfterClone verifies that checkpoint
// metadata can be enumerated after cloning and fetching the metadata branch.
func TestCloneAndResume_SessionListingWorksAfterClone(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Repo A: create session, checkpoint, commit, push
	checkpointID := createCheckpointedCommit(t, env, "Implement auth", "auth.go", "package auth", "Implement auth")

	// Push everything
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")

	// Clone and fetch metadata
	cloneEnv := env.CloneFrom(bareDir)
	cloneEnv.FetchMetadataBranch(bareDir)

	// List checkpoints from the clone by scanning the metadata branch tree
	checkpoints := listCheckpointsInDir(t, cloneEnv.RepoDir)

	if len(checkpoints) == 0 {
		t.Fatal("should find checkpoints from original repo after clone")
	}

	// Verify our checkpoint is in the list
	found := false
	for _, cp := range checkpoints {
		if cp == checkpointID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("checkpoint %s from original repo should be in list, got: %v", checkpointID, checkpoints)
	}
}

// TestCloneAndResume_SessionLogRetrievalWorksAfterClone verifies that
// checkpoint transcripts can be read after clone + fetch.
func TestCloneAndResume_SessionLogRetrievalWorksAfterClone(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Repo A: create session with a specific transcript content
	checkpointID := createCheckpointedCommit(t, env, "Write tests for auth", "auth_test.go", "package auth_test", "Write auth tests")

	// Push
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")

	// Clone and fetch metadata
	cloneEnv := env.CloneFrom(bareDir)
	cloneEnv.FetchMetadataBranch(bareDir)

	// Read transcript from the clone
	transcriptFilePath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	content, found := cloneEnv.ReadFileFromBranch(paths.MetadataBranchName, transcriptFilePath)
	if !found {
		t.Fatalf("transcript should be readable from clone at %s", transcriptFilePath)
	}

	if content == "" {
		t.Error("transcript content should not be empty")
	}

	// Verify it contains our expected content
	if !strings.Contains(content, "Write tests for auth") {
		t.Error("transcript should contain the user prompt from the original session")
	}
}

// TestCloneAndResume_NewSessionPushAppends verifies that after cloning,
// creating a new session, and pushing, the remote has BOTH original and
// new checkpoint data.
func TestCloneAndResume_NewSessionPushAppends(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Repo A: session + commit + push
	originalCheckpointID := createCheckpointedCommit(t, env, "Add models", "models.go", "package models", "Add models")
	t.Logf("Original checkpoint: %s", originalCheckpointID)

	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")

	// Clone into repo B and fetch metadata
	cloneEnv := env.CloneFrom(bareDir)
	cloneEnv.FetchMetadataBranch(bareDir)

	// Repo B: create NEW session on a new branch, commit, push
	cloneEnv.GitCheckoutNewBranch("feature/clone-work")

	newCheckpointID := createCheckpointedCommit(t, cloneEnv, "Add handlers", "handlers.go", "package handlers", "Add handlers")
	t.Logf("New checkpoint: %s", newCheckpointID)

	// Push from clone
	cloneEnv.RunPrePush("origin")

	// Verify remote has BOTH checkpoints
	originalSummaryPath := CheckpointSummaryPath(originalCheckpointID)
	if !fileExistsOnRemoteBranch(t, bareDir, originalSummaryPath) {
		t.Errorf("original checkpoint %s should still exist on remote", originalCheckpointID)
	}

	newSummaryPath := CheckpointSummaryPath(newCheckpointID)
	if !fileExistsOnRemoteBranch(t, bareDir, newSummaryPath) {
		t.Errorf("new checkpoint %s should exist on remote", newCheckpointID)
	}
}

// =============================================================================
// P1 -- Non-Fast-Forward Recovery
// =============================================================================

// TestConcurrentPush_SecondPusherRebasesAndRetries verifies that when two clones
// push to the same remote, the second pusher fetches, rebases, and retries.
func TestConcurrentPush_SecondPusherRebasesAndRetries(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Clone A
	cloneA := env.CloneFrom(bareDir)
	cloneA.GitCheckoutNewBranch("feature/clone-a")

	// Clone B
	cloneB := env.CloneFrom(bareDir)
	cloneB.GitCheckoutNewBranch("feature/clone-b")

	// Clone A: session + commit
	checkpointA := createCheckpointedCommit(t, cloneA, "Work in clone A", "a.go", "package a", "Work from A")
	t.Logf("Clone A checkpoint: %s", checkpointA)

	// Clone B: session + commit
	checkpointB := createCheckpointedCommit(t, cloneB, "Work in clone B", "b.go", "package b", "Work from B")
	t.Logf("Clone B checkpoint: %s", checkpointB)

	// A pushes first (should succeed cleanly)
	cloneA.RunPrePush("origin")

	// B pushes second (will get non-fast-forward, should fetch+rebase+retry)
	cloneB.RunPrePush("origin")

	// Remote should have BOTH checkpoints
	summaryA := CheckpointSummaryPath(checkpointA)
	if !fileExistsOnRemoteBranch(t, bareDir, summaryA) {
		t.Errorf("remote should have checkpoint from clone A: %s", checkpointA)
	}

	summaryB := CheckpointSummaryPath(checkpointB)
	if !fileExistsOnRemoteBranch(t, bareDir, summaryB) {
		t.Errorf("remote should have checkpoint from clone B: %s", checkpointB)
	}

	// Verify B's local metadata branch tip has exactly 1 parent (linear rebase, not merge).
	// This confirms the fetch+rebase+retry path was taken.
	parentCount := cloneB.GetBranchTipParentCount(paths.MetadataBranchName)
	if parentCount != 1 {
		t.Errorf("clone B's metadata branch tip should have 1 parent (rebased), got %d", parentCount)
	}
}

// =============================================================================
// P1 -- Graceful Degradation
// =============================================================================

// TestGracefulDegradation_UnreachableCheckpointRemotePushContinues verifies that
// when checkpoint_remote is configured but cannot be resolved (because origin is a
// local path, not an SSH/HTTPS URL), PrePush falls back to pushing checkpoints to
// origin. Trails are always pushed to origin regardless of checkpoint_remote config.
//
// This test exercises the PrePush -> resolvePushSettings -> fallback code path.
// The actual doPushBranch graceful degradation (push to unreachable URL returns nil,
// not an error) is tested in push_common_test.go:
//   - TestDoPushBranch_UnreachableTarget_ReturnsNil
//   - TestPushBranchIfNeeded_UnreachableTarget_ReturnsNil
func TestGracefulDegradation_UnreachableCheckpointRemotePushContinues(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareOrigin := env.SetupBareRemote()

	// Configure checkpoint_remote with a nonexistent repo. Since origin is a local
	// file path, resolvePushSettings cannot parse it as a URL and will silently fall
	// back to pushing checkpoints to origin (the default behavior).
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "nonexistent-org/unreachable-repo",
			},
		},
	})

	// Create session, checkpoint, and commit
	_ = createCheckpointedCommit(t, env, "Some work", "work.go", "package work", "Some work")

	// Verify local checkpoint branch exists
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("should have local checkpoint branch after condensation")
	}

	// Run PrePush with checkpoint_remote configured. Since origin is a local path,
	// resolvePushSettings will fail to derive a checkpoint URL and fall back to
	// pushing checkpoints to origin.
	env.RunPrePush("origin")

	// Checkpoints should be on origin (fallback behavior when checkpoint URL derivation fails)
	if !env.BranchExistsOnRemote(bareOrigin, paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should be on origin when checkpoint_remote URL derivation fails")
	}
}

// TestGracefulDegradation_UnreachableCheckpointRemoteOnCloneIsSilent verifies that
// when a clone configures checkpoint_remote pointing to an unreachable path, starting
// a session does not error. fetchMetadataBranchIfMissing silently swallows fetch failures.
func TestGracefulDegradation_UnreachableCheckpointRemoteOnCloneIsSilent(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	bareDir := env.SetupBareRemote()

	// Create a session in repo A and push
	createCheckpointedCommit(t, env, "Initial work", "init.go", "package init", "Initial work")
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")

	// Clone from origin
	cloneEnv := env.CloneFrom(bareDir)

	// Configure checkpoint_remote to an unreachable path in the clone.
	// When the checkpoint_remote URL can't be reached, fetch fails silently.
	cloneEnv.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "nonexistent-org/nonexistent-repo",
			},
		},
	})

	// Starting a new session should not error even though checkpoint_remote is unreachable.
	// The session machinery itself doesn't trigger fetchMetadataBranchIfMissing (that happens
	// in resolvePushSettings during PrePush), so this verifies the session starts cleanly.
	session := cloneEnv.NewSession()
	transcriptPath := session.CreateTranscript("Clone work", []FileChange{
		{Path: "clone.go", Content: "package clone"},
	})

	if err := cloneEnv.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Clone work", transcriptPath); err != nil {
		t.Fatalf("session start should not fail with unreachable checkpoint_remote: %v", err)
	}

	if err := cloneEnv.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("session stop should not fail: %v", err)
	}

	// PrePush should also not fail -- checkpoint_remote URL derivation will fail
	// (origin is a local path, can't parse it), so it falls back to pushing to origin.
	cloneEnv.WriteFile("clone.go", "package clone")
	cloneEnv.GitAdd("clone.go")
	cloneEnv.GitCommitWithShadowHooks("Clone work", "clone.go")
	cloneEnv.RunPrePush("origin")

	// Verify that the session actually created a local checkpoint despite the
	// unreachable checkpoint_remote config.
	if !cloneEnv.BranchExists(paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 should exist locally after session + commit in clone")
	}
}

// =============================================================================
// Helpers
// =============================================================================

// fileExistsOnRemoteBranch checks if a file exists in the metadata branch tree on a bare remote.
func fileExistsOnRemoteBranch(t *testing.T, bareDir, filePath string) bool {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "cat-file", "-t", paths.MetadataBranchName+":"+filePath)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}

// getRemoteBranchHash returns the commit hash of a branch on a bare remote.
func getRemoteBranchHash(t *testing.T, bareDir, branchName string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "refs/heads/"+branchName)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get hash for %s on remote: %v", branchName, err)
	}
	return strings.TrimSpace(string(output))
}

// listCheckpointsInDir reads checkpoint IDs from the metadata branch tree.
// This intentionally uses a separate implementation (git ls-tree) rather than
// the production ListCheckpoints() to avoid testing the code with itself.
// The sharded directory structure is documented in CLAUDE.md.
func listCheckpointsInDir(t *testing.T, repoDir string) []string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "ls-tree", "-r", "--name-only", paths.MetadataBranchName)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.Output()
	if err != nil {
		t.Logf("ls-tree failed (branch may not exist): %v", err)
		return nil
	}

	// Parse the sharded structure: <prefix>/<suffix>/metadata.json
	seen := make(map[string]bool)
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "/")
		// Match <prefix>/<suffix>/metadata.json (top-level, not session-level)
		if len(parts) == 3 && parts[2] == paths.MetadataFileName {
			cpID := parts[0] + parts[1]
			if !seen[cpID] {
				seen[cpID] = true
				ids = append(ids, cpID)
			}
		}
	}

	return ids
}
