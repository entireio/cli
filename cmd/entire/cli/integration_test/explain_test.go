//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestExplain_NoCurrentSession(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Without any flags, explain shows the branch view (not an error)
	output, err := env.RunCLIWithError("explain")

	if err != nil {
		t.Errorf("expected success for branch view, got error: %v, output: %s", err, output)
		return
	}

	// Should show branch information and checkpoint count
	if !strings.Contains(output, "Branch:") {
		t.Errorf("expected 'Branch:' header in output, got: %s", output)
	}
	if !strings.Contains(output, "Checkpoints:") {
		t.Errorf("expected 'Checkpoints:' in output, got: %s", output)
	}
}

func TestExplain_SessionFilter(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// --session now filters the list view instead of showing session details
	// A nonexistent session ID should show an empty list, not an error
	output, err := env.RunCLIWithError("explain", "--session", "nonexistent-session-id")

	if err != nil {
		t.Errorf("expected success (empty list) for session filter, got error: %v, output: %s", err, output)
		return
	}

	// Should show branch header
	if !strings.Contains(output, "Branch:") {
		t.Errorf("expected 'Branch:' header in output, got: %s", output)
	}

	// Should show 0 checkpoints (filter found no matches)
	if !strings.Contains(output, "Checkpoints: 0") {
		t.Errorf("expected 'Checkpoints: 0' for nonexistent session filter, got: %s", output)
	}

	// Should show filter info
	if !strings.Contains(output, "Filtered by session:") {
		t.Errorf("expected 'Filtered by session:' in output, got: %s", output)
	}
}

