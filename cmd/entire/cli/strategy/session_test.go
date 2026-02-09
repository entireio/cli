package strategy

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const testSessionID = "2025-01-15-test-session"

var testCheckpointID = id.MustCheckpointID("abc123def456")

func TestSessionStruct(t *testing.T) {
	now := time.Now()
	checkpoints := []Checkpoint{
		{
			CheckpointID:     id.MustCheckpointID("abc123def456"),
			Message:          "First checkpoint",
			Timestamp:        now.Add(-time.Hour),
			IsTaskCheckpoint: false,
			ToolUseID:        "",
		},
		{
			CheckpointID:     id.MustCheckpointID("def456abc789"),
			Message:          "Task checkpoint",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			ToolUseID:        "toolu_123",
		},
	}

	session := Session{
		ID:          "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e",
		Description: "Fix lint errors",
		Strategy:    StrategyNameManualCommit,
		StartTime:   now.Add(-2 * time.Hour),
		Checkpoints: checkpoints,
	}

	if session.ID != "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e" {
		t.Errorf("expected session ID to match, got %s", session.ID)
	}
	if session.Description != "Fix lint errors" {
		t.Errorf("expected description to match, got %s", session.Description)
	}
	if session.Strategy != StrategyNameManualCommit {
		t.Errorf("expected strategy to be manual-commit, got %s", session.Strategy)
	}
	if len(session.Checkpoints) != 2 {
		t.Errorf("expected 2 checkpoints, got %d", len(session.Checkpoints))
	}
	if session.StartTime.IsZero() {
		t.Error("expected StartTime to be set")
	}
}

func TestCheckpointStruct(t *testing.T) {
	now := time.Now()

	// Test session checkpoint (not task)
	sessionCheckpoint := Checkpoint{
		CheckpointID:     "abc1234567890",
		Message:          "Session save",
		Timestamp:        now,
		IsTaskCheckpoint: false,
		ToolUseID:        "",
	}

	if sessionCheckpoint.CheckpointID != "abc1234567890" {
		t.Errorf("expected CheckpointID to match, got %s", sessionCheckpoint.CheckpointID)
	}
	if sessionCheckpoint.Message != "Session save" {
		t.Errorf("expected Message to match, got %s", sessionCheckpoint.Message)
	}
	if sessionCheckpoint.Timestamp != now {
		t.Error("expected Timestamp to match")
	}
	if sessionCheckpoint.IsTaskCheckpoint {
		t.Error("expected session checkpoint to not be a task checkpoint")
	}
	if sessionCheckpoint.ToolUseID != "" {
		t.Error("expected session checkpoint to have empty ToolUseID")
	}

	// Test task checkpoint
	taskCheckpoint := Checkpoint{
		CheckpointID:     "def0987654321",
		Message:          "Task: implement feature",
		Timestamp:        now,
		IsTaskCheckpoint: true,
		ToolUseID:        "toolu_abc123",
	}

	if taskCheckpoint.CheckpointID != "def0987654321" {
		t.Errorf("expected CheckpointID to match, got %s", taskCheckpoint.CheckpointID)
	}
	if taskCheckpoint.Message != "Task: implement feature" {
		t.Errorf("expected Message to match, got %s", taskCheckpoint.Message)
	}
	if taskCheckpoint.Timestamp != now {
		t.Error("expected Timestamp to match")
	}
	if !taskCheckpoint.IsTaskCheckpoint {
		t.Error("expected task checkpoint to be a task checkpoint")
	}
	if taskCheckpoint.ToolUseID != "toolu_abc123" {
		t.Errorf("expected ToolUseID to match, got %s", taskCheckpoint.ToolUseID)
	}
}

func TestSessionCheckpointCount(t *testing.T) {
	session := Session{
		ID:          "test-session",
		Description: "Test",
		Checkpoints: []Checkpoint{
			{CheckpointID: "a"},
			{CheckpointID: "b"},
			{CheckpointID: "c"},
		},
	}

	if session.ID != "test-session" {
		t.Errorf("expected ID to match, got %s", session.ID)
	}
	if session.Description != "Test" {
		t.Errorf("expected Description to match, got %s", session.Description)
	}
	if len(session.Checkpoints) != 3 {
		t.Errorf("expected 3 checkpoints, got %d", len(session.Checkpoints))
	}
	// Verify checkpoint IDs are accessible
	if session.Checkpoints[0].CheckpointID != "a" {
		t.Errorf("expected first checkpoint ID to be 'a', got %s", session.Checkpoints[0].CheckpointID)
	}
}

