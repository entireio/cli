//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestClaudeCode_WorktreePathFallback exercises the lifecycle handler through
// the Stop hook when Claude Code reports a transcript_path under a
// "--claude-worktrees-<branch>" project directory that does not exist on disk.
// The resolver in resolveTranscriptPath should redirect to the parent repo's
// project directory (where the file actually lives) so checkpoint creation
// proceeds. Regression test for the dropped-checkpoint bug seen when Claude
// is invoked from inside its own .claude/worktrees feature.
func TestClaudeCode_WorktreePathFallback(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// Fake HOME so claudecode.GetSessionBaseDir() resolves under our control —
	// it deliberately bypasses ENTIRE_TEST_CLAUDE_PROJECT_DIR.
	fakeHome := t.TempDir()

	parentSegment := claudecode.SanitizePathForClaude(env.RepoDir)
	realDir := filepath.Join(fakeHome, ".claude", "projects", parentSegment)
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir parent project: %v", err)
	}

	session := env.NewSession()
	session.TranscriptPath = filepath.Join(realDir, session.ID+".jsonl")

	// Reported path encodes the worktree CWD. Must not exist.
	reportedPath := filepath.Join(
		fakeHome, ".claude", "projects",
		parentSegment+"--claude-worktrees-feature",
		session.ID+".jsonl",
	)
	if _, err := os.Stat(reportedPath); !os.IsNotExist(err) {
		t.Fatalf("reported path should not exist on disk; stat err=%v", err)
	}

	extraEnv := []string{
		"HOME=" + fakeHome,
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR=" + env.ClaudeProjectDir,
	}

	runHook := func(hookName string, payload map[string]string) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s: %v", hookName, err)
		}
		cmd := exec.Command(getTestBinary(), "hooks", "claude-code", hookName)
		cmd.Dir = env.RepoDir
		cmd.Stdin = bytes.NewReader(body)
		cmd.Env = append(testutil.GitIsolatedEnv(), extraEnv...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook %s failed: %v\nOutput: %s", hookName, err, out)
		}
	}

	// Order matches a real session: prompt-submit captures pre-state, then the
	// agent does work (writes a file + transcript), then Stop fires.
	runHook("user-prompt-submit", map[string]string{
		"session_id":      session.ID,
		"transcript_path": reportedPath,
	})

	env.WriteFile("worktree_file.txt", "from claude")
	realPath := session.CreateTranscript("Add a worktree file", []FileChange{
		{Path: "worktree_file.txt", Content: "from claude"},
	})

	// Backdate mtime so waitForTranscriptFlush treats the file as stale and
	// skips its 3s sentinel poll. Freshness logic is incidental to the
	// regression under test — keep the test fast.
	stale := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(realPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	runHook("stop", map[string]string{
		"session_id":      session.ID,
		"transcript_path": reportedPath,
	})

	points := env.GetRewindPoints()
	if len(points) == 0 {
		t.Fatal("expected at least 1 rewind point — resolver failed to redirect to the parent-encoded transcript")
	}
}
