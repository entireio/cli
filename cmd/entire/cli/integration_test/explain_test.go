//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

func TestExplain_NoCurrentSession(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Without any flags, explain shows the branch view (not an error)
	output, err := env.RunCLIWithError("checkpoint", "explain")

	if err != nil {
		t.Errorf("expected success for branch view, got error: %v, output: %s", err, output)
		return
	}

	// Should show branch information and checkpoint count (new metadata-row shape)
	if !strings.Contains(output, "branch  ") {
		t.Errorf("expected 'branch' row in output, got: %s", output)
	}
	if !strings.Contains(output, "checkpoints") {
		t.Errorf("expected 'checkpoints' row in output, got: %s", output)
	}
}

func TestExplain_SessionFilter(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// --session now filters the list view instead of showing session details
	// A nonexistent session ID should show an empty list, not an error
	output, err := env.RunCLIWithError("checkpoint", "explain", "--session", "nonexistent-session-id")

	if err != nil {
		t.Errorf("expected success (empty list) for session filter, got error: %v, output: %s", err, output)
		return
	}

	// Should show branch header (new metadata-row shape)
	if !strings.Contains(output, "branch  ") {
		t.Errorf("expected 'branch' row in output, got: %s", output)
	}

	// Should show 0 checkpoints (filter found no matches)
	if !strings.Contains(output, "checkpoints  0") {
		t.Errorf("expected 'checkpoints  0' for nonexistent session filter, got: %s", output)
	}

	// Should show filter info as a metadata row (label aligned to widest "checkpoints")
	if !strings.Contains(output, "session      nonexistent-session-id") {
		t.Errorf("expected 'session ... nonexistent-session-id' row in output, got: %s", output)
	}
}

func TestExplain_MutualExclusivity(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Try to provide both --session and --commit flags
	output, err := env.RunCLIWithError("checkpoint", "explain", "--session", "test-session", "--commit", "abc123")

	if err == nil {
		t.Errorf("expected error when both flags provided, got output: %s", output)
		return
	}

	if !strings.Contains(strings.ToLower(output), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %s", output)
	}
}

func TestExplain_CheckpointNotFound(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Try to explain a non-existent checkpoint
	output, err := env.RunCLIWithError("checkpoint", "explain", "--checkpoint", "nonexistent123")

	if err == nil {
		t.Errorf("expected error for nonexistent checkpoint, got output: %s", output)
		return
	}

	if !strings.Contains(output, "checkpoint not found") {
		t.Errorf("expected 'checkpoint not found' error, got: %s", output)
	}
}

func TestExplain_CheckpointMutualExclusivity(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Try to provide --checkpoint with --session
	output, err := env.RunCLIWithError("checkpoint", "explain", "--session", "test-session", "--checkpoint", "abc123")

	if err == nil {
		t.Errorf("expected error when both flags provided, got output: %s", output)
		return
	}

	if !strings.Contains(strings.ToLower(output), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %s", output)
	}
}

func TestExplain_CommitWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Create a regular commit without Entire-Checkpoint trailer
	env.WriteFile("test.txt", "content")
	env.GitAdd("test.txt")
	env.GitCommit("Regular commit without Entire trailer")

	// Get the commit hash
	commitHash := env.GetHeadHash()

	// Run explain --commit
	output, err := env.RunCLIWithError("checkpoint", "explain", "--commit", commitHash[:7])
	if err != nil {
		t.Fatalf("unexpected error: %v, output: %s", err, output)
	}

	// Should show "No associated Entire checkpoint" failure block
	if !strings.Contains(output, "✗ No associated Entire checkpoint") {
		t.Errorf("expected styled failure block, got: %s", output)
	}
	if !strings.Contains(output, "  reason") {
		t.Errorf("expected reason row, got: %s", output)
	}
}

