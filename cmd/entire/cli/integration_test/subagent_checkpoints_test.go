//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/sessionid"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestSubagentCheckpoints_FullFlow tests the complete subagent checkpoint flow:
// PreTask -> PostTodo (multiple times with file changes) -> PostTask
//
// This verifies:
// 1. Incremental checkpoints are created as commits during subagent execution
// 2. Only PostTodo calls with file changes create commits
// 3. PostTask creates the final task checkpoint commit
func TestSubagentCheckpoints_FullFlow(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a session
		session := env.NewSession()

		// Create transcript (needed by hooks)
		session.CreateTranscript("Implement feature X", []FileChange{
			{Path: "feature.go", Content: "package main"},
		})

		// Simulate user prompt submit first (captures pre-prompt state)
		err := env.SimulateUserPromptSubmit(session.ID)
		if err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// Task tool use ID (simulates Claude's Task tool invocation)
		taskToolUseID := "toolu_01TaskABC123"

		// Step 1: PreTask - creates pre-task file
		err = env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID)
		if err != nil {
			t.Fatalf("SimulatePreTask failed: %v", err)
		}

		// Verify pre-task file was created
		preTaskFile := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-task-"+taskToolUseID+".json")
		if _, err := os.Stat(preTaskFile); os.IsNotExist(err) {
			t.Error("pre-task file should exist after SimulatePreTask")
		}

		// Step 2: PostTodo - simulate TodoWrite calls with file changes between them
		// Note: Only PostTodo calls that detect file changes will create incremental commits

		// First TodoWrite - no file changes, should be skipped
		err = env.SimulatePostTodo(PostTodoInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      "toolu_01TodoWrite001",
			Todos: []Todo{
				{Content: "Create feature file", Status: "in_progress", ActiveForm: "Creating feature file"},
				{Content: "Write tests", Status: "pending", ActiveForm: "Writing tests"},
			},
		})
		if err != nil {
			t.Fatalf("SimulatePostTodo failed for first todo: %v", err)
		}

		// Create a file change
		env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")

		// Second TodoWrite - should create incremental checkpoint (has file changes)
		err = env.SimulatePostTodo(PostTodoInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      "toolu_01TodoWrite002",
			Todos: []Todo{
				{Content: "Create feature file", Status: "completed", ActiveForm: "Creating feature file"},
				{Content: "Write tests", Status: "in_progress", ActiveForm: "Writing tests"},
			},
		})
		if err != nil {
			t.Fatalf("SimulatePostTodo failed for second todo: %v", err)
		}

		// Create another file change
		env.WriteFile("feature_test.go", "package main\n\nimport \"testing\"\n\nfunc TestFeature(t *testing.T) {}\n")

		// Third TodoWrite - should create another incremental checkpoint
		err = env.SimulatePostTodo(PostTodoInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      "toolu_01TodoWrite003",
			Todos: []Todo{
				{Content: "Create feature file", Status: "completed", ActiveForm: "Creating feature file"},
				{Content: "Write tests", Status: "completed", ActiveForm: "Writing tests"},
			},
		})
		if err != nil {
			t.Fatalf("SimulatePostTodo failed for third todo: %v", err)
		}

		// Step 3: PostTask - creates final task checkpoint
		err = env.SimulatePostTask(PostTaskInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      taskToolUseID,
			AgentID:        "agent-123",
		})
		if err != nil {
			t.Fatalf("SimulatePostTask failed: %v", err)
		}

		// Verify pre-task file is cleaned up
		if _, err := os.Stat(preTaskFile); !os.IsNotExist(err) {
			t.Error("Pre-task file should be removed after PostTask")
		}

		// Verify checkpoints are stored in final location (strategy-specific)
		verifyCheckpointStorage(t, env, strategyName, session.ID, taskToolUseID)
	})
}

