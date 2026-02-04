//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewind_FullWorkflow tests the complete rewind workflow for all strategies.
// From the user's perspective, all strategies should behave identically:
// - Sessions create rewind points
// - Rewind restores file contents
// - Transcript is available for session resumption
//
// All strategies use a single session with multiple checkpoints, simulating
// one continuous Claude session with multiple prompts.
func TestRewind_FullWorkflow(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Use same session for all checkpoints (works for all strategies)
		session := env.NewSession()

		// Checkpoint 1: Add ruby script
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		rubyV1 := "puts rand(100)"
		env.WriteFile("random.rb", rubyV1)

		session.CreateTranscript(
			"Add a ruby script that returns a random number",
			[]FileChange{{Path: "random.rb", Content: rubyV1}},
		)
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop checkpoint1 failed: %v", err)
		}

		// Checkpoint 2: Change to red
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		rubyV2 := `puts "\e[31m#{rand(100)}\e[0m"`
		env.WriteFile("random.rb", rubyV2)

		session.CreateTranscript(
			"Change the output to use red color",
			[]FileChange{{Path: "random.rb", Content: rubyV2}},
		)
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop checkpoint2 failed: %v", err)
		}

		// Checkpoint 3: Change to green
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		rubyV3 := `puts "\e[32m#{rand(100)}\e[0m"`
		env.WriteFile("random.rb", rubyV3)

		session.CreateTranscript(
			"Change the output to use green color",
			[]FileChange{{Path: "random.rb", Content: rubyV3}},
		)
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop checkpoint3 failed: %v", err)
		}

		// Verify current state is green version
		content := env.ReadFile("random.rb")
		if content != rubyV3 {
			t.Errorf("before rewind: got %q, want %q", content, rubyV3)
		}

		// Get rewind points - should have 3
		points := env.GetRewindPoints()
		if len(points) != 3 {
			t.Fatalf("expected 3 rewind points, got %d", len(points))
		}

		// Points are ordered newest first, so checkpoint 1 is last
		checkpoint1Point := points[2]
		if checkpoint1Point.Message == "" {
			t.Error("rewind point should have a message")
		}

		// Execute rewind to checkpoint 1
		if err := env.Rewind(checkpoint1Point.ID); err != nil {
			t.Fatalf("Rewind failed: %v", err)
		}

		// CORE ASSERTION: File contents restored
		content = env.ReadFile("random.rb")
		if content != rubyV1 {
			t.Errorf("after rewind: got %q, want %q", content, rubyV1)
		}

		// CORE ASSERTION: Transcript available for resumption
		transcriptPath := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
		if _, err := os.Stat(transcriptPath); os.IsNotExist(err) {
			t.Errorf("transcript should be copied for claude -r, expected at %s", transcriptPath)
		}
	})
}

