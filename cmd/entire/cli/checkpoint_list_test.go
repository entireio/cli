package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestPendingCheckpointJSON_MatchesRewindListContract pins the machine-readable
// shape emitted by `checkpoint list --pending --json`. It must stay byte-for-byte
// compatible with the JSON `rewind --list` historically produced, so consumers that
// parsed rewind --list keep working after repointing. Field names, omitempty
// behavior, and the RFC3339 date encoding are the contract.
func TestPendingCheckpointJSON_MatchesRewindListContract(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)

	// A logs-only (committed) point: has condensation_id, session_id,
	// session_prompt; tool_use_id is empty and must be omitted.
	logsOnly := pendingCheckpointJSON{
		ID:               "abc123def456",
		Message:          "user commit",
		MetadataDir:      ".entire/metadata/s1",
		Date:             fixed.Format(time.RFC3339),
		IsTaskCheckpoint: false,
		IsLogsOnly:       true,
		CondensationID:   "deadbeefcafe",
		SessionID:        "s1",
		SessionPrompt:    "do the thing",
	}
	// A task-record (uncommitted) point: has tool_use_id; condensation_id,
	// session_id, session_prompt are empty and must be omitted. metadata_dir and
	// the two bools have no omitempty and must always render.
	task := pendingCheckpointJSON{
		ID:               "0f1e2d3c",
		Message:          "task step",
		MetadataDir:      "",
		Date:             fixed.Format(time.RFC3339),
		IsTaskCheckpoint: true,
		ToolUseID:        "toolu_123",
		IsLogsOnly:       false,
	}

	data, err := jsonutil.MarshalIndentWithNewline([]pendingCheckpointJSON{logsOnly, task}, "", "  ")
	require.NoError(t, err)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	require.Len(t, got, 2)

	// logs-only point: exact key set.
	require.ElementsMatch(t,
		[]string{"id", "message", "metadata_dir", "date", "is_task_checkpoint", "is_logs_only", "condensation_id", "session_id", "session_prompt"},
		keysOf(got[0]),
		"logs-only point keys must match the rewind --list contract (tool_use_id omitted when empty)")
	require.Equal(t, "abc123def456", got[0]["id"])
	require.Equal(t, "deadbeefcafe", got[0]["condensation_id"])
	require.Equal(t, fixed.Format(time.RFC3339), got[0]["date"])

	// task point: tool_use_id present; condensation_id/session_id/session_prompt
	// omitted; metadata_dir + bools always present.
	require.ElementsMatch(t,
		[]string{"id", "message", "metadata_dir", "date", "is_task_checkpoint", "tool_use_id", "is_logs_only"},
		keysOf(got[1]),
		"task point keys must match the rewind --list contract")
	require.Equal(t, "toolu_123", got[1]["tool_use_id"])
	require.Equal(t, true, got[1]["is_task_checkpoint"])
	require.Empty(t, got[1]["metadata_dir"])
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestRunCheckpointPendingList_EmptyReturnsEmptyArray verifies the pending JSON
// view emits `[]` (not `null`) when there are no pending checkpoints — the drop-in
// contract downstream JSON parsers rely on.
func TestRunCheckpointPendingList_EmptyReturnsEmptyArray(t *testing.T) {
	setupCheckpointListRepo(t)

	var stdout bytes.Buffer
	require.NoError(t, runCheckpointPendingListJSON(context.Background(), &stdout))
	// Byte-for-byte match with the historical `rewind --list` output: an empty
	// array, and the trailing double newline (MarshalIndentWithNewline appends
	// one, Fprintln another). Consumers parse via json.Unmarshal, which tolerates
	// trailing whitespace; the exactness protects the drop-in contract.
	require.Equal(t, "[]\n\n", stdout.String())
	require.Equal(t, "[]", strings.TrimSpace(stdout.String()))
}

// TestRunCheckpointPendingListHuman_Empty pins the human-view message shown when
// no pending checkpoints exist.
func TestRunCheckpointPendingListHuman_Empty(t *testing.T) {
	setupCheckpointListRepo(t)

	var stdout bytes.Buffer
	require.NoError(t, runCheckpointPendingListHuman(context.Background(), &stdout))
	require.Contains(t, stdout.String(), "No pending checkpoints found.")
	require.Contains(t, stdout.String(), "becomes a checkpoint when you commit it",
		"turn ends create no checkpoint; the footer must say commits do")
	require.NotContains(t, stdout.String(), "created automatically during active agent sessions")
}

