package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestResolveWorktreeBranch_RegularRepo(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Read the default branch name directly from HEAD to avoid hard-coding it
	headData, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	wantBranch := strings.TrimPrefix(strings.TrimSpace(string(headData)), "ref: refs/heads/")

	branch := resolveWorktreeBranch(context.Background(), dir)
	if branch != wantBranch {
		t.Errorf("resolveWorktreeBranch() = %q, want %q", branch, wantBranch)
	}
}

func TestResolveWorktreeBranch_DetachedHEAD(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Create a commit so we can detach HEAD
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := wt.Add("test.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	hash, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Detach HEAD by writing the raw hash to .git/HEAD
	headPath := filepath.Join(dir, ".git", "HEAD")
	if err := os.WriteFile(headPath, []byte(hash.String()+"\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	branch := resolveWorktreeBranch(context.Background(), dir)
	if branch != "HEAD" {
		t.Errorf("resolveWorktreeBranch() = %q, want %q for detached HEAD", branch, "HEAD")
	}
}

func TestResolveWorktreeBranch_WorktreeGitFile(t *testing.T) {
	// Simulate a worktree where .git is a file pointing to a gitdir
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	// Create a fake gitdir with a HEAD file
	gitdir := filepath.Join(dir, "fake-gitdir")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatalf("mkdir gitdir: %v", err)
	}
	headPath := filepath.Join(gitdir, "HEAD")
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/feature-branch\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	// Create a worktree-style .git file
	worktreeDir := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	gitFile := filepath.Join(worktreeDir, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}

	branch := resolveWorktreeBranch(context.Background(), worktreeDir)
	if branch != "feature-branch" {
		t.Errorf("resolveWorktreeBranch() = %q, want %q", branch, "feature-branch")
	}
}

func TestResolveWorktreeBranch_WorktreeRelativePath(t *testing.T) {
	// Simulate a worktree where .git file uses a relative gitdir path
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	// Create the main .git dir structure
	mainGitDir := filepath.Join(dir, "main-repo", ".git", "worktrees", "wt1")
	if err := os.MkdirAll(mainGitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	headPath := filepath.Join(mainGitDir, "HEAD")
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/develop\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	// Create worktree directory with relative .git file
	worktreeDir := filepath.Join(dir, "main-repo", "worktrees-dir", "wt1")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// Relative path from worktree to the gitdir
	relPath := filepath.Join("..", "..", ".git", "worktrees", "wt1")
	gitFile := filepath.Join(worktreeDir, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+relPath+"\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}

	branch := resolveWorktreeBranch(context.Background(), worktreeDir)
	if branch != "develop" {
		t.Errorf("resolveWorktreeBranch() = %q, want %q", branch, "develop")
	}
}

func TestResolveWorktreeBranch_NonExistentPath(t *testing.T) {
	t.Parallel()
	branch := resolveWorktreeBranch(context.Background(), "/nonexistent/path/that/does/not/exist")
	if branch != "" {
		t.Errorf("resolveWorktreeBranch() = %q, want empty string for non-existent path", branch)
	}
}

func TestResolveWorktreeBranch_NotARepo(t *testing.T) {
	dir := t.TempDir()
	// No .git directory or file
	branch := resolveWorktreeBranch(context.Background(), dir)
	if branch != "" {
		t.Errorf("resolveWorktreeBranch() = %q, want empty string for non-repo directory", branch)
	}
}

func TestResolveWorktreeBranch_ReftableStub(t *testing.T) {
	t.Parallel()

	// Simulate a reftable repo where .git/HEAD contains "ref: refs/heads/.invalid"
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/.invalid\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	branch := resolveWorktreeBranch(context.Background(), dir)
	// Should fall back to git, which will fail on this fake repo and return "HEAD"
	if branch != "HEAD" {
		t.Errorf("resolveWorktreeBranch() = %q, want %q for reftable stub", branch, "HEAD")
	}
}

func TestRunStatus_Enabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Enabled") {
		t.Errorf("Expected output to show 'Enabled', got: %s", stdout.String())
	}
}

func TestRunStatus_Disabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Disabled") {
		t.Errorf("Expected output to show 'Disabled', got: %s", stdout.String())
	}
}

func TestRunStatus_NotSetUp(t *testing.T) {
	setupTestRepo(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "○ not set up") {
		t.Errorf("Expected output to show '○ not set up', got: %s", output)
	}
	if !strings.Contains(output, "entire enable") {
		t.Errorf("Expected output to mention 'entire enable', got: %s", output)
	}
}