func TestExplain_MutualExclusivity(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Try to provide both --session and --commit flags
	output, err := env.RunCLIWithError("explain", "--session", "test-session", "--commit", "abc123")

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
	output, err := env.RunCLIWithError("explain", "--checkpoint", "nonexistent123")

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
	output, err := env.RunCLIWithError("explain", "--session", "test-session", "--checkpoint", "abc123")

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
	output, err := env.RunCLIWithError("explain", "--commit", commitHash[:7])
	if err != nil {
		t.Fatalf("unexpected error: %v, output: %s", err, output)
	}

	// Should show "No associated Entire checkpoint" message
	if !strings.Contains(output, "No associated Entire checkpoint") {
		t.Errorf("expected 'No associated Entire checkpoint' message, got: %s", output)
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
	output, err := env.RunCLIWithError("explain", "--commit", commitHash[:7])

	// We expect an error because the checkpoint abc123def456 doesn't exist
	if err == nil {
		// If it succeeded, check if it found the checkpoint (it shouldn't)
		if strings.Contains(output, "Checkpoint:") {
			t.Logf("checkpoint was found (unexpected but ok if test created one)")
		}
	} else {
		// Expected: checkpoint not found error
		if !strings.Contains(output, "checkpoint not found") {
			t.Errorf("expected 'checkpoint not found' error, got: %s", output)
		}
	}
}

func TestExplain_CheckpointV2EnabledFallsBackToV1(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Create a v1-only checkpoint (checkpoints_v2 disabled by default).
	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create v1 fallback file")
	require.NoError(t, err)

	content := "v1 fallback content"
	env.WriteFile("fallback.txt", content)

	session.CreateTranscript(
		"Create v1 fallback file",
		[]FileChange{{Path: "fallback.txt", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Create v1 fallback file", "fallback.txt")
	checkpointID := env.GetLatestCheckpointIDFromHistory()

	// Simulate enabling checkpoints_v2 after the v1-only checkpoint already exists.
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"checkpoints_v2": true},
	})

	output, err := env.RunCLIWithError("explain", "--checkpoint", checkpointID[:6])
	require.NoError(t, err, "expected explain checkpoint fallback to v1 to succeed: %s", output)

	if !strings.Contains(output, "Checkpoint: "+checkpointID) {
		t.Errorf("expected checkpoint ID in output, got: %s", output)
	}
	if !strings.Contains(output, "Intent: Create v1 fallback file") {
		t.Errorf("expected intent from v1 transcript in output, got: %s", output)
	}
}

func TestExplain_CheckpointV2EnabledPrefersV2WhenDualWriteExists(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"checkpoints_v2": true},
	})

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create v2 preferred file")
	require.NoError(t, err)

	content := "v2 preferred content"
	env.WriteFile("v2-preferred.txt", content)
	session.CreateTranscript(
		"Create v2 preferred file",
		[]FileChange{{Path: "v2-preferred.txt", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	// Creates dual-write checkpoint (v1 + v2).
	env.GitCommitWithShadowHooks("Create v2 preferred file", "v2-preferred.txt")
	checkpointID := env.GetLatestCheckpointIDFromHistory()

	// Corrupt only the v1 transcript for this checkpoint. If explain wrongly prefers
	// v1 when v2 is available, the intent will show this v1-only prompt.
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	v1Store := checkpoint.NewGitStore(repo)
	cpID := id.MustCheckpointID(checkpointID)

	summary, err := v1Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.NotEmpty(t, summary.Sessions)

	v1Content, err := v1Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err)

	err = v1Store.UpdateCommitted(context.Background(), checkpoint.UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    v1Content.Metadata.SessionID,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"v1 overridden prompt"}]}}` + "\n")),
		Prompts:      []string{"v1 overridden prompt"},
		Agent:        v1Content.Metadata.Agent,
	})
	require.NoError(t, err)

	output, err := env.RunCLIWithError("explain", "--checkpoint", checkpointID[:6])
	require.NoError(t, err, "expected explain to prefer v2 checkpoint data: %s", output)

	if !strings.Contains(output, "Intent: Create v2 preferred file") {
		t.Errorf("expected intent from v2 compact transcript, got: %s", output)
	}
	if strings.Contains(output, "v1 overridden prompt") {
		t.Errorf("unexpected v1-overridden intent found in output: %s", output)
	}
}

func TestExplain_CheckpointV2NoFullTranscriptUsesCompact(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Enable v2 to get dual-write checkpoints.
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"checkpoints_v2": true},
	})

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create compact-only file")
	require.NoError(t, err)

	content := "compact only content"
	env.WriteFile("compact-only.txt", content)
	session.CreateTranscript(
		"Create compact-only file",
		[]FileChange{{Path: "compact-only.txt", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Create compact-only file", "compact-only.txt")
	checkpointID := env.GetLatestCheckpointIDFromHistory()

	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)

	// Delete the v2 /full/current ref so no raw transcript is available from v2.
	err = repo.Storer.RemoveReference(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)

	// Overwrite the v1 transcript with a marker so we can detect if explain
	// falls back to v1 instead of using the v2 compact transcript.
	v1Store := checkpoint.NewGitStore(repo)
	cpID := id.MustCheckpointID(checkpointID)
	v1Content, err := v1Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err)

	err = v1Store.UpdateCommitted(context.Background(), checkpoint.UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    v1Content.Metadata.SessionID,
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"v1 marker prompt"}]}}` + "\n")),
		Prompts:      []string{"v1 marker prompt"},
		Agent:        v1Content.Metadata.Agent,
	})
	require.NoError(t, err)

	// Default explain (not --full) should succeed using compact transcript from v2 /main.
	output, err := env.RunCLIWithError("explain", "--checkpoint", checkpointID[:6])
	require.NoError(t, err, "expected explain to succeed with compact transcript when /full/* is missing: %s", output)

	require.Contains(t, output, "Checkpoint: "+checkpointID)
	// Intent should come from the v2 compact transcript, not the v1 marker.
	require.Contains(t, output, "Intent: Create compact-only file")
	require.NotContains(t, output, "v1 marker prompt",
		"explain should use v2 compact transcript, not fall back to v1")
}

