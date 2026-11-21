//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestSubagentCheckpoints_CommittedMidTurn_LeavesNothingPending reproduces the
// orphaned pending work behind the e2e failure of
// TestSingleSessionSubagentCommitInTurn.
//
// The subagent writes a file and commits it itself, mid-turn. That commit condenses
// the session. post-task then fires with nothing left pending — the file is
// already in HEAD — so it must not put the file back into FilesTouched. Doing so
// leaves work pending after condensation has already run, and no commit ever
// claims it, so it outlives the session.
//
// The trap is that the subagent's transcript still records the Write. Deciding from
// the transcript alone conflates "the subagent wrote this at some point" with "there
// is an uncommitted change here" — see filterToUncommittedFiles, which the turn-end
// path already applies for exactly this reason.
func TestSubagentCheckpoints_CommittedMidTurn_LeavesNothingPending(t *testing.T) {
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

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// The subagent writes the file and commits it itself, still inside the turn.
	env.WriteFile(editedFile, "Red is a warm colour.\n")
	env.GitCommitWithHooksAsAgent("Add red.md", editedFile)

	// Condensation ran on that commit and consumed the pending files.
	if state, err := env.GetSessionState(session.ID); err != nil || state == nil || containsFile(state.FilesTouched, editedFile) {
		t.Fatalf("precondition: %s should not be pending after the mid-turn commit (state=%+v, err=%v)", editedFile, state, err)
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	state, err := env.GetSessionState(session.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: state=%+v err=%v", state, err)
	}
	if containsFile(state.FilesTouched, editedFile) {
		t.Errorf("post-task left already-committed %s pending: %v\n"+
			"no commit will claim it — turn-end skips when no files changed", editedFile, state.FilesTouched)
	}
	if rec := state.FindTaskRecord(taskToolUseID); rec != nil {
		t.Errorf("post-task left a task record pending after storing it in the commit's checkpoint: %+v", rec)
	}
	env.AssertNoShadowBranches()

	// The subagent's own commit carries its transcript: the record could not
	// exist when that commit condensed, so post-task adds it afterwards.
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("the subagent's commit should carry an Entire-Checkpoint trailer")
	}
	assertTaskTranscriptInCheckpoint(t, env, checkpointID, taskToolUseID, subagentID, editedFile)

	// Turn-end rewrites the checkpoint's session transcript; the task stays.
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	assertTaskTranscriptInCheckpoint(t, env, checkpointID, taskToolUseID, subagentID, editedFile)
}

// TestSubagentCheckpoints_CommittedMidTurn_PartialCommitStoresTaskInBoth pins
// that a subagent committing only some of its files gets its task in that
// commit's checkpoint, and again in the checkpoint of the commit that later
// carries the rest.
func TestSubagentCheckpoints_CommittedMidTurn_PartialCommitStoresTaskInBoth(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write two files and commit one", nil)

	const (
		taskToolUseID = "toolu_01PartialCommit"
		subagentID    = "a9900112233445566"
		committedFile = "docs/committed.md"
		laterFile     = "docs/later.md"
	)
	session.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: committedFile, Content: "Committed.\n"},
		{Path: laterFile, Content: "Later.\n"},
	})

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	env.WriteFile(committedFile, "Committed.\n")
	env.WriteFile(laterFile, "Later.\n")
	env.GitCommitWithHooksAsAgent("Add committed.md", committedFile)
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if firstCheckpointID == "" {
		t.Fatal("precondition: the subagent's commit should carry an Entire-Checkpoint trailer")
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}
	assertTaskTranscriptInCheckpoint(t, env, firstCheckpointID, taskToolUseID, subagentID, committedFile)

	state, err := env.GetSessionState(session.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: state=%+v err=%v", state, err)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if rec == nil || !containsFile(rec.Files, laterFile) || containsFile(rec.Files, committedFile) {
		t.Fatalf("the uncommitted rest should stay pending as a record listing only %s, got %+v", laterFile, rec)
	}

	env.GitCommitWithHooksAsAgent("Add later.md", laterFile)
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if secondCheckpointID == "" || secondCheckpointID == firstCheckpointID {
		t.Fatalf("precondition: the later commit should get its own checkpoint, got %q (first %q)", secondCheckpointID, firstCheckpointID)
	}
	assertTaskTranscriptInCheckpoint(t, env, secondCheckpointID, taskToolUseID, subagentID, laterFile)
}

