//go:build e2e

package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFactoryTaskCheckpointExistsBeforeCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if s.Agent.Name() != "factoryai-droid" {
			t.Skip("factory-only regression test")
		}

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skip("factoryai-droid does not support interactive mode")
		}

		s.WaitFor(t, session, s.Agent.PromptPattern(), 30*time.Second)
		s.Send(t, session,
			"Can you run a Worker that inspects this code and comes up with a short summary about what it is about? Have the Worker write that summary to docs/factory-hook-check.md as one short paragraph followed by exactly 3 bullet points. Do not create or edit the file in the main agent process. Only the Worker should write the file. Do not commit. Do not ask for confirmation.")
		s.WaitFor(t, session, s.Agent.PromptPattern(), 90*time.Second)

		testutil.WaitForFileExists(t, s.Dir, "docs/factory-hook-check.md", 10*time.Second)

		waitForTaskRewindPoint(t, s.Dir, 15*time.Second)
	})
}

func TestFactoryCommittedCheckpointExcludesPreExistingUntrackedFiles(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if s.Agent.Name() != "factoryai-droid" {
			t.Skip("factory-only regression test")
		}

		sentinelPath := filepath.Join(s.Dir, "docs", "factory-preexisting-human-note.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(sentinelPath), 0o755))
		require.NoError(t, os.WriteFile(sentinelPath, []byte("human-owned sentinel\n"), 0o644))

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skip("factoryai-droid does not support interactive mode")
		}

		s.WaitFor(t, session, s.Agent.PromptPattern(), 30*time.Second)
		s.Send(t, session,
			"Can you run a Worker that inspects this code and writes its findings to docs/factory-prehook-worker.md as one short paragraph followed by exactly 3 bullet points? Do not read, modify, or mention docs/factory-preexisting-human-note.md. Do not create or edit the file in the main agent process. Only the Worker should write docs/factory-prehook-worker.md. Do not commit. Do not ask for confirmation.")
		s.WaitFor(t, session, s.Agent.PromptPattern(), 90*time.Second)

		testutil.WaitForFileExists(t, s.Dir, "docs/factory-prehook-worker.md", 10*time.Second)
		waitForTaskRewindPoint(t, s.Dir, 15*time.Second)

		s.Git(t, "add", "docs/factory-prehook-worker.md")
		s.Git(t, "commit", "-m", "Add factory worker checkpoint regression fixtures")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		meta := testutil.WaitForSessionMetadata(t, s.Dir, cpID, 0, 30*time.Second)

		assert.Contains(t, meta.FilesTouched, "docs/factory-prehook-worker.md",
			"worker-created file should be tracked in committed checkpoint metadata")
		assert.NotContains(t, meta.FilesTouched, "docs/factory-preexisting-human-note.md",
			"pre-existing untracked sentinel should not leak into committed checkpoint metadata")
	})
}

func waitForTaskRewindPoint(t *testing.T, dir string, timeout time.Duration) entire.RewindPoint {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		points := entire.RewindList(t, dir)
		for _, point := range points {
			if point.IsLogsOnly || !point.IsTaskCheckpoint {
				continue
			}
			if point.ToolUseID == "" {
				continue
			}
			return point
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("expected task rewind point within %s", timeout)
	return entire.RewindPoint{}
}
