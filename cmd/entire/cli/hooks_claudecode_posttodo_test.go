package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const (
	testTaskToolUseA = "toolu_task_a"
	testTaskToolUseB = "toolu_task_b"
)

// writePreTaskFileWithModTime creates a pre-task-<toolUseID>.json file under .entire/tmp/
// and backdates/forwards its mtime, so tests can control which pre-task file
// FindActivePreTaskFile treats as "most recently modified" without needing real sleeps.
func writePreTaskFileWithModTime(t *testing.T, toolUseID string, modTime time.Time) {
	t.Helper()
	path := filepath.Join(paths.EntireTmpDir, "pre-task-"+toolUseID+".json")
	if err := os.WriteFile(path, []byte(`{"tool_use_id": "`+toolUseID+`"}`), 0o644); err != nil {
		t.Fatalf("failed to write pre-task file: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("failed to set pre-task file mtime: %v", err)
	}
}

func TestResolveIncrementalCheckpointTask_NoPreTaskFile(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "agent-A")
	if found {
		t.Errorf("resolveIncrementalCheckpointTask() found = true, taskToolUseID = %q; want false (main agent context)", taskToolUseID)
	}
}

// Bootstrap: the first PostTodo for a subagent instance has no remembered link yet, so
// it must fall back to FindActivePreTaskFile - and then remember the result so future
// calls for the same agent_id don't need to fall back again.
func TestResolveIncrementalCheckpointTask_Bootstrap(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	now := time.Now()
	writePreTaskFileWithModTime(t, testTaskToolUseA, now)

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "agent-A")
	if !found {
		t.Fatal("resolveIncrementalCheckpointTask() found = false, want true")
	}
	if taskToolUseID != testTaskToolUseA {
		t.Errorf("resolveIncrementalCheckpointTask() = %q, want %q", taskToolUseID, testTaskToolUseA)
	}

	linked, linkFound := LookupAgentTaskLink(ctx, "agent-A")
	if !linkFound {
		t.Fatal("expected resolveIncrementalCheckpointTask() to remember an agent-task link on bootstrap")
	}
	if linked != testTaskToolUseA {
		t.Errorf("remembered link = %q, want %q", linked, testTaskToolUseA)
	}
}

// Parallel sibling scenario: two pre-task files exist (task-A and task-B). agent-A
// already has a remembered link to task-A. Even though task-B's pre-task file is more
// recently modified (which is what FindActivePreTaskFile alone would pick), PostTodo
// with agent_id=A must still resolve to task-A.
func TestResolveIncrementalCheckpointTask_ParallelSiblingsPreferRememberedLink(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	older := time.Now().Add(-1 * time.Minute)
	newer := time.Now()
	writePreTaskFileWithModTime(t, testTaskToolUseA, older)
	writePreTaskFileWithModTime(t, testTaskToolUseB, newer)

	// Sanity check: without a remembered link, the mtime heuristic alone would pick
	// task-B (the bug this fix addresses).
	if taskToolUseID, found := FindActivePreTaskFile(ctx); !found || taskToolUseID != testTaskToolUseB {
		t.Fatalf("sanity check failed: FindActivePreTaskFile() = (%q, %v), want (toolu_task_b, true)", taskToolUseID, found)
	}

	if err := RememberAgentTaskLink(ctx, "agent-A", testTaskToolUseA); err != nil {
		t.Fatalf("RememberAgentTaskLink() error = %v", err)
	}

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "agent-A")
	if !found {
		t.Fatal("resolveIncrementalCheckpointTask() found = false, want true")
	}
	if taskToolUseID != testTaskToolUseA {
		t.Errorf("resolveIncrementalCheckpointTask() = %q, want %q (remembered link, not the mtime heuristic)",
			taskToolUseID, testTaskToolUseA)
	}

	// The sibling agent, with its own remembered link, must resolve to its own task.
	if err := RememberAgentTaskLink(ctx, "agent-B", testTaskToolUseB); err != nil {
		t.Fatalf("RememberAgentTaskLink() error = %v", err)
	}
	siblingTask, siblingFound := resolveIncrementalCheckpointTask(ctx, "agent-B")
	if !siblingFound || siblingTask != testTaskToolUseB {
		t.Errorf("resolveIncrementalCheckpointTask(agent-B) = (%q, %v), want (toolu_task_b, true)", siblingTask, siblingFound)
	}
}

