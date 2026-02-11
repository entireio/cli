//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/sessionid"
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

func TestPiHookRunner_SimulateUserPromptSubmit(t *testing.T) {
	t.Parallel()

	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewPiSession()
		env.WriteFile("newfile.txt", "content")

		if err := env.SimulatePiUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulatePiUserPromptSubmit failed: %v", err)
		}

		entireSessionID := sessionid.EntireSessionID(session.ID)
		statePath := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-prompt-"+entireSessionID+".json")
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			t.Fatalf("pre-prompt state file should exist at %s", statePath)
		}
	})
}

func TestPiHookRunner_SimulateSessionStartWithOutput(t *testing.T) {
	t.Parallel()

	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewPiSession()
		output := env.SimulatePiSessionStartWithOutput(session.ID)
		if output.Err != nil {
			t.Fatalf("SimulatePiSessionStartWithOutput failed: %v\nStderr: %s", output.Err, output.Stderr)
		}

		var resp struct {
			SystemMessage string `json:"systemMessage,omitempty"`
		}
		if err := json.Unmarshal(output.Stdout, &resp); err != nil {
			t.Fatalf("failed to parse session-start output JSON: %v\nStdout: %s", err, output.Stdout)
		}

		if resp.SystemMessage == "" {
			t.Fatalf("expected session-start systemMessage, got empty")
		}
		if !strings.Contains(resp.SystemMessage, "Powered by Entire") {
			t.Fatalf("expected systemMessage to contain 'Powered by Entire', got: %s", resp.SystemMessage)
		}
		if !strings.Contains(resp.SystemMessage, "linked to your next commit") {
			t.Fatalf("expected systemMessage to contain link text, got: %s", resp.SystemMessage)
		}
	})
}

func TestPiHookRunner_SimulateBeforeAndAfterTool(t *testing.T) {
	t.Parallel()

	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewPiSession()

		if err := env.SimulatePiBeforeTool(session.ID, "", "write", map[string]any{"path": "file.txt"}); err != nil {
			t.Fatalf("SimulatePiBeforeTool failed: %v", err)
		}

		if err := env.SimulatePiAfterTool(session.ID, "", "write", map[string]any{"path": "file.txt"}, map[string]any{"status": "ok"}); err != nil {
			t.Fatalf("SimulatePiAfterTool failed: %v", err)
		}
	})
}

func TestPiHookRunner_SimulateStop(t *testing.T) {
	t.Parallel()

	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewPiSession()

		if err := env.SimulatePiSessionStart(session.ID); err != nil {
			t.Fatalf("SimulatePiSessionStart failed: %v", err)
		}
		if err := env.SimulatePiUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulatePiUserPromptSubmit failed: %v", err)
		}

		env.WriteFile("created.txt", "created by pi")
		transcriptPath := session.CreatePiTranscript("Create a file", []FileChange{
			{Path: "created.txt", Content: "created by pi"},
		})

		if err := env.SimulatePiStop(session.ID, transcriptPath); err != nil {
			t.Fatalf("SimulatePiStop failed: %v", err)
		}

		points := env.GetRewindPoints()
		if len(points) == 0 {
			t.Fatal("expected at least one rewind point after Pi stop hook")
		}

		contextPath := filepath.Join(env.RepoDir, ".entire", "metadata", session.ID, "context.md")
		contextData, err := os.ReadFile(contextPath)
		if err != nil {
			t.Fatalf("expected Pi context file at %s: %v", contextPath, err)
		}

		contextText := string(contextData)
		if !strings.Contains(contextText, "# Session Context") ||
			!strings.Contains(contextText, "Session ID: "+session.ID) ||
			!strings.Contains(contextText, "## Prompts") ||
			!strings.Contains(contextText, "## Summary") {
			t.Fatalf("Pi context file missing expected sections: %s", contextText)
		}
	})
}

func TestPiHookRunner_SimulateStop_AlreadyCommitted(t *testing.T) {
	t.Parallel()

	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewPiSession()

		if err := env.SimulatePiUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulatePiUserPromptSubmit failed: %v", err)
		}

		env.WriteFile("created.txt", "created by pi")
		env.GitAdd("created.txt")
		env.GitCommit("User committed changes manually")

		transcriptPath := session.CreatePiTranscript("Create a file", []FileChange{
			{Path: "created.txt", Content: "created by pi"},
		})

		if err := env.SimulatePiStop(session.ID, transcriptPath); err != nil {
			t.Fatalf("SimulatePiStop should handle already-committed files gracefully, got: %v", err)
		}
	})
}

func TestPiHookRunner_SimulateSessionEnd(t *testing.T) {
	t.Parallel()

	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewPiSession()
		if err := env.SimulatePiSessionEnd(session.ID, ""); err != nil {
			t.Fatalf("SimulatePiSessionEnd failed: %v", err)
		}
	})
}

func TestPiHookRunner_SimulateSessionSwitch(t *testing.T) {
	t.Parallel()

	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		oldSession := env.NewPiSession()
		newSession := env.NewPiSession()

		if err := env.SimulatePiUserPromptSubmit(oldSession.ID); err != nil {
			t.Fatalf("SimulatePiUserPromptSubmit(old) failed: %v", err)
		}
		if err := env.SimulatePiUserPromptSubmit(newSession.ID); err != nil {
			t.Fatalf("SimulatePiUserPromptSubmit(new) failed: %v", err)
		}

		if err := env.SimulatePiSessionEnd(newSession.ID, ""); err != nil {
			t.Fatalf("SimulatePiSessionEnd(new) failed: %v", err)
		}

		preSwitchNewState, err := env.GetSessionState(newSession.ID)
		if err != nil {
			t.Fatalf("GetSessionState(new) before switch failed: %v", err)
		}
		if preSwitchNewState == nil {
			t.Fatalf("expected new session state before switch")
		}
		if preSwitchNewState.Phase != session.PhaseEnded {
			t.Fatalf("new session phase before switch = %q, want %q", preSwitchNewState.Phase, session.PhaseEnded)
		}

		if err := env.SimulatePiSessionSwitch(oldSession.ID, newSession.ID, ""); err != nil {
			t.Fatalf("SimulatePiSessionSwitch failed: %v", err)
		}

		oldState, err := env.GetSessionState(oldSession.ID)
		if err != nil {
			t.Fatalf("GetSessionState(old) after switch failed: %v", err)
		}
		if oldState != nil && oldState.Phase != session.PhaseEnded {
			t.Fatalf("old session phase after switch = %q, want %q", oldState.Phase, session.PhaseEnded)
		}

		newState, err := env.GetSessionState(newSession.ID)
		if err != nil {
			t.Fatalf("GetSessionState(new) after switch failed: %v", err)
		}
		if newState != nil {
			if newState.Phase != session.PhaseIdle {
				t.Fatalf("new session phase after switch = %q, want %q", newState.Phase, session.PhaseIdle)
			}
			if newState.EndedAt != nil {
				t.Fatalf("new session EndedAt should be cleared on session-start re-entry")
			}
		}
	})
}