// TestTaskCheckpoint_RewindWorkflow tests rewind to task checkpoint (subagent completion).
// This verifies that:
// - Task checkpoints are created via PostToolUse[Task] hook
// - Task checkpoints appear as rewind points with IsTaskCheckpoint flag
// - Rewind to task checkpoint restores file state at that point
// - Transcript is available for session resumption
func TestTaskCheckpoint_RewindWorkflow(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, _ string) {
		// Start a session
		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// Build the transcript progressively
		session.TranscriptBuilder.AddUserMessage("Create some files using subagents")
		session.TranscriptBuilder.AddAssistantMessage("I'll use the Task tool to help.")

		// === Task 1: Create first file ===
		task1ID := "toolu_task1_abc"
		task1AgentID := "agent_task1_xyz"

		// Add Task tool use to transcript
		session.TranscriptBuilder.AddTaskToolUse(task1ID, "Create task1.txt")
		if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		// Pre-task hook (before subagent runs)
		if err := env.SimulatePreTask(session.ID, session.TranscriptPath, task1ID); err != nil {
			t.Fatalf("SimulatePreTask task1 failed: %v", err)
		}

		// Simulate subagent creating the file
		task1Content := "Created by task 1"
		env.WriteFile("task1.txt", task1Content)

		// Create subagent transcript
		subagent1 := NewTranscriptBuilder()
		subagent1.AddUserMessage("Create task1.txt")
		toolID1 := subagent1.AddToolUse("mcp__acp__Write", "task1.txt", task1Content)
		subagent1.AddToolResult(toolID1)
		subagent1.AddAssistantMessage("Created task1.txt")

		subagent1Path := filepath.Join(filepath.Dir(session.TranscriptPath), "agent-"+task1AgentID+".jsonl")
		if err := subagent1.WriteToFile(subagent1Path); err != nil {
			t.Fatalf("failed to write subagent1 transcript: %v", err)
		}

		// Add task result to main transcript
		session.TranscriptBuilder.AddTaskToolResult(task1ID, task1AgentID)
		if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		// Post-task hook (creates checkpoint)
		if err := env.SimulatePostTask(PostTaskInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      task1ID,
			AgentID:        task1AgentID,
		}); err != nil {
			t.Fatalf("SimulatePostTask task1 failed: %v", err)
		}

		// === Task 2: Create second file ===
		task2ID := "toolu_task2_def"
		task2AgentID := "agent_task2_uvw"

		session.TranscriptBuilder.AddAssistantMessage("Now creating another file.")
		session.TranscriptBuilder.AddTaskToolUse(task2ID, "Create task2.txt")
		if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		if err := env.SimulatePreTask(session.ID, session.TranscriptPath, task2ID); err != nil {
			t.Fatalf("SimulatePreTask task2 failed: %v", err)
		}

		// Simulate subagent creating the second file
		task2Content := "Created by task 2"
		env.WriteFile("task2.txt", task2Content)

		// Create subagent transcript
		subagent2 := NewTranscriptBuilder()
		subagent2.AddUserMessage("Create task2.txt")
		toolID2 := subagent2.AddToolUse("mcp__acp__Write", "task2.txt", task2Content)
		subagent2.AddToolResult(toolID2)
		subagent2.AddAssistantMessage("Created task2.txt")

		subagent2Path := filepath.Join(filepath.Dir(session.TranscriptPath), "agent-"+task2AgentID+".jsonl")
		if err := subagent2.WriteToFile(subagent2Path); err != nil {
			t.Fatalf("failed to write subagent2 transcript: %v", err)
		}

		// Add task result to main transcript
		session.TranscriptBuilder.AddTaskToolResult(task2ID, task2AgentID)
		if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		// Post-task hook (creates checkpoint)
		if err := env.SimulatePostTask(PostTaskInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      task2ID,
			AgentID:        task2AgentID,
		}); err != nil {
			t.Fatalf("SimulatePostTask task2 failed: %v", err)
		}

		// End session
		session.TranscriptBuilder.AddAssistantMessage("All tasks complete!")
		if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}

		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		// Verify both files exist
		if !env.FileExists("task1.txt") {
			t.Error("task1.txt should exist after session")
		}
		if !env.FileExists("task2.txt") {
			t.Error("task2.txt should exist after session")
		}

		// Get rewind points
		points := env.GetRewindPoints()

		// Filter to task checkpoints only
		taskPoints := filterTaskCheckpoints(points)
		if len(taskPoints) < 2 {
			t.Fatalf("expected at least 2 task checkpoints, got %d (total points: %d)", len(taskPoints), len(points))
		}

		// Find task 1 checkpoint
		var task1Point *RewindPoint
		for i := range taskPoints {
			if taskPoints[i].ToolUseID == task1ID {
				task1Point = &taskPoints[i]
				break
			}
		}
		if task1Point == nil {
			t.Fatalf("could not find task 1 checkpoint (looking for ToolUseID=%s)", task1ID)
		}
		if !task1Point.IsTaskCheckpoint {
			t.Error("task1 point should have IsTaskCheckpoint=true")
		}

		// Rewind to task 1 checkpoint
		if err := env.Rewind(task1Point.ID); err != nil {
			t.Fatalf("Rewind to task1 failed: %v", err)
		}

		// CORE ASSERTION: task1.txt should exist with correct content
		if !env.FileExists("task1.txt") {
			t.Error("task1.txt should exist after rewind to task 1")
		} else {
			content := env.ReadFile("task1.txt")
			if content != task1Content {
				t.Errorf("task1.txt content: got %q, want %q", content, task1Content)
			}
		}

		// CORE ASSERTION: Transcript available for resumption (uses model session ID for Claude compatibility)
		transcriptPath := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
		if _, err := os.Stat(transcriptPath); os.IsNotExist(err) {
			t.Errorf("transcript should be copied for claude -r, expected at %s", transcriptPath)
		} else {
			// Verify transcript contains task1 but ideally not task2 completion
			transcriptContent := env.ReadFileAbsolute(transcriptPath)
			if !strings.Contains(transcriptContent, task1ID) {
				t.Error("restored transcript should contain task1 tool use")
			}
		}
	})
}

// filterTaskCheckpoints returns only task checkpoints from the rewind points.
func filterTaskCheckpoints(points []RewindPoint) []RewindPoint {
	var tasks []RewindPoint
	for _, p := range points {
		if p.IsTaskCheckpoint {
			tasks = append(tasks, p)
		}
	}
	return tasks
}

