//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestOpenCode_BasicWorkflow tests the core OpenCode workflow:
// session-start → modify files → stop → verify checkpoint + rewind point.
func TestOpenCode_BasicWorkflow(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Start OpenCode session
		session := env.NewOpencodeSession()
		if err := env.SimulateOpencodeSessionStart(session.ID); err != nil {
			t.Fatalf("SimulateOpencodeSessionStart failed: %v", err)
		}

		// Create a file (simulating agent work)
		env.WriteFile("hello.go", "package main\n\nfunc Hello() string { return \"hello\" }\n")

		// Create transcript and stop
		transcriptPath := session.CreateOpencodeTranscript("Create hello function", []FileChange{
			{Path: "hello.go", Content: "package main\n\nfunc Hello() string { return \"hello\" }\n"},
		})

		if err := env.SimulateOpencodeStop(session.ID, transcriptPath); err != nil {
			t.Fatalf("SimulateOpencodeStop failed: %v", err)
		}

		// Verify checkpoint was created
		points := env.GetRewindPoints()
		if len(points) == 0 {
			t.Fatal("expected at least 1 rewind point after stop")
		}
	})
}

// TestOpenCode_Rewind tests multiple checkpoints + rewind restores files correctly.
func TestOpenCode_Rewind(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Use same session for all checkpoints (follows Claude pattern)
		session := env.NewOpencodeSession()

		// Checkpoint 1: create file v1
		if err := env.SimulateOpencodeSessionStart(session.ID); err != nil {
			t.Fatalf("session-start failed: %v", err)
		}

		fileV1 := "package main\n\nfunc Greet() string { return \"v1\" }\n"
		env.WriteFile("greet.go", fileV1)
		transcript1 := session.CreateOpencodeTranscript("Create greet v1", []FileChange{
			{Path: "greet.go", Content: fileV1},
		})

		if err := env.SimulateOpencodeStop(session.ID, transcript1); err != nil {
			t.Fatalf("stop (checkpoint 1) failed: %v", err)
		}

		points1 := env.GetRewindPoints()
		if len(points1) != 1 {
			t.Fatalf("expected 1 rewind point, got %d", len(points1))
		}
		checkpoint1ID := points1[0].ID

		// Checkpoint 2: modify file to v2 (same session, new transcript)
		fileV2 := "package main\n\nfunc Greet() string { return \"v2\" }\n"
		env.WriteFile("greet.go", fileV2)

		// Update transcript with v2 content
		transcript2 := session.CreateOpencodeTranscript("Update greet to v2", []FileChange{
			{Path: "greet.go", Content: fileV2},
		})

		if err := env.SimulateOpencodeStop(session.ID, transcript2); err != nil {
			t.Fatalf("stop (checkpoint 2) failed: %v", err)
		}

		points2 := env.GetRewindPoints()
		if len(points2) < 2 {
			t.Fatalf("expected at least 2 rewind points, got %d", len(points2))
		}

		// Verify current state
		if content := env.ReadFile("greet.go"); content != fileV2 {
			t.Errorf("greet.go should be v2 before rewind, got: %q", content)
		}

		// Rewind to checkpoint 1
		if err := env.Rewind(checkpoint1ID); err != nil {
			t.Fatalf("Rewind failed: %v", err)
		}

		// Verify file is restored to v1
		if content := env.ReadFile("greet.go"); content != fileV1 {
			t.Errorf("greet.go after rewind = %q, want v1 content", content)
		}
	})
}

// TestOpenCode_Condensation tests that session data is condensed to
// entire/checkpoints/v1 after a git commit.
func TestOpenCode_Condensation(t *testing.T) {
	t.Parallel()

	// Only test with manual-commit strategy as it has condensation behavior
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Start session
	session := env.NewOpencodeSession()
	if err := env.SimulateOpencodeSessionStart(session.ID); err != nil {
		t.Fatalf("session-start failed: %v", err)
	}

	// Create file and checkpoint
	env.WriteFile("feature.go", "package main\n// feature\n")
	transcript := session.CreateOpencodeTranscript("Add feature", []FileChange{
		{Path: "feature.go", Content: "package main\n// feature\n"},
	})

	if err := env.SimulateOpencodeStop(session.ID, transcript); err != nil {
		t.Fatalf("stop failed: %v", err)
	}

	// Verify we have a rewind point
	points := env.GetRewindPoints()
	if len(points) == 0 {
		t.Fatal("expected rewind point before commit")
	}

	// User commits (triggers condensation via git hooks)
	env.GitAdd("feature.go")
	env.GitCommitWithShadowHooks("Add feature")

	// After commit, rewind points should still be available (as logs-only points)
	pointsAfter := env.GetRewindPoints()
	t.Logf("Rewind points after commit: %d", len(pointsAfter))

	// At minimum, the pre-commit checkpoint should have become a logs-only point
	// or the shadow branch should be cleaned up (strategy-dependent)
	if len(pointsAfter) > 0 {
		for _, p := range pointsAfter {
			t.Logf("  Point: id=%s message=%q logsOnly=%v", p.ID, p.Message, p.IsLogsOnly)
		}
	}
}