// TestSubagentCheckpoints_CommittedMidTurn_SkipsEarlierParentCheckpoint pins
// that only checkpoints condensed while the subagent ran receive its task: the
// parent's own commit earlier in the same turn keeps its contents.
func TestSubagentCheckpoints_CommittedMidTurn_SkipsEarlierParentCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	const (
		taskToolUseID = "toolu_01EarlierParentCommit"
		subagentID    = "a1122334455667788"
		parentFile    = "docs/parent.md"
		subagentFile  = "docs/green.md"
	)
	session.CreateTranscript("write docs/parent.md and commit, then use a subagent for docs/green.md",
		[]FileChange{{Path: parentFile, Content: "Parent.\n"}})
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: subagentFile, Content: "Green.\n"}})

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	env.WriteFile(parentFile, "Parent.\n")
	env.GitCommitWithHooksAsAgent("Add parent.md", parentFile)
	parentCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if parentCheckpointID == "" {
		t.Fatal("precondition: the parent's commit should carry an Entire-Checkpoint trailer")
	}

	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	env.WriteFile(subagentFile, "Green.\n")
	env.GitCommitWithHooksAsAgent("Add green.md", subagentFile)
	subagentCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if subagentCheckpointID == "" || subagentCheckpointID == parentCheckpointID {
		t.Fatalf("precondition: the subagent's commit should get its own checkpoint, got %q (parent %q)",
			subagentCheckpointID, parentCheckpointID)
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	assertTaskTranscriptInCheckpoint(t, env, subagentCheckpointID, taskToolUseID, subagentID, subagentFile)
	if _, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(parentCheckpointID, taskToolUseID, "task.json")); ok {
		t.Errorf("the parent's earlier checkpoint %s received a task that started after it", parentCheckpointID)
	}
}

// TestSubagentCheckpoints_CommittedMidTurn_SkipsParallelReadOnlySubagent pins
// that a subagent running alongside the one that committed is not attached to
// that commit's checkpoint when it wrote nothing itself.
func TestSubagentCheckpoints_CommittedMidTurn_SkipsParallelReadOnlySubagent(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("run two subagents: one writes docs/blue.md and commits it, one only reads", nil)

	const (
		committerToolUseID = "toolu_01ParallelCommitter"
		committerID        = "a2233445566778899"
		readerToolUseID    = "toolu_01ParallelReader"
		readerID           = "a3344556677889900"
		editedFile         = "docs/blue.md"
	)
	session.CreateSubagentTranscript(committerID, []FileChange{{Path: editedFile, Content: "Blue.\n"}})
	session.CreateSubagentTranscript(readerID, nil)

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	for _, toolUseID := range []string{committerToolUseID, readerToolUseID} {
		if err := env.SimulatePreTask(session.ID, session.TranscriptPath, toolUseID); err != nil {
			t.Fatalf("SimulatePreTask(%s) failed: %v", toolUseID, err)
		}
	}

	env.WriteFile(editedFile, "Blue.\n")
	env.GitCommitWithHooksAsAgent("Add blue.md", editedFile)
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("precondition: the committing subagent's commit should carry an Entire-Checkpoint trailer")
	}

	for _, input := range []PostTaskInput{
		{SessionID: session.ID, TranscriptPath: session.TranscriptPath, ToolUseID: readerToolUseID, AgentID: readerID},
		{SessionID: session.ID, TranscriptPath: session.TranscriptPath, ToolUseID: committerToolUseID, AgentID: committerID},
	} {
		if err := env.SimulatePostTask(input); err != nil {
			t.Fatalf("SimulatePostTask(%s) failed: %v", input.ToolUseID, err)
		}
	}

	assertTaskTranscriptInCheckpoint(t, env, checkpointID, committerToolUseID, committerID, editedFile)
	if _, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, readerToolUseID, "task.json")); ok {
		t.Errorf("read-only subagent %s was attached to a commit it did not make", readerToolUseID)
	}
}

