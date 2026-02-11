package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrepareCommitMsg_AmendPreservesExistingTrailer verifies that when amending
// a commit that already has an Entire-Checkpoint trailer, the trailer is preserved
// unchanged. source="commit" indicates an amend operation.
func TestPrepareCommitMsg_AmendPreservesExistingTrailer(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	sessionID := "test-session-amend-preserve"
	err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)

	// Write a commit message file that already has the trailer
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	existingMsg := "Original commit message\n\nEntire-Checkpoint: abc123def456\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(existingMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(commitMsgFile, "commit")
	require.NoError(t, err)

	// Read the file back and verify the trailer is still present
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	cpID, found := trailers.ParseCheckpoint(string(content))
	assert.True(t, found, "trailer should still be present after amend")
	assert.Equal(t, "abc123def456", cpID.String(),
		"trailer should preserve the original checkpoint ID")
}

// TestPrepareCommitMsg_AmendRestoresTrailerFromPendingCheckpointID verifies the amend
// bug fix: when a user does `git commit --amend -m "new message"`, the Entire-Checkpoint
// trailer is lost because the new message replaces the old one. PrepareCommitMsg restores
// the trailer from PendingCheckpointID in session state.
func TestPrepareCommitMsg_AmendRestoresTrailerFromPendingCheckpointID(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	sessionID := "test-session-amend-restore"
	err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)

	// Simulate state after condensation: PendingCheckpointID is set
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	state.PendingCheckpointID = "abc123def456"
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Write a commit message file with NO trailer (user did --amend -m "new message")
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	newMsg := "New amended message\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(newMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(commitMsgFile, "commit")
	require.NoError(t, err)

	// Read the file back - trailer should be restored from PendingCheckpointID
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	cpID, found := trailers.ParseCheckpoint(string(content))
	assert.True(t, found,
		"trailer should be restored from PendingCheckpointID on amend")
	assert.Equal(t, "abc123def456", cpID.String(),
		"restored trailer should use PendingCheckpointID value")
}

// TestPrepareCommitMsg_AmendNoTrailerNoPendingID verifies that when amending with
// no existing trailer and no PendingCheckpointID in session state, no trailer is added.
// This is the case where the session has never been condensed yet.
func TestPrepareCommitMsg_AmendNoTrailerNoPendingID(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	sessionID := "test-session-amend-no-id"
	err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)

	// Verify PendingCheckpointID is empty (default)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Empty(t, state.PendingCheckpointID, "PendingCheckpointID should be empty by default")

	// Write a commit message file with NO trailer
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	newMsg := "Amended without any session context\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(newMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(commitMsgFile, "commit")
	require.NoError(t, err)

	// Read the file back - no trailer should be added
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	_, found := trailers.ParseCheckpoint(string(content))
	assert.False(t, found,
		"no trailer should be added when PendingCheckpointID is empty")

	// Message should be unchanged
	assert.Equal(t, newMsg, string(content),
		"commit message should be unchanged when no trailer to restore")
}

// TestPrepareCommitMsg_ConsecutiveCommits_UniqueCheckpointIDs verifies that
// consecutive commits during an active session each get their own unique
// checkpoint ID. This is a regression test for a bug where PendingCheckpointID
// (set by PostCommit for deferred condensation) was incorrectly reused by
// PrepareCommitMsg for subsequent commits, causing duplicate checkpoint IDs.
//
// The fix: PrepareCommitMsg should always generate a fresh ID for new commits.
// Idempotency for hook re-runs on the same commit is handled by checking if
// the commit message already has a trailer (not by reusing PendingCheckpointID).
func TestPrepareCommitMsg_ConsecutiveCommits_UniqueCheckpointIDs(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	sessionID := "test-session-consecutive-commits"
	err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)

	// Create content on the shadow branch so filterSessionsWithNewContent finds it
	createShadowBranchWithTranscript(t, dir, sessionID)

	// Set the session to ACTIVE_COMMITTED with a PendingCheckpointID
	// This simulates the state after PostCommit ran for a previous commit
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	state.Phase = session.PhaseActiveCommitted
	state.PendingCheckpointID = "fedcba987654" // ID from previous commit
	state.StepCount = 1
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Write a NEW commit message file (simulating a second commit)
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	normalMsg := "Feature: second commit\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(normalMsg), 0o644))

	// Call PrepareCommitMsg for the new commit
	err = s.PrepareCommitMsg(commitMsgFile, "")
	require.NoError(t, err)

	// Read the file back - trailer should have a NEW checkpoint ID
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	cpID, found := trailers.ParseCheckpoint(string(content))
	assert.True(t, found,
		"trailer should be present for commit with active session content")
	assert.NotEqual(t, "fedcba987654", cpID.String(),
		"consecutive commits should get unique checkpoint IDs, not reuse PendingCheckpointID from previous commit")
	assert.Len(t, cpID.String(), 12,
		"checkpoint ID should be 12 hex characters")
}

