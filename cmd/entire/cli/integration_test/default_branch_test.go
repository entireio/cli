//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestDefaultBranch_WorksOnMain tests that the strategy works on main branch.
func TestDefaultBranch_WorksOnMain(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	branch := env.GetCurrentBranch()
	if branch != "main" && branch != "master" {
		t.Fatalf("expected to be on main or master branch, got %q", branch)
	}

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("file.txt", "content on main")
	session.CreateTranscript(
		"Add a file",
		[]FileChange{{Path: "file.txt", Content: "content on main"}},
	)

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after checkpoint on main branch", shadowBranch)
	}
	if content, found := env.ReadFileFromBranch(shadowBranch, "file.txt"); !found || content != "content on main" {
		t.Errorf("file.txt on shadow branch = %q (found=%v), want %q", content, found, "content on main")
	}
}

// TestDefaultBranch_WorksOnFeatureBranch tests that Entire tracking works on feature branches.
func TestDefaultBranch_WorksOnFeatureBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	branch := env.GetCurrentBranch()
	if branch != "feature/test-branch" {
		t.Fatalf("expected to be on feature/test-branch, got %q", branch)
	}

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("feature.txt", "content on feature branch")
	session.CreateTranscript(
		"Add a feature file",
		[]FileChange{{Path: "feature.txt", Content: "content on feature branch"}},
	)

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after checkpoint on feature branch", shadowBranch)
	}
	if content, found := env.ReadFileFromBranch(shadowBranch, "feature.txt"); !found || content != "content on feature branch" {
		t.Errorf("feature.txt on shadow branch = %q (found=%v), want %q", content, found, "content on feature branch")
	}
}

// TestDefaultBranch_PostTaskWorksOnMain tests that task checkpoints work on main.
func TestDefaultBranch_PostTaskWorksOnMain(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	branch := env.GetCurrentBranch()
	if branch != "main" && branch != "master" {
		t.Fatalf("expected to be on main or master branch, got %q", branch)
	}

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	session.TranscriptBuilder.AddUserMessage("Create a file using a subagent")
	session.TranscriptBuilder.AddAssistantMessage("I'll use the Task tool.")

	taskID := "toolu_task_main"
	agentID := "agent_main_xyz"

	session.TranscriptBuilder.AddTaskToolUse(taskID, "Create task.txt")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	env.WriteFile("task.txt", "Created by task on main")

	session.TranscriptBuilder.AddTaskToolResult(taskID, agentID)
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskID,
		AgentID:        agentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after task checkpoint on main", shadowBranch)
	}
	taskCheckpointPath := ".entire/metadata/" + session.ID + "/tasks/" + taskID + "/" + paths.CheckpointFileName
	if !env.FileExistsInBranch(shadowBranch, taskCheckpointPath) {
		t.Errorf("task checkpoint %s should exist on shadow branch %s", taskCheckpointPath, shadowBranch)
	}
}