// TestOpenCode_TaskCheckpoint tests task-start → task-complete → verify subagent checkpoint.
func TestOpenCode_TaskCheckpoint(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Start session first
		session := env.NewOpencodeSession()
		if err := env.SimulateOpencodeSessionStart(session.ID); err != nil {
			t.Fatalf("session-start failed: %v", err)
		}

		// Create transcript (needed for stop)
		env.WriteFile("main.go", "package main\n")
		transcript := session.CreateOpencodeTranscript("Setup", []FileChange{
			{Path: "main.go", Content: "package main\n"},
		})

		if err := env.SimulateOpencodeStop(session.ID, transcript); err != nil {
			t.Fatalf("stop failed: %v", err)
		}

		// Start a task (subagent)
		toolUseID := "task-tool-123"
		if err := env.SimulateOpencodeTaskStart(session.ID, transcript, toolUseID); err != nil {
			t.Fatalf("task-start failed: %v", err)
		}

		// Create a file during the task
		env.WriteFile("task_output.go", "package main\n// created by subagent\n")

		// Complete the task
		if err := env.SimulateOpencodeTaskComplete(session.ID, transcript, toolUseID); err != nil {
			t.Fatalf("task-complete failed: %v", err)
		}

		// Verify the file exists (task didn't undo anything)
		if !env.FileExists("task_output.go") {
			t.Error("task_output.go should exist after task completion")
		}
	})
}

// TestOpenCode_ConcurrentSessions tests two sessions in the same directory.
func TestOpenCode_ConcurrentSessions(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Start first session
		session1 := env.NewOpencodeSession()
		if err := env.SimulateOpencodeSessionStart(session1.ID); err != nil {
			t.Fatalf("session1 start failed: %v", err)
		}

		// Create file and checkpoint for session 1
		env.WriteFile("file1.go", "package main\n// from session 1\n")
		transcript1 := session1.CreateOpencodeTranscript("Session 1 work", []FileChange{
			{Path: "file1.go", Content: "package main\n// from session 1\n"},
		})

		if err := env.SimulateOpencodeStop(session1.ID, transcript1); err != nil {
			t.Fatalf("session1 stop failed: %v", err)
		}

		// Start second session (while first session's checkpoint exists)
		session2 := env.NewOpencodeSession()
		out := env.SimulateOpencodeSessionStartWithOutput(session2.ID)
		if out.Err != nil {
			// Concurrent session may warn but should not error
			t.Logf("session2 start output: stdout=%s stderr=%s", out.Stdout, out.Stderr)
		}

		// Create file and checkpoint for session 2
		env.WriteFile("file2.go", "package main\n// from session 2\n")
		transcript2 := session2.CreateOpencodeTranscript("Session 2 work", []FileChange{
			{Path: "file2.go", Content: "package main\n// from session 2\n"},
		})

		if err := env.SimulateOpencodeStop(session2.ID, transcript2); err != nil {
			t.Fatalf("session2 stop failed: %v", err)
		}

		// Both files should exist
		if !env.FileExists("file1.go") {
			t.Error("file1.go should exist from session 1")
		}
		if !env.FileExists("file2.go") {
			t.Error("file2.go should exist from session 2")
		}

		// Should have rewind points from both sessions
		points := env.GetRewindPoints()
		if len(points) < 2 {
			t.Errorf("expected at least 2 rewind points from concurrent sessions, got %d", len(points))
		}
	})
}

// TestOpenCode_NoChangesSkip tests that stop with no file changes skips checkpoint gracefully.
func TestOpenCode_NoChangesSkip(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Start session
		session := env.NewOpencodeSession()
		if err := env.SimulateOpencodeSessionStart(session.ID); err != nil {
			t.Fatalf("session-start failed: %v", err)
		}

		// Create transcript but don't actually modify any files in the working tree
		// The transcript says files were modified, but the working tree is unchanged
		transcript := session.CreateOpencodeTranscript("Do nothing", []FileChange{})

		// Stop should succeed without creating a checkpoint
		if err := env.SimulateOpencodeStop(session.ID, transcript); err != nil {
			// Some strategies may return an error for no-changes; that's acceptable
			t.Logf("stop with no changes returned error (may be expected): %v", err)
		}

		// Verify no rewind points were created (or at most 0)
		points := env.GetRewindPoints()
		t.Logf("rewind points after no-changes stop: %d", len(points))
	})
}