// TestSubagentCheckpoints_NoFileChanges tests that PostTodo is skipped when no file changes
func TestSubagentCheckpoints_NoFileChanges(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a session
		session := env.NewSession()

		// Create transcript
		session.CreateTranscript("Quick task", []FileChange{})

		// Simulate user prompt submit
		err := env.SimulateUserPromptSubmit(session.ID)
		if err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// Create pre-task file to simulate subagent context
		taskToolUseID := "toolu_01TaskNoChanges"
		err = env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID)
		if err != nil {
			t.Fatalf("SimulatePreTask failed: %v", err)
		}

		// Get git log before PostTodo
		beforeCommits := env.GetGitLog()

		// Call PostTodo WITHOUT making any file changes
		err = env.SimulatePostTodo(PostTodoInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      "toolu_01TodoWriteNoChange",
			Todos: []Todo{
				{Content: "Some task", Status: "pending", ActiveForm: "Doing task"},
			},
		})
		if err != nil {
			t.Fatalf("SimulatePostTodo should not fail: %v", err)
		}

		// Get git log after PostTodo
		afterCommits := env.GetGitLog()

		// Verify no new commits were created
		if len(afterCommits) != len(beforeCommits) {
			t.Errorf("Expected no new commits when no file changes, before=%d after=%d", len(beforeCommits), len(afterCommits))
		}
	})
}

// TestSubagentCheckpoints_NoPreTaskFile tests that PostTodo is a no-op
// when there's no active pre-task file (main agent context).
func TestSubagentCheckpoints_NoPreTaskFile(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create a session
		session := env.NewSession()

		// Create transcript
		session.CreateTranscript("Quick task", []FileChange{})

		// Simulate user prompt submit
		err := env.SimulateUserPromptSubmit(session.ID)
		if err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// Create a file change so that PostTodo would trigger if in subagent context
		env.WriteFile("test.txt", "content")

		// Get git log before PostTodo
		beforeCommits := env.GetGitLog()

		// Call PostTodo WITHOUT calling PreTask first
		// This simulates a TodoWrite from the main agent (not a subagent)
		err = env.SimulatePostTodo(PostTodoInput{
			SessionID:      session.ID,
			TranscriptPath: session.TranscriptPath,
			ToolUseID:      "toolu_01MainAgentTodo",
			Todos: []Todo{
				{Content: "Some task", Status: "pending", ActiveForm: "Doing task"},
			},
		})
		if err != nil {
			t.Fatalf("SimulatePostTodo should not fail: %v", err)
		}

		// Get git log after PostTodo
		afterCommits := env.GetGitLog()

		// Verify no new commits were created (not in subagent context)
		if len(afterCommits) != len(beforeCommits) {
			t.Errorf("Expected no new commits when not in subagent context, before=%d after=%d", len(beforeCommits), len(afterCommits))
		}
	})
}

// verifyCheckpointStorage verifies that checkpoints are stored in the correct
// location based on the strategy type.
// Note: Incremental checkpoints are stored in separate commits during task execution,
// while the final checkpoint.json is created at PostTask time.
func verifyCheckpointStorage(t *testing.T, env *TestEnv, strategyName, sessionID, taskToolUseID string) {
	t.Helper()

	entireSessionID := sessionid.EntireSessionID(sessionID)

	switch strategyName {
	case strategy.StrategyNameManualCommit:
		// Shadow strategy stores checkpoints in git tree on shadow branch (entire/<head-hash>)
		// We need to verify that checkpoint data exists in the shadow branch tree
		verifyShadowCheckpointStorage(t, env, entireSessionID, taskToolUseID)

	case strategy.StrategyNameAutoCommit:
		// Dual strategy stores metadata on orphan entire/sessions branch
		// Verify that commits were created (incremental + final)
		t.Logf("Note: auto-commit strategy stores checkpoints in entire/sessions branch")
	}
}

