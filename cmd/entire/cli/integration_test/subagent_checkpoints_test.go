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

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
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
	env := NewFeatureBranchEnv(t)
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

	// Incremental checkpoints (PostTodo) still live on the shadow branch.
	verifyIncrementalCheckpointStorage(t, env, session.ID, taskToolUseID)

	// PostTask completes a durable task record on session state (#2058) —
	// no final shadow task step is written anymore.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if rec == nil || rec.CompletedAt.IsZero() {
		t.Fatalf("expected a completed task record for %s after PostTask, got %+v", taskToolUseID, rec)
	}
	if !containsFile(rec.Files, "feature.go") || !containsFile(rec.Files, "feature_test.go") {
		t.Errorf("the completed record must carry the task's files, got %v", rec.Files)
	}
	if !containsFile(state.FilesTouched, "feature.go") || !containsFile(state.FilesTouched, "feature_test.go") {
		t.Errorf("task files must merge into FilesTouched, got %v", state.FilesTouched)
	}
}

// TestSubagentCheckpoints_NoFileChanges tests that PostTodo is skipped when no file changes
func TestSubagentCheckpoints_NoFileChanges(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
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
}

// TestSubagentCheckpoints_PostTaskNoFileChanges tests that PostTask is skipped when no file changes
// and the pre-task state is still cleaned up.
func TestSubagentCheckpoints_PostTaskNoFileChanges(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Create a session
	session := env.NewSession()

	// Create transcript (no file changes in transcript either)
	session.CreateTranscript("Quick task with no file changes", []FileChange{})

	// Simulate user prompt submit
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create pre-task file to simulate subagent context
	taskToolUseID := "toolu_01TaskNoFileChanges"
	err = env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID)
	if err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Verify pre-task file was created
	preTaskFile := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-task-"+taskToolUseID+".json")
	if _, err := os.Stat(preTaskFile); os.IsNotExist(err) {
		t.Fatal("pre-task file should exist after SimulatePreTask")
	}

	// Get git log before PostTask
	beforeCommits := env.GetGitLog()

	// Call PostTask WITHOUT making any file changes
	err = env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        "agent-no-changes",
	})
	if err != nil {
		t.Fatalf("SimulatePostTask should not fail: %v", err)
	}

	// Get git log after PostTask
	afterCommits := env.GetGitLog()

	// Verify no new commits were created on the main branch
	if len(afterCommits) != len(beforeCommits) {
		t.Errorf("Expected no new commits when no file changes, before=%d after=%d", len(beforeCommits), len(afterCommits))
	}

	// Verify pre-task file is cleaned up even though no checkpoint was created
	if _, err := os.Stat(preTaskFile); !os.IsNotExist(err) {
		t.Error("Pre-task file should be removed after PostTask even with no file changes")
	}
}

// TestSubagentCheckpoints_NoPreTaskFile tests that PostTodo is a no-op
// when there's no active pre-task file (main agent context).
func TestSubagentCheckpoints_NoPreTaskFile(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
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
}