func TestExplain_CheckpointV2MalformedFallsBackToV1(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Enable v2 to get dual-write checkpoints.
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"checkpoints_v2": true},
	})

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create v1 resilience file")
	require.NoError(t, err)

	content := "v1 resilience content"
	env.WriteFile("v1-resilience.txt", content)
	session.CreateTranscript(
		"Create v1 resilience file",
		[]FileChange{{Path: "v1-resilience.txt", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Create v1 resilience file", "v1-resilience.txt")
	checkpointID := env.GetLatestCheckpointIDFromHistory()

	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)

	// Corrupt the v2 /main ref by replacing it with a tree containing invalid
	// metadata.json. This causes ReadCommitted to return a JSON parse error
	// (not ErrCheckpointNotFound), which tests whether the resolver falls back
	// to v1 for non-sentinel errors.
	corruptV2MainRef(t, repo, checkpointID)

	// Explain should fall back to the valid v1 checkpoint.
	output, err := env.RunCLIWithError("explain", "--checkpoint", checkpointID[:6])
	require.NoError(t, err, "expected explain to fall back to v1 when v2 is malformed: %s", output)

	require.Contains(t, output, "Checkpoint: "+checkpointID)
	require.Contains(t, output, "Intent: Create v1 resilience file")
}

// corruptV2MainRef replaces the v2 /main ref's tree with one where the given
// checkpoint's metadata.json contains invalid JSON. This triggers a parse error
// in V2GitStore.ReadCommitted (a non-sentinel error).
func corruptV2MainRef(t *testing.T, repo *git.Repository, checkpointID string) {
	t.Helper()

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	ref, err := repo.Storer.Reference(refName)
	require.NoError(t, err, "v2 /main ref should exist")

	// Get the current commit to use as parent.
	parentHash := ref.Hash()

	// Create a blob with invalid JSON for metadata.json.
	garbageBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(`{invalid json!!!`))
	require.NoError(t, err)

	cpID := id.MustCheckpointID(checkpointID)
	cpPath := cpID.Path() // e.g. "ab/cdef123456"
	parts := strings.SplitN(cpPath, "/", 2)
	require.Len(t, parts, 2, "checkpoint path should have shard/remainder format")

	// Build tree bottom-up: metadata.json → checkpoint dir → shard dir → root
	cpTree := &object.Tree{Entries: []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: garbageBlob},
	}}
	cpTreeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, cpTree.Encode(cpTreeObj))
	cpTreeHash, err := repo.Storer.SetEncodedObject(cpTreeObj)
	require.NoError(t, err)

	shardTree := &object.Tree{Entries: []object.TreeEntry{
		{Name: parts[1], Mode: filemode.Dir, Hash: cpTreeHash},
	}}
	shardTreeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, shardTree.Encode(shardTreeObj))
	shardTreeHash, err := repo.Storer.SetEncodedObject(shardTreeObj)
	require.NoError(t, err)

	rootTree := &object.Tree{Entries: []object.TreeEntry{
		{Name: parts[0], Mode: filemode.Dir, Hash: shardTreeHash},
	}}
	rootTreeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, rootTree.Encode(rootTreeObj))
	rootTreeHash, err := repo.Storer.SetEncodedObject(rootTreeObj)
	require.NoError(t, err)

	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, rootTreeHash, parentHash,
		"corrupt metadata for test", "Test", "test@test.com")
	require.NoError(t, err)

	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(refName, commitHash)))
}

// TestExplain_BranchListingShowsCheckpointsAndPrompts runs the same scenario
// with v2 disabled and enabled, verifying that `entire explain` (branch listing)
// finds committed checkpoints and displays their prompts in both modes.
func TestExplain_BranchListingShowsCheckpointsAndPrompts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		v2   bool
	}{
		{"v1_only", false},
		{"v2_enabled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)

			if tc.v2 {
				env.PatchSettings(map[string]any{
					"strategy_options": map[string]any{"checkpoints_v2": true},
				})
			}

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
			output, err := env.RunCLIWithError("explain")
			require.NoError(t, err, "explain should succeed: %s", output)

			require.Contains(t, output, "Branch:")
			require.Contains(t, output, "Checkpoints: 1")
			require.Contains(t, output, "Implement user authentication",
				"branch listing should show the commit message or prompt")
		})
	}
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
	output := env.RunCLI("explain", "--checkpoint", checkpointID)

	// Verify the output contains checkpoint content (prompt text)
	if !strings.Contains(output, "Add feature module") {
		t.Errorf("expected output to contain prompt text, got:\n%s", output)
	}
}