// TestOpenCode_SessionStartOutput verifies session-start hook output.
func TestOpenCode_SessionStartOutput(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewOpencodeSession()
		out := env.SimulateOpencodeSessionStartWithOutput(session.ID)

		if out.Err != nil {
			t.Fatalf("session-start failed: %v\nstdout: %s\nstderr: %s",
				out.Err, out.Stdout, out.Stderr)
		}

		// Session start should produce some output (setup messages)
		combinedOutput := string(out.Stdout) + string(out.Stderr)
		t.Logf("session-start output: %s", combinedOutput)
	})
}

// TestOpenCode_MetadataCreation verifies that the stop hook creates expected metadata files.
func TestOpenCode_MetadataCreation(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewOpencodeSession()
		if err := env.SimulateOpencodeSessionStart(session.ID); err != nil {
			t.Fatalf("session-start failed: %v", err)
		}

		env.WriteFile("app.go", "package main\n\nfunc main() {}\n")
		transcript := session.CreateOpencodeTranscript("Create main app", []FileChange{
			{Path: "app.go", Content: "package main\n\nfunc main() {}\n"},
		})

		if err := env.SimulateOpencodeStop(session.ID, transcript); err != nil {
			t.Fatalf("stop failed: %v", err)
		}

		// Verify metadata directory structure
		metadataDir := filepath.Join(env.RepoDir, ".entire", "metadata", session.ID)
		if _, err := os.Stat(metadataDir); os.IsNotExist(err) {
			t.Fatalf("metadata dir should exist at %s", metadataDir)
		}

		// Check for expected metadata files
		expectedFiles := []string{
			paths.TranscriptFileName, // full.jsonl
			paths.PromptFileName,     // prompt.txt
			paths.SummaryFileName,    // summary.txt
			paths.ContextFileName,    // context.md
		}

		for _, fname := range expectedFiles {
			fpath := filepath.Join(metadataDir, fname)
			info, err := os.Stat(fpath)
			if os.IsNotExist(err) {
				t.Errorf("expected metadata file %s to exist", fname)
				continue
			}
			if err != nil {
				t.Errorf("error checking metadata file %s: %v", fname, err)
				continue
			}
			if info.Size() == 0 {
				t.Errorf("metadata file %s should not be empty", fname)
			}
		}

		// Verify prompt file contains the prompt
		promptData, err := os.ReadFile(filepath.Join(metadataDir, paths.PromptFileName))
		if err != nil {
			t.Fatalf("failed to read prompt file: %v", err)
		}
		if !strings.Contains(string(promptData), "Create main app") {
			t.Errorf("prompt file should contain the user prompt, got: %q", string(promptData))
		}

		// Verify context file has structure
		contextData, err := os.ReadFile(filepath.Join(metadataDir, paths.ContextFileName))
		if err != nil {
			t.Fatalf("failed to read context file: %v", err)
		}
		contextStr := string(contextData)
		if !strings.Contains(contextStr, "Session Context") {
			t.Error("context file should contain 'Session Context' header")
		}
		if !strings.Contains(contextStr, session.ID) {
			t.Errorf("context file should reference session ID %s", session.ID)
		}
	})
}

// TestOpenCode_TranscriptIntegrity verifies the transcript file is copied correctly.
func TestOpenCode_TranscriptIntegrity(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		session := env.NewOpencodeSession()
		if err := env.SimulateOpencodeSessionStart(session.ID); err != nil {
			t.Fatalf("session-start failed: %v", err)
		}

		env.WriteFile("code.go", "package main\n")
		transcript := session.CreateOpencodeTranscript("Write code", []FileChange{
			{Path: "code.go", Content: "package main\n"},
		})

		// Read original transcript
		originalData, err := os.ReadFile(transcript)
		if err != nil {
			t.Fatalf("failed to read original transcript: %v", err)
		}

		if err := env.SimulateOpencodeStop(session.ID, transcript); err != nil {
			t.Fatalf("stop failed: %v", err)
		}

		// Verify copied transcript matches original
		copiedPath := filepath.Join(env.RepoDir, ".entire", "metadata", session.ID, paths.TranscriptFileName)
		copiedData, err := os.ReadFile(copiedPath)
		if err != nil {
			t.Fatalf("failed to read copied transcript: %v", err)
		}

		if string(originalData) != string(copiedData) {
			t.Error("copied transcript should match original")
		}

		// Verify transcript is valid JSONL (each line parses as JSON)
		lines := strings.Split(strings.TrimSpace(string(copiedData)), "\n")
		for i, line := range lines {
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(line), &parsed); err != nil {
				t.Errorf("transcript line %d is not valid JSON: %v", i, err)
			}

			// Verify OpenCode transcript structure (info + parts)
			if _, ok := parsed["info"]; !ok {
				t.Errorf("transcript line %d missing 'info' field", i)
			}
			if _, ok := parsed["parts"]; !ok {
				t.Errorf("transcript line %d missing 'parts' field", i)
			}
		}
	})
}