// TestCheckpointListCmd_Routing drives the real `checkpoint list` command
// end-to-end and asserts each flag combination routes to the right
// dataset/renderer. The repo is seeded with a committed checkpoint, which the
// condensed dataset lists by checkpoint ID and the pending dataset lists as a
// logs-only resume point keyed by the commit — the two shapes differ, which is
// what the assertions pin.
func TestCheckpointListCmd_Routing(t *testing.T) {
	setupCheckpointListRepoWithCommittedCheckpoint(t)

	// --json → condensed dataset, branchCheckpointJSON shape.
	condensed := runListCmd(t, "--json")
	require.True(t, json.Valid([]byte(condensed)), "condensed --json must be valid JSON, got: %s", condensed)
	require.Contains(t, condensed, `"checkpoint_id"`, "condensed --json must use branchCheckpointJSON shape")
	require.NotContains(t, condensed, `"metadata_dir"`, "condensed --json must not carry pending-only fields")

	// --pending (human) → pending renderer: the commit's logs-only row.
	pendingHuman := runListCmd(t, "--pending")
	require.Contains(t, pendingHuman, "Second change",
		"--pending (human) must route to the pending human renderer")
	require.NotContains(t, pendingHuman, "No pending checkpoints found.")

	// --pending --json → pending JSON renderer (distinct from the human
	// renderer above and from the condensed dataset).
	pendingJSON := runListCmd(t, "--pending", "--json")
	require.True(t, json.Valid([]byte(pendingJSON)), "pending --json must be valid JSON, got: %s", pendingJSON)
	require.Contains(t, pendingJSON, `"condensation_id": "c1c2c3d4e5f6"`,
		"--pending --json must route to the pending JSON renderer")
	require.Contains(t, pendingJSON, `"is_logs_only": true`)
	require.NotContains(t, pendingJSON, `"checkpoint_id"`, "pending --json must never carry condensed-only fields")
}

// seedPendingPreviewSession records a session in the current worktree with
// turn-end work (two turns, two files, one running task record on HEAD) and
// two prompts in prompt.txt, the state `checkpoint list --pending` previews.
func seedPendingPreviewSession(t *testing.T, repoDir string) {
	t.Helper()
	ctx := context.Background()
	worktree, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)
	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	started := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	last := started.Add(30 * time.Minute)
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:           "2026-10-07-preview",
		AgentType:           agent.AgentTypeClaudeCode,
		BaseCommit:          head.Hash().String(),
		WorktreePath:        worktree,
		Phase:               session.PhaseIdle,
		StartedAt:           started,
		LastInteractionTime: &last,
		StepCount:           2,
		FilesTouched:        []string{"src/b.go", "src/a.go"},
		LastPrompt:          "add the tests",
		TaskRecords: []session.TaskRecord{{
			ToolUseID: "toolu_01ABCDEFGHIJKLMN", SubagentType: "Explore", TaskDescription: "find callers", StartedAt: started,
		}},
	}))
	promptDir := filepath.Join(repoDir, paths.SessionMetadataDirFromSessionID("2026-10-07-preview"))
	require.NoError(t, os.MkdirAll(promptDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(promptDir, paths.PromptFileName),
		[]byte("implement the parser\n\n---\n\nadd the tests"), 0o600))
}

// TestCheckpointListPending_PreviewText pins the human view of the
// next-checkpoint preview, rendered without a TTY. Uses t.Chdir — do NOT add
// t.Parallel().
func TestCheckpointListPending_PreviewText(t *testing.T) {
	_, dir := setupCheckpointListRepo(t)
	seedPendingPreviewSession(t, dir)

	out := runListCmd(t, "--pending")
	require.Equal(t, `Next checkpoint (written when you commit):
  Session 2026-10-07-preview (Claude Code): 2 turns, 2 files, 1 task
    file    src/a.go
    file    src/b.go
    task    Running 'Explore' agent: find callers (toolu_01ABCD)
    prompt  implement the parser
    prompt  add the tests
`, out, "the task record shows once, in the preview, not again as a [Task] row")
}