// TestSubagentCheckpoints_CommittedMidTurn_OverlappingCommittersKeepOwnCheckpoints
// pins that two foreground subagents running together, each committing its own
// file, are each stored in the checkpoint of their own commit, not both in the
// newest one.
func TestSubagentCheckpoints_CommittedMidTurn_OverlappingCommittersKeepOwnCheckpoints(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("run two subagents that each write and commit one file", nil)

	const (
		firstToolUseID  = "toolu_01OverlapFirst"
		firstID         = "a4455667788990011"
		firstFile       = "docs/first.md"
		secondToolUseID = "toolu_01OverlapSecond"
		secondID        = "a5566778899001122"
		secondFile      = "docs/second.md"
	)
	session.CreateSubagentTranscript(firstID, []FileChange{{Path: firstFile, Content: "First.\n"}})
	session.CreateSubagentTranscript(secondID, []FileChange{{Path: secondFile, Content: "Second.\n"}})

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	for _, toolUseID := range []string{firstToolUseID, secondToolUseID} {
		if err := env.SimulatePreTask(session.ID, session.TranscriptPath, toolUseID); err != nil {
			t.Fatalf("SimulatePreTask(%s) failed: %v", toolUseID, err)
		}
	}

	env.WriteFile(firstFile, "First.\n")
	env.GitCommitWithHooksAsAgent("Add first.md", firstFile)
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	env.WriteFile(secondFile, "Second.\n")
	env.GitCommitWithHooksAsAgent("Add second.md", secondFile)
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if firstCheckpointID == "" || secondCheckpointID == "" || firstCheckpointID == secondCheckpointID {
		t.Fatalf("precondition: each commit should get its own checkpoint, got %q and %q", firstCheckpointID, secondCheckpointID)
	}

	for _, input := range []PostTaskInput{
		{SessionID: session.ID, TranscriptPath: session.TranscriptPath, ToolUseID: firstToolUseID, AgentID: firstID},
		{SessionID: session.ID, TranscriptPath: session.TranscriptPath, ToolUseID: secondToolUseID, AgentID: secondID},
	} {
		if err := env.SimulatePostTask(input); err != nil {
			t.Fatalf("SimulatePostTask(%s) failed: %v", input.ToolUseID, err)
		}
	}

	assertTaskTranscriptInCheckpoint(t, env, firstCheckpointID, firstToolUseID, firstID, firstFile)
	assertTaskTranscriptInCheckpoint(t, env, secondCheckpointID, secondToolUseID, secondID, secondFile)
	for _, misplaced := range []struct{ checkpointID, toolUseID string }{
		{firstCheckpointID, secondToolUseID},
		{secondCheckpointID, firstToolUseID},
	} {
		if _, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
			CheckpointTaskFilePath(misplaced.checkpointID, misplaced.toolUseID, "task.json")); ok {
			t.Errorf("checkpoint %s received task %s from the other subagent's commit", misplaced.checkpointID, misplaced.toolUseID)
		}
	}
}

// TestSubagentCheckpoints_CommittedMidTurn_AmendKeepsTask pins that a subagent
// amending a commit made before it started still gets its task stored: the
// amend keeps the commit's checkpoint ID, so the turn's checkpoint list repeats
// an ID that was already there at the task's start.
func TestSubagentCheckpoints_CommittedMidTurn_AmendKeepsTask(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	const (
		taskToolUseID = "toolu_01AmendParentCommit"
		subagentID    = "a6677889900112233"
		parentFile    = "docs/parent.md"
		subagentFile  = "docs/amended.md"
	)
	session.CreateTranscript("write docs/parent.md and commit, then have a subagent amend that commit",
		[]FileChange{{Path: parentFile, Content: "Parent.\n"}})
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: subagentFile, Content: "Amended.\n"}})

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	env.WriteFile(parentFile, "Parent.\n")
	env.GitCommitWithHooksAsAgent("Add parent.md", parentFile)
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("precondition: the parent's commit should carry an Entire-Checkpoint trailer")
	}

	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	env.WriteFile(subagentFile, "Amended.\n")
	env.GitCommitAmendWithHooks(testutil.RunGit(t, env.RepoDir, "log", "-1", "--format=%B"), subagentFile)
	if amended := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()); amended != checkpointID {
		t.Fatalf("precondition: the amend should keep checkpoint %s, got %q", checkpointID, amended)
	}
	if state, err := env.GetSessionState(session.ID); err != nil || state == nil ||
		len(state.TurnCheckpointIDs) != 2 || state.TurnCheckpointIDs[1] != checkpointID {
		t.Fatalf("precondition: the amend should condense %s again, got state=%+v err=%v", checkpointID, state, err)
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	assertTaskTranscriptInCheckpoint(t, env, checkpointID, taskToolUseID, subagentID, subagentFile)
}