// verifyIncrementalCheckpointStorage verifies PostTodo's incremental
// checkpoints are stored in the shadow branch git tree (the surviving shadow
// task write; final captures are task records on session state).
func verifyIncrementalCheckpointStorage(t *testing.T, env *TestEnv, sessionID, taskToolUseID string) {
	t.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	shadowBranchName := env.GetShadowBranchName()
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranchName), true)
	if err != nil {
		t.Fatalf("shadow branch %s not found: %v", shadowBranchName, err)
	}
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		t.Fatalf("failed to get shadow commit: %v", err)
	}
	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get shadow tree: %v", err)
	}

	checkpointsPrefix := ".entire/metadata/" + sessionID + "/tasks/" + taskToolUseID + "/checkpoints/"
	foundCheckpointFiles := 0
	err = shadowTree.Files().ForEach(func(f *object.File) error {
		if strings.HasPrefix(f.Name, checkpointsPrefix) && strings.HasSuffix(f.Name, ".json") {
			foundCheckpointFiles++
			content, readErr := f.Contents()
			if readErr != nil {
				t.Errorf("failed to read checkpoint file %s: %v", f.Name, readErr)
				return nil
			}
			var cp strategy.SubagentCheckpoint
			if jsonErr := json.Unmarshal([]byte(content), &cp); jsonErr != nil {
				t.Errorf("checkpoint file %s is invalid JSON: %v", f.Name, jsonErr)
			}
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

	if foundCheckpointFiles == 0 {
		t.Errorf("expected incremental checkpoint files under %s", checkpointsPrefix)
	}
}

// containsFile reports whether files contains path.
func containsFile(files []string, path string) bool {
	for _, f := range files {
		if f == path {
			return true
		}
	}
	return false
}

// hasTaskRecord reports whether state has a record — live or completed — for
// toolUseID.
func hasTaskRecord(state *strategy.SessionState, toolUseID string) bool {
	for _, task := range state.TaskRecords {
		if task.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}

// hasLiveTaskRecord reports whether state has a still-in-flight (uncompleted)
// record for toolUseID. Unlike the prior claim-and-remove model, a completed
// record now persists (for the future condensation materializer) rather than
// being deleted — so "the marker was cleared" is "no longer live", not "no
// longer present". See session.TaskRecord and State.CompleteTaskRecord.
func hasLiveTaskRecord(state *strategy.SessionState, toolUseID string) bool {
	for _, task := range state.LiveTaskRecords() {
		if task.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}

// TestSubagentCheckpoints_BackgroundLaunch_DefersToSubagentStop covers the
// background-subagent bug this PR fixes: Claude Code background subagents
// (run_in_background: true) return a launch stub immediately, so post-task
// (PostToolUse) used to fire seconds after launch — before the subagent had
// done any real work — and save (or skip) a task step from that stub alone.
// The real completion signal, SubagentStop, fired no hook entire listened to,
// so everything the subagent actually did was invisible to entire.
//
// This verifies the fix end to end, using a realistic Claude Code subagent
// transcript (session.CreateSubagentTranscript — the same builder
// TestSubagentCheckpoints_StoresSubagentTranscript uses) so the real
// transcript analyzer, not a stub, is what extracts the modified file:
//  1. post-task with run_in_background: true records an in-flight marker and
//     completes nothing — capture is deferred, not lost.
//  2. subagent-stop (the authoritative final capture) completes the record
//     with the subagent's real modified file and declared transcript path.
//  3. the next commit's condensation materializes the record's transcript
//     under the permanent checkpoint's tasks/ subtree (#2058's pointer model,
//     anchored through the real hook pipeline).
func TestSubagentCheckpoints_BackgroundLaunch_DefersToSubagentStop(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("delegate a background task", nil)

	const (
		taskToolUseID = "toolu_01BackgroundABC123"
		subagentID    = "a1111222233334444"
		editedFile    = "docs/background.md"
	)

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Launch stub: PostToolUse fires immediately with run_in_background: true
	// and the launch-assigned agentId, before the subagent has done any work.
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:       session.ID,
		TranscriptPath:  session.TranscriptPath,
		ToolUseID:       taskToolUseID,
		AgentID:         subagentID,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// Marker recorded; nothing completed from the stub.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected in-flight marker for %s after background launch stub, state=%+v", taskToolUseID, state)
	}

	// The subagent does its actual work: a realistic transcript (parsed by the
	// real Claude Code transcript analyzer, not a stub) plus the resulting
	// uncommitted file.
	subagentTranscriptPath := session.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: "# Background\n"},
	})
	env.WriteFile(editedFile, "# Background\n\nWritten by a background subagent.\n")

	// The real completion signal.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           session.ID,
		TranscriptPath:      session.TranscriptPath,
		AgentID:             subagentID,
		AgentTranscriptPath: subagentTranscriptPath,
		ToolUseID:           taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	// The record is completed with the analyzer-extracted file and the
	// declared transcript path, and the file joins the session's FilesTouched.
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("in-flight marker for %s should be completed after subagent-stop, state=%+v", taskToolUseID, state)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if rec == nil {
		t.Fatalf("expected a completed task record for %s", taskToolUseID)
	}
	if !containsFile(rec.Files, editedFile) {
		t.Errorf("the completed record must carry the subagent's real modified file, got %v", rec.Files)
	}
	if rec.DeclaredTranscriptPath != subagentTranscriptPath {
		t.Errorf("the completed record must carry the declared transcript path, got %q", rec.DeclaredTranscriptPath)
	}
	if !containsFile(state.FilesTouched, editedFile) {
		t.Errorf("the subagent's file must merge into FilesTouched, got %v", state.FilesTouched)
	}

	// The next commit condenses the session; the materializer must store the
	// record's transcript under the checkpoint's tasks/ subtree — the #2058
	// end-to-end guarantee this whole pipeline exists for.
	env.GitCommitWithShadowHooksAsAgent("Add background doc", editedFile)
	checkpointID := env.TryGetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("expected a condensed checkpoint after committing the subagent's work")
	}
	storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
	if !ok {
		t.Fatalf("subagent transcript not materialized under the checkpoint's tasks/ subtree")
	}
	if !strings.Contains(storedTranscript, editedFile) {
		t.Errorf("materialized subagent transcript does not reference the modified file %q: %q", editedFile, storedTranscript)
	}
}