func TestEmptySession(t *testing.T) {
	session := Session{}

	if session.ID != "" {
		t.Error("expected empty session to have empty ID")
	}
	if session.Description != "" {
		t.Error("expected empty session to have empty Description")
	}
	if session.Checkpoints != nil {
		t.Error("expected empty session to have nil Checkpoints")
	}
}

// TestShadowStrategyImplementsSessionSource verifies that manual-commit strategy implements SessionSource
func TestManualCommitStrategyImplementsSessionSource(t *testing.T) {
	// Manual-commit strategy should implement SessionSource
	var strat = NewManualCommitStrategy()
	source, ok := strat.(SessionSource)
	if !ok {
		t.Fatal("ManualCommitStrategy should implement SessionSource interface")
	}

	// GetAdditionalSessions should be callable
	_, err := source.GetAdditionalSessions()
	if err != nil {
		t.Logf("GetAdditionalSessions returned error: %v", err)
	}
}

func TestListSessionsFunctionsWithoutRepo(t *testing.T) {
	// Without a git repo, these will fail - just verifying they're callable
	_, err := ListSessions()
	if err != nil {
		t.Logf("ListSessions returned error (expected without git repo): %v", err)
	}

	_, err = GetSession("test-session-id")
	if err != nil && !errors.Is(err, ErrNoSession) {
		t.Logf("GetSession returned error (expected without git repo): %v", err)
	}
}

func TestListSessionsEmptyRepo(t *testing.T) {
	tmpDir := t.TempDir()
	initTestRepo(t, tmpDir)
	t.Chdir(tmpDir)

	// No entire/checkpoints/v1 branch exists yet
	sessions, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}

	if len(sessions) != 0 {
		t.Errorf("ListSessions() returned %d sessions, want 0", len(sessions))
	}
}

func TestListSessionsWithCheckpoints(t *testing.T) {
	tmpDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks() failed: %v", err)
	}
	tmpDir = resolved

	initTestRepo(t, tmpDir)
	t.Chdir(tmpDir)

	repo, err := OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository() failed: %v", err)
	}

	// Create entire/checkpoints/v1 branch with test checkpoints
	createTestMetadataBranch(t, repo, testSessionID)

	sessions, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1", len(sessions))
	}

	sess := sessions[0]
	if sess.ID != testSessionID {
		t.Errorf("Session.ID = %q, want %q", sess.ID, testSessionID)
	}
	if len(sess.Checkpoints) != 1 {
		t.Errorf("len(Checkpoints) = %d, want 1", len(sess.Checkpoints))
	}
	if sess.Checkpoints[0].CheckpointID != testCheckpointID {
		t.Errorf("Checkpoint.CheckpointID = %q, want %q", sess.Checkpoints[0].CheckpointID, testCheckpointID)
	}
}

func TestListSessionsWithDescription(t *testing.T) {
	tmpDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks() failed: %v", err)
	}
	tmpDir = resolved

	initTestRepo(t, tmpDir)
	t.Chdir(tmpDir)

	repo, err := OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository() failed: %v", err)
	}

	// Create entire/checkpoints/v1 branch with test checkpoint including prompt.txt
	expectedDesc := "Fix the bug in the login form"
	createTestMetadataBranchWithPrompt(t, repo, testSessionID, testCheckpointID, expectedDesc)

	sessions, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1", len(sessions))
	}

	sess := sessions[0]
	if sess.Description != expectedDesc {
		t.Errorf("Session.Description = %q, want %q", sess.Description, expectedDesc)
	}
}

func TestGetSessionByID(t *testing.T) {
	tmpDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks() failed: %v", err)
	}
	tmpDir = resolved

	initTestRepo(t, tmpDir)
	t.Chdir(tmpDir)

	repo, err := OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository() failed: %v", err)
	}

	createTestMetadataBranch(t, repo, testSessionID)

	// Test exact match
	sess, err := GetSession(testSessionID)
	if err != nil {
		t.Fatalf("GetSession() error = %v, want nil", err)
	}
	if sess.ID != testSessionID {
		t.Errorf("Session.ID = %q, want %q", sess.ID, testSessionID)
	}

	// Test prefix match
	sess, err = GetSession("2025-01-15")
	if err != nil {
		t.Fatalf("GetSession() with prefix error = %v, want nil", err)
	}
	if sess.ID != testSessionID {
		t.Errorf("Session.ID = %q, want %q", sess.ID, testSessionID)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	initTestRepo(t, tmpDir)
	t.Chdir(tmpDir)

	_, err := GetSession("nonexistent-session")
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("GetSession() error = %v, want ErrNoSession", err)
	}
}

