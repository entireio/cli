//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Factory AI Droid dispatches Workers as detached sessions: the parent's
// PostToolUse hook fires when the Worker is launched, before it has touched the
// worktree, and the Worker then runs its own SessionStart/UserPromptSubmit/Stop
// cycle. These tests pin the resulting attribution — a Worker's turn must land
// as a task checkpoint on the parent, not as an unrelated top-level session.

// TestFactoryDroidWorkerSessionBecomesTaskCheckpoint covers the regression that
// broke TestFactoryTaskCheckpointExistsBeforeCommit: a Worker's work reached the
// shadow branch, but under its own session ID, so no task checkpoint existed.
func TestFactoryDroidWorkerSessionBecomesTaskCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// The parent turn: the user asks for a Worker, and Droid dispatches one.
	parent := env.NewFactoryDroidSession()
	parent.CreateDroidTranscript("Run a Worker that writes docs/summary.md", nil)
	if err := env.SimulateFactoryDroidUserPromptSubmit(parent.ID); err != nil {
		t.Fatalf("parent UserPromptSubmit failed: %v", err)
	}

	// The Worker turn: its own session, writing the file the parent asked for.
	const toolUseID = "toolu_worker_01"
	worker := env.NewFactoryDroidSession()
	if err := env.SimulateFactoryDroidUserPromptSubmit(worker.ID); err != nil {
		t.Fatalf("worker UserPromptSubmit failed: %v", err)
	}
	env.WriteFile("docs/summary.md", "A short summary.\n")
	worker.CreateDroidTranscript("# Task Tool Invocation", []FileChange{
		{Path: "docs/summary.md", Content: "A short summary.\n"},
	})
	worker.MarkAsWorkerSession(parent.ID, toolUseID, "worker: Summarize the repo")

	if err := env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath); err != nil {
		t.Fatalf("worker Stop failed: %v", err)
	}

	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after the Worker's turn", shadowBranch)
	}

	// The work belongs to the parent's task, not to the Worker's own session.
	taskCheckpoint := ".entire/metadata/" + parent.ID + "/tasks/" + toolUseID + "/" + paths.CheckpointFileName
	if !env.FileExistsInBranch(shadowBranch, taskCheckpoint) {
		t.Errorf("expected task checkpoint %s on shadow branch %s", taskCheckpoint, shadowBranch)
	}

	// A top-level checkpoint for the Worker session would misattribute the work
	// to a session the user never drove — the exact shape of the regression.
	// A session checkpoint is what writes the session's own transcript.
	workerSessionCheckpoint := ".entire/metadata/" + worker.ID + "/" + paths.TranscriptFileName
	if env.FileExistsInBranch(shadowBranch, workerSessionCheckpoint) {
		t.Errorf("Worker session must not produce its own session checkpoint at %s", workerSessionCheckpoint)
	}

	// The parent's transcript rides along, so the task reads back in context.
	parentTranscript := ".entire/metadata/" + parent.ID + "/" + paths.TranscriptFileName
	if !env.FileExistsInBranch(shadowBranch, parentTranscript) {
		t.Errorf("expected the parent transcript at %s", parentTranscript)
	}

	// The Worker's transcript is preserved under the task, keyed by its session ID.
	workerTranscript := ".entire/metadata/" + parent.ID + "/tasks/" + toolUseID + "/" +
		paths.AgentTranscriptFileName(worker.ID)
	if !env.FileExistsInBranch(shadowBranch, workerTranscript) {
		t.Errorf("expected the Worker transcript at %s", workerTranscript)
	}
}

// TestFactoryDroidTopLevelSessionStillCheckpoints guards the fallback: an
// ordinary Droid session has no calling IDs and must keep its own checkpoint.
func TestFactoryDroidTopLevelSessionStillCheckpoints(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	session := env.NewFactoryDroidSession()
	if err := env.SimulateFactoryDroidUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("UserPromptSubmit failed: %v", err)
	}
	env.WriteFile("feature.go", "package main\n")
	session.CreateDroidTranscript("Add a feature", []FileChange{
		{Path: "feature.go", Content: "package main\n"},
	})

	if err := env.SimulateFactoryDroidStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	shadowBranch := env.GetShadowBranchName()
	sessionCheckpoint := ".entire/metadata/" + session.ID + "/" + paths.TranscriptFileName
	if !env.FileExistsInBranch(shadowBranch, sessionCheckpoint) {
		t.Errorf("expected an ordinary session checkpoint at %s", sessionCheckpoint)
	}

	// It must not be diverted into a task checkpoint under some other session.
	if env.FileExistsInBranch(shadowBranch, ".entire/metadata/"+session.ID+"/tasks") {
		t.Errorf("a top-level session must not be recorded as a task")
	}
}