// createShadowBranchWithTranscript creates a shadow branch commit with a minimal
// transcript file so that filterSessionsWithNewContent detects new content.
// This uses low-level go-git plumbing to create the branch directly.
func createShadowBranchWithTranscript(t *testing.T, repoDir string, sessionID string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	baseCommit := head.Hash().String()

	// Build the tree with a transcript file at the expected path
	metadataDir := paths.EntireMetadataDir + "/" + sessionID
	transcriptPath := metadataDir + "/" + paths.TranscriptFileName
	transcriptContent := `{"type":"message","role":"assistant","content":"hello"}` + "\n"

	// Create blob for transcript
	blobObj := &plumbing.MemoryObject{}
	blobObj.SetType(plumbing.BlobObject)
	blobObj.SetSize(int64(len(transcriptContent)))
	writer, err := blobObj.Writer()
	require.NoError(t, err)
	_, err = writer.Write([]byte(transcriptContent))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	blobHash, err := repo.Storer.SetEncodedObject(blobObj)
	require.NoError(t, err)

	// Build nested tree structure: .entire/metadata/<sessionID>/full.jsonl
	// We need to build trees bottom-up
	innerTree := object.Tree{
		Entries: []object.TreeEntry{
			{Name: paths.TranscriptFileName, Mode: 0o100644, Hash: blobHash},
		},
	}
	innerTreeObj := repo.Storer.NewEncodedObject()
	innerTreeObj.SetType(plumbing.TreeObject)
	require.NoError(t, innerTree.Encode(innerTreeObj))
	innerTreeHash, err := repo.Storer.SetEncodedObject(innerTreeObj)
	require.NoError(t, err)

	// Build .entire/metadata/<sessionID> level
	sessionTree := object.Tree{
		Entries: []object.TreeEntry{
			{Name: sessionID, Mode: 0o040000, Hash: innerTreeHash},
		},
	}
	sessionTreeObj := repo.Storer.NewEncodedObject()
	sessionTreeObj.SetType(plumbing.TreeObject)
	require.NoError(t, sessionTree.Encode(sessionTreeObj))
	sessionTreeHash, err := repo.Storer.SetEncodedObject(sessionTreeObj)
	require.NoError(t, err)

	// Build .entire/metadata level
	metadataTree := object.Tree{
		Entries: []object.TreeEntry{
			{Name: "metadata", Mode: 0o040000, Hash: sessionTreeHash},
		},
	}
	metadataTreeObj := repo.Storer.NewEncodedObject()
	metadataTreeObj.SetType(plumbing.TreeObject)
	require.NoError(t, metadataTree.Encode(metadataTreeObj))
	metadataTreeHash, err := repo.Storer.SetEncodedObject(metadataTreeObj)
	require.NoError(t, err)

	// Build .entire level
	entireTree := object.Tree{
		Entries: []object.TreeEntry{
			{Name: ".entire", Mode: 0o040000, Hash: metadataTreeHash},
		},
	}
	entireTreeObj := repo.Storer.NewEncodedObject()
	entireTreeObj.SetType(plumbing.TreeObject)
	require.NoError(t, entireTree.Encode(entireTreeObj))
	entireTreeHash, err := repo.Storer.SetEncodedObject(entireTreeObj)
	require.NoError(t, err)

	// Create commit on shadow branch
	now := time.Now()
	commitObj := &object.Commit{
		Author: object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  now,
		},
		Committer: object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  now,
		},
		Message:  "checkpoint\n\nEntire-Metadata: " + metadataDir + "\nEntire-Session: " + sessionID + "\nEntire-Strategy: manual-commit\n",
		TreeHash: entireTreeHash,
	}
	commitEnc := repo.Storer.NewEncodedObject()
	require.NoError(t, commitObj.Encode(commitEnc))
	commitHash, err := repo.Storer.SetEncodedObject(commitEnc)
	require.NoError(t, err)

	// Create the shadow branch reference
	// WorktreeID is empty for main worktree, which matches what setupGitRepo creates
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(baseCommit, "")
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	require.NoError(t, repo.Storer.SetReference(ref))

	// Verify the transcript is readable
	verifyCommit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)
	verifyTree, err := verifyCommit.Tree()
	require.NoError(t, err)
	file, err := verifyTree.File(transcriptPath)
	require.NoError(t, err, "transcript file should exist at %s", transcriptPath)
	content, err := file.Contents()
	require.NoError(t, err)
	require.NotEmpty(t, content, "transcript should have content")
}