// TestExplain_CheckpointV2FetchesFromRemoteWhenMissingLocally verifies that
// explain --checkpoint fetches v2 metadata from the remote when the v2 refs
// don't exist locally. Same scenario as the v1 test but with checkpoints_v2 enabled.
func TestExplain_CheckpointV2FetchesFromRemoteWhenMissingLocally(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Enable v2 checkpoints with push
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoints_v2": true,
			"push_v2_refs":   true,
		},
	})

	// Set up bare remote
	env.SetupBareRemote()

	// Create a session, make changes, checkpoint, and commit
	session := env.NewSession()
	transcriptPath := session.CreateTranscript("Add v2 feature", []FileChange{
		{Path: "v2feature.go", Content: "package v2feature"},
	})

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "Add v2 feature", transcriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("v2feature.go", "package v2feature")
	env.GitAdd("v2feature.go")

	if err := env.SimulateStop(session.ID, transcriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add v2 feature", "v2feature.go")

	checkpointID := env.GetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}

	// Push checkpoint data (v1 + v2 refs) to remote
	env.RunPrePush("origin")

	// Delete ALL local metadata refs (v1 and v2) to simulate
	// a collaborator's repo that has never fetched them.
	// RemoveReference may fail if a remote-tracking ref was never
	// populated; we tolerate that but assert absence below so the test
	// actually exercises the "fetch from remote when missing" path.
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)

	refsToRemove := []plumbing.ReferenceName{
		// v1 refs
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName),
		// v2 refs
		plumbing.ReferenceName(paths.V2MainRefName),
		plumbing.ReferenceName(paths.V2FullCurrentRefName),
	}
	for _, ref := range refsToRemove {
		_ = repo.Storer.RemoveReference(ref)
		_, err := repo.Storer.Reference(ref)
		require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "ref %s should be absent", ref)
	}

	// This should succeed by fetching metadata from the remote
	output := env.RunCLI("explain", "--checkpoint", checkpointID)

	if !strings.Contains(output, "Add v2 feature") {
		t.Errorf("expected output to contain prompt text, got:\n%s", output)
	}
}

// TestExplain_CheckpointFetchDoesNotRewindLocalAheadBranch verifies that running
// explain --checkpoint with a non-matching prefix does NOT rewind a
// locally-ahead entire/checkpoints/v1 branch. If the fetch path force-updates
// the local ref to match origin, locally-committed (but unpushed) checkpoints
// become orphaned and undiscoverable — potentially subject to GC.
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
	_, _ = env.RunCLIWithError("explain", "--checkpoint", "000000000000")

	// Re-open repo (go-git caches ref state per handle).
	repo, err = git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	afterRef, err := repo.Storer.Reference(localRefName)
	require.NoError(t, err, "local metadata branch should still exist after fetch-on-miss")
	require.Equal(t, beforeHash, afterRef.Hash(),
		"local metadata branch must not be rewound by fetch-on-miss; locally-ahead checkpoints would otherwise be orphaned")

	// Independently, checkpoint B must still be discoverable by explain.
	output := env.RunCLI("explain", "--checkpoint", checkpointB)
	require.Contains(t, output, "Add module B",
		"locally-committed checkpoint must remain discoverable after fetch-on-miss")
}