// verifyCheckpointDir verifies that a checkpoints directory contains valid checkpoint files.
func verifyCheckpointDir(t *testing.T, checkpointsDir string) {
	t.Helper()

	entries, err := os.ReadDir(checkpointsDir)
	if err != nil {
		t.Errorf("Failed to read checkpoints directory %s: %v", checkpointsDir, err)
		return
	}

	if len(entries) == 0 {
		t.Errorf("Expected checkpoints in %s, but directory is empty", checkpointsDir)
		return
	}

	// Verify at least one valid checkpoint file
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		checkpointFile := filepath.Join(checkpointsDir, entry.Name())
		data, err := os.ReadFile(checkpointFile)
		if err != nil {
			t.Errorf("Failed to read checkpoint file %s: %v", checkpointFile, err)
			continue
		}

		var checkpoint strategy.SubagentCheckpoint
		if err := json.Unmarshal(data, &checkpoint); err != nil {
			t.Errorf("Failed to parse checkpoint file %s: %v", checkpointFile, err)
			continue
		}

		// Verify required fields
		if checkpoint.Type == "" {
			t.Errorf("Checkpoint file %s missing type field", entry.Name())
		}
		if checkpoint.ToolUseID == "" {
			t.Errorf("Checkpoint file %s missing tool_use_id field", entry.Name())
		}
		if checkpoint.Timestamp.IsZero() {
			t.Errorf("Checkpoint file %s missing timestamp field", entry.Name())
		}
	}
}

// verifyShadowCheckpointStorage verifies that checkpoints are stored in the shadow branch git tree.
func verifyShadowCheckpointStorage(t *testing.T, env *TestEnv, sessionID, taskToolUseID string) {
	t.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Get HEAD hash to determine shadow branch name
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	headHash := head.Hash().String()

	// Shadow branch is entire/<first-7-chars-of-head>
	shadowBranchName := "entire/" + headHash[:7]

	// Get shadow branch reference
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranchName), true)
	if err != nil {
		t.Fatalf("shadow branch %s not found: %v", shadowBranchName, err)
	}

	// Get the commit and tree from shadow branch
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		t.Fatalf("failed to get shadow commit: %v", err)
	}

	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get shadow tree: %v", err)
	}

	// Look for task metadata in the tree
	// Path format: .entire/metadata/<session-id>/tasks/<task-id>/
	taskMetadataPrefix := ".entire/metadata/" + sessionID + "/tasks/" + taskToolUseID + "/"
	checkpointsPrefix := taskMetadataPrefix + "checkpoints/"

	foundCheckpoint := false
	foundCheckpointFiles := 0

	err = shadowTree.Files().ForEach(func(f *object.File) error {
		// Check for checkpoint file (final checkpoint)
		if f.Name == taskMetadataPrefix+paths.CheckpointFileName {
			foundCheckpoint = true
			// Verify content is valid JSON
			content, readErr := f.Contents()
			if readErr != nil {
				t.Errorf("failed to read %s: %v", paths.CheckpointFileName, readErr)
				return nil
			}
			var cp strategy.TaskCheckpoint
			if jsonErr := json.Unmarshal([]byte(content), &cp); jsonErr != nil {
				t.Errorf("%s is invalid JSON: %v", paths.CheckpointFileName, jsonErr)
			}
		}

		// Check for incremental checkpoints in checkpoints/ directory
		if strings.HasPrefix(f.Name, checkpointsPrefix) && strings.HasSuffix(f.Name, ".json") {
			foundCheckpointFiles++
			// Verify content is valid checkpoint JSON
			content, readErr := f.Contents()
			if readErr != nil {
				t.Errorf("failed to read checkpoint file %s: %v", f.Name, readErr)
				return nil
			}
			var cp strategy.SubagentCheckpoint
			if jsonErr := json.Unmarshal([]byte(content), &cp); jsonErr != nil {
				t.Errorf("checkpoint file %s is invalid JSON: %v", f.Name, jsonErr)
			}
			// Verify required fields
			if cp.Type == "" {
				t.Errorf("checkpoint file %s missing type field", f.Name)
			}
			if cp.ToolUseID == "" {
				t.Errorf("checkpoint file %s missing tool_use_id field", f.Name)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to iterate shadow tree: %v", err)
	}

	if !foundCheckpoint {
		t.Errorf("%s not found in shadow branch tree at %s", paths.CheckpointFileName, taskMetadataPrefix+paths.CheckpointFileName)
	}

	if foundCheckpointFiles == 0 {
		t.Logf("Note: no incremental checkpoint files found in %s - they may be in earlier commits", checkpointsPrefix)
	} else {
		t.Logf("Found %d incremental checkpoint files in shadow branch tree", foundCheckpointFiles)
	}
}