// TestListSessionsMultiSessionCheckpoint verifies that archived sessions in multi-session
// checkpoints are listed correctly. When multiple sessions are condensed to the same
// checkpoint, they are stored as:
//   - Root level: latest session (SessionID)
//   - 1/, 2/, etc.: archived sessions (in SessionIDs array)
//
// ListSessions should return ALL sessions from SessionIDs, not just SessionID.
func TestListSessionsMultiSessionCheckpoint(t *testing.T) {
	tmpDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks() failed: %v", err)
	}
	tmpDir = resolved

	initTestRepo(t, tmpDir)
	t.Chdir(tmpDir)

	repo, err := OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository() failed: %v", err)
	}

	// Create a multi-session checkpoint with 2 sessions:
	// - session-A: archived in 1/ subfolder
	// - session-B: latest at root level
	sessionA := "2025-01-14-session-A"
	sessionB := "2025-01-15-session-B"
	checkpointID := id.MustCheckpointID("ab12cd34ef56")

	createTestMultiSessionCheckpoint(t, repo, checkpointID, sessionB, []string{sessionA, sessionB})

	sessions, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}

	// Should return 2 sessions - one for each session ID in the checkpoint
	if len(sessions) != 2 {
		t.Fatalf("ListSessions() returned %d sessions, want 2 (both session-A and session-B)", len(sessions))
	}

	// Verify both sessions are present
	sessionIDs := make(map[string]bool)
	for _, sess := range sessions {
		sessionIDs[sess.ID] = true
	}

	if !sessionIDs[sessionA] {
		t.Errorf("ListSessions() missing archived session %q", sessionA)
	}
	if !sessionIDs[sessionB] {
		t.Errorf("ListSessions() missing latest session %q", sessionB)
	}

	// Both sessions should have the same checkpoint
	for _, sess := range sessions {
		if len(sess.Checkpoints) != 1 {
			t.Errorf("Session %q has %d checkpoints, want 1", sess.ID, len(sess.Checkpoints))
		}
		if len(sess.Checkpoints) > 0 && sess.Checkpoints[0].CheckpointID != checkpointID {
			t.Errorf("Session %q checkpoint ID = %q, want %q", sess.ID, sess.Checkpoints[0].CheckpointID, checkpointID)
		}
	}
}

// Helper function to create a test metadata branch with a multi-session checkpoint
func createTestMultiSessionCheckpoint(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID, primarySessionID string, allSessionIDs []string) {
	t.Helper()

	entries := make(map[string]object.TreeEntry)
	checkpointPath := checkpointID.Path()

	// Create session-level metadata for each session (0-based indexing)
	var sessionFilePaths []checkpoint.SessionFilePaths
	for i, sessionID := range allSessionIDs {
		sessionDir := strconv.Itoa(i) // 0-based: 0, 1, 2, ...
		sessionMetadata := checkpoint.CommittedMetadata{
			CheckpointID: checkpointID,
			SessionID:    sessionID,
			CreatedAt:    time.Now(),
		}
		sessionMetadataJSON, err := json.Marshal(sessionMetadata)
		if err != nil {
			t.Fatalf("failed to marshal session metadata: %v", err)
		}
		sessionMetadataBlobHash, err := checkpoint.CreateBlobFromContent(repo, sessionMetadataJSON)
		if err != nil {
			t.Fatalf("failed to create session metadata blob: %v", err)
		}
		sessionMetadataPath := checkpointPath + "/" + sessionDir + "/" + paths.MetadataFileName
		entries[sessionMetadataPath] = object.TreeEntry{
			Name: sessionMetadataPath,
			Mode: filemode.Regular,
			Hash: sessionMetadataBlobHash,
		}
		// Use absolute paths with leading "/" as per new format
		sessionFilePaths = append(sessionFilePaths, checkpoint.SessionFilePaths{
			Metadata: "/" + checkpointPath + "/" + sessionDir + "/" + paths.MetadataFileName,
		})
	}

	// Create root CheckpointSummary with Sessions array (using absolute paths)
	summary := checkpoint.CheckpointSummary{
		CheckpointID: checkpointID,
		Sessions:     sessionFilePaths,
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("failed to marshal summary: %v", err)
	}
	summaryBlobHash, err := checkpoint.CreateBlobFromContent(repo, summaryJSON)
	if err != nil {
		t.Fatalf("failed to create summary blob: %v", err)
	}
	entries[checkpointPath+"/"+paths.MetadataFileName] = object.TreeEntry{
		Name: checkpointPath + "/" + paths.MetadataFileName,
		Mode: filemode.Regular,
		Hash: summaryBlobHash,
	}

	// Build tree
	treeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		t.Fatalf("failed to build tree: %v", err)
	}

	// Create orphan commit
	now := time.Now()
	sig := object.Signature{
		Name:  "Test",
		Email: "test@test.com",
		When:  now,
	}
	commit := &object.Commit{
		TreeHash:  treeHash,
		Author:    sig,
		Committer: sig,
		Message:   "Multi-session checkpoint\n\n" + trailers.SessionTrailerKey + ": " + primarySessionID,
	}

	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("failed to encode commit: %v", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("failed to store commit: %v", err)
	}

	// Create branch reference
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to create branch: %v", err)
	}
}