// TestSubagentCheckpoints_CommittedMidTurn_RetriesFailedAttachAtTurnEnd pins
// that a task whose write into its commit's checkpoint fails at post-task is
// kept and written at turn end, not lost.
func TestSubagentCheckpoints_CommittedMidTurn_RetriesFailedAttachAtTurnEnd(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session, checkpointID := subagentCommitWithUnavailableStore(t, env, "toolu_01RetryAtTurnEnd", "a7788990011223344", "docs/retry.md")

	state, err := env.GetSessionState(session.ID)
	if err != nil || state == nil || len(state.PendingTaskAttachments) != 1 {
		t.Fatalf("a failed write should leave one pending attachment, got state=%+v err=%v", state, err)
	}
	if containsFile(state.FilesTouched, "docs/retry.md") || state.FindTaskRecord("toolu_01RetryAtTurnEnd") != nil {
		t.Fatalf("a pending attachment must not become pending work for a later commit: %+v", state)
	}

	restoreCheckpointBranch(t, env)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	assertTaskTranscriptInCheckpoint(t, env, checkpointID, "toolu_01RetryAtTurnEnd", "a7788990011223344", "docs/retry.md")
	if state, err := env.GetSessionState(session.ID); err != nil || state == nil || len(state.PendingTaskAttachments) != 0 {
		t.Errorf("turn end should clear a delivered attachment, got state=%+v err=%v", state, err)
	}
}

// TestSubagentCheckpoints_CommittedMidTurn_GivesUpAfterRepeatedTurnEndFailures
// pins that an attachment whose write keeps failing is dropped after a bounded
// number of turn ends instead of being retried forever.
func TestSubagentCheckpoints_CommittedMidTurn_GivesUpAfterRepeatedTurnEndFailures(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session, _ := subagentCommitWithUnavailableStore(t, env, "toolu_01GivesUp", "a8899001122334455", "docs/gives-up.md")

	for turn := 1; turn <= 3; turn++ {
		if turn > 1 {
			if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
				t.Fatalf("SimulateUserPromptSubmit (turn %d) failed: %v", turn, err)
			}
		}
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop (turn %d) failed: %v", turn, err)
		}
		state, err := env.GetSessionState(session.ID)
		if err != nil || state == nil {
			t.Fatalf("GetSessionState (turn %d) failed: state=%+v err=%v", turn, state, err)
		}
		if want := turn < 3; (len(state.PendingTaskAttachments) == 1) != want {
			t.Fatalf("after %d failed turn-end retries, pending attachment kept = %v, want %v",
				turn, len(state.PendingTaskAttachments) == 1, want)
		}
	}
	restoreCheckpointBranch(t, env)
}

// subagentCommitWithUnavailableStore runs a foreground subagent that commits
// its own file while the checkpoint store becomes unavailable before its
// post-task, so the write into the commit's checkpoint fails. The store stays
// unavailable until restoreCheckpointBranch.
func subagentCommitWithUnavailableStore(t *testing.T, env *TestEnv, toolUseID, agentID, file string) (*Session, string) {
	t.Helper()
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write "+file+" and commit it", nil)
	session.CreateSubagentTranscript(agentID, []FileChange{{Path: file, Content: "Written.\n"}})

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, toolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	env.WriteFile(file, "Written.\n")
	env.GitCommitWithHooksAsAgent("Add "+file, file)
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("precondition: the subagent's commit should carry an Entire-Checkpoint trailer")
	}

	testutil.RunGit(t, env.RepoDir, "update-ref", hiddenCheckpointRef, "refs/heads/"+paths.MetadataBranchName)
	testutil.RunGit(t, env.RepoDir, "update-ref", "-d", "refs/heads/"+paths.MetadataBranchName)
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      toolUseID,
		AgentID:        agentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}
	return session, checkpointID
}

