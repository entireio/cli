//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"entire.io/cli/cmd/entire/cli/sessionid"
)

func TestHookRunner_SimulateUserPromptSubmit(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create an untracked file to capture
		env.WriteFile("newfile.txt", "content")

		modelSessionID := "test-session-1"
		err := env.SimulateUserPromptSubmit(modelSessionID)
		if err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// Verify pre-prompt state was captured (uses entire session ID with date prefix)
		entireSessionID := sessionid.EntireSessionID(modelSessionID)
		statePath := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-prompt-"+entireSessionID+".json")
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			t.Error("pre-prompt state file should exist")
		}
	})
}

func TestHookRunner_SimulateStop(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a session
		session := env.NewSession()

		// Simulate user prompt submit first
		err := env.SimulateUserPromptSubmit(session.ID)
		if err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// Create a file (as if Claude Code wrote it)
		env.WriteFile("created.txt", "created by claude")

		// Create transcript
		session.CreateTranscript("Create a file", []FileChange{
			{Path: "created.txt", Content: "created by claude"},
		})

		// Simulate stop
		err = env.SimulateStop(session.ID, session.TranscriptPath)
		if err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		// Verify a commit was created (check git log) - skip for manual-commit strategy
		// manual-commit strategy doesn't create commits on the main branch
		if strategyName != "manual-commit" {
			hash := env.GetHeadHash()
			if len(hash) != 40 {
				t.Errorf("expected valid commit hash, got %s", hash)
			}
		}
	})
}

// TestHookRunner_SimulateStop_AlreadyCommitted tests that the stop hook handles
// the case where files were modified during the session but already committed
// by the user before the hook runs. This should not fail.
func TestHookRunner_SimulateStop_AlreadyCommitted(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a session
		session := env.NewSession()

		// Simulate user prompt submit first (captures pre-prompt state)
		err := env.SimulateUserPromptSubmit(session.ID)
		if err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// Create a file (as if Claude Code wrote it)
		env.WriteFile("created.txt", "created by claude")

		// USER COMMITS THE FILE BEFORE HOOK RUNS
		// This simulates the scenario where user runs `git commit` manually
		// or the changes are committed via another mechanism
		env.GitAdd("created.txt")
		env.GitCommit("User committed changes manually")

		// Create transcript (still references the file as modified during session)
		session.CreateTranscript("Create a file", []FileChange{
			{Path: "created.txt", Content: "created by claude"},
		})

		// Simulate stop - this should NOT fail even though file is already committed
		err = env.SimulateStop(session.ID, session.TranscriptPath)
		if err != nil {
			t.Fatalf("SimulateStop should handle already-committed files gracefully, got error: %v", err)
		}
	})
}

func TestSession_CreateTranscript(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewSession()
		transcriptPath := session.CreateTranscript("Test prompt", []FileChange{
			{Path: "file1.txt", Content: "content1"},
			{Path: "file2.txt", Content: "content2"},
		})

		// Verify transcript file exists
		if _, err := os.Stat(transcriptPath); os.IsNotExist(err) {
			t.Error("transcript file should exist")
		}

		// Verify session ID format
		if session.ID != "test-session-1" {
			t.Errorf("session ID = %s, want test-session-1", session.ID)
		}
	})
}
