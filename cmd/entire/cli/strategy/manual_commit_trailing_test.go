package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestTrailingTranscript_NoLastCheckpointID_Skips(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		SessionID: "test-session",
		StepCount: 0,
		Phase:     session.PhaseIdle,
		AgentType: agent.AgentTypeClaudeCode,
		// LastCheckpointID intentionally empty
	}

	s := &ManualCommitStrategy{}
	err := s.handleTrailingTranscript(state)
	if err != nil {
		t.Fatalf("handleTrailingTranscript() error = %v", err)
	}
	// Should be a no-op - nothing to verify except no error
}

func TestTrailingTranscript_NewFilesTouched_Skips(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		SessionID:        "test-session",
		StepCount:        1, // SaveChanges created a checkpoint
		LastCheckpointID: id.MustCheckpointID("a1b2c3d4e5f6"),
		Phase:            session.PhaseIdle,
		AgentType:        agent.AgentTypeClaudeCode,
	}

	s := &ManualCommitStrategy{}
	err := s.handleTrailingTranscript(state)
	if err != nil {
		t.Fatalf("handleTrailingTranscript() error = %v", err)
	}
	// Should be a no-op since StepCount > 0 (new files touched)
}

func TestTrailingTranscript_NoNewTranscript_Skips(t *testing.T) {
	t.Parallel()

	// Create a temporary transcript file with known content
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	transcriptContent := `{"type":"human","message":{"content":"hello"}}
{"type":"assistant","message":{"content":"world"}}
`
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o600); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	state := &SessionState{
		SessionID:                 "test-session",
		StepCount:                 0,
		LastCheckpointID:          id.MustCheckpointID("a1b2c3d4e5f6"),
		Phase:                     session.PhaseIdle,
		AgentType:                 agent.AgentTypeClaudeCode,
		TranscriptPath:            transcriptPath,
		CheckpointTranscriptStart: 2, // Already condensed all 2 lines
	}

	s := &ManualCommitStrategy{}
	err := s.handleTrailingTranscript(state)
	if err != nil {
		t.Fatalf("handleTrailingTranscript() error = %v", err)
	}
	// Should be a no-op since transcript hasn't grown
}

func TestTrailingTranscript_NoNewFiles_AppendsToCheckpoint(t *testing.T) {
	t.Parallel()

	// Create a git repo with a committed checkpoint
	tmpDir := t.TempDir()
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	readmeFile := filepath.Join(tmpDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	store := checkpoint.NewGitStore(repo)
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	sessionID := "test-session-trailing"

	// Write initial checkpoint
	initialTranscript := `{"type":"human","message":{"content":"initial prompt"}}
{"type":"assistant","message":{"content":"initial response"}}`

	err = store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   []byte(initialTranscript),
		Prompts:      []string{"initial prompt"},
		Context:      []byte("# Initial Context"),
		FilesTouched: []string{"file1.go"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Create transcript file with trailing content (3 lines total, 2 condensed)
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	fullTranscript := `{"type":"human","message":{"content":"initial prompt"}}
{"type":"assistant","message":{"content":"initial response"}}
{"type":"human","message":{"content":"trailing question"}}
{"type":"assistant","message":{"content":"trailing answer"}}
`
	if err := os.WriteFile(transcriptPath, []byte(fullTranscript), 0o600); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	state := &SessionState{
		SessionID:                 sessionID,
		StepCount:                 0,
		LastCheckpointID:          checkpointID,
		Phase:                     session.PhaseIdle,
		AgentType:                 agent.AgentTypeClaudeCode,
		TranscriptPath:            transcriptPath,
		CheckpointTranscriptStart: 2, // 2 lines already condensed
	}

	// Create strategy with injected store.
	// Pre-trigger the sync.Once by calling Do before it runs OpenRepository.
	s := &ManualCommitStrategy{}
	s.checkpointStoreOnce.Do(func() {
		s.checkpointStore = store
	})

	err = s.handleTrailingTranscript(state)
	if err != nil {
		t.Fatalf("handleTrailingTranscript() error = %v", err)
	}

	// Verify trailing content was appended
	content, err := store.ReadSessionContentByID(context.Background(), checkpointID, sessionID)
	if err != nil {
		t.Fatalf("ReadSessionContentByID() error = %v", err)
	}

	// Transcript should contain the trailing lines appended
	if len(content.Transcript) <= len([]byte(initialTranscript)) {
		t.Errorf("transcript length (%d) should be greater than initial (%d)",
			len(content.Transcript), len(initialTranscript))
	}

	// Prompts should include trailing prompts
	if content.Prompts == "initial prompt" {
		t.Error("prompts should include trailing prompts, but only has initial")
	}

	// CheckpointTranscriptStart should be updated to include trailing lines
	if state.CheckpointTranscriptStart <= 2 {
		t.Errorf("CheckpointTranscriptStart = %d, want > 2 (should be updated to include trailing lines)",
			state.CheckpointTranscriptStart)
	}
}
