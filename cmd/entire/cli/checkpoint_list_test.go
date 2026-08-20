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

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestPendingRewindPointJSON_MatchesRewindListContract pins the machine-readable
// shape emitted by `checkpoint list --pending --json`. It must stay byte-for-byte
// compatible with the JSON `rewind --list` historically produced, so consumers that
// parsed rewind --list keep working after repointing. Field names, omitempty
// behavior, and the RFC3339 date encoding are the contract.
func TestPendingRewindPointJSON_MatchesRewindListContract(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)

	// A logs-only (committed) point: has condensation_id, session_id,
	// session_prompt; tool_use_id is empty and must be omitted.
	logsOnly := pendingRewindPointJSON{
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
	// A task (shadow, uncommitted) point: has tool_use_id; condensation_id,
	// session_id, session_prompt are empty and must be omitted. metadata_dir and
	// the two bools have no omitempty and must always render.
	task := pendingRewindPointJSON{
		ID:               "0f1e2d3c",
		Message:          "task step",
		MetadataDir:      "",
		Date:             fixed.Format(time.RFC3339),
		IsTaskCheckpoint: true,
		ToolUseID:        "toolu_123",
		IsLogsOnly:       false,
	}

	data, err := jsonutil.MarshalIndentWithNewline([]pendingRewindPointJSON{logsOnly, task}, "", "  ")
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
// view emits `[]` (not `null`) when there are no rewind points — the drop-in
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
// no pending rewind points exist.
func TestRunCheckpointPendingListHuman_Empty(t *testing.T) {
	setupCheckpointListRepo(t)

	var stdout bytes.Buffer
	require.NoError(t, runCheckpointPendingListHuman(context.Background(), &stdout))
	require.Contains(t, stdout.String(), "No pending rewind points found.")
}

// TestCheckpointListCmd_Routing drives the real `checkpoint list` command
// end-to-end and asserts each flag combination routes to the right
// dataset/renderer. The repo is seeded with a shadow checkpoint so the
// condensed dataset is non-empty; the pending dataset stays empty because
// GetRewindPoints requires active-session state (created by lifecycle hooks,
// exercised by the integration canary), which a raw ephemeral-store seed does
// not register — this also demonstrates the two datasets are distinct.
func TestCheckpointListCmd_Routing(t *testing.T) {
	setupCheckpointListRepoWithShadowCheckpoint(t)

	// --json → condensed dataset, branchCheckpointJSON shape.
	condensed := runListCmd(t, "--json")
	require.True(t, json.Valid([]byte(condensed)), "condensed --json must be valid JSON, got: %s", condensed)
	require.Contains(t, condensed, `"checkpoint_id"`, "condensed --json must use branchCheckpointJSON shape")
	require.NotContains(t, condensed, `"metadata_dir"`, "condensed --json must not carry pending-only fields")

	// --pending (human) → pending renderer, empty here.
	pendingHuman := runListCmd(t, "--pending")
	require.Contains(t, pendingHuman, "No pending rewind points found.",
		"--pending (human) must route to the pending human renderer")

	// --pending --json → pending JSON renderer, empty array (distinct from the
	// human renderer above and from the condensed dataset).
	pendingJSON := runListCmd(t, "--pending", "--json")
	require.JSONEq(t, "[]", pendingJSON,
		"--pending --json must route to the pending JSON renderer")
	require.NotContains(t, pendingJSON, `"checkpoint_id"`, "pending --json must never carry condensed-only fields")
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

// setupCheckpointListRepoWithShadowCheckpoint extends setupCheckpointListRepo by
// seeding a checkpoint on the v1 metadata branch with real code changes, so the
// condensed branch view is non-empty for routing tests.
func setupCheckpointListRepoWithShadowCheckpoint(t *testing.T) {
	t.Helper()
	repo, tmpDir := setupCheckpointListRepo(t)

	sessionID := "2026-07-09-list-test-session"
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
	require.NoError(t, os.MkdirAll(metadataDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, paths.PromptFileName), []byte("seed prompt"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644))

	head, err := repo.Head()
	require.NoError(t, err)
	baseCommit := head.Hash().String()[:7]

	store := checkpoint.NewEphemeralStore(repo, checkpoint.DefaultV1Refs())
	_, err = store.Write(context.Background(), checkpoint.Step{
		SessionID:         sessionID,
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.txt"},
		MetadataDir:       ".entire/metadata/" + sessionID,
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint (baseline)",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("second modification"), 0o644))
	_, err = store.Write(context.Background(), checkpoint.Step{
		SessionID:         sessionID,
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.txt"},
		MetadataDir:       ".entire/metadata/" + sessionID,
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "Second checkpoint with code changes",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: false,
	})
	require.NoError(t, err)

	// Sanity: the seeded checkpoint must be visible to the condensed branch view,
	// otherwise the routing assertions below would pass vacuously on an empty array.
	points, _, err := getBranchCheckpoints(context.Background(), repo, 10)
	require.NoError(t, err)
	require.NotEmpty(t, points, "seed must produce at least one branch checkpoint")
}