const hiddenCheckpointRef = "refs/hidden/checkpoints"

// restoreCheckpointBranch undoes subagentCommitWithUnavailableStore's hiding.
func restoreCheckpointBranch(t *testing.T, env *TestEnv) {
	t.Helper()
	testutil.RunGit(t, env.RepoDir, "update-ref", "refs/heads/"+paths.MetadataBranchName, hiddenCheckpointRef)
	testutil.RunGit(t, env.RepoDir, "update-ref", "-d", hiddenCheckpointRef)
}

// assertTaskTranscriptInCheckpoint fails unless checkpointID stores the task's
// subagent transcript and that transcript names file.
func assertTaskTranscriptInCheckpoint(t *testing.T, env *TestEnv, checkpointID, toolUseID, agentID, file string) {
	t.Helper()
	stored, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, toolUseID, "agent-"+agentID+".jsonl"))
	if !ok {
		t.Fatalf("subagent transcript for %s not stored under checkpoint %s's tasks/ subtree", toolUseID, checkpointID)
	}
	if !strings.Contains(stored, file) {
		t.Errorf("stored subagent transcript does not reference %q: %q", file, stored)
	}
	if _, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, toolUseID, "task.json")); !ok {
		t.Errorf("task.json for %s not stored under checkpoint %s", toolUseID, checkpointID)
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

// TestSubagentCheckpoints_CommitWhileIdleWithTaskRecord_LinksAndCondensesContent
// drives the real prepare-commit-msg + post-commit hooks for a background
// subagent's commit landing while the parent session is IDLE. On top of #2032's
// slow path the commit already linked via HasTaskContent; what this pins is that
// the link is backed by content — the commit's own condensation materializes the
// task record's transcript-so-far under the checkpoint's tasks/ subtree, so the
// trailer resolves to the subagent's real work rather than dangling. That is
// reachable only because idleWithLiveTaskRecord bypasses
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
		SessionID:      sess.ID,
		TranscriptPath: sess.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
		Background:     true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// Turn ends: the parent goes IDLE while the background subagent keeps
	// running. The record survives, still live. This is the shape
	// idleWithLiveTaskRecord exists for: an IDLE session whose background task is
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
	env.GitCommitWithHooksAsAgent("Add idle-marker doc", editedFile)

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

// TestSubagentCheckpoints_CommitAfterBackgroundTaskCompletes_LinksViaFiles pins
// that a completed task record still gets its work linked once it no longer
// bypasses the overlap check. The background subagent finishes after the
// parent's turn ended, leaving its edit uncommitted; completion merges the edit
// into FilesTouched, and that — not the record's presence — is what links the
// later commit and materializes the subagent's transcript.
func TestSubagentCheckpoints_CommitAfterBackgroundTaskCompletes_LinksViaFiles(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		tty  bool
	}{
		{name: "agent commit", tty: false},
		{name: "terminal commit", tty: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			sess := env.NewSession()
			sess.CreateTranscript("delegate a background task", nil)

			const (
				taskToolUseID = "toolu_01CompletedThenCommit"
				subagentID    = "e4444555566667777"
				editedFile    = "docs/completed.md"
				editedContent = "# Completed\n\nWritten by a background subagent that has since finished.\n"
			)

			if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
			}
			if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
				t.Fatalf("SimulatePreTask failed: %v", err)
			}
			if err := env.SimulatePostTask(PostTaskInput{
				SessionID:      sess.ID,
				TranscriptPath: sess.TranscriptPath,
				ToolUseID:      taskToolUseID,
				AgentID:        subagentID,
				Background:     true,
			}); err != nil {
				t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
			}
			if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
				t.Fatalf("SimulateStop failed: %v", err)
			}

			subagentTranscript := sess.CreateSubagentTranscript(subagentID, []FileChange{
				{Path: editedFile, Content: editedContent},
			})
			env.WriteFile(editedFile, editedContent)
			if err := env.SimulateSubagentStop(SubagentStopInput{
				SessionID:           sess.ID,
				TranscriptPath:      sess.TranscriptPath,
				AgentID:             subagentID,
				AgentTranscriptPath: subagentTranscript,
			}); err != nil {
				t.Fatalf("SimulateSubagentStop failed: %v", err)
			}

			state, err := env.GetSessionState(sess.ID)
			if err != nil || state == nil {
				t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
			}
			if state.Phase != session.PhaseIdle || len(state.LiveTaskRecords()) != 0 || !containsFile(state.FilesTouched, editedFile) {
				t.Fatalf("precondition: want IDLE session with only a completed record and %s in FilesTouched, got phase=%s files=%v records=%+v",
					editedFile, state.Phase, state.FilesTouched, state.TaskRecords)
			}

			if tt.tty {
				env.GitCommitWithHooks("Add completed doc", editedFile)
			} else {
				env.GitCommitWithHooksAsAgent("Add completed doc", editedFile)
			}

			checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
			if checkpointID == "" {
				t.Fatalf("commit of a completed subagent's files should carry an Entire-Checkpoint trailer")
			}
			storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
				CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
			if !ok {
				t.Fatalf("subagent transcript not materialized under the checkpoint's tasks/ subtree")
			}
			if !strings.Contains(storedTranscript, editedFile) {
				t.Errorf("materialized subagent transcript does not reference %q", editedFile)
			}
		})
	}
}