func TestRunStatus_NotGitRepository(t *testing.T) {
	setupTestDir(t) // No git init

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "✕ not a git repository") {
		t.Errorf("Expected output to show '✕ not a git repository', got: %s", stdout.String())
	}
}

func TestRunStatus_LocalSettingsOnly(t *testing.T) {
	setupTestRepo(t)
	writeLocalSettings(t, `{"enabled": true}`)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show effective status first (dot + Enabled + separator + strategy)
	if !strings.Contains(output, "Enabled") {
		t.Errorf("Expected output to show 'Enabled', got: %s", output)
	}
	// Should show per-file details
	if !strings.Contains(output, "Local") || !strings.Contains(output, "enabled") {
		t.Errorf("Expected output to show 'Local' and 'enabled', got: %s", output)
	}
	if strings.Contains(output, "Project") {
		t.Errorf("Should not show Project settings when only local exists, got: %s", output)
	}
}

func TestRunStatus_BothProjectAndLocal(t *testing.T) {
	setupTestRepo(t)
	// Project: enabled=true, strategy=manual-commit
	// Local: enabled=false, strategy=manual-commit
	// Detailed mode shows effective status first, then each file separately
	writeSettings(t, `{"enabled": true}`)
	writeLocalSettings(t, `{"enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show effective status first (local overrides project)
	if !strings.Contains(output, "Disabled") || !strings.Contains(output, "manual-commit") {
		t.Errorf("Expected output to show effective 'Disabled' with 'manual-commit', got: %s", output)
	}
	// Should show both settings separately
	if !strings.Contains(output, "Project") || !strings.Contains(output, "manual-commit") {
		t.Errorf("Expected output to show Project with manual-commit, got: %s", output)
	}
	if !strings.Contains(output, "Local") || !strings.Contains(output, "disabled") {
		t.Errorf("Expected output to show Local with disabled, got: %s", output)
	}
}

func TestRunStatus_BothProjectAndLocal_Short(t *testing.T) {
	setupTestRepo(t)
	// Project: enabled=true, strategy=manual-commit
	// Local: enabled=false, strategy=manual-commit
	// Short mode shows merged/effective settings
	writeSettings(t, `{"enabled": true}`)
	writeLocalSettings(t, `{"enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show merged/effective state (local overrides project)
	if !strings.Contains(output, "Disabled") || !strings.Contains(output, "manual-commit") {
		t.Errorf("Expected output to show 'Disabled' with 'manual-commit', got: %s", output)
	}
}

func TestRunStatus_ShowsManualCommitStrategy(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show effective status first
	if !strings.Contains(output, "Disabled") || !strings.Contains(output, "manual-commit") {
		t.Errorf("Expected output to show effective 'Disabled' with 'manual-commit', got: %s", output)
	}
	// Should show per-file details
	if !strings.Contains(output, "Project") || !strings.Contains(output, "disabled") {
		t.Errorf("Expected output to show 'Project' and 'disabled', got: %s", output)
	}
}

func TestTimeAgo(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"just now", 10 * time.Second, "just now"},
		{"30 seconds", 30 * time.Second, "just now"},
		{"1 minute", 1 * time.Minute, "1m ago"},
		{"5 minutes", 5 * time.Minute, "5m ago"},
		{"59 minutes", 59 * time.Minute, "59m ago"},
		{"1 hour", 1 * time.Hour, "1h ago"},
		{"3 hours", 3 * time.Hour, "3h ago"},
		{"23 hours", 23 * time.Hour, "23h ago"},
		{"1 day", 24 * time.Hour, "1d ago"},
		{"7 days", 7 * 24 * time.Hour, "7d ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timeAgo(time.Now().Add(-tt.duration))
			if got != tt.want {
				t.Errorf("timeAgo(%v ago) = %q, want %q", tt.duration, got, tt.want)
			}
		})
	}
}

