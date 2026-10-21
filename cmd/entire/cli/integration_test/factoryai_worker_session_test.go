//go:build integration

package integration

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// Factory AI Droid dispatches Workers as detached sessions: the parent's
// PostToolUse hook fires when the Worker is launched, before it has touched the
// worktree, and the Worker then runs its own SessionStart/UserPromptSubmit/Stop
// cycle. These tests pin the resulting attribution — a Worker's turn must land
// as a task record on the parent, not as an unrelated top-level session.

// TestFactoryDroidWorkerSessionBecomesTaskCheckpoint covers the regression that
// broke the E2E TestFactoryTaskRecordExistsBeforeCommit: a Worker's work was
// recorded, but under its own session ID, so the parent had nothing
// attributing it to the task.
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

	// The work belongs to the parent's task, not to the Worker's own session:
	// a COMPLETED task record on the PARENT, keyed by the launch tool use,
	// carrying the Worker's files and declaring its transcript path for the
	// materializer (create-if-missing parent state is part of the guarantee).
	state, err := env.GetSessionState(parent.ID)
	if err != nil {
		t.Fatalf("GetSessionState(parent) failed: %v", err)
	}
	if state == nil {
		t.Fatal("parent session state must exist after the Worker's turn (create-if-missing guarantee)")
	}
	rec := state.FindTaskRecord(toolUseID)
	if rec == nil || rec.CompletedAt.IsZero() {
		t.Fatalf("expected a completed task record on the parent for %s, got %+v", toolUseID, rec)
	}
	if rec.AgentID != worker.ID {
		t.Errorf("the Worker's session ID must double as the record's agent ID: got %q, want %q", rec.AgentID, worker.ID)
	}
	if rec.DeclaredTranscriptPath != worker.TranscriptPath {
		t.Errorf("the record must declare the Worker's transcript path: got %q, want %q",
			rec.DeclaredTranscriptPath, worker.TranscriptPath)
	}
	if !containsFile(rec.Files, "docs/summary.md") {
		t.Errorf("the record must carry the Worker's files, got %v", rec.Files)
	}
	if !containsFile(state.FilesTouched, "docs/summary.md") {
		t.Errorf("the Worker's files must merge into the parent's FilesTouched, got %v", state.FilesTouched)
	}

	// A turn-end step on the Worker's own session would misattribute the work
	// to a session the user never drove — the exact shape of the regression.
	if workerState, err := env.GetSessionState(worker.ID); err != nil {
		t.Fatalf("GetSessionState(worker) failed: %v", err)
	} else if workerState != nil && workerState.StepCount != 0 {
		t.Errorf("a Worker's turn must write a task record, not a step on its own session (StepCount=%d)", workerState.StepCount)
	}
	env.AssertNoShadowBranches()

	// Multi-turn Workers upsert into the SAME record: a second turn must MERGE
	// its files with turn 1's (not overwrite them) and re-declare the
	// transcript path, with the record staying completed throughout.
	t.Run("second worker turn merges files", func(t *testing.T) {
		t.Parallel()
		if err := env.SimulateFactoryDroidUserPromptSubmit(worker.ID); err != nil {
			t.Fatalf("worker second UserPromptSubmit failed: %v", err)
		}
		env.WriteFile("docs/details.md", "More detail.\n")
		// Turn 2's transcript names ONLY the new file, so turn 1's file can
		// reappear on the record only via the upsert's merge.
		worker.CreateDroidTranscript("# Task Tool Invocation", []FileChange{
			{Path: "docs/details.md", Content: "More detail.\n"},
		})
		worker.MarkAsWorkerSession(parent.ID, toolUseID, "worker: Summarize the repo")
		if err := env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath); err != nil {
			t.Fatalf("worker second Stop failed: %v", err)
		}

		state, err := env.GetSessionState(parent.ID)
		if err != nil {
			t.Fatalf("GetSessionState(parent) failed: %v", err)
		}
		rec := state.FindTaskRecord(toolUseID)
		if rec == nil || rec.CompletedAt.IsZero() {
			t.Fatalf("the record must stay completed across Worker turns, got %+v", rec)
		}
		if !containsFile(rec.Files, "docs/summary.md") || !containsFile(rec.Files, "docs/details.md") {
			t.Errorf("a second Worker turn must merge files with turn 1's, got %v", rec.Files)
		}
		if !containsFile(state.FilesTouched, "docs/summary.md") || !containsFile(state.FilesTouched, "docs/details.md") {
			t.Errorf("both turns' files must be in the parent's FilesTouched, got %v", state.FilesTouched)
		}
		if rec.DeclaredTranscriptPath != worker.TranscriptPath {
			t.Errorf("turn 2 must re-declare the Worker transcript path, got %q", rec.DeclaredTranscriptPath)
		}
	})
}