// TestRewind_MultipleNewFiles tests that sessions creating multiple files work correctly.
// All strategies use a single session with multiple checkpoints.
func TestRewind_MultipleNewFiles(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Add .gitignore to the repo
		env.WriteFile(".gitignore", ".entire/\n")
		env.GitAdd(".gitignore")
		env.GitCommit("Add gitignore")

		// Use same session for all checkpoints (works for all strategies)
		session := env.NewSession()

		// Checkpoint 1: creating multiple files in different directories
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		files := []FileChange{
			{Path: "src/main.go", Content: "package main"},
			{Path: "src/util.go", Content: "package main\n\nfunc helper() {}"},
			{Path: "tests/main_test.go", Content: "package main_test"},
			{Path: "config.yaml", Content: "key: value"},
		}

		for _, f := range files {
			env.WriteFile(f.Path, f.Content)
		}

		session.CreateTranscript("Create project structure", files)
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		// Verify all files exist
		for _, f := range files {
			if !env.FileExists(f.Path) {
				t.Errorf("file %s should exist after session", f.Path)
			}
		}

		// Checkpoint 2: Modify one file
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		modifiedContent := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}"
		env.WriteFile("src/main.go", modifiedContent)
		session.CreateTranscript("Add main function",
			[]FileChange{{Path: "src/main.go", Content: modifiedContent}})
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		// Verify modification
		if content := env.ReadFile("src/main.go"); content != modifiedContent {
			t.Errorf("got %q, want %q", content, modifiedContent)
		}

		// Rewind to checkpoint 1
		points := env.GetRewindPoints()
		if len(points) < 2 {
			t.Fatalf("expected at least 2 rewind points, got %d", len(points))
		}

		// Checkpoint 1 is the older one (last in the list)
		if err := env.Rewind(points[len(points)-1].ID); err != nil {
			t.Fatalf("Rewind failed: %v", err)
		}

		// Verify original content restored
		if content := env.ReadFile("src/main.go"); content != "package main" {
			t.Errorf("after rewind: got %q, want %q", content, "package main")
		}
	})
}

// TestRewind_MultipleConsecutive tests multiple consecutive rewinds.
// All strategies use a single session with multiple checkpoints.
func TestRewind_MultipleConsecutive(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Add .gitignore to the repo
		env.WriteFile(".gitignore", ".entire/\n")
		env.GitAdd(".gitignore")
		env.GitCommit("Add gitignore")

		// Use same session for all checkpoints (works for all strategies)
		session := env.NewSession()

		// Create 3 checkpoints with different versions
		versions := []string{"version 1", "version 2", "version 3"}
		for _, version := range versions {
			if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}

			env.WriteFile("file.txt", version)
			session.CreateTranscript(
				"Update file",
				[]FileChange{{Path: "file.txt", Content: version}},
			)
			if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop checkpoint %s failed: %v", version, err)
			}
		}

		// Verify we're at version 3
		if content := env.ReadFile("file.txt"); content != "version 3" {
			t.Errorf("expected version 3, got %q", content)
		}

		points := env.GetRewindPoints()
		if len(points) != 3 {
			t.Fatalf("expected 3 rewind points, got %d", len(points))
		}

		// Rewind to version 2 (second newest = index 1)
		if err := env.Rewind(points[1].ID); err != nil {
			t.Fatalf("Rewind to v2 failed: %v", err)
		}
		if content := env.ReadFile("file.txt"); content != "version 2" {
			t.Errorf("after rewind to v2: got %q, want %q", content, "version 2")
		}

		// Get fresh points after rewind
		points = env.GetRewindPoints()
		if len(points) == 0 {
			t.Fatalf("No rewind points found after first rewind")
		}

		// Rewind to version 1 (oldest)
		if err := env.Rewind(points[len(points)-1].ID); err != nil {
			t.Fatalf("Rewind to v1 failed: %v", err)
		}
		if content := env.ReadFile("file.txt"); content != "version 1" {
			t.Errorf("after rewind to v1: got %q, want %q", content, "version 1")
		}
	})

}

// TestRewind_DifferentSessions tests that commit and auto-commit strategies support
// multiple different sessions without committing, while manual-commit strategy requires
// the same session (or a commit between sessions).
func TestRewind_DifferentSessions(t *testing.T) {
	t.Parallel()

	t.Run("auto_commit_supports_different_sessions", func(t *testing.T) {
		t.Parallel()
		for _, strategyName := range []string{"auto-commit"} {
			strategyName := strategyName // capture for parallel
			t.Run(strategyName, func(t *testing.T) {
				t.Parallel()
				env := NewFeatureBranchEnv(t, strategyName)

				// Session 1
				session1 := env.NewSession()
				if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
					t.Fatalf("SimulateUserPromptSubmit session1 failed: %v", err)
				}
				env.WriteFile("file.txt", "version 1")
				session1.CreateTranscript("Create file", []FileChange{{Path: "file.txt", Content: "version 1"}})
				if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
					t.Fatalf("SimulateStop session1 failed: %v", err)
				}

				// Session 2 (different session ID, no commit between)
				session2 := env.NewSession()
				if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
					t.Fatalf("SimulateUserPromptSubmit session2 failed: %v", err)
				}
				env.WriteFile("file.txt", "version 2")
				session2.CreateTranscript("Update file", []FileChange{{Path: "file.txt", Content: "version 2"}})
				if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
					t.Fatalf("SimulateStop session2 failed: %v", err)
				}

				// Both sessions should create rewind points
				points := env.GetRewindPoints()
				if len(points) != 2 {
					t.Errorf("expected 2 rewind points, got %d", len(points))
				}
			})
		}
	})
}