// TestSubagentCheckpoints_BackgroundTaskCompletesMidTurn_ParentCommitLinks pins
// the ordering from issue #2653: a background subagent appends to a tracked
// file and its completion arrives while the parent is still mid-turn, and the
// parent commits in that same turn, before any Stop. The commit must link to the
// session through the subagent's file and carry the subagent's transcript.
func TestSubagentCheckpoints_BackgroundTaskCompletesMidTurn_ParentCommitLinks(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	const (
		taskToolUseID = "toolu_01CompletesMidTurn"
		subagentID    = "f5555666677778888"
		editedFile    = "notes.txt"
		baseContent   = "base\n"
		editedContent = "base\nadded by the background subagent\n"
	)
	env.WriteFile(editedFile, baseContent)
	env.GitAdd(editedFile)
	env.GitCommit("Add notes")

	sess := env.NewSession()
	sess.CreateTranscript("delegate a background task, then commit", nil)

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      sess.ID,
		TranscriptPath: sess.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
		Background:     true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	subagentTranscript := sess.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: editedContent},
	})
	env.WriteFile(editedFile, editedContent)
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           sess.ID,
		TranscriptPath:      sess.TranscriptPath,
		AgentID:             subagentID,
		AgentTranscriptPath: subagentTranscript,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
	}
	if !state.Phase.IsActive() || len(state.LiveTaskRecords()) != 0 {
		t.Fatalf("precondition: want an ACTIVE parent with only a completed record, got phase=%s records=%+v",
			state.Phase, state.TaskRecords)
	}

	env.GitCommitWithHooksAsAgent("Update notes", editedFile)

	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatalf("parent's mid-turn commit of the subagent's file should carry an Entire-Checkpoint trailer")
	}
	metadataContent, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	if !ok {
		t.Fatalf("session metadata missing for checkpoint %s", checkpointID)
	}
	var metadata checkpoint.Metadata
	if err := json.Unmarshal([]byte(metadataContent), &metadata); err != nil {
		t.Fatalf("parse session metadata: %v", err)
	}
	if metadata.SessionID != sess.ID {
		t.Errorf("checkpoint session = %q, want %q", metadata.SessionID, sess.ID)
	}
	if !containsFile(metadata.FilesTouched, editedFile) {
		t.Errorf("checkpoint files_touched = %v, want it to contain %s", metadata.FilesTouched, editedFile)
	}
	storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
	if !ok {
		t.Fatalf("subagent transcript not materialized under the checkpoint's tasks/ subtree")
	}
	if !strings.Contains(storedTranscript, editedFile) {
		t.Errorf("materialized subagent transcript does not reference %q", editedFile)
	}

	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	state, err = env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
	}
	if containsFile(state.FilesTouched, editedFile) {
		t.Errorf("%s still pending after it was committed: %v", editedFile, state.FilesTouched)
	}
}