func TestWriteActiveSessions(t *testing.T) {
	setupTestRepo(t)

	// Create a state store with test data
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	recentInteraction := now.Add(-5 * time.Minute)

	// Create active sessions with token usage
	states := []*session.State{
		{
			SessionID:           "abc-1234-session",
			WorktreePath:        "/Users/test/repo",
			StartedAt:           now.Add(-2 * time.Hour),
			LastInteractionTime: &recentInteraction,
			LastPrompt:          "Fix auth bug in login flow",
			AgentType:           types.AgentType("Claude Code"),
			TokenUsage: &agent.TokenUsage{
				InputTokens:  800,
				OutputTokens: 400,
			},
		},
		{
			SessionID:    "def-5678-session",
			WorktreePath: "/Users/test/repo",
			StartedAt:    now.Add(-15 * time.Minute),
			LastPrompt:   "Add dark mode support for the entire application and all components",
			AgentType:    agent.AgentTypeCursor,
			TokenUsage: &agent.TokenUsage{
				InputTokens:  500,
				OutputTokens: 300,
			},
		},
		{
			SessionID:    "ghi-9012-session",
			WorktreePath: "/Users/test/repo/.worktrees/3",
			StartedAt:    now.Add(-5 * time.Minute),
		},
	}

	for _, s := range states {
		if err := store.Save(context.Background(), s); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	output := buf.String()

	// Should contain "Active Sessions" in section header
	if !strings.Contains(output, "Active Sessions") {
		t.Errorf("Expected 'Active Sessions' header, got: %s", output)
	}

	// Should contain agent labels (without brackets in new format)
	if !strings.Contains(output, "Claude Code") {
		t.Errorf("Expected agent label 'Claude Code', got: %s", output)
	}
	if !strings.Contains(output, "Cursor") {
		t.Errorf("Expected agent label 'Cursor', got: %s", output)
	}
	// Session without AgentType should show unknown placeholder
	if !strings.Contains(output, unknownPlaceholder) {
		t.Errorf("Expected '%s' for missing agent type, got: %s", unknownPlaceholder, output)
	}

	// Should contain truncated session IDs
	if !strings.Contains(output, "abc-123") {
		t.Errorf("Expected truncated session ID 'abc-123', got: %s", output)
	}

	// Should contain first prompts with chevron
	if !strings.Contains(output, "> \"Fix auth bug in login flow\"") {
		t.Errorf("Expected first prompt with chevron, got: %s", output)
	}

	// Session without LastPrompt should NOT show a prompt line
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "ghi-901") {
			if strings.Contains(line, "\"") {
				t.Errorf("Session without prompt should not show quoted text on first line, got: %s", line)
			}
		}
	}

	// Should show "active 5m ago" for session with LastInteractionTime that differs from StartedAt
	if !strings.Contains(output, "active 5m ago") {
		t.Errorf("Expected 'active 5m ago' for session with LastInteractionTime, got: %s", output)
	}

	// Session started 15m ago with no LastInteractionTime should NOT show "active" in stats
	for _, line := range lines {
		if strings.Contains(line, "Cursor") {
			if strings.Contains(line, "active") {
				t.Errorf("Session without LastInteractionTime should not show 'active', got: %s", line)
			}
		}
	}

	// Should contain per-session token counts
	if !strings.Contains(output, "tokens 1.2k") {
		t.Errorf("Expected per-session 'tokens 1.2k' for first session (800+400), got: %s", output)
	}

	// Should contain aggregate footer with session count (no total tokens in footer)
	if !strings.Contains(output, "3 sessions") {
		t.Errorf("Expected aggregate '3 sessions' in footer, got: %s", output)
	}

	// Should NOT contain phase indicators (removed)
	if strings.Contains(output, "● active") || strings.Contains(output, "● idle") || strings.Contains(output, "● ended") {
		t.Errorf("Output should not contain phase indicators, got: %s", output)
	}

	// Should NOT contain file counts (removed)
	if strings.Contains(output, "files ") {
		t.Errorf("Output should not contain file counts, got: %s", output)
	}
}

func TestWriteActiveSessions_ActiveTimeOmittedWhenClose(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	// LastInteractionTime is only 30 seconds after StartedAt — should be omitted
	startedAt := now.Add(-10 * time.Minute)
	lastInteraction := startedAt.Add(30 * time.Second)

	state := &session.State{
		SessionID:           "close-time-session",
		WorktreePath:        "/Users/test/repo",
		StartedAt:           startedAt,
		LastInteractionTime: &lastInteraction,
		LastPrompt:          "test prompt",
		AgentType:           types.AgentType("Claude Code"),
	}

	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	output := buf.String()
	// Should not show "active Xm ago" when LastInteractionTime is close to StartedAt
	// But "active" may appear in phase indicator, so check for the specific pattern
	if strings.Contains(output, "active 10m ago") || strings.Contains(output, "active 9m ago") {
		t.Errorf("Expected no separate 'active' time when LastInteractionTime is close to StartedAt, got: %s", output)
	}
}

