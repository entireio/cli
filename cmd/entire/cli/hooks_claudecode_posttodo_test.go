package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
