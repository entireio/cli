//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestSubagentCheckpoints_CommittedMidTurn_LeavesNoShadowBranch reproduces the
// orphaned shadow branch behind the e2e failure of
// TestSingleSessionSubagentCommitInTurn.
//
// The subagent writes a file and commits it itself, mid-turn. That commit condenses
// the session and deletes the shadow branch. post-task then fires with nothing left
// to snapshot — the file is already in HEAD — so it must skip the task checkpoint.
// Creating one instead mints a *new* shadow branch after condensation has already
// run, and nothing ever condenses it away: turn-end sees no file modifications and
// skips, so the branch outlives the session.
//
// The trap is that the subagent's transcript still records the Write. Deciding from
// the transcript alone conflates "the subagent wrote this at some point" with "there
// is an uncommitted change here" — see filterToUncommittedFiles, which the turn-end
// path already applies for exactly this reason.
func TestSubagentCheckpoints_CommittedMidTurn_LeavesNoShadowBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write docs/red.md and commit it", nil)

	const (
		taskToolUseID = "toolu_01CommitInTurn"
		subagentID    = "a0011223344556677"
		editedFile    = "docs/red.md"
	)
	// The subagent's own transcript records the Write; the main transcript does not.
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "Red is warm.\n"}})

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// The subagent writes the file and commits it itself, still inside the turn.
	env.WriteFile(editedFile, "Red is a warm colour.\n")
	env.GitCommitWithShadowHooksAsAgent("Add red.md", editedFile)

	// Condensation ran on that commit and cleaned up the shadow branch.
	if got := shadowBranches(env); len(got) != 0 {
		t.Fatalf("precondition: shadow branch should be gone after the mid-turn commit, got %v", got)
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	if got := shadowBranches(env); len(got) != 0 {
		t.Errorf("post-task created a shadow branch for already-committed work: %v\n"+
			"nothing will condense it away — turn-end skips when no files changed", got)
	}
}

// TestSubagentCheckpoints_UncommittedWork_StillCheckpoints is the companion guard:
// filtering already-committed paths must not stop a subagent whose work is still
// uncommitted from getting its task checkpoint.
func TestSubagentCheckpoints_UncommittedWork_StillCheckpoints(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write docs/blue.md", nil)

	const (
		taskToolUseID = "toolu_01UncommittedWork"
		subagentID    = "a7766554433221100"
		editedFile    = "docs/blue.md"
	)
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "Blue is cool.\n"}})

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Left uncommitted, unlike the test above.
	env.WriteFile(editedFile, "Blue is a cool colour.\n")

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if rec == nil || rec.CompletedAt.IsZero() || !containsFile(rec.Files, editedFile) {
		t.Errorf("expected a completed task record carrying uncommitted subagent work, got %+v", rec)
	}
}

// shadowBranches returns the per-base-commit shadow branches, excluding the
// permanent committed-checkpoint branch which is not session-scoped.
func shadowBranches(env *TestEnv) []string {
	var out []string
	for _, b := range env.ListBranchesWithPrefix("entire/") {
		if b == paths.MetadataBranchName {
			continue
		}
		out = append(out, b)
	}
	return out
}

// TestSubagentCheckpoints_CommitWhileIdleWithTaskRecord_LinksAndCondensesContent
// drives the real prepare-commit-msg + post-commit hooks for a background
// subagent's commit landing while the parent session is IDLE. On top of #2032's
// slow path the commit already linked via HasTaskContent; what this pins is that
// the link is backed by content — the commit's own condensation materializes the
// task record's transcript-so-far under the checkpoint's tasks/ subtree, so the
// trailer resolves to the subagent's real work rather than dangling. That is
// reachable only because idleWithTaskContent bypasses
// shouldCondenseWithOverlapCheck's overlap requirement for this record-bearing
// IDLE session, whose FilesTouched carries no evidence tying it to editedFile.
func TestSubagentCheckpoints_CommitWhileIdleWithTaskRecord_LinksAndCondensesContent(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	sess.CreateTranscript("delegate a background task", nil)

	const (
		taskToolUseID = "toolu_01IdleMarkerCommit"
		subagentID    = "d4444555566667777"
		editedFile    = "docs/idlemarker.md"
	)

	// Real Claude Code always sends a transcript_path on UserPromptSubmit; it
	// is what populates the persisted SessionState.TranscriptPath condensation
	// later stores as the parent transcript.
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Background launch: record created while the parent is still ACTIVE
	// (mid-turn).
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:       sess.ID,
		TranscriptPath:  sess.TranscriptPath,
		ToolUseID:       taskToolUseID,
		AgentID:         subagentID,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// Turn ends: the parent goes IDLE while the background subagent keeps
	// running. The record survives, still live. This is the shape
	// idleWithTaskContent exists for: an IDLE session whose background task is
	// still genuinely in flight.
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
	}
	if state.Phase != session.PhaseIdle {
		t.Fatalf("expected session to be IDLE after turn-end, got %+v", state)
	}
	if !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected live task record to survive turn-end, state=%+v", state)
	}

	// The subagent does its actual work while the parent sits idle between
	// turns: a realistic transcript (the real Claude Code transcript
	// analyzer, not a stub) plus the resulting file.
	const editedContent = "# Idle marker\n\nWritten by a background subagent while the parent is idle.\n"
	sess.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: editedContent},
	})
	env.WriteFile(editedFile, editedContent)

	// The commit lands while the session is IDLE, through the real
	// prepare-commit-msg + post-commit hook chain, with no TTY (agent-mode
	// commit) — the exact shape of the incident.
	env.GitCommitWithShadowHooksAsAgent("Add idle-marker doc", editedFile)

	headHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(headHash)
	if checkpointID == "" {
		t.Fatalf("commit made while idle with a live task record should carry an Entire-Checkpoint trailer")
	}

	// THE content guarantee: the commit's condensation materialized the live
	// record's transcript-so-far into the permanent checkpoint's tasks/
	// subtree, so the trailer points at the subagent's real work.
	storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
	if !ok {
		t.Fatalf("subagent transcript not materialized under the checkpoint's tasks/ subtree")
	}
	if !strings.Contains(storedTranscript, editedFile) {
		t.Errorf("materialized subagent transcript does not reference %q: %q", editedFile, storedTranscript)
	}

	// The live record survives condensation: the task is still running, and
	// SubagentStop (not this commit) remains the authoritative completion
	// signal; the next condensation re-materializes it.
	state, err = env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
	}
	if !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected live task record for %s to survive condensation, state=%+v", taskToolUseID, state)
	}
}