// TestSubagentCheckpoints_JointCommitWithRunningSubagent_KeepsBothSessions pins
// that the read-only gate does not drop a co-author. A background subagent is
// still running under an IDLE parent, so its edit is not in the parent's
// FilesTouched yet; a second session commits mid-turn, and the one commit
// carries both sessions' files. The second session's transcript claim arms the
// read-only gate, which must find the running subagent's edit in its own
// transcript before treating the parent as read-only.
func TestSubagentCheckpoints_JointCommitWithRunningSubagent_KeepsBothSessions(t *testing.T) {
	t.Parallel()
	jointCommitWithRunningSubagent(t, NewFeatureBranchEnv(t), nil)
}

// jointCommitWithRunningSubagent runs the joint-commit scenario above;
// beforeCommit, when set, adjusts the IDLE parent's state just before the
// commit.
func jointCommitWithRunningSubagent(t *testing.T, env *TestEnv, beforeCommit func(parentID string)) {
	t.Helper()
	const (
		taskToolUseID = "toolu_01JointCommit"
		subagentID    = "f4444555566667777"
		subagentFile  = "docs/joint.md"
		subagentBody  = "# Joint\n\nWritten by a background subagent that is still running.\n"
		codingFile    = "feature.go"
		codingBody    = "package main\n\nfunc Feature() {}\n"
	)

	parent := idleParentWithRunningSubagent(t, env, taskToolUseID, subagentID, subagentFile, subagentBody)

	coding := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(coding.ID, coding.TranscriptPath); err != nil {
		t.Fatalf("coding user-prompt-submit failed: %v", err)
	}
	env.WriteFile(codingFile, codingBody)
	coding.CreateTranscript("Add the feature and commit everything", []FileChange{{Path: codingFile, Content: codingBody}})

	if beforeCommit != nil {
		beforeCommit(parent.ID)
	}
	env.GitCommitWithHooksAsAgent("Add feature and joint doc", codingFile, subagentFile)

	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("joint commit should carry an Entire-Checkpoint trailer")
	}
	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(checkpointID))
	if !found {
		t.Fatalf("CheckpointSummary not found for %s", checkpointID)
	}
	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summaryContent), &summary); err != nil {
		t.Fatalf("Failed to parse CheckpointSummary: %v", err)
	}
	env.AssertCheckpointContainsSession(t, summary, coding.ID)
	env.AssertCheckpointContainsSession(t, summary, parent.ID)

	storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
	if !ok {
		t.Fatalf("running subagent's transcript not materialized under the checkpoint's tasks/ subtree")
	}
	if !strings.Contains(storedTranscript, subagentFile) {
		t.Errorf("materialized subagent transcript does not reference %q", subagentFile)
	}
}

// idleParentWithRunningSubagent starts a session that delegated a background
// task and went IDLE; the subagent wrote file and is still running, so the
// file is not yet in the parent's FilesTouched.
func idleParentWithRunningSubagent(t *testing.T, env *TestEnv, taskToolUseID, subagentID, subagentFile, subagentBody string) *Session {
	t.Helper()
	parent := env.NewSession()
	parent.CreateTranscript("delegate a background task", nil)
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("parent user-prompt-submit failed: %v", err)
	}
	if err := env.SimulatePreTask(parent.ID, parent.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("parent pre-task failed: %v", err)
	}
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      parent.ID,
		TranscriptPath: parent.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
		Background:     true,
	}); err != nil {
		t.Fatalf("parent post-task (background stub) failed: %v", err)
	}
	if err := env.SimulateStop(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("parent stop failed: %v", err)
	}
	// The subagent writes its file and keeps running: no SubagentStop.
	parent.CreateSubagentTranscript(subagentID, []FileChange{{Path: subagentFile, Content: subagentBody}})
	env.WriteFile(subagentFile, subagentBody)

	parentState, err := env.GetSessionState(parent.ID)
	if err != nil || parentState == nil {
		t.Fatalf("GetSessionState for parent failed: %v (state=%v)", err, parentState)
	}
	if parentState.Phase != session.PhaseIdle || len(parentState.FilesTouched) != 0 || !hasLiveTaskRecord(parentState, taskToolUseID) {
		t.Fatalf("precondition: want IDLE parent with a live record and empty FilesTouched, got phase=%s files=%v records=%+v",
			parentState.Phase, parentState.FilesTouched, parentState.TaskRecords)
	}
	return parent
}