// TestSubagentCheckpoints_TurnEnd_ThenSubagentStop covers turn-end (Stop)
// landing between a background launch stub and the eventual subagent-stop:
// the retired incremental backstop must NOT resurface (no shadow-tree task
// write), the in-flight marker must survive the turn untouched, and
// subagent-stop remains the authoritative capture that completes the record —
// in-flight coverage now comes from condensation materializing the record's
// transcript-so-far, not from turn-end snapshots.
func TestSubagentCheckpoints_TurnEnd_ThenSubagentStop(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("delegate a background task", nil)

	const (
		taskToolUseID = "toolu_01TurnEndBackstop"
		subagentID    = "b2222333344445555"
		editedFile    = "docs/turnend.md"
	)

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:       session.ID,
		TranscriptPath:  session.TranscriptPath,
		ToolUseID:       taskToolUseID,
		AgentID:         subagentID,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// The subagent has made progress by the time the parent's turn ends, but
	// SubagentStop hasn't arrived yet.
	subagentTranscriptPath := session.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: "# In progress\n"},
	})
	env.WriteFile(editedFile, "# In progress\n\nStill running.\n")

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (turn-end) failed: %v", err)
	}

	// No incremental task checkpoint: the turn-end backstop is retired.
	shadowBranch := env.GetShadowBranchName()
	incrementalPath := paths.EntireMetadataDir + "/" + session.ID + "/tasks/" + taskToolUseID +
		"/checkpoints/001-" + taskToolUseID + ".json"
	if env.FileExistsInBranch(shadowBranch, incrementalPath) {
		t.Fatalf("turn-end must not write shadow-tree task checkpoints anymore, found %s", incrementalPath)
	}

	// The marker survives: the task is still running, and subagent-stop
	// remains the authoritative final capture.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected in-flight marker for %s to survive turn-end, state=%+v", taskToolUseID, state)
	}

	// subagent-stop arrives: the authoritative final capture.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           session.ID,
		TranscriptPath:      session.TranscriptPath,
		AgentID:             subagentID,
		AgentTranscriptPath: subagentTranscriptPath,
		ToolUseID:           taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	// Final completion landed. The full final-capture assertions (declared
	// transcript path, materialized transcript) live in
	// TestSubagentCheckpoints_BackgroundLaunch_DefersToSubagentStop.
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || hasLiveTaskRecord(state, taskToolUseID) {
		t.Errorf("in-flight marker for %s should be completed after subagent-stop", taskToolUseID)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if rec == nil || !containsFile(rec.Files, editedFile) {
		t.Errorf("the completed record must carry the subagent's file, got %+v", rec)
	}
}

// TestSubagentCheckpoints_ForegroundDoubleFire_CapturesOnce: a foreground task
// (no run_in_background) completes immediately at post-task time — its record
// is created-on-completion, already completed. Claude Code fires SubagentStop
// for every completed Task, foreground and background alike, so entire also
// sees a SubagentStop for this same tool_use_id after the foreground
// completion already ran. Without the exactly-once completion guard, that
// second event would re-run the capture. This verifies the regression stays
// closed: the record completes exactly once and the SubagentStop double-fire
// produces no additional commit.
func TestSubagentCheckpoints_ForegroundDoubleFire_CapturesOnce(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("run a foreground task", nil)

	const (
		taskToolUseID = "toolu_01ForegroundDoubleFire"
		subagentID    = "c3333444455556666"
		editedFile    = "docs/foreground.md"
	)

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	env.WriteFile(editedFile, "# Foreground\n\nWritten by a foreground subagent.\n")

	// Foreground completion: PostToolUse fires with no run_in_background, so
	// this is captured immediately — the existing, unchanged behavior.
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	// Foreground tasks complete their record at post-task time.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || !hasTaskRecord(state, taskToolUseID) || hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("foreground post-task must leave a COMPLETED record, state=%+v", state)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if !containsFile(rec.Files, editedFile) {
		t.Fatalf("the foreground record must carry the task's file, got %v", rec.Files)
	}
	firstCompletedAt := rec.CompletedAt
	commitsAfterPostTask := env.GetGitLog()

	// The double-fire: SubagentStop for the same tool_use_id, with the record
	// already completed. Must be a no-op, not a second capture.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		AgentID:        subagentID,
		ToolUseID:      taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	commitsAfterSubagentStop := env.GetGitLog()
	if len(commitsAfterSubagentStop) != len(commitsAfterPostTask) {
		t.Errorf("SubagentStop double-fire created a new commit: before=%d after=%d",
			len(commitsAfterPostTask), len(commitsAfterSubagentStop))
	}

	// Still exactly one completion.
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	rec = state.FindTaskRecord(taskToolUseID)
	if rec == nil || !rec.CompletedAt.Equal(firstCompletedAt) {
		t.Errorf("the double-fire must not re-complete the record, got %+v", rec)
	}
}
