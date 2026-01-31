//go:build integration

package integration

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// TestMultiSessionWarning_Disabled tests that warning is disabled by default.
// When a second session starts with existing uncommitted work, no warning should be shown.
func TestMultiSessionWarning_Disabled(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Session 1: Create some uncommitted work
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	env.WriteFile("src/file1.ts", "content v1")
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "src/file1.ts", Content: "content v1"}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Session 2: Start new session (warning should not be shown by default)
	session2 := env.NewSession()
	output := env.SimulateUserPromptSubmitWithOutput(session2.ID)

	// Verify no error (warning disabled)
	if output.Err != nil {
		t.Errorf("Expected no error with warning disabled, got: %v", output.Err)
	}

	// Verify no warning message in output
	outputStr := string(output.Stdout) + string(output.Stderr)
	if len(outputStr) > 0 {
		t.Logf("Output: %s", outputStr)
	}
	// Note: We can't easily verify the absence of a warning without the feature implemented,
	// but at least we verify the hook succeeds
}

// TestMultiSessionWarning_Enabled tests that warning is shown when enabled.
// When enabled and a second session starts with existing uncommitted work, a warning should be shown.
func TestMultiSessionWarning_Enabled(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Enable multi-session warning in settings
	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	settingsData := env.ReadFileAbsolute(settingsPath)

	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(settingsData), &settings); err != nil {
		t.Fatalf("Failed to parse settings: %v", err)
	}

	settings["enable_multisession_warning"] = true

	updatedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal settings: %v", err)
	}
	updatedSettings = append(updatedSettings, '\n')

	env.WriteFile(".entire/"+paths.SettingsFileName, string(updatedSettings))

	// Session 1: Create some uncommitted work
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	env.WriteFile("src/file1.ts", "content v1")
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "src/file1.ts", Content: "content v1"}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify session state exists
	state, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("Failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("Expected session state to exist after checkpoint")
	}
	if state.CheckpointCount != 1 {
		t.Errorf("Expected checkpoint count 1, got %d", state.CheckpointCount)
	}

	// Session 2: Start new session (warning should be shown)
	// Note: This test will need to be updated once the feature is implemented
	// to actually respond to the interactive warning prompt
	session2 := env.NewSession()

	// For now, we just verify the hook runs
	// Once implemented, this would need to use RunCommandInteractive to respond to the prompt
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		// Once warning is implemented, we might expect this to fail if user cancels
		t.Logf("SimulateUserPromptSubmit (session 2): %v", err)
	}

	// TODO: Once feature is implemented, add test with RunCommandInteractive:
	// 1. Test user cancels -> session should not start
	// 2. Test user continues -> session should start and merge
}

// TestMultiSessionWarning_NoWarningWhenCommitted tests that warning is not shown
// when previous session has been committed (no uncommitted work).
func TestMultiSessionWarning_NoWarningWhenCommitted(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Enable multi-session warning
	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	settingsData := env.ReadFileAbsolute(settingsPath)

	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(settingsData), &settings); err != nil {
		t.Fatalf("Failed to parse settings: %v", err)
	}

	settings["enable_multisession_warning"] = true

	updatedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal settings: %v", err)
	}
	updatedSettings = append(updatedSettings, '\n')

	env.WriteFile(".entire/"+paths.SettingsFileName, string(updatedSettings))

	// Session 1: Create work and commit it
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	env.WriteFile("src/file1.ts", "content v1")
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "src/file1.ts", Content: "content v1"}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// User commits
	env.GitCommitWithShadowHooks("Add file1", "src/file1.ts")

	// Session 2: Start new session (warning should NOT be shown because previous work was committed)
	session2 := env.NewSession()
	output := env.SimulateUserPromptSubmitWithOutput(session2.ID)

	// Verify no error
	if output.Err != nil {
		t.Errorf("Expected no error when previous session was committed, got: %v", output.Err)
	}
}

// TestMultiSessionWarning_DifferentBaseCommit tests that warning is not shown
// when sessions are on different base commits (e.g., user made a commit between sessions).
func TestMultiSessionWarning_DifferentBaseCommit(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Enable multi-session warning
	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	settingsData := env.ReadFileAbsolute(settingsPath)

	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(settingsData), &settings); err != nil {
		t.Fatalf("Failed to parse settings: %v", err)
	}

	settings["enable_multisession_warning"] = true

	updatedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal settings: %v", err)
	}
	updatedSettings = append(updatedSettings, '\n')

	env.WriteFile(".entire/"+paths.SettingsFileName, string(updatedSettings))

	// Session 1: Create some uncommitted work on base commit 1
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	env.WriteFile("src/file1.ts", "content v1")
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "src/file1.ts", Content: "content v1"}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// User makes a separate commit (not using shadow hooks, just a regular commit)
	env.WriteFile("unrelated.txt", "unrelated work")
	env.GitAdd("unrelated.txt")
	env.GitCommit("Unrelated commit")

	// Now we're on a new base commit
	// Session 2: Start new session (warning should NOT be shown because base commit changed)
	session2 := env.NewSession()
	output := env.SimulateUserPromptSubmitWithOutput(session2.ID)

	// Verify no error (different base commit)
	if output.Err != nil {
		t.Errorf("Expected no error when on different base commit, got: %v", output.Err)
	}
}