// Parallel sibling bootstrap: when agent-A has already claimed the newer pre-task,
// agent-B's first PostTodo must latch onto the remaining unclaimed task rather than
// the same mtime winner.
func TestResolveIncrementalCheckpointTask_BootstrapPrefersUnclaimed(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	older := time.Now().Add(-1 * time.Minute)
	newer := time.Now()
	writePreTaskFileWithModTime(t, testTaskToolUseA, older)
	writePreTaskFileWithModTime(t, testTaskToolUseB, newer)

	if err := RememberAgentTaskLink(ctx, "agent-A", testTaskToolUseB); err != nil {
		t.Fatalf("RememberAgentTaskLink() error = %v", err)
	}

	// mtime heuristic alone would still pick task-B (already claimed).
	if taskToolUseID, found := FindActivePreTaskFile(ctx); !found || taskToolUseID != testTaskToolUseB {
		t.Fatalf("sanity check failed: FindActivePreTaskFile() = (%q, %v), want (toolu_task_b, true)", taskToolUseID, found)
	}

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "agent-B")
	if !found {
		t.Fatal("resolveIncrementalCheckpointTask(agent-B) found = false, want true")
	}
	if taskToolUseID != testTaskToolUseA {
		t.Errorf("resolveIncrementalCheckpointTask(agent-B) = %q, want %q (unclaimed older task)",
			taskToolUseID, testTaskToolUseA)
	}
}

func TestResolveIncrementalCheckpointTask_BootstrapClaimsInSpawnOrder(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	// task-A was spawned first, task-B second.
	writePreTaskFileWithModTime(t, testTaskToolUseA, time.Now().Add(-1*time.Minute))
	writePreTaskFileWithModTime(t, testTaskToolUseB, time.Now())

	// The first agent to report gets the first-spawned task.
	first, found := resolveIncrementalCheckpointTask(ctx, "agent-A")
	if !found || first != testTaskToolUseA {
		t.Errorf("first bootstrap = (%q, %v), want (%q, true) — claims must follow spawn order",
			first, found, testTaskToolUseA)
	}

	// The next agent gets the remaining unclaimed task.
	second, found := resolveIncrementalCheckpointTask(ctx, "agent-B")
	if !found || second != testTaskToolUseB {
		t.Errorf("second bootstrap = (%q, %v), want (%q, true)", second, found, testTaskToolUseB)
	}
}

// Nested subagent bootstrap: when a parent pre-task is still unclaimed, a child
// whose post-task hook stamped agent_id on its own pre-task must not inherit the
// parent's older unclaimed file on its first TodoWrite.
func TestResolveIncrementalCheckpointTask_BootstrapPrefersStampedAgentID(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	writePreTaskFileWithModTime(t, testTaskToolUseA, time.Now().Add(-2*time.Minute))
	writePreTaskFileWithModTime(t, testTaskToolUseB, time.Now())

	require.NoError(t, StampPreTaskAgentID(ctx, testTaskToolUseB, "agent-nested"))

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "agent-nested")
	if !found {
		t.Fatal("resolveIncrementalCheckpointTask() found = false, want true")
	}
	if taskToolUseID != testTaskToolUseB {
		t.Errorf("resolveIncrementalCheckpointTask() = %q, want %q (agent_id stamp beats oldest unclaimed parent)",
			taskToolUseID, testTaskToolUseB)
	}
}

// TestResolveIncrementalCheckpointTask_ClaimWriteFailureSkipsCheckpoint proves the
// resolver fails closed when the claim cannot be persisted, matching the
// lock-acquisition policy. Returning the task anyway would checkpoint against a
// claim no sibling can see, so a later sibling could select the same task and
// this agent could be remapped on its next TodoWrite.
func TestResolveIncrementalCheckpointTask_ClaimWriteFailureSkipsCheckpoint(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	writePreTaskFileWithModTime(t, testTaskToolUseA, time.Now())

	// Force the link write to fail deterministically: writing to a path that is
	// already a directory returns EISDIR.
	linkPath := filepath.Join(paths.EntireTmpDir, agentTaskLinkFileName("agent-A"))
	if err := os.MkdirAll(linkPath, 0o755); err != nil {
		t.Fatalf("failed to create directory at link path: %v", err)
	}

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "agent-A")
	if found {
		t.Errorf("resolveIncrementalCheckpointTask() = (%q, true), want (\"\", false) when the claim cannot be written",
			taskToolUseID)
	}
}

