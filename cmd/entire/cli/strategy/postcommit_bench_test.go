package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// BenchmarkPostCommit measures the full PostCommit hook execution time.
// This is the baseline before introducing a postCommitCache.
//
// Setup: 1 active session with a shadow branch checkpoint, then a commit
// with the Entire-Checkpoint trailer. PostCommit reads HEAD, finds the session,
// runs condensation (filesOverlapWithContent, CondenseSession, carry-forward).
func BenchmarkPostCommit(b *testing.B) {
	b.Run("SingleSession_Active", benchPostCommitSingleSession(session.PhaseActive))
	b.Run("SingleSession_Idle", benchPostCommitSingleSession(session.PhaseIdle))
	b.Run("MultipleSessions_2", benchPostCommitMultipleSessions(2))
	b.Run("MultipleSessions_3", benchPostCommitMultipleSessions(3))
}

func benchPostCommitSingleSession(phase session.Phase) func(*testing.B) {
	return func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			dir := benchSetupPostCommitRepo(b, phase, 1)
			b.Chdir(dir)
			paths.ClearWorktreeRootCache()
			b.StartTimer()

			s := &ManualCommitStrategy{}
			if err := s.PostCommit(context.Background()); err != nil {
				b.Fatalf("PostCommit: %v", err)
			}
		}
	}
}

func benchPostCommitMultipleSessions(sessionCount int) func(*testing.B) {
	return func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			dir := benchSetupPostCommitRepo(b, session.PhaseActive, sessionCount)
			b.Chdir(dir)
			paths.ClearWorktreeRootCache()
			b.StartTimer()

			s := &ManualCommitStrategy{}
			if err := s.PostCommit(context.Background()); err != nil {
				b.Fatalf("PostCommit: %v", err)
			}
		}
	}
}

// benchSetupPostCommitRepo creates a git repo with N sessions that have shadow branch
// checkpoints, then creates a commit with the Entire-Checkpoint trailer.
// Returns the repo directory path, ready for PostCommit() to run.
func benchSetupPostCommitRepo(b *testing.B, phase session.Phase, sessionCount int) string {
	b.Helper()

	dir := b.TempDir()
	// Resolve symlinks (macOS /var -> /private/var)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		b.Fatalf("git init: %v", err)
	}

	// Configure git user
	cfg, err := repo.Config()
	if err != nil {
		b.Fatalf("config: %v", err)
	}
	cfg.User.Name = "Bench User"
	cfg.User.Email = "bench@example.com"
	if err := repo.SetConfig(cfg); err != nil {
		b.Fatalf("set config: %v", err)
	}

	// Create initial files and commit
	wt, err := repo.Worktree()
	if err != nil {
		b.Fatalf("worktree: %v", err)
	}

	// Create multiple files to make file overlap checks realistic
	for i := range 5 {
		name := fmt.Sprintf("src/file_%d.go", i)
		abs := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			b.Fatalf("mkdir: %v", err)
		}
		content := fmt.Sprintf("package main\nfunc f%d() {}\n", i)
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			b.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			b.Fatalf("add: %v", err)
		}
	}

	if _, err := wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Bench", Email: "bench@test.com", When: time.Now()},
	}); err != nil {
		b.Fatalf("commit: %v", err)
	}

	s := &ManualCommitStrategy{}

	// Chdir to repo dir for the entire setup (SaveStep, loadSessionState, etc.
	// all depend on paths.WorktreeRoot() which uses cwd). b.Chdir restores
	// the original directory when the benchmark function returns.
	b.Chdir(dir)
	paths.ClearWorktreeRootCache()

	// Set up each session with a shadow branch checkpoint
	modifiedFiles := []string{"src/file_0.go", "src/file_1.go"}
	for i := range sessionCount {
		sessionID := fmt.Sprintf("bench-session-%d", i)

		// Modify files with agent content
		for _, f := range modifiedFiles {
			abs := filepath.Join(dir, f)
			content := fmt.Sprintf("package main\n// modified by agent session %d\nfunc f() {}\n", i)
			if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
				b.Fatalf("write: %v", err)
			}
		}

		// Create metadata directory with transcript
		metadataDir := ".entire/metadata/" + sessionID
		metadataDirAbs := filepath.Join(dir, metadataDir)
		if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
			b.Fatalf("mkdir: %v", err)
		}
		transcript := `{"type":"human","message":{"content":"implement feature"}}
{"type":"assistant","message":{"content":"I'll implement that for you."}}
`
		if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
			b.Fatalf("write transcript: %v", err)
		}

		paths.ClearWorktreeRootCache()

		if err := s.SaveStep(context.Background(), StepContext{
			SessionID:      sessionID,
			ModifiedFiles:  modifiedFiles,
			NewFiles:       []string{},
			DeletedFiles:   []string{},
			MetadataDir:    metadataDir,
			MetadataDirAbs: metadataDirAbs,
			CommitMessage:  "Checkpoint 1",
			AuthorName:     "Bench",
			AuthorEmail:    "bench@test.com",
		}); err != nil {
			b.Fatalf("SaveStep: %v", err)
		}

		// Set the session phase
		state, err := s.loadSessionState(context.Background(), sessionID)
		if err != nil {
			b.Fatalf("load state: %v", err)
		}
		state.Phase = phase
		state.FilesTouched = modifiedFiles
		if err := s.saveSessionState(context.Background(), state); err != nil {
			b.Fatalf("save state: %v", err)
		}
	}

	// Create the user commit with checkpoint trailer (the commit PostCommit will process)
	cpID, err := id.Generate()
	if err != nil {
		b.Fatalf("generate ID: %v", err)
	}

	// Modify a file and commit with trailer
	testFile := filepath.Join(dir, "src/file_0.go")
	if err := os.WriteFile(testFile, []byte("package main\n// modified by agent session 0\nfunc f() {}\n"), 0o644); err != nil {
		b.Fatalf("write: %v", err)
	}
	if _, err := wt.Add("src/file_0.go"); err != nil {
		b.Fatalf("add: %v", err)
	}

	commitMsg := fmt.Sprintf("implement feature\n\n%s: %s\n", trailers.CheckpointTrailerKey, cpID)
	if _, err := wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Bench", Email: "bench@test.com", When: time.Now()},
	}); err != nil {
		b.Fatalf("commit: %v", err)
	}

	return dir
}