// TestCheckpointListPending_PreviewJSON pins the preview row of the JSON
// view: an id-less element with is_next_checkpoint and a next_checkpoint
// object, and no duplicate is_task_checkpoint row for the previewed session.
// Uses t.Chdir — do NOT add t.Parallel().
func TestCheckpointListPending_PreviewJSON(t *testing.T) {
	_, dir := setupCheckpointListRepo(t)
	seedPendingPreviewSession(t, dir)

	out := runListCmd(t, "--pending", "--json")
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows), out)
	require.Len(t, rows, 1, "the previewed session's task record must not also appear as its own row: %s", out)

	row := rows[0]
	require.ElementsMatch(t,
		[]string{"id", "message", "metadata_dir", "date", "is_task_checkpoint", "is_logs_only", "session_id", "session_prompt", "is_next_checkpoint", "next_checkpoint"},
		keysOf(row))
	require.Empty(t, row["id"], "turn-end work has no checkpoint ID until a commit")
	require.Equal(t, true, row["is_next_checkpoint"])
	require.Equal(t, false, row["is_task_checkpoint"])
	require.Equal(t, false, row["is_logs_only"])
	require.Equal(t, "Next checkpoint: 2 turns, 2 files, 1 task", row["message"])
	require.Equal(t, "2026-10-07T09:30:00Z", row["date"])
	require.Equal(t, "2026-10-07-preview", row["session_id"])
	require.Equal(t, "add the tests", row["session_prompt"])
	require.Equal(t, paths.SessionMetadataDirFromSessionID("2026-10-07-preview"), row["metadata_dir"])

	next, ok := row["next_checkpoint"].(map[string]any)
	require.True(t, ok, "next_checkpoint must be an object: %s", out)
	require.Equal(t, map[string]any{
		"agent":         "Claude Code",
		"turns":         float64(2),
		"files_touched": []any{"src/a.go", "src/b.go"},
		"task_records": []any{map[string]any{
			"tool_use_id": "toolu_01ABCDEFGHIJKLMN", "subagent_type": "Explore", "description": "find callers", "status": "running",
		}},
		"prompts": []any{"implement the parser", "add the tests"},
	}, next)
}

// TestCheckpointListPending_PreviewEmptyArraysPresent pins that the preview's
// arrays render as [] rather than null when empty, so consumers can range over
// them. Uses t.Chdir — do NOT add t.Parallel().
func TestCheckpointListPending_PreviewEmptyArraysPresent(t *testing.T) {
	setupCheckpointListRepo(t)
	worktree, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	require.NoError(t, strategy.SaveSessionState(context.Background(), &strategy.SessionState{
		SessionID:    "2026-10-07-turn-only",
		AgentType:    agent.AgentTypeClaudeCode,
		WorktreePath: worktree,
		Phase:        session.PhaseIdle,
		StartedAt:    time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC),
		StepCount:    1,
	}))

	out := runListCmd(t, "--pending", "--json")
	require.Contains(t, out, `"files_touched": []`)
	require.Contains(t, out, `"task_records": []`)
	require.Contains(t, out, `"prompts": []`)
}

// TestCheckpointListCmd_SessionWithPendingErrors verifies --session is rejected
// with --pending (the pending dataset is session-agnostic, mirroring rewind --list).
func TestCheckpointListCmd_SessionWithPendingErrors(t *testing.T) {
	setupCheckpointListRepo(t)

	cmd := newCheckpointListCmd()
	cmd.SetArgs([]string{"--pending", "--session", "abc"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "--session cannot be combined with --pending")
}

// runListCmd executes `checkpoint list <args>` via the real cobra command and
// returns stdout. Entire must already be enabled in CWD (setup helpers do this).
func runListCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newCheckpointListCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	require.NoError(t, cmd.Execute(), "checkpoint list %v failed; stderr: %s", args, stderr.String())
	return stdout.String()
}

// setupCheckpointListRepo initializes an enabled Entire repo with one commit in a
// temp CWD. No checkpoints are seeded.
func setupCheckpointListRepo(t *testing.T) (*git.Repository, string) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)

	w, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("initial"), 0o644))
	_, err = w.Add("test.txt")
	require.NoError(t, err)
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	enableEntire(t, tmpDir)
	return repo, tmpDir
}

// setupCheckpointListRepoWithCommittedCheckpoint extends setupCheckpointListRepo
// by writing a checkpoint to the v1 metadata branch and committing a code change
// whose Entire-Checkpoint trailer links it, so the condensed branch view is
// non-empty for routing tests.
func setupCheckpointListRepoWithCommittedCheckpoint(t *testing.T) {
	t.Helper()
	repo, tmpDir := setupCheckpointListRepo(t)

	cpID := checkpointid.MustCheckpointID("c1c2c3d4e5f6")
	require.NoError(t, checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).Write(context.Background(), checkpoint.Session(checkpoint.WriteOptions{
		CheckpointID: cpID,
		SessionID:    "2026-07-09-list-test-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"test": true}` + "\n")),
		Prompts:      []string{"seed prompt"},
		FilesTouched: []string{"test.txt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})))

	testutil.WriteFile(t, tmpDir, "test.txt", "second modification")
	testutil.GitAdd(t, tmpDir, "test.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("Second change", cpID))

	// Sanity: the seeded checkpoint must be visible to the condensed branch view,
	// otherwise the routing assertions below would pass vacuously on an empty array.
	points, _, err := getBranchCheckpoints(context.Background(), repo, 10)
	require.NoError(t, err)
	require.NotEmpty(t, points, "seed must produce at least one branch checkpoint")
}