func TestWriteActiveSessions_NoSessions(t *testing.T) {
	setupTestRepo(t)

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	// Should produce no output when there are no sessions
	if buf.Len() != 0 {
		t.Errorf("Expected empty output with no sessions, got: %s", buf.String())
	}
}

func TestWriteActiveSessions_EndedSessionsExcluded(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	endedAt := time.Now()
	state := &session.State{
		SessionID:    "ended-session",
		WorktreePath: "/Users/test/repo",
		StartedAt:    time.Now().Add(-10 * time.Minute),
		EndedAt:      &endedAt,
	}

	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	// Should produce no output when all sessions are ended
	if buf.Len() != 0 {
		t.Errorf("Expected empty output with only ended sessions, got: %s", buf.String())
	}
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input int
		want  string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1k"},
		{1200, "1.2k"},
		{4800, "4.8k"},
		{14300, "14.3k"},
		{100000, "100k"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			got := formatTokenCount(tt.input)
			if got != tt.want {
				t.Errorf("formatTokenCount(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTotalTokens(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := totalTokens(nil); got != 0 {
			t.Errorf("totalTokens(nil) = %d, want 0", got)
		}
	})

	t.Run("basic", func(t *testing.T) {
		t.Parallel()
		tu := &agent.TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
		}
		if got := totalTokens(tu); got != 150 {
			t.Errorf("totalTokens() = %d, want 150", got)
		}
	})

	t.Run("with subagents", func(t *testing.T) {
		t.Parallel()
		tu := &agent.TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			SubagentTokens: &agent.TokenUsage{
				InputTokens:  200,
				OutputTokens: 100,
			},
		}
		if got := totalTokens(tu); got != 450 {
			t.Errorf("totalTokens() = %d, want 450", got)
		}
	})

	t.Run("all fields", func(t *testing.T) {
		t.Parallel()
		tu := &agent.TokenUsage{
			InputTokens:         100,
			CacheCreationTokens: 50,
			CacheReadTokens:     25,
			OutputTokens:        75,
		}
		if got := totalTokens(tu); got != 250 {
			t.Errorf("totalTokens() = %d, want 250", got)
		}
	})
}

func TestTotalTokens_ExcludesAPICallCount(t *testing.T) {
	t.Parallel()

	// APICallCount should NOT be included in token totals — it's a separate metric
	tu := &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
		APICallCount: 999, // should be ignored
	}
	got := totalTokens(tu)
	if got != 150 {
		t.Errorf("totalTokens() = %d, want 150 (APICallCount should be excluded)", got)
	}
}

func TestTotalTokens_DeepSubagentNesting(t *testing.T) {
	t.Parallel()

	tu := &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
		SubagentTokens: &agent.TokenUsage{
			InputTokens:  200,
			OutputTokens: 100,
			SubagentTokens: &agent.TokenUsage{
				InputTokens:  50,
				OutputTokens: 25,
			},
		},
	}
	// 100+50 + 200+100 + 50+25 = 525
	if got := totalTokens(tu); got != 525 {
		t.Errorf("totalTokens() = %d, want 525 (deep nesting)", got)
	}
}

func TestActiveTimeDisplay(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := activeTimeDisplay(nil); got != "" {
			t.Errorf("activeTimeDisplay(nil) = %q, want empty", got)
		}
	})

	t.Run("recent", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		if got := activeTimeDisplay(&now); got != "active now" {
			t.Errorf("activeTimeDisplay(now) = %q, want 'active now'", got)
		}
	})

	t.Run("older", func(t *testing.T) {
		t.Parallel()
		older := time.Now().Add(-5 * time.Minute)
		got := activeTimeDisplay(&older)
		if got != "active 5m ago" {
			t.Errorf("activeTimeDisplay(-5m) = %q, want 'active 5m ago'", got)
		}
	})
}

func TestShouldUseColor_NonTTY(t *testing.T) {
	t.Parallel()

	// bytes.Buffer is not a terminal → should return false
	var buf bytes.Buffer
	if shouldUseColor(&buf) {
		t.Error("shouldUseColor(bytes.Buffer) should be false")
	}
}

