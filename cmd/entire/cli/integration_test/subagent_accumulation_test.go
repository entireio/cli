//go:build integration

package integration

import (
	"fmt"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestSubagentAccumulation_Issue591 reproduces the issue #591 shape: multiple
// subagent sessions accumulate as ENDED. The fix is eager-condense-on-stop:
// subagents with no uncommitted files at stop time are marked FullyCondensed
// immediately, so PostCommit skips them entirely on all subsequent commits.
//
// Scenario:
// 1. Parent session spawns 4 subagents; each creates + commits its own file
// 2. Each subagent ends (stop hook runs → CondenseAndMarkFullyCondensed marks them FullyCondensed)
// 3. Parent commits parent_work.go — PostCommit skips all FullyCondensed subagents
// 4. Follow-up commit — subagents remain FullyCondensed and skipped
func TestSubagentAccumulation_Issue591(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	type subagentInfo struct {
		SessionID string
		File      string
	}

	t.Log("Phase 1: start parent session and create subagent sessions with committed files")

	parent := env.NewSession()
	if err := env.SimulateUserPromptSubmit(parent.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for parent failed: %v", err)
	}
	parent.TranscriptBuilder.AddUserMessage("Use subagents to create files, then create the parent file.")

	const numSubagents = 4
	subagents := make([]subagentInfo, 0, numSubagents)

	for i := 0; i < numSubagents; i++ {
		sub := env.NewSession()
		file := fmt.Sprintf("subagent_work_%d.go", i)
		content := fmt.Sprintf("package main\n\nfunc SubagentWork%d() {}\n", i)

		if err := env.SimulateUserPromptSubmit(sub.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit for subagent %d failed: %v", i, err)
		}

		env.WriteFile(file, content)
		sub.CreateTranscript("Create "+file, []FileChange{
			{Path: file, Content: content},
		})

		// Commit the subagent's file BEFORE stopping, so FilesTouched is empty at stop time.
		// This allows CondenseAndMarkFullyCondensed to eagerly condense at stop.
		env.GitAdd(file)
		env.GitCommitWithShadowHooks("Add "+file+" from subagent", file)

		if err := env.SimulateStop(sub.ID, sub.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop for subagent %d failed: %v", i, err)
		}
		if err := env.SimulateSessionEnd(sub.ID); err != nil {
			t.Fatalf("SimulateSessionEnd for subagent %d failed: %v", i, err)
		}

		// Verify eager-condense-on-stop marked the subagent FullyCondensed
		state, err := env.GetSessionState(sub.ID)
		if err != nil {
			t.Fatalf("GetSessionState for subagent %d failed: %v", i, err)
		}
		if state == nil {
			t.Fatalf("subagent %d session state missing after stop", i)
		}
		if state.Phase != session.PhaseEnded {
			t.Fatalf("subagent %d phase = %s, want ended", i, state.Phase)
		}
		if !state.FullyCondensed {
			t.Fatalf("subagent %d should be FullyCondensed after eager-condense-on-stop (FilesTouched was empty)", i)
		}

		taskToolUseID := fmt.Sprintf("toolu_parent_subagent_%d", i)
		parent.TranscriptBuilder.AddTaskToolUse(taskToolUseID, "Create "+file)
		parent.TranscriptBuilder.AddTaskToolResult(taskToolUseID, sub.ID)

		subagents = append(subagents, subagentInfo{
			SessionID: sub.ID,
			File:      file,
		})
	}

	t.Log("Phase 2: parent commits its own work — PostCommit should skip all FullyCondensed subagents")

	parentFile := "parent_work.go"
	parentContent := "package main\n\nfunc ParentWork() {}\n"
	env.WriteFile(parentFile, parentContent)
	parentToolUseID := parent.TranscriptBuilder.AddToolUse("mcp__acp__Write", parentFile, parentContent)
	parent.TranscriptBuilder.AddToolResult(parentToolUseID)
	parent.TranscriptBuilder.AddAssistantMessage("Done.")
	if err := parent.TranscriptBuilder.WriteToFile(parent.TranscriptPath); err != nil {
		t.Fatalf("failed to write parent transcript: %v", err)
	}

	if err := env.SimulateStop(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for parent failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Parent commit", parentFile)

	t.Log("Phase 3: verify FullyCondensed subagents were skipped or cleaned up by PostCommit")

	for i, sub := range subagents {
		state, err := env.GetSessionState(sub.SessionID)
		if err != nil {
			t.Fatalf("GetSessionState after parent commit for subagent %d failed: %v", i, err)
		}
		// State may be nil: listAllSessionStates cleans up ENDED sessions whose
		// shadow branch was deleted and LastCheckpointID is empty. This is expected
		// for sessions that were eagerly condensed at stop time (shadow branch cleaned
		// up before PostCommit could set LastCheckpointID).
		if state == nil {
			t.Logf("subagent %d state cleaned up after parent commit — OK (eagerly condensed)", i)
			continue
		}
		if state.Phase != session.PhaseEnded {
			t.Fatalf("subagent %d phase after parent commit = %s, want ended", i, state.Phase)
		}
		if !state.FullyCondensed {
			t.Fatalf("subagent %d should remain FullyCondensed after parent commit", i)
		}
	}

	t.Log("Phase 4: make another unrelated commit and verify fully-condensed subagents stay skipped")

	followUp := env.NewSession()
	if err := env.SimulateUserPromptSubmit(followUp.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for follow-up session failed: %v", err)
	}

	followUpFile := "follow_up.go"
	followUpContent := "package main\n\nfunc FollowUp() {}\n"
	env.WriteFile(followUpFile, followUpContent)
	followUp.CreateTranscript("Create "+followUpFile, []FileChange{
		{Path: followUpFile, Content: followUpContent},
	})
	if err := env.SimulateStop(followUp.ID, followUp.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for follow-up session failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Follow-up commit", followUpFile)

	for i, sub := range subagents {
		state, err := env.GetSessionState(sub.SessionID)
		if err != nil {
			t.Fatalf("GetSessionState after follow-up commit for subagent %d failed: %v", i, err)
		}
		if state == nil {
			// Already cleaned up in Phase 3 — still gone, which is correct
			continue
		}
		if !state.FullyCondensed {
			t.Fatalf("subagent %d should remain FullyCondensed after follow-up commit", i)
		}
		if state.StepCount != 0 {
			t.Fatalf("subagent %d StepCount = %d after follow-up commit, want 0", i, state.StepCount)
		}
	}
}