func TestExplain_CommitWithCheckpointTrailer(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Create a commit with Entire-Checkpoint trailer
	env.WriteFile("test.txt", "content")
	env.GitAdd("test.txt")
	env.GitCommitWithCheckpointID("Commit with checkpoint", "abc123def456")

	// Get the commit hash
	commitHash := env.GetHeadHash()

	// Run explain --commit - it should try to look up the checkpoint
	// Since the checkpoint doesn't actually exist in the store, it should error
	output, err := env.RunCLIWithError("checkpoint", "explain", "--commit", commitHash[:7])

	// We expect an error because the checkpoint abc123def456 doesn't exist
	if err == nil {
		// If it succeeded, check if it found the checkpoint (it shouldn't)
		if strings.Contains(output, "● Checkpoint") {
			t.Logf("checkpoint was found (unexpected but ok if test created one)")
		}
	} else {
		// Expected: checkpoint not found error
		if !strings.Contains(output, "checkpoint not found") {
			t.Errorf("expected 'checkpoint not found' error, got: %s", output)
		}
	}
}

// TestExplain_BranchListingShowsCheckpointsAndPrompts verifies that `entire
// explain` branch listing finds committed checkpoints and displays prompts.
func TestExplain_BranchListingShowsCheckpointsAndPrompts(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Implement user authentication")
	require.NoError(t, err)

	env.WriteFile("auth.go", "package auth\nfunc Login() {}\n")
	session.CreateTranscript(
		"Implement user authentication",
		[]FileChange{{Path: "auth.go", Content: "package auth\nfunc Login() {}\n"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Implement user authentication", "auth.go")

	// `entire explain` (no flags) should show the branch listing with the checkpoint.
	output, err := env.RunCLIWithError("checkpoint", "explain")
	require.NoError(t, err, "explain should succeed: %s", output)

	require.Contains(t, output, "branch  ")
	require.Contains(t, output, "checkpoints  1")
	require.Contains(t, output, "Implement user authentication",
		"branch listing should show the commit message or prompt")
}

// TestExplain_CheckpointFetchesFromRemoteWhenMissingLocally verifies that
// explain --checkpoint fetches metadata from the remote when the
// entire/checkpoints/v1 branch doesn't exist locally (e.g., reviewing
// someone else's PR).
func TestExplain_CheckpointFetchesFromRemoteWhenMissingLocally(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Set up bare remote
	env.SetupBareRemote()

	// Create a session, make changes, checkpoint, and commit (triggers condensation)
	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Add feature module", []FileChange{
		{Path: "feature.go", Content: "package feature"},
	})

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Add feature module", transcriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("feature.go", "package feature")
	env.GitAdd("feature.go")

	if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit with hooks (triggers prepare-commit-msg + post-commit = condensation)
	env.GitCommitWithShadowHooks("Add feature module", "feature.go")

	// Get the checkpoint ID before we delete the local branch
	checkpointID := env.GetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}

	// Push checkpoint data to remote
	env.RunPrePush("origin")

	// Delete local metadata branch and remote-tracking ref to simulate
	// a collaborator's repo that has never fetched the metadata branch.
	// RemoveReference may fail if the remote-tracking ref was never
	// populated; we tolerate that but assert absence below so the test
	// actually exercises the "fetch from remote when missing" path.
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)

	localRef := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	remoteRef := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	_ = repo.Storer.RemoveReference(localRef)
	_ = repo.Storer.RemoveReference(remoteRef)

	_, err = repo.Storer.Reference(localRef)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "local metadata ref should be absent")
	_, err = repo.Storer.Reference(remoteRef)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "remote-tracking metadata ref should be absent")

	// This should succeed by fetching metadata from the remote
	output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)

	// Verify the output contains checkpoint content (prompt text)
	if !strings.Contains(output, "Add feature module") {
		t.Errorf("expected output to contain prompt text, got:\n%s", output)
	}
}