// Helper function to create a test metadata branch with a checkpoint (uses testCheckpointID)
func createTestMetadataBranch(t *testing.T, repo *git.Repository, sessionID string) {
	t.Helper()
	createTestMetadataBranchWithPrompt(t, repo, sessionID, testCheckpointID, "")
}

// Helper function to create a test metadata branch with a checkpoint and optional prompt
func createTestMetadataBranchWithPrompt(t *testing.T, repo *git.Repository, sessionID string, checkpointID id.CheckpointID, prompt string) {
	t.Helper()

	// Create empty tree for orphan commit
	entries := make(map[string]object.TreeEntry)

	checkpointPath := checkpointID.Path()
	sessionDir := "0" // First session (0-based indexing)

	// Create session-level metadata in 1/ subdirectory
	sessionMetadata := CheckpointInfo{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		CreatedAt:    time.Now(),
	}
	sessionMetadataJSON, err := json.Marshal(sessionMetadata)
	if err != nil {
		t.Fatalf("failed to marshal session metadata: %v", err)
	}
	sessionMetadataBlobHash, err := checkpoint.CreateBlobFromContent(repo, sessionMetadataJSON)
	if err != nil {
		t.Fatalf("failed to create session metadata blob: %v", err)
	}
	sessionMetadataPath := checkpointPath + "/" + sessionDir + "/" + paths.MetadataFileName
	entries[sessionMetadataPath] = object.TreeEntry{
		Name: sessionMetadataPath,
		Mode: filemode.Regular,
		Hash: sessionMetadataBlobHash,
	}

	// Add prompt.txt in session subdirectory if provided
	promptAbsPath := ""
	if prompt != "" {
		promptBlobHash, promptErr := checkpoint.CreateBlobFromContent(repo, []byte(prompt))
		if promptErr != nil {
			t.Fatalf("failed to create prompt blob: %v", promptErr)
		}
		fullPromptPath := checkpointPath + "/" + sessionDir + "/" + paths.PromptFileName
		entries[fullPromptPath] = object.TreeEntry{
			Name: fullPromptPath,
			Mode: filemode.Regular,
			Hash: promptBlobHash,
		}
		// Use absolute path with leading "/"
		promptAbsPath = "/" + fullPromptPath
	}

	// Create root CheckpointSummary with absolute paths
	rootSummary := checkpoint.CheckpointSummary{
		CheckpointID: checkpointID,
		Sessions: []checkpoint.SessionFilePaths{
			{
				Metadata: "/" + checkpointPath + "/" + sessionDir + "/" + paths.MetadataFileName,
				Prompt:   promptAbsPath,
			},
		},
	}
	summaryJSON, err := json.Marshal(rootSummary)
	if err != nil {
		t.Fatalf("failed to marshal root summary: %v", err)
	}
	summaryBlobHash, err := checkpoint.CreateBlobFromContent(repo, summaryJSON)
	if err != nil {
		t.Fatalf("failed to create summary blob: %v", err)
	}
	rootMetadataPath := checkpointPath + "/" + paths.MetadataFileName
	entries[rootMetadataPath] = object.TreeEntry{
		Name: rootMetadataPath,
		Mode: filemode.Regular,
		Hash: summaryBlobHash,
	}

	// Build tree
	treeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		t.Fatalf("failed to build tree: %v", err)
	}

	// Create orphan commit
	now := time.Now()
	sig := object.Signature{
		Name:  "Test",
		Email: "test@test.com",
		When:  now,
	}
	commit := &object.Commit{
		TreeHash:  treeHash,
		Author:    sig,
		Committer: sig,
		Message:   "Test checkpoint\n\n" + trailers.SessionTrailerKey + ": " + sessionID,
	}

	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("failed to encode commit: %v", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("failed to store commit: %v", err)
	}

	// Create branch reference
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to create branch: %v", err)
	}
}
