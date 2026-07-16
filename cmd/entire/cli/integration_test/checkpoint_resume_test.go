//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestCheckpointResume_ByCheckpointID resumes a checkpoint by ID from another
// branch: the CLI must find the branch containing the checkpoint's commit,
// check it out, and restore the session log.
func TestCheckpointResume_ByCheckpointID(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	content := "puts 'resume by id'"
	env.WriteFile("hello.rb", content)
	session.CreateTranscript(
		"Create a hello script",
		[]FileChange{{Path: "hello.rb", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	env.GitCommitWithShadowHooks("Create a hello script", "hello.rb")

	featureBranch := env.GetCurrentBranch()
	checkpointID := env.GetLatestCheckpointID()
	env.GitCheckoutBranch(masterBranch)

	output := env.RunCLI("checkpoint", "resume", checkpointID)

	if branch := env.GetCurrentBranch(); branch != featureBranch {
		t.Errorf("expected to be on %s, got %s\noutput: %s", featureBranch, branch, output)
	}
	if !strings.Contains(output, session.ID) {
		t.Errorf("output should reference restored session %s, got: %s", session.ID, output)
	}
}

// TestCheckpointResume_BareNonTTY verifies the agent-safe fallback: with no
// target and no TTY, the command lists recent checkpoints with full IDs.
func TestCheckpointResume_BareNonTTY(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	content := "puts 'bare listing'"
	env.WriteFile("list.rb", content)
	session.CreateTranscript(
		"Create a listing script",
		[]FileChange{{Path: "list.rb", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	env.GitCommitWithShadowHooks("Create a listing script", "list.rb")

	checkpointID := env.GetLatestCheckpointID()
	output := env.RunCLI("checkpoint", "resume")

	if !strings.Contains(output, checkpointID) {
		t.Errorf("bare listing should contain full checkpoint ID %s, got: %s", checkpointID, output)
	}
	if !strings.Contains(output, "entire checkpoint resume <checkpoint-id>") {
		t.Errorf("bare listing should include the resume hint, got: %s", output)
	}
}