func TestShouldUseColor_NoColorEnv(t *testing.T) {
	// NO_COLOR env var should force color off even for a real file
	t.Setenv("NO_COLOR", "1")

	f, err := os.CreateTemp(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if shouldUseColor(f) {
		t.Error("shouldUseColor should be false when NO_COLOR is set")
	}
}

func TestShouldUseColor_RegularFile(t *testing.T) {
	t.Parallel()

	// A regular file (not a terminal) should return false
	f, err := os.CreateTemp(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if shouldUseColor(f) {
		t.Error("shouldUseColor(regular file) should be false")
	}
}

func TestNewStatusStyles_NonTTY(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)

	if sty.colorEnabled {
		t.Error("newStatusStyles(bytes.Buffer) should have colorEnabled=false")
	}
}

func TestRender_ColorDisabled(t *testing.T) {
	t.Parallel()

	// When color is disabled, render should return text unchanged
	sty := statusStyles{colorEnabled: false}
	style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))

	got := sty.render(style, "hello")
	if got != "hello" {
		t.Errorf("render with color disabled = %q, want %q", got, "hello")
	}
}

func TestRender_ColorEnabled_CallsStyleRender(t *testing.T) {
	t.Parallel()

	// When colorEnabled=true, render should call style.Render (not return plain text).
	// Note: lipgloss may strip ANSI in test environments without a terminal, so we
	// can't assert ANSI codes. Instead, verify the code path is exercised and
	// the text content is preserved.
	sty := statusStyles{
		colorEnabled: true,
		bold:         lipgloss.NewStyle().Bold(true),
	}

	got := sty.render(sty.bold, "hello")
	if !strings.Contains(got, "hello") {
		t.Errorf("render with color enabled should preserve text content, got: %q", got)
	}
}

func TestRender_ColorToggle(t *testing.T) {
	t.Parallel()

	style := lipgloss.NewStyle().Bold(true)

	// Color disabled: must return exact input
	styOff := statusStyles{colorEnabled: false}
	got := styOff.render(style, "test")
	if got != "test" {
		t.Errorf("render(colorEnabled=false) = %q, want exact %q", got, "test")
	}

	// Color enabled: exercises style.Render code path, text preserved
	styOn := statusStyles{colorEnabled: true}
	got = styOn.render(style, "test")
	if !strings.Contains(got, "test") {
		t.Errorf("render(colorEnabled=true) should contain 'test', got: %q", got)
	}
}

func TestSectionRule_PlainText(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false, width: 40}
	rule := sty.sectionRule("Active Sessions", 40)

	// Plain text should contain the label
	if !strings.Contains(rule, "Active Sessions") {
		t.Errorf("sectionRule should contain label, got: %q", rule)
	}
	if !strings.Contains(rule, "─") {
		t.Errorf("sectionRule should contain rule characters, got: %q", rule)
	}
	// With color disabled, should have no ANSI escapes
	if strings.Contains(rule, "\x1b[") {
		t.Errorf("sectionRule with color disabled should have no ANSI escapes, got: %q", rule)
	}
}

func TestHorizontalRule_PlainText(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false}
	rule := sty.horizontalRule(15)

	// Should be no ANSI escapes
	if strings.Contains(rule, "\x1b[") {
		t.Errorf("horizontalRule with color disabled should have no ANSI escapes, got: %q", rule)
	}
	if len([]rune(rule)) != 15 {
		t.Errorf("horizontalRule(15) has %d runes, want 15", len([]rune(rule)))
	}
}

func TestHorizontalRule(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)

	rule := sty.horizontalRule(20)
	if len([]rune(rule)) != 20 {
		t.Errorf("horizontalRule(20) has %d runes, want 20", len([]rune(rule)))
	}
	// All characters should be the box-drawing dash
	for _, r := range rule {
		if r != '─' {
			t.Errorf("horizontalRule contains unexpected rune %q", r)
			break
		}
	}
}

func TestGetTerminalWidth_NonTTY(t *testing.T) {
	t.Parallel()

	// A bytes.Buffer is not a terminal — should fall back to 60
	var buf bytes.Buffer
	width := getTerminalWidth(&buf)
	// In CI/test environments without a real terminal on Stdout/Stderr,
	// the fallback should be 60. If running in a terminal, it may be
	// capped at 80. Either is acceptable.
	if width != 60 && width > 80 {
		t.Errorf("getTerminalWidth(bytes.Buffer) = %d, want 60 or ≤80", width)
	}
}

