package recap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const testAgentClaude = "claude-code"

func TestAggregateToolProfiles_SumsCountsAndDurations(t *testing.T) {
	t.Parallel()
	cps := []RecapCheckpoint{
		{ToolProfile: &ToolProfile{
			Total:      10,
			Categories: map[string]ToolCategoryMetrics{"shell": {Count: 7, DurationMs: 700}, "fileOps": {Count: 3}},
		}},
		{ToolProfile: &ToolProfile{
			Total:      4,
			Categories: map[string]ToolCategoryMetrics{"shell": {Count: 1, DurationMs: 100}, "search": {Count: 3}},
		}},
		{ToolProfile: nil}, // tolerated, skipped
	}
	got := AggregateToolProfiles(cps)
	if got == nil {
		t.Fatal("expected non-nil ToolProfile")
	}
	if got.Total != 14 {
		t.Errorf("Total = %d, want 14", got.Total)
	}
	if got.Categories["shell"].Count != 8 {
		t.Errorf("shell count = %d, want 8", got.Categories["shell"].Count)
	}
	if got.Categories["shell"].DurationMs != 800 {
		t.Errorf("shell durationMs = %d, want 800", got.Categories["shell"].DurationMs)
	}
	if got.Categories["fileOps"].Count != 3 {
		t.Errorf("fileOps count = %d, want 3", got.Categories["fileOps"].Count)
	}
	if got.Categories["search"].Count != 3 {
		t.Errorf("search count = %d, want 3", got.Categories["search"].Count)
	}
}

func TestAggregateToolProfiles_AllNilReturnsNil(t *testing.T) {
	t.Parallel()
	if got := AggregateToolProfiles(nil); got != nil {
		t.Errorf("nil input → want nil, got %+v", got)
	}
	if got := AggregateToolProfiles([]RecapCheckpoint{{ToolProfile: nil}}); got != nil {
		t.Errorf("all-nil input → want nil, got %+v", got)
	}
}

func TestAggregateByWorktree_GroupsAndSortsByRecency(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{
			WorktreeID: "wt-a", WorktreePath: "/a", Repo: "org/a",
			LastInteraction: t0,
			AgentsUsed:      []string{testAgentClaude},
			Checkpoints:     []RecapCheckpoint{{LinkedCommit: "abc"}}, // committed
		},
		{
			WorktreeID: "wt-a", WorktreePath: "/a", Repo: "org/a",
			LastInteraction: t0.Add(1 * time.Hour), // later — becomes group's LastActivity
			AgentsUsed:      []string{"codex"},
			Checkpoints:     []RecapCheckpoint{{LinkedCommit: ""}}, // uncommitted → HasUncommitted
		},
		{
			WorktreeID: "wt-b", WorktreePath: "/b", Repo: "org/b",
			LastInteraction: t0.Add(-2 * time.Hour), // earliest
			AgentsUsed:      []string{testAgentClaude},
			Checkpoints:     []RecapCheckpoint{{LinkedCommit: "def"}},
		},
	}
	got := AggregateByWorktree(sessions)
	if len(got) != 2 {
		t.Fatalf("got %d rollups, want 2", len(got))
	}
	// Most recent activity first — wt-a (t0+1h) before wt-b (t0-2h).
	if got[0].WorktreeID != "wt-a" {
		t.Errorf("got[0].WorktreeID = %q, want wt-a", got[0].WorktreeID)
	}
	if got[0].SessionCount != 2 {
		t.Errorf("wt-a SessionCount = %d, want 2", got[0].SessionCount)
	}
	if !got[0].HasUncommitted {
		t.Error("wt-a HasUncommitted = false, want true (session 2 has uncommitted checkpoint)")
	}
	if !got[0].LastActivity.Equal(t0.Add(1 * time.Hour)) {
		t.Errorf("wt-a LastActivity = %v, want %v", got[0].LastActivity, t0.Add(1*time.Hour))
	}
	if len(got[0].Agents) != 2 || got[0].Agents[0] != testAgentClaude || got[0].Agents[1] != "codex" {
		t.Errorf("wt-a Agents = %v, want [claude-code codex]", got[0].Agents)
	}

	if got[1].WorktreeID != "wt-b" || got[1].HasUncommitted {
		t.Errorf("got[1] = %+v, want wt-b with HasUncommitted=false", got[1])
	}
}

func TestAggregateByWorktree_FallsBackToPathWhenIDEmpty(t *testing.T) {
	t.Parallel()
	sessions := []RecapSession{
		{WorktreeID: "", WorktreePath: "/main", LastInteraction: time.Now()},
	}
	got := AggregateByWorktree(sessions)
	if len(got) != 1 {
		t.Fatalf("got %d rollups, want 1", len(got))
	}
	if got[0].WorktreePath != "/main" {
		t.Errorf("WorktreePath = %q, want /main", got[0].WorktreePath)
	}
}

func TestAggregateByWorktree_SkipsSessionsWithoutWorktree(t *testing.T) {
	t.Parallel()
	got := AggregateByWorktree([]RecapSession{{WorktreeID: "", WorktreePath: ""}})
	if len(got) != 0 {
		t.Errorf("expected 0 rollups for session with no worktree info, got %d", len(got))
	}
}

func TestResolveRepoFromWorktree_EmptyPathReturnsUnknown(t *testing.T) {
	t.Parallel()
	if got := ResolveRepoFromWorktree(context.Background(), ""); got != "unknown" {
		t.Errorf("empty path → %q, want unknown", got)
	}
}

func TestResolveRepoFromWorktree_NoRemoteReturnsUnknown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir) // git init, no remote
	if got := ResolveRepoFromWorktree(context.Background(), dir); got != "unknown" {
		t.Errorf("no-remote repo → %q, want unknown", got)
	}
}

func TestResolveRepoFromWorktree_SSHRemote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir)
	runGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	if got := ResolveRepoFromWorktree(context.Background(), dir); got != "acme/widget" {
		t.Errorf("ssh remote → %q, want acme/widget", got)
	}
}

func TestResolveRepoFromWorktree_HTTPSRemote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/gadget.git")
	if got := ResolveRepoFromWorktree(context.Background(), dir); got != "acme/gadget" {
		t.Errorf("https remote → %q, want acme/gadget", got)
	}
}

// Test helpers ---------------------------------------------------------------

func initRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	// Make sure we have something resembling a repo so later commands don't error.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
}