func TestExplain_CheckpointFetchDoesNotRewindLocalAheadBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()

	// Checkpoint A: commit locally, push to origin.
	sessionA := env.NewSession()
	transcriptA := sessionA.CreateTranscript("Add module A", []FileChange{
		{Path: "a.go", Content: "package a"},
	})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sessionA.ID, "Add module A", transcriptA))
	env.WriteFile("a.go", "package a")
	env.GitAdd("a.go")
	require.NoError(t, env.SimulateStop(sessionA.ID, transcriptA))
	env.GitCommitWithShadowHooks("Add module A", "a.go")
	env.RunPrePush("origin")

	// Checkpoint B: commit locally, DO NOT push. Local entire/checkpoints/v1 is
	// now ahead of origin by one commit.
	sessionB := env.NewSession()
	transcriptB := sessionB.CreateTranscript("Add module B", []FileChange{
		{Path: "b.go", Content: "package b"},
	})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sessionB.ID, "Add module B", transcriptB))
	env.WriteFile("b.go", "package b")
	env.GitAdd("b.go")
	require.NoError(t, env.SimulateStop(sessionB.ID, transcriptB))
	env.GitCommitWithShadowHooks("Add module B", "b.go")

	checkpointB := env.GetLatestCheckpointID()
	require.NotEmpty(t, checkpointB, "should have a checkpoint ID for B")

	// Snapshot local metadata branch hash (includes B) so we can verify it
	// doesn't rewind after the fetch.
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	localRefName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	beforeRef, err := repo.Storer.Reference(localRefName)
	require.NoError(t, err)
	beforeHash := beforeRef.Hash()

	// Run explain with a checkpoint prefix that doesn't match anything locally,
	// forcing the "fetch on miss" path. The prefix is 12 zeros: vanishingly
	// unlikely to collide with a real checkpoint ID.
	// The command is expected to fail (no such checkpoint) — we're testing the
	// side effect on the local ref, not the command's success.
	_, _ = env.RunCLIWithError("checkpoint", "explain", "--checkpoint", "000000000000")

	// Re-open repo (go-git caches ref state per handle).
	repo, err = git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	afterRef, err := repo.Storer.Reference(localRefName)
	require.NoError(t, err, "local metadata branch should still exist after fetch-on-miss")
	require.Equal(t, beforeHash, afterRef.Hash(),
		"local metadata branch must not be rewound by fetch-on-miss; locally-ahead checkpoints would otherwise be orphaned")

	// Independently, checkpoint B must still be discoverable by explain.
	output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointB)
	require.Contains(t, output, "Add module B",
		"locally-committed checkpoint must remain discoverable after fetch-on-miss")
}

func TestExplain_CheckpointSucceedsAfterTreelessFetch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	bareURL := env.SetupBareRemote()

	checkpointID := createAndPushCheckpoint(t, env, "treeless_v1.go", "Treeless v1 prompt")

	cloneDir := setupTreelessClone(t, bareURL, "+refs/heads/"+paths.MetadataBranchName+":refs/heads/"+paths.MetadataBranchName)
	requireBlobMissing(t, cloneDir, checkpointID)

	output := runExplainInDir(t, cloneDir, checkpointID)
	require.Contains(t, output, "Treeless v1 prompt",
		"explain should succeed and surface the prompt despite blobs being absent locally")
}

func createAndPushCheckpoint(t *testing.T, env *TestEnv, fileName, prompt string) string {
	t.Helper()
	session := env.NewSession()
	transcriptPath := session.CreateTranscript(prompt, []FileChange{
		{Path: fileName, Content: "package treeless"},
	})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, prompt, transcriptPath))
	env.WriteFile(fileName, "package treeless")
	env.GitAdd(fileName)
	require.NoError(t, env.SimulateStop(session.ID, transcriptPath))
	env.GitCommitWithShadowHooks("Add "+fileName, fileName)
	cpID := env.GetLatestCheckpointID()
	require.NotEmpty(t, cpID, "expected a checkpoint after condensation")
	env.RunPrePush("origin")
	return cpID
}