// No agent_id at all (older Claude Code, or an agent that doesn't send it) preserves the
// pre-existing FindActivePreTaskFile behavior and never remembers a link.
func TestResolveIncrementalCheckpointTask_NoAgentIDFallsBackToMtimeHeuristic(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	now := time.Now()
	writePreTaskFileWithModTime(t, testTaskToolUseA, now)

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "")
	if !found || taskToolUseID != testTaskToolUseA {
		t.Errorf("resolveIncrementalCheckpointTask(\"\") = (%q, %v), want (toolu_task_a, true)", taskToolUseID, found)
	}

	// No agent_id means no link should have been created for anything.
	entries, err := os.ReadDir(paths.EntireTmpDir)
	if err != nil {
		t.Fatalf("failed to read tmp dir: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" && len(entry.Name()) > len(agentTaskLinkFilePrefix) &&
			entry.Name()[:len(agentTaskLinkFilePrefix)] == agentTaskLinkFilePrefix {
			t.Errorf("unexpected agent-task link file created without an agent_id: %s", entry.Name())
		}
	}
}

// TestResolveIncrementalCheckpointTask_LockAcquireFailureBailsRatherThanRaces proves
// the lock is required, not best-effort: if flock.Acquire fails, the function must
// return early rather than falling through to the unprotected
// FindUnclaimedActivePreTaskFile + RememberAgentTaskLink sequence, which would
// recreate the exact double-claim race the lock exists to close.
func TestResolveIncrementalCheckpointTask_LockAcquireFailureBailsRatherThanRaces(t *testing.T) {
	setupTmpDirRepo(t)
	ctx := context.Background()

	writePreTaskFileWithModTime(t, testTaskToolUseA, time.Now())

	// Force flock.Acquire to fail deterministically: os.OpenFile(O_CREATE|O_RDWR)
	// on a path that is already a directory returns EISDIR.
	lockPath := agentTaskBootstrapLockPath(ctx)
	if err := os.MkdirAll(lockPath, 0o755); err != nil {
		t.Fatalf("failed to create directory at lock path: %v", err)
	}

	taskToolUseID, found := resolveIncrementalCheckpointTask(ctx, "agent-A")
	if found {
		t.Errorf("resolveIncrementalCheckpointTask() = (%q, true), want (\"\", false) when the bootstrap lock cannot be acquired", taskToolUseID)
	}

	if _, linkFound := LookupAgentTaskLink(ctx, "agent-A"); linkFound {
		t.Error("no agent-task link should be written when the bootstrap lock could not be acquired")
	}
}

// TestResolveIncrementalCheckpointTask_ConcurrentBootstrapNeverDoubleClaims races many
// sibling agentIDs' first PostTodo bootstrap against a fixed number of unclaimed
// pre-task files (each hook invocation is a separate OS process in production, but
// goroutines exercise the same flock-guarded critical section). Without the
// agentTaskBootstrapLockPath serialization, two goroutines could both read the same
// unclaimed pre-task via FindUnclaimedActivePreTaskFile before either wrote its
// RememberAgentTaskLink, double-claiming one task and leaving another with zero
// claimants.
func TestResolveIncrementalCheckpointTask_ConcurrentBootstrapNeverDoubleClaims(t *testing.T) {
	setupTmpDirRepo(t)

	const numTasks = 6
	taskIDs := make([]string, numTasks)
	for i := range taskIDs {
		taskIDs[i] = fmt.Sprintf("toolu_task_%d", i)
		writePreTaskFileWithModTime(t, taskIDs[i], time.Now())
	}

	var wg sync.WaitGroup
	results := make([]string, numTasks)
	for i := range numTasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agentID := fmt.Sprintf("agent-%d", i)
			taskToolUseID, found := resolveIncrementalCheckpointTask(context.Background(), agentID)
			if found {
				results[i] = taskToolUseID
			}
		}(i)
	}
	wg.Wait()

	claimCount := make(map[string]int)
	for _, taskID := range results {
		if taskID == "" {
			t.Error("a sibling failed to resolve any task")
			continue
		}
		claimCount[taskID]++
	}
	for taskID, count := range claimCount {
		if count != 1 {
			t.Errorf("task %s claimed by %d siblings, want exactly 1 (double-claim indicates the bootstrap race is not serialized)", taskID, count)
		}
	}
	if len(claimCount) != numTasks {
		t.Errorf("distinct tasks claimed = %d, want %d", len(claimCount), numTasks)
	}
}