func TestGetTerminalWidth_RegularFile(t *testing.T) {
	t.Parallel()

	// A regular file (not a terminal) should not report a terminal width
	f, err := os.CreateTemp(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	width := getTerminalWidth(f)
	// Regular file fd won't have a terminal size, so it should fall back
	if width != 60 && width > 80 {
		t.Errorf("getTerminalWidth(regular file) = %d, want 60 or ≤80", width)
	}
}

func TestNewStatusStyles_Width(t *testing.T) {
	t.Parallel()

	// For a non-terminal writer, width should be the fallback (60)
	// unless Stdout/Stderr happen to be terminals
	var buf bytes.Buffer
	sty := newStatusStyles(&buf)

	if sty.width == 0 {
		t.Error("newStatusStyles should set a non-zero width")
	}
	if sty.width > 80 {
		t.Errorf("newStatusStyles width = %d, should be capped at 80", sty.width)
	}
}

func TestSectionRule_NarrowWidth(t *testing.T) {
	t.Parallel()

	// When width is very small (smaller than prefix + label), trailing should be at least 1
	sty := statusStyles{colorEnabled: false, width: 10}
	rule := sty.sectionRule("Active Sessions", 10)

	// Should still contain the label and at least one trailing dash
	if !strings.Contains(rule, "Active Sessions") {
		t.Errorf("sectionRule with narrow width should still contain label, got: %q", rule)
	}
	if !strings.Contains(rule, "─") {
		t.Errorf("sectionRule with narrow width should have at least one trailing dash, got: %q", rule)
	}
}

func TestActiveTimeDisplay_Hours(t *testing.T) {
	t.Parallel()

	hoursAgo := time.Now().Add(-3 * time.Hour)
	got := activeTimeDisplay(&hoursAgo)
	if got != "active 3h ago" {
		t.Errorf("activeTimeDisplay(-3h) = %q, want 'active 3h ago'", got)
	}
}

func TestActiveTimeDisplay_Days(t *testing.T) {
	t.Parallel()

	daysAgo := time.Now().Add(-48 * time.Hour)
	got := activeTimeDisplay(&daysAgo)
	if got != "active 2d ago" {
		t.Errorf("activeTimeDisplay(-48h) = %q, want 'active 2d ago'", got)
	}
}

func TestFormatSettingsStatusShort_Enabled(t *testing.T) {
	setupTestRepo(t)

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled:  true,
		Strategy: "manual-commit",
	}

	result := formatSettingsStatusShort(context.Background(), s, sty)

	if !strings.Contains(result, "●") {
		t.Errorf("Enabled status should have green dot, got: %q", result)
	}
	if !strings.Contains(result, "Enabled") {
		t.Errorf("Expected 'Enabled' in output, got: %q", result)
	}
	if !strings.Contains(result, "manual-commit") {
		t.Errorf("Expected strategy in output, got: %q", result)
	}
}

func TestFormatSettingsStatusShort_Disabled(t *testing.T) {
	setupTestRepo(t)

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled:  false,
		Strategy: "manual-commit",
	}

	result := formatSettingsStatusShort(context.Background(), s, sty)

	if !strings.Contains(result, "○") {
		t.Errorf("Disabled status should have open dot, got: %q", result)
	}
	if !strings.Contains(result, "Disabled") {
		t.Errorf("Expected 'Disabled' in output, got: %q", result)
	}
	if !strings.Contains(result, "manual-commit") {
		t.Errorf("Expected strategy in output, got: %q", result)
	}
}

func TestFormatSettingsStatus_Project(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled:  true,
		Strategy: "manual-commit",
	}

	result := formatSettingsStatus("Project", s, sty)

	if !strings.Contains(result, "Project") {
		t.Errorf("Expected 'Project' prefix, got: %q", result)
	}
	if !strings.Contains(result, "enabled") {
		t.Errorf("Expected 'enabled' in output, got: %q", result)
	}
	if !strings.Contains(result, "manual-commit") {
		t.Errorf("Expected strategy in output, got: %q", result)
	}
}

func TestFormatSettingsStatus_LocalDisabled(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled:  false,
		Strategy: "manual-commit",
	}

	result := formatSettingsStatus("Local", s, sty)

	if !strings.Contains(result, "Local") {
		t.Errorf("Expected 'Local' prefix, got: %q", result)
	}
	if !strings.Contains(result, "disabled") {
		t.Errorf("Expected 'disabled' in output, got: %q", result)
	}
	if !strings.Contains(result, "manual-commit") {
		t.Errorf("Expected strategy in output, got: %q", result)
	}
}

func TestFormatSettingsStatus_Separators(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled:  true,
		Strategy: "manual-commit",
	}

	result := formatSettingsStatus("Project", s, sty)

	// Should use · as separator (plain text, no ANSI)
	if !strings.Contains(result, "·") {
		t.Errorf("Expected '·' separators in output, got: %q", result)
	}
}