// TestExplain_CheckpointV2FetchDoesNotRewindLocalAheadRefs verifies that
// running explain --checkpoint with a non-matching prefix does NOT rewind a
// locally-ahead v2 ref (refs/entire/v2/main). v2 uses a direct-write refspec
// (`+refs/entire/v2/main:refs/entire/v2/main`), so a naive fetch force-rewinds
// the local ref, orphaning locally-committed-but-unpushed v2 checkpoint data.
func TestExplain_CheckpointV2FetchDoesNotRewindLocalAheadRefs(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoints_v2": true,
			"push_v2_refs":   true,
		},
	})
	env.SetupBareRemote()

	// Checkpoint A: commit locally, push to origin.
	sessionA := env.NewSession()
	transcriptA := sessionA.CreateTranscript("Add v2 module A", []FileChange{
		{Path: "a.go", Content: "package a"},
	})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sessionA.ID, "Add v2 module A", transcriptA))
	env.WriteFile("a.go", "package a")
	env.GitAdd("a.go")
	require.NoError(t, env.SimulateStop(sessionA.ID, transcriptA))
	env.GitCommitWithShadowHooks("Add v2 module A", "a.go")
	env.RunPrePush("origin")

	// Checkpoint B: commit locally, DO NOT push. Local v2 /main is now ahead.
	sessionB := env.NewSession()
	transcriptB := sessionB.CreateTranscript("Add v2 module B", []FileChange{
		{Path: "b.go", Content: "package b"},
	})
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sessionB.ID, "Add v2 module B", transcriptB))
	env.WriteFile("b.go", "package b")
	env.GitAdd("b.go")
	require.NoError(t, env.SimulateStop(sessionB.ID, transcriptB))
	env.GitCommitWithShadowHooks("Add v2 module B", "b.go")

	checkpointB := env.GetLatestCheckpointID()
	require.NotEmpty(t, checkpointB, "should have a checkpoint ID for B")

	// Snapshot local v2 /main hash (includes B's condensation) so we can verify
	// it doesn't rewind after the fetch.
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	v2MainRef := plumbing.ReferenceName(paths.V2MainRefName)
	beforeRef, err := repo.Storer.Reference(v2MainRef)
	require.NoError(t, err, "local v2 /main ref should exist after condensation")
	beforeHash := beforeRef.Hash()

	// Run explain with a non-matching prefix to force the fetch-on-miss path
	// for both v1 and v2. The command is expected to fail; we're testing the
	// side effect on the local v2 ref.
	_, _ = env.RunCLIWithError("explain", "--checkpoint", "000000000000")

	repo, err = git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	afterRef, err := repo.Storer.Reference(v2MainRef)
	require.NoError(t, err, "local v2 /main ref should still exist after fetch-on-miss")
	require.Equal(t, beforeHash, afterRef.Hash(),
		"local v2 /main ref must not be rewound by fetch-on-miss; locally-ahead v2 checkpoints would otherwise be orphaned")

	// Independently, checkpoint B must still be discoverable.
	output := env.RunCLI("explain", "--checkpoint", checkpointB)
	require.Contains(t, output, "Add v2 module B",
		"locally-committed v2 checkpoint must remain discoverable after fetch-on-miss")
}

// TestExplain_BranchListingV2OnlyAfterV1Deleted verifies that the branch listing
// works when only v2 data exists (v1 metadata branch deleted after dual-write).
func TestExplain_BranchListingV2OnlyAfterV1Deleted(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{"checkpoints_v2": true},
	})

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create v2 resilience file")
	require.NoError(t, err)

	content := "v2 resilience content"
	env.WriteFile("resilience.txt", content)
	session.CreateTranscript(
		"Create v2 resilience file",
		[]FileChange{{Path: "resilience.txt", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Create v2 resilience file", "resilience.txt")

	// Delete the v1 metadata branch.
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	_ = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("entire/checkpoints/v1"))

	// Branch listing should still work using v2 data.
	output, err := env.RunCLIWithError("explain")
	require.NoError(t, err, "explain should succeed with v2 only: %s", output)

	require.Contains(t, output, "Checkpoints: 1",
		"checkpoint should be visible from v2 after v1 deletion")
	require.Contains(t, output, "Create v2 resilience file",
		"prompt/intent should be readable from v2 after v1 deletion")
}