// setupTreelessClone creates a fresh git repo in a fresh TempDir, fetches
// the given refspec from bareURL with --filter=blob:none --depth=1 (so
// trees but no blobs land locally), and writes a minimal entire settings
// file pointing at bareURL as the checkpoint_remote. Returns the new dir.
//
// Note: the bare and the fetch must go through the smart protocol for
// --filter to be honored; the default local-path transport optimization
// copies packs verbatim and ignores filters. We set
// uploadpack.allowFilter=true on the bare and use a file:// URL with
// protocol.file.allow=always to force the smart path.
func setupTreelessClone(t *testing.T, barePath, refspec string) string {
	t.Helper()
	gitEnv := testutil.GitIsolatedEnv()
	enableFilterOnBare(t, barePath, gitEnv)

	cloneDir := t.TempDir()
	fileURL := "file://" + barePath

	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "protocol.file.allow=always", "fetch", "--filter=blob:none", "--depth=1", "--no-tags", fileURL, refspec},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = cloneDir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	require.NoError(t, writeMinimalEntireSettings(cloneDir, barePath))
	return cloneDir
}

// enableFilterOnBare sets uploadpack.allowFilter=true on the bare repo so
// that --filter=blob:none on fetch is honored.
func enableFilterOnBare(t *testing.T, barePath string, gitEnv []string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", barePath, "config", "uploadpack.allowFilter", "true")
	cmd.Env = gitEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to set uploadpack.allowFilter on bare: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(t.Context(), "git", "-C", barePath, "config", "uploadpack.allowAnySHA1InWant", "true")
	cmd.Env = gitEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to set uploadpack.allowAnySHA1InWant on bare: %v\n%s", err, out)
	}
}

// writeMinimalEntireSettings writes the smallest valid settings.json that
// configures the manual-commit strategy with filtered_fetches enabled and
// a custom checkpoint_remote URL — the partial-clone setup that triggered
// the original bug.
func writeMinimalEntireSettings(dir, bareURL string) error {
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		return err
	}
	settings := map[string]any{
		"enabled":   true,
		"local_dev": true,
		"strategy":  "manual-commit",
		"strategy_options": map[string]any{
			"filtered_fetches": true,
			"checkpoint_remote": map[string]any{
				"provider": "url",
				"url":      bareURL,
			},
		},
	}
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(entireDir, paths.SettingsFileName), data, 0o644)
}

// runExplainInDir runs `entire explain --checkpoint <id>` in dir and
// returns combined output. Fails the test if the command errors. Uses
// execx.NonInteractive (project rule for spawning the entire binary in
// tests) so the child has no controlling terminal.
func runExplainInDir(t *testing.T, dir, checkpointID string) string {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), getTestBinary(), "explain", "--checkpoint", checkpointID)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("explain failed: %v\n%s", err, out)
	}
	return string(out)
}

// requireBlobMissing asserts that at least one metadata blob for the
// checkpoint is genuinely absent from the local object store. Confirms the
// treeless-clone setup actually reproduces the bug-triggering state — if
// every blob were locally available, the test would pass without
// exercising the fix.
func requireBlobMissing(t *testing.T, dir, checkpointID string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "metadata ref should exist after treeless fetch")

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	rootTree, err := commit.Tree()
	require.NoError(t, err)
	cpSubtree, err := rootTree.Tree(checkpointID[:2] + "/" + checkpointID[2:])
	require.NoError(t, err, "cp subtree should be navigable from local trees")

	for _, entry := range cpSubtree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		if _, err := repo.BlobObject(entry.Hash); err != nil {
			return // confirmed: at least one blob is missing
		}
	}
	t.Fatalf("expected at least one metadata blob to be missing in fresh treeless clone (cp=%s)", checkpointID)
}