// TestFactoryDroidReadOnlyWorkerReachesParentCheckpoint covers a Worker turn
// that changes no files: it must still become a completed task record on the
// parent, so its transcript and tokens reach the parent's next checkpoint,
// with the tokens on the task once and never on the Worker's own session.
func TestFactoryDroidReadOnlyWorkerReachesParentCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	parent := env.NewFactoryDroidSession()
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmit(parent.ID))

	// A read-only Worker: it reads and reports, consuming tokens, and writes
	// nothing.
	const toolUseID = "toolu_reader_01"
	worker := env.NewFactoryDroidSession()
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmit(worker.ID))
	appendLines(t, worker.TranscriptPath,
		`{"type":"message","id":"w1","message":{"role":"user","content":[{"type":"text","text":"Summarize the repo"}]}}`,
		`{"type":"message","id":"w2","message":{"role":"assistant","id":"msg_w1","content":[{"type":"text","text":"It is a test repository."}],"usage":{"input_tokens":70,"output_tokens":7}}}`)
	worker.MarkAsWorkerSession(parent.ID, toolUseID, "worker: Summarize the repo")

	// Twice: a repeated Stop must not count the Worker's usage again.
	require.NoError(t, env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath))
	require.NoError(t, env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath))

	state, err := env.GetSessionState(parent.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec, "a read-only Worker turn must still record a task on the parent")
	require.False(t, rec.CompletedAt.IsZero(), "the record must be completed")
	require.Empty(t, rec.Files, "a read-only Worker touches no files")
	require.Equal(t, worker.TranscriptPath, rec.DeclaredTranscriptPath)
	require.NotNil(t, rec.TokenUsage, "the record must carry the Worker's usage")
	require.Equal(t, 70, rec.TokenUsage.InputTokens, "the Worker's usage, once")
	require.Equal(t, 7, rec.TokenUsage.OutputTokens, "the Worker's usage, once")

	if workerState, err := env.GetSessionState(worker.ID); err != nil {
		t.Fatalf("GetSessionState(worker) failed: %v", err)
	} else if workerState != nil {
		require.Zero(t, workerState.StepCount, "a Worker's turn must not step its own session")
		if workerState.TokenUsage != nil {
			require.Zero(t, workerState.TokenUsage.InputTokens, "the Worker's usage belongs to the parent's task, not its own session")
		}
	}

	// The parent's own turn changes a file; committing it condenses the
	// parent, Worker task included.
	env.WriteFile("feature.go", "package main\n")
	parent.CreateDroidTranscript("Add a feature", []FileChange{{Path: "feature.go", Content: "package main\n"}})
	require.NoError(t, env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath))
	env.GitCommitWithHooks("Add feature", "feature.go")
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "expected a checkpoint for the parent's commit")

	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID, "task.json"))
	require.True(t, ok, "the read-only Worker's task must be materialized in the parent's checkpoint")
	var task checkpoint.TaskRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &task))
	require.Equal(t, worker.ID, task.AgentID)
	require.NotNil(t, task.TokenUsage, "the stored task must carry the Worker's usage: %s", raw)
	require.Equal(t, 70, task.TokenUsage.InputTokens, "the stored task holds the Worker's usage once: %s", raw)
	require.Equal(t, 7, task.TokenUsage.OutputTokens, "the stored task holds the Worker's usage once: %s", raw)

	transcript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, toolUseID, paths.AgentTranscriptFileName(worker.ID)))
	require.True(t, ok, "the Worker's transcript must be stored with its task")
	require.Contains(t, transcript, "It is a test repository.")

	if usage := readCommittedTokenUsage(t, env, checkpointID); usage != nil {
		require.NotEqual(t, 70, usage.InputTokens, "the parent session's own usage must not absorb the Worker's")
	}

	// A second Worker turn after that commit: the next checkpoint's task must
	// hold only the second turn, not the first again.
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmit(parent.ID))
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmitWithTranscript(worker.ID, worker.TranscriptPath))
	appendLines(t, worker.TranscriptPath,
		`{"type":"message","id":"w3","message":{"role":"user","content":[{"type":"text","text":"And the tests?"}]}}`,
		`{"type":"message","id":"w4","message":{"role":"assistant","id":"msg_w2","content":[{"type":"text","text":"There are none."}],"usage":{"input_tokens":30,"output_tokens":3}}}`)
	require.NoError(t, env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath))

	env.WriteFile("feature2.go", "package main\n")
	parent.CreateDroidTranscript("Add another feature", []FileChange{{Path: "feature2.go", Content: "package main\n"}})
	require.NoError(t, env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath))
	env.GitCommitWithHooks("Add feature 2", "feature2.go")
	checkpoint2 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpoint2)
	require.NotEqual(t, checkpointID, checkpoint2, "expected a second checkpoint")

	raw, ok = env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpoint2, toolUseID, "task.json"))
	require.True(t, ok, "the second Worker turn must be materialized in the second checkpoint")
	var task2 checkpoint.TaskRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &task2))
	require.NotNil(t, task2.TokenUsage, "the second stored task must carry the Worker's usage: %s", raw)
	require.Equal(t, 30, task2.TokenUsage.InputTokens, "the first Worker turn is already in the first checkpoint: %s", raw)
	require.Equal(t, 3, task2.TokenUsage.OutputTokens, "the first Worker turn is already in the first checkpoint: %s", raw)
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

	// An ordinary turn-end step on its own session...
	state := env.AssertTurnEndRecorded(session.ID, "feature.go")
	// ...not diverted into a task record.
	if len(state.TaskRecords) != 0 {
		t.Errorf("a top-level session must not be recorded as a task, got %+v", state.TaskRecords)
	}
}
