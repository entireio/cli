package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestResolveWorktreeBranch_RegularRepo(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	testutil.InitRepo(t, dir)

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

	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("git open: %v", err)
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
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Enabled") {
		t.Errorf("Expected output to show 'Enabled', got: %s", stdout.String())
	}
}

// `entire status` surfaces the agent-help pointer for agents on transports
// without context injection (Cursor / Copilot / Droid), but only once entire is
// set up — not for not-set-up or not-a-git-repo states.
func TestRunStatus_ShowsAgentHelpHintWhenSetUp(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), agentHelpCommand) {
		t.Errorf("expected agent-help hint in status output, got: %s", stdout.String())
	}
}

func TestRunStatus_NoAgentHelpHintWhenNotSetUp(t *testing.T) {
	setupTestRepo(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if strings.Contains(stdout.String(), agentHelpCommand) {
		t.Errorf("agent-help hint should not appear when not set up, got: %s", stdout.String())
	}
}

// agentHelpCommand is user-facing — docs, the installed skills, and agents key
// off the exact string — so pin its value here. Every other assertion uses the
// const; this guards against it silently drifting.
func TestAgentHelpCommandValue(t *testing.T) {
	t.Parallel()
	const want = "entire agent-help"
	if agentHelpCommand != want {
		t.Errorf("agentHelpCommand = %q, want %q", agentHelpCommand, want)
	}
}

func TestRunStatus_Disabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Disabled") {
		t.Errorf("Expected output to show 'Disabled', got: %s", stdout.String())
	}
	// The agent-help footer renders whenever entire is set up, including disabled.
	if !strings.Contains(stdout.String(), agentHelpCommand) {
		t.Errorf("Expected agent-help hint in disabled (but set-up) status, got: %s", stdout.String())
	}
}

func TestRunStatus_NotSetUp(t *testing.T) {
	setupTestRepo(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
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
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
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
	if err := runStatus(context.Background(), &stdout, true, false); err != nil {
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
	// Project: enabled=true
	// Local: enabled=false
	// Detailed mode shows effective status first, then each file separately
	writeSettings(t, `{"enabled": true}`)
	writeLocalSettings(t, `{"enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, true, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show effective status first (local overrides project)
	if !strings.Contains(output, "Disabled") {
		t.Errorf("Expected output to show effective 'Disabled', got: %s", output)
	}
	// Should show both settings separately
	if !strings.Contains(output, "Project") || !strings.Contains(output, "enabled") {
		t.Errorf("Expected output to show Project with enabled, got: %s", output)
	}
	if !strings.Contains(output, "Local") || !strings.Contains(output, "disabled") {
		t.Errorf("Expected output to show Local with disabled, got: %s", output)
	}
}

func TestRunStatus_BothProjectAndLocal_Short(t *testing.T) {
	setupTestRepo(t)
	// Project: enabled=true
	// Local: enabled=false
	// Short mode shows merged/effective settings
	writeSettings(t, `{"enabled": true}`)
	writeLocalSettings(t, `{"enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show merged/effective state (local overrides project)
	if !strings.Contains(output, "Disabled") {
		t.Errorf("Expected output to show 'Disabled', got: %s", output)
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

	// Should contain full session IDs
	if !strings.Contains(output, "abc-1234-session") {
		t.Errorf("Expected full session ID 'abc-1234-session', got: %s", output)
	}
	if !strings.Contains(output, "def-5678-session") {
		t.Errorf("Expected full session ID 'def-5678-session', got: %s", output)
	}
	if !strings.Contains(output, "ghi-9012-session") {
		t.Errorf("Expected full session ID 'ghi-9012-session', got: %s", output)
	}

	// Should contain first prompts with chevron
	if !strings.Contains(output, "> \"Fix auth bug in login flow\"") {
		t.Errorf("Expected first prompt with chevron, got: %s", output)
	}

	// Session without LastPrompt should NOT show a prompt line
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "ghi-9012-session") {
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

// A session ended by `entire session attach` carries PhaseEnded with no EndedAt
// stamp. Status must drop it like any other ended session — filtering on EndedAt
// alone left it listed as active while `entire session stop`, which filters on
// IsEnded, refused to offer it.
func TestWriteActiveSessions_PhaseEndedWithoutEndedAtExcluded(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	state := &session.State{
		SessionID:    "attach-ended-session",
		WorktreePath: "/Users/test/repo",
		StartedAt:    time.Now().Add(-10 * time.Minute),
		Phase:        session.PhaseEnded,
	}

	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	if buf.Len() != 0 {
		t.Errorf("Expected empty output for a PhaseEnded session with no EndedAt, got: %s", buf.String())
	}
	if got := filterActiveSessions([]*session.State{state}); len(got) != 0 {
		t.Errorf("filterActiveSessions kept %d ended session(s); status and stop must agree", len(got))
	}
}

func TestWriteActiveSessions_ShowsDivergenceWarningWhenBaseCommitStale(t *testing.T) {
	setupTestRepo(t)

	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	// Create two commits: the session tracks the first, HEAD is the second
	testutil.WriteFile(t, repoDir, "tracked.txt", "base")
	testutil.GitAdd(t, repoDir, "tracked.txt")
	testutil.GitCommit(t, repoDir, "base commit")
	baseCommit := testutil.GetHeadHash(t, repoDir)

	testutil.WriteFile(t, repoDir, "tracked.txt", "base\nnew")
	testutil.GitAdd(t, repoDir, "tracked.txt")
	testutil.GitCommit(t, repoDir, "second commit")

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	state := &session.State{
		SessionID:             "stale-base-session",
		WorktreePath:          repoDir,
		StartedAt:             now.Add(-10 * time.Minute),
		BaseCommit:            baseCommit,
		AttributionBaseCommit: baseCommit,
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	output := buf.String()
	if !strings.Contains(output, "tracking diverged from current HEAD") {
		t.Fatalf("expected divergence warning when BaseCommit != HEAD, got: %s", output)
	}

	// Verify session state was NOT mutated (read-only)
	reloaded, err := store.Load(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if reloaded.BaseCommit != baseCommit {
		t.Fatalf("BaseCommit was mutated: got %q, want %q", reloaded.BaseCommit, baseCommit)
	}
	if reloaded.AttributionBaseCommit != baseCommit {
		t.Fatalf("AttributionBaseCommit was mutated: got %q, want %q", reloaded.AttributionBaseCommit, baseCommit)
	}
}

func TestWriteActiveSessions_NoWarningWhenReconciled(t *testing.T) {
	setupTestRepo(t)

	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	testutil.WriteFile(t, repoDir, "tracked.txt", "content")
	testutil.GitAdd(t, repoDir, "tracked.txt")
	testutil.GitCommit(t, repoDir, "initial commit")
	headCommit := testutil.GetHeadHash(t, repoDir)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	state := &session.State{
		SessionID:             "reconciled-session",
		WorktreePath:          repoDir,
		StartedAt:             now.Add(-10 * time.Minute),
		BaseCommit:            headCommit,
		AttributionBaseCommit: headCommit,
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	output := buf.String()
	if strings.Contains(output, "diverged") || strings.Contains(output, "attribution") {
		t.Fatalf("expected no divergence or attribution warning when BaseCommit == HEAD and AttributionBaseCommit == BaseCommit, got: %s", output)
	}
}

func TestWriteActiveSessions_ShowsSoftWarningWhenAttributionDiverged(t *testing.T) {
	setupTestRepo(t)

	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	testutil.WriteFile(t, repoDir, "tracked.txt", "content")
	testutil.GitAdd(t, repoDir, "tracked.txt")
	testutil.GitCommit(t, repoDir, "initial commit")
	headCommit := testutil.GetHeadHash(t, repoDir)

	// Simulate: hooks already reconciled BaseCommit to HEAD, but
	// AttributionBaseCommit is still pointing at the old commit (stale).
	oldBaseCommit := strings.Repeat("a", 40)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	state := &session.State{
		SessionID:             "attribution-diverged-session",
		WorktreePath:          repoDir,
		StartedAt:             now.Add(-10 * time.Minute),
		BaseCommit:            headCommit,
		AttributionBaseCommit: oldBaseCommit,
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	sty := newStatusStyles(&buf)
	writeActiveSessions(context.Background(), &buf, sty)

	output := buf.String()
	if !strings.Contains(output, "attribution") {
		t.Fatalf("expected attribution warning when AttributionBaseCommit != BaseCommit, got: %s", output)
	}

	// Verify session state was NOT mutated (read-only)
	reloaded, err := store.Load(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if reloaded.BaseCommit != headCommit {
		t.Fatalf("BaseCommit was mutated: got %q, want %q", reloaded.BaseCommit, headCommit)
	}
	if reloaded.AttributionBaseCommit != oldBaseCommit {
		t.Fatalf("AttributionBaseCommit was mutated: got %q, want %q", reloaded.AttributionBaseCommit, oldBaseCommit)
	}
}

// TestComputeSessionDivergenceWarnings_EmptyBaseCommit_EmitsLinkageWarning verifies
// that a partially-initialized session (BaseCommit == "") produces an explicit
// "linkage incomplete" warning rather than silently disappearing from status.
// Silently skipping such sessions was flagged as an observability regression:
// operators lose the clearest signal that the session cannot be migrated or
// attributed until reinitialization.
func TestComputeSessionDivergenceWarnings_EmptyBaseCommit_EmitsLinkageWarning(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	head := headLinkage{commitHash: strings.Repeat("e", 40)}

	active := []*session.State{
		{
			SessionID:             "partially-initialized",
			WorktreePath:          repoRoot,
			BaseCommit:            "",
			AttributionBaseCommit: strings.Repeat("a", 40),
		},
	}

	warnings := computeSessionDivergenceWarnings(repoRoot, active, head)

	msg, ok := warnings["partially-initialized"]
	if !ok {
		t.Fatal("expected a linkage-incomplete warning for session with empty BaseCommit, got none")
	}
	if !strings.Contains(msg, "linkage incomplete") {
		t.Fatalf("expected warning to mention linkage incomplete, got %q", msg)
	}
	// Must NOT be the attribution-divergence message — that would be misleading
	// since the session isn't diverged; it's un-initialized.
	if strings.Contains(msg, "attribution base diverged") {
		t.Fatalf("empty-BaseCommit session should not produce attribution-divergence wording, got %q", msg)
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

func TestTotalTokens_SaturatesOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	tu := &agent.TokenUsage{
		InputTokens: maxInt,
		SubagentTokens: &agent.TokenUsage{
			OutputTokens: 1,
		},
	}
	if got := totalTokens(tu); got != maxInt {
		t.Errorf("totalTokens() = %d, want %d", got, maxInt)
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
		Enabled: true,
	}

	result := formatSettingsStatusShort(context.Background(), s, sty)

	if !strings.Contains(result, "●") {
		t.Errorf("Enabled status should have green dot, got: %q", result)
	}
	if !strings.Contains(result, "Enabled") {
		t.Errorf("Expected 'Enabled' in output, got: %q", result)
	}
}

func TestFormatSettingsStatusShort_Disabled(t *testing.T) {
	setupTestRepo(t)

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled: false,
	}

	result := formatSettingsStatusShort(context.Background(), s, sty)

	if !strings.Contains(result, "○") {
		t.Errorf("Disabled status should have open dot, got: %q", result)
	}
	if !strings.Contains(result, "Disabled") {
		t.Errorf("Expected 'Disabled' in output, got: %q", result)
	}
}

func TestRunStatus_ShowsEnabledAgents(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Agents ·") {
		t.Errorf("Expected 'Agents ·' in output, got: %s", output)
	}
	if !strings.Contains(output, "Claude Code") {
		t.Errorf("Expected 'Claude Code' in output, got: %s", output)
	}
}

// TestRunStatus_ScannerSelectionLine covers the "Secret scanners · ..." line
// across both effective-settings call sites (short and --detailed) and both
// the non-default and default (line absent) cases.
func TestRunStatus_ScannerSelectionLine(t *testing.T) {
	tests := []struct {
		name        string
		settings    string
		detailed    bool
		want        string // substring checked for presence or absence per wantPresent
		wantPresent bool
	}{
		{
			name:        "goredact only, short",
			settings:    testSettingsGoredactOnly,
			want:        "Secret scanners · goredact",
			wantPresent: true,
		},
		{
			name:        "both enabled, short",
			settings:    `{"enabled": true, "redaction": {"betterleaks": {"enabled": true}, "goredact": {"enabled": true}}}`,
			want:        "Secret scanners · betterleaks, goredact",
			wantPresent: true,
		},
		{
			name:        "default, short",
			settings:    testSettingsEnabled,
			want:        "Secret scanners",
			wantPresent: false,
		},
		{
			name:        "goredact only, detailed",
			settings:    testSettingsGoredactOnly,
			detailed:    true,
			want:        "Secret scanners · goredact",
			wantPresent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTestRepo(t)
			writeSettings(t, tc.settings)

			var stdout bytes.Buffer
			if err := runStatus(context.Background(), &stdout, tc.detailed, false); err != nil {
				t.Fatalf("runStatus() error = %v", err)
			}

			output := stdout.String()
			if got := strings.Contains(output, tc.want); got != tc.wantPresent {
				t.Errorf("strings.Contains(output, %q) = %v, want %v; output: %s", tc.want, got, tc.wantPresent, output)
			}
		})
	}
}

func TestRunStatus_EnabledNoAgentsHidesHooksLine(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	// No agent hooks installed

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if strings.Contains(output, "Agents ·") {
		t.Errorf("Should not show hooks line when no agents installed, got: %s", output)
	}
}

func TestRunStatus_DetailedShowsEnabledAgents(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, true, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Agents ·") {
		t.Errorf("Expected 'Agents ·' in detailed output, got: %s", output)
	}
	if !strings.Contains(output, "Claude Code") {
		t.Errorf("Expected 'Claude Code' in detailed output, got: %s", output)
	}
}

func TestWriteActiveSessions_OmitsTokensWhenNoTokenData(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	recentInteraction := now.Add(-5 * time.Minute)

	states := []*session.State{
		{
			SessionID:           "no-token-session",
			WorktreePath:        "/Users/test/repo",
			StartedAt:           now.Add(-30 * time.Minute),
			LastInteractionTime: &recentInteraction,
			Phase:               session.PhaseActive,
			LastPrompt:          "explain this code",
			AgentType:           "Claude Code",
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

	if strings.Contains(output, "tokens") {
		t.Errorf("Session with no token data should NOT show tokens, got: %s", output)
	}
}

func TestWriteActiveSessions_ShowsTokensWithCheckpoints(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	recentInteraction := now.Add(-5 * time.Minute)

	states := []*session.State{
		{
			SessionID:           "has-checkpoint-session",
			WorktreePath:        "/Users/test/repo",
			StartedAt:           now.Add(-30 * time.Minute),
			LastInteractionTime: &recentInteraction,
			Phase:               session.PhaseActive,
			LastPrompt:          "fix the bug",
			AgentType:           "Claude Code",
			StepCount:           2,
			TokenUsage: &agent.TokenUsage{
				InputTokens:  800,
				OutputTokens: 400,
			},
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

	if !strings.Contains(output, "tokens 1.2k") {
		t.Errorf("Session with checkpoints should show tokens, got: %s", output)
	}
}

func TestRunStatus_DetailedDisabledDoesNotShowAgents(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	writeClaudeHooksFixture(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, true, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if strings.Contains(output, "Agents ·") {
		t.Errorf("Disabled detailed status should not show agents, got: %s", output)
	}
}

func TestRunStatus_DisabledDoesNotShowAgents(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	writeClaudeHooksFixture(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if strings.Contains(output, "Agents ·") {
		t.Errorf("Disabled status should not show agents, got: %s", output)
	}
}

func TestFormatSettingsStatus_Project(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled: true,
	}

	result := formatSettingsStatus("Project", s, sty)

	if !strings.Contains(result, "Project") {
		t.Errorf("Expected 'Project' prefix, got: %q", result)
	}
	if !strings.Contains(result, "enabled") {
		t.Errorf("Expected 'enabled' in output, got: %q", result)
	}
}

func TestFormatSettingsStatus_LocalDisabled(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled: false,
	}

	result := formatSettingsStatus("Local", s, sty)

	if !strings.Contains(result, "Local") {
		t.Errorf("Expected 'Local' prefix, got: %q", result)
	}
	if !strings.Contains(result, "disabled") {
		t.Errorf("Expected 'disabled' in output, got: %q", result)
	}
}

func TestWriteActiveSessions_StaleIndicator(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	staleInteraction := now.Add(-2 * time.Hour) // well past 1hr threshold

	states := []*session.State{
		{
			SessionID:           "stale-session-1",
			WorktreePath:        "/Users/test/repo",
			StartedAt:           now.Add(-3 * time.Hour),
			LastInteractionTime: &staleInteraction,
			Phase:               session.PhaseActive,
			LastPrompt:          "fix the bug",
			AgentType:           "Claude Code",
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

	if !strings.Contains(output, "stale") {
		t.Errorf("Expected 'stale' indicator for session with interaction >1hr ago, got: %s", output)
	}
	if !strings.Contains(output, "entire doctor") {
		t.Errorf("Expected 'entire doctor' hint in stale indicator, got: %s", output)
	}
}

func TestIsStuckActiveSession(t *testing.T) {
	t.Parallel()

	now := time.Now()
	recent := now.Add(-5 * time.Minute)
	stale := now.Add(-2 * time.Hour)
	brandNew := now.Add(-10 * time.Second)

	tests := []struct {
		name  string
		state *session.State
		want  bool
	}{
		{
			name:  "active with stale interaction",
			state: &session.State{Phase: session.PhaseActive, LastInteractionTime: &stale},
			want:  true,
		},
		{
			name:  "active with nil interaction and old start",
			state: &session.State{Phase: session.PhaseActive, LastInteractionTime: nil, StartedAt: now.Add(-2 * time.Hour)},
			want:  true,
		},
		{
			name:  "active with nil interaction and recent start",
			state: &session.State{Phase: session.PhaseActive, LastInteractionTime: nil, StartedAt: brandNew},
			want:  false,
		},
		{
			name:  "active with recent interaction",
			state: &session.State{Phase: session.PhaseActive, LastInteractionTime: &recent},
			want:  false,
		},
		{
			name:  "idle with stale interaction",
			state: &session.State{Phase: session.PhaseIdle, LastInteractionTime: &stale},
			want:  false,
		},
		{
			name:  "ended with stale interaction",
			state: &session.State{Phase: session.PhaseEnded, LastInteractionTime: &stale},
			want:  false,
		},
		{
			name:  "empty phase with stale interaction",
			state: &session.State{Phase: "", LastInteractionTime: &stale},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.IsStuckActive(); got != tt.want {
				t.Errorf("IsStuckActive() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteActiveSessions_StaleWithNilInteractionOldStart(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()

	states := []*session.State{
		{
			SessionID:           "old-nil-interaction-session",
			WorktreePath:        "/Users/test/repo",
			StartedAt:           now.Add(-2 * time.Hour),
			LastInteractionTime: nil,
			Phase:               session.PhaseActive,
			LastPrompt:          "do something",
			AgentType:           "Claude Code",
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

	if !strings.Contains(output, "stale") {
		t.Errorf("Old session with nil LastInteractionTime should show stale indicator, got: %s", output)
	}
}

func TestWriteActiveSessions_NotStaleWhenBrandNew(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()

	states := []*session.State{
		{
			SessionID:           "brand-new-session",
			WorktreePath:        "/Users/test/repo",
			StartedAt:           now.Add(-10 * time.Second),
			LastInteractionTime: nil,
			Phase:               session.PhaseActive,
			LastPrompt:          "hello",
			AgentType:           "Claude Code",
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

	if strings.Contains(output, "stale") {
		t.Errorf("Brand-new session should NOT show stale indicator, got: %s", output)
	}
}

func TestWriteActiveSessions_NotStaleWhenRecent(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	recentInteraction := now.Add(-5 * time.Minute)

	states := []*session.State{
		{
			SessionID:           "fresh-session-1",
			WorktreePath:        "/Users/test/repo",
			StartedAt:           now.Add(-30 * time.Minute),
			LastInteractionTime: &recentInteraction,
			Phase:               session.PhaseActive,
			LastPrompt:          "add feature",
			AgentType:           "Claude Code",
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

	if strings.Contains(output, "stale") {
		t.Errorf("Session with recent interaction should NOT show stale indicator, got: %s", output)
	}
}

func TestFormatSettingsStatus_Separators(t *testing.T) {
	t.Parallel()

	sty := statusStyles{colorEnabled: false, width: 60}
	s := &EntireSettings{
		Enabled: true,
	}

	result := formatSettingsStatus("Project", s, sty)

	// Should use · as separator (plain text, no ANSI)
	if !strings.Contains(result, "·") {
		t.Errorf("Expected '·' separators in output, got: %q", result)
	}
}

func TestRunStatusJSON_Enabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if !result.Enabled {
		t.Error("Expected enabled=true")
	}
	found := false
	for _, a := range result.Agents {
		if a == "Claude Code" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Expected agents to contain 'Claude Code', got %v", result.Agents)
	}
	if result.Error != "" {
		t.Errorf("Expected no error, got %q", result.Error)
	}
	// No-channel agents (Cursor/Copilot/Droid/MCP) parse --json, not the text
	// footer, so the agent-help pointer must be present once entire is set up.
	if result.AgentHelp != agentHelpCommand {
		t.Errorf("Expected agent_help='entire agent-help', got %q", result.AgentHelp)
	}
}

// secret_scanners is omitted for the default selection and lists the enabled
// engines for a non-default one.
func TestRunStatusJSON_SecretScanners(t *testing.T) {
	cases := []struct {
		name     string
		settings string
		want     []string // nil: field omitted for the default selection
	}{
		{"default omitted", testSettingsEnabled, nil},
		{"goredact only listed", testSettingsGoredactOnly, []string{"goredact"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupTestRepo(t)
			writeSettings(t, tc.settings)

			var stdout bytes.Buffer
			if err := runStatus(context.Background(), &stdout, false, true); err != nil {
				t.Fatalf("runStatus() error = %v", err)
			}

			var result statusJSON
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if !slices.Equal(result.SecretScanners, tc.want) {
				t.Errorf("secret_scanners = %v, want %v", result.SecretScanners, tc.want)
			}
		})
	}
}

// TestRunStatusJSON_HooksOutdated — when Claude Code hooks are installed under
// the outdated Task/TodoWrite matchers, `entire status --json` reports the agent
// under hooks_outdated so scripts/agents can detect the drift.
func TestRunStatusJSON_HooksOutdated(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stale := `{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]}],
    "PreToolUse": [{"matcher": "Task", "hooks": [{"type": "command", "command": "entire hooks claude-code pre-task"}]}],
    "PostToolUse": [
      {"matcher": "Task", "hooks": [{"type": "command", "command": "entire hooks claude-code post-task"}]},
      {"matcher": "TodoWrite", "hooks": [{"type": "command", "command": "entire hooks claude-code post-todo"}]}
    ]
  }
}`
	if err := os.WriteFile(".claude/settings.json", []byte(stale), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !slices.Contains(result.HooksOutdated, "claude-code") {
		t.Errorf("Expected hooks_outdated to contain 'claude-code', got %v", result.HooksOutdated)
	}
}

func TestRunStatusJSON_CodexLinkedWorktreeHooksReportInactiveDiscovery(t *testing.T) {
	setupTestRepo(t)
	repoRoot, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, repoRoot, "worktree", "add", "-b", "codex-linked-status", linkedRoot)
	t.Chdir(linkedRoot)
	writeSettings(t, testSettingsEnabled)
	if err := os.MkdirAll(".codex", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacy := `{"hooks":{"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop"}]}]}}`
	if err := os.WriteFile(filepath.Join(".codex", "hooks.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if slices.Contains(result.Agents, "Codex") {
		t.Fatalf("ignored worktree-local hooks must not report Codex installed: %v", result.Agents)
	}
	if slices.Contains(result.HooksOutdated, "codex") {
		t.Fatalf("Codex freshness must be represented by codex_hooks only: %v", result.HooksOutdated)
	}
	if result.CodexHooks == nil {
		t.Fatal("expected codex_hooks diagnostic")
	}
	if result.CodexHooks.State != codexHookStateInactiveWorktreePath {
		t.Fatalf("codex_hooks.state = %q, want %q", result.CodexHooks.State, codexHookStateInactiveWorktreePath)
	}
	if result.CodexHooks.WorktreePath != resolvedHooksPath(t, linkedRoot) {
		t.Fatalf("worktree_path = %q", result.CodexHooks.WorktreePath)
	}
	if result.CodexHooks.DiscoveredPath != resolvedHooksPath(t, repoRoot) {
		t.Fatalf("discovered_path = %q", result.CodexHooks.DiscoveredPath)
	}
}

func TestRunStatus_CodexLinkedWorktreeRootHooksAreActive(t *testing.T) {
	tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
	ag := &codex.CodexAgent{}
	t.Chdir(repoRoot)
	_, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("install root Codex hooks: %v", err)
	}
	t.Chdir(linkedRoot)
	_, err = ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("install linked-worktree Codex hooks: %v", err)
	}
	t.Chdir(linkedRoot)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "Codex ignores this worktree's hooks file") {
		t.Fatalf("active root hooks should not produce a status warning: %s", out)
	}
	if strings.Contains(out, "CURRENT-WORKTREE FILE NOT DISCOVERED") {
		t.Fatalf("active root hooks should not produce a doctor warning: %s", out)
	}
}

func TestRunStatusJSON_CodexInvalidWorktreePrecedesDiscovery(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("directory symlinks require privileges on Windows")
	}

	for _, test := range []struct {
		name       string
		discovered string
	}{
		{name: "healthy discovered hooks", discovered: canonicalCodexHooksJSON()},
		{name: "invalid discovered hooks", discovered: "{"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmp, repoRoot, linkedRoot := setupLinkedRepoForDoctorTest(t)
			writeCodexHooksForDiagnosticTest(t, repoRoot, test.discovered)
			if err := os.Symlink(filepath.Join(repoRoot, ".codex"), filepath.Join(linkedRoot, ".codex")); err != nil {
				t.Fatalf("create redirected project layer: %v", err)
			}
			t.Chdir(linkedRoot)
			t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))
			writeSettings(t, testSettingsEnabled)

			var stdout bytes.Buffer
			if err := runStatus(context.Background(), &stdout, false, true); err != nil {
				t.Fatalf("runStatus() error = %v", err)
			}
			var result statusJSON
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if result.CodexHooks == nil {
				t.Fatal("expected codex_hooks diagnostic")
			}
			wantState := codexHookStateProjectLayerMissing
			if test.discovered == "{" {
				wantState = codexHookStateMalformedDiscovered
			}
			if result.CodexHooks.State != wantState {
				t.Fatalf("codex_hooks.state = %q, want %q", result.CodexHooks.State, wantState)
			}
			if result.CodexHooks.WorktreePath != resolvedHooksPath(t, linkedRoot) {
				t.Fatalf("worktree_path = %q", result.CodexHooks.WorktreePath)
			}
			if test.discovered == "{" && !strings.Contains(result.CodexHooks.Error, "unexpected end of JSON input") {
				t.Fatalf("codex_hooks.error = %q", result.CodexHooks.Error)
			}
		})
	}
}

func TestRunStatusJSON_Disabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if result.Enabled {
		t.Error("Expected enabled=false")
	}
	// Disabled-but-set-up still advertises the passive agent-help pointer (the
	// hint is gated on "set up", not on "enabled") — matches the text footer.
	if result.AgentHelp != agentHelpCommand {
		t.Errorf("Expected agent_help='entire agent-help' when disabled-but-set-up, got %q", result.AgentHelp)
	}
}

func TestRunStatusJSON_NotSetUp(t *testing.T) {
	setupTestRepo(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if result.Enabled {
		t.Error("Expected enabled=false")
	}
	if result.Error != "not set up" {
		t.Errorf("Expected error='not set up', got %q", result.Error)
	}
	// Mirrors the text footer: no agent-help pointer until entire is set up.
	if result.AgentHelp != "" {
		t.Errorf("agent_help should be empty when not set up, got %q", result.AgentHelp)
	}
}

func TestRunStatusJSON_NotGitRepo(t *testing.T) {
	setupTestDir(t)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if result.Enabled {
		t.Error("Expected enabled=false")
	}
	if result.Error != "not a git repository" {
		t.Errorf("Expected error='not a git repository', got %q", result.Error)
	}
	if result.AgentHelp != "" {
		t.Errorf("agent_help should be empty when not a git repo, got %q", result.AgentHelp)
	}
}

func TestRunStatusJSON_WithActiveSessions(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	state := &session.State{
		SessionID:    "test-json-session",
		WorktreePath: "/test/repo",
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
		AgentType:    "Claude Code",
		ModelName:    "sonnet-4.1",
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(result.ActiveSessions) != 1 {
		t.Fatalf("Expected 1 active session, got %d", len(result.ActiveSessions))
	}
	s := result.ActiveSessions[0]
	if s.Agent != "Claude Code" {
		t.Errorf("Expected agent='Claude Code', got %q", s.Agent)
	}
	if s.Model != "sonnet-4.1" {
		t.Errorf("Expected model='sonnet-4.1', got %q", s.Model)
	}
	if s.Status != "active" {
		t.Errorf("Expected status='active', got %q", s.Status)
	}
	if result.AgentHelp != agentHelpCommand {
		t.Errorf("Expected agent_help='entire agent-help' with active sessions, got %q", result.AgentHelp)
	}
}

func TestRunStatusJSON_DeduplicatesSessions(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	states := []*session.State{
		{
			SessionID:    "codex-idle-1",
			WorktreePath: "/test/repo",
			StartedAt:    now.Add(-30 * time.Minute),
			Phase:        session.PhaseIdle,
			AgentType:    "Codex",
		},
		{
			SessionID:    "codex-idle-2",
			WorktreePath: "/test/repo",
			StartedAt:    now.Add(-20 * time.Minute),
			Phase:        session.PhaseIdle,
			AgentType:    "Codex",
		},
		{
			SessionID:    "codex-active",
			WorktreePath: "/test/repo",
			StartedAt:    now.Add(-5 * time.Minute),
			Phase:        session.PhaseActive,
			AgentType:    "Codex",
			ModelName:    "codex-mini",
		},
	}
	for _, s := range states {
		if err := store.Save(context.Background(), s); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(result.ActiveSessions) != 1 {
		t.Fatalf("Expected 1 deduplicated session, got %d", len(result.ActiveSessions))
	}
	s := result.ActiveSessions[0]
	if s.Agent != "Codex" {
		t.Errorf("Expected agent='Codex', got %q", s.Agent)
	}
	if s.Status != "active" {
		t.Errorf("Expected status='active' (active wins over idle), got %q", s.Status)
	}
	if s.Model != "codex-mini" {
		t.Errorf("Expected model='codex-mini' from active session, got %q", s.Model)
	}
}

// writeStatusHeadCheckpoint writes a v1 checkpoint with the requested
// review/investigation flags, then amends HEAD to carry the
// Entire-Checkpoint trailer. Mirrors the helper used in
// head_checkpoint_flags_test.go but inlined to keep status_test.go
// self-contained for readers comparing to other status tests.
func writeStatusHeadCheckpoint(t *testing.T, hasReview, hasInvestigation bool) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repo, err := git.PlainOpen(cwd)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}

	// Use a deterministic id per (review, investigation) pairing so multiple
	// status tests writing different combinations don't collide on the same id.
	cpHex := "abcdef011234"
	switch {
	case hasReview && hasInvestigation:
		cpHex = "abcdef011111"
	case hasReview:
		cpHex = "abcdef012222"
	case hasInvestigation:
		cpHex = "abcdef013333"
	}
	cpID := id.MustCheckpointID(cpHex)
	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	if err := store.Write(context.Background(), checkpoint.Session{
		CheckpointID:     cpID,
		SessionID:        "status-test-session",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")),
		AuthorName:       "Status Test",
		AuthorEmail:      "status-test@entire.local",
		HasReview:        hasReview,
		HasInvestigation: hasInvestigation,
	}); err != nil {
		t.Fatalf("WriteCommitted: %v", err)
	}

	runGitInDir(t, cwd, "commit", "--amend", "-m", "init\n\nEntire-Checkpoint: "+cpID.String())
}

func TestRunStatus_PrintsInvestigationLine(t *testing.T) {
	setupTestRepo(t)
	// Need an initial commit before we can amend it with the trailer.
	testutil.WriteFile(t, ".", "init.txt", "init")
	testutil.GitAdd(t, ".", "init.txt")
	testutil.GitCommit(t, ".", "init")
	writeSettings(t, `{"enabled": true}`)
	writeStatusHeadCheckpoint(t, false, true)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Investigation") || !strings.Contains(out, "investigated") {
		t.Errorf("expected 'Investigation' / 'investigated' line in status output; got:\n%s", out)
	}
	if strings.Contains(out, "Review · ") {
		t.Errorf("Review line must not appear when only HasInvestigation is set; got:\n%s", out)
	}
}

func TestRunStatus_PrintsBothReviewAndInvestigation(t *testing.T) {
	setupTestRepo(t)
	testutil.WriteFile(t, ".", "init.txt", "init")
	testutil.GitAdd(t, ".", "init.txt")
	testutil.GitCommit(t, ".", "init")
	writeSettings(t, `{"enabled": true}`)
	writeStatusHeadCheckpoint(t, true, true)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Review") || !strings.Contains(out, "reviewed") {
		t.Errorf("expected 'Review' / 'reviewed' line in status output; got:\n%s", out)
	}
	if !strings.Contains(out, "Investigation") || !strings.Contains(out, "investigated") {
		t.Errorf("expected 'Investigation' / 'investigated' line in status output; got:\n%s", out)
	}
}

// --- Checkpoint sync visibility (single-remote gate observability) ---

// electedOriginRemote and testCheckpointStoreRepo are the remote name and
// checkpoint_remote slug the checkpoint-sync tests assert against.
const (
	electedOriginRemote     = "origin"
	testCheckpointStoreRepo = "org/checkpoints"
)

// checkpointSyncTestCommit creates a commit in the cwd test repo and returns
// its hash. setupTestRepo leaves the repo without commits, and both the v1
// counter and ref updates need at least one.
func checkpointSyncTestCommit(t *testing.T, name, content string) string {
	t.Helper()
	testutil.WriteFile(t, ".", name, content)
	testutil.GitAdd(t, ".", name)
	testutil.GitCommit(t, ".", "commit "+name)
	return testutil.GetHeadHash(t, ".")
}

func TestRunStatus_CheckpointSyncDestination_Origin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, ".", "publish", "https://example.com/publish.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoints sync to: origin") {
		t.Errorf("expected destination line naming origin, got:\n%s", out)
	}
	if strings.Contains(out, "(set by checkpoint_push_remote)") {
		t.Errorf("default election must not carry the config annotation, got:\n%s", out)
	}
	if strings.Contains(out, "not yet on") {
		t.Errorf("counter line must be omitted when nothing is unpushed, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncDestination_ConfigAnnotated(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "private"}}`)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, ".", "private", "https://example.com/private.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoints sync to: private (set by checkpoint_push_remote)") {
		t.Errorf("expected annotated destination line for configured remote, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncDestination_CapturedAnnotated(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, ".", "fork", "https://example.com/fork.git")
	// A captured election (evidence-elected by a past tracked push) must be
	// distinguishable from both the silent default and the explicit setting.
	if err := os.WriteFile(filepath.Join(".git", "entire-checkpoint-sync-remotes.json"), []byte(`{"remotes":["fork"]}`), 0o600); err != nil {
		t.Fatalf("write capture state: %v", err)
	}

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoints sync to: fork (follows your branch's push destination)") {
		t.Errorf("expected annotated destination line for captured remote, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncFailClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "gone"}}`)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoints NOT syncing:") {
		t.Errorf("expected fail-closed warning line, got:\n%s", out)
	}
	if !strings.Contains(out, `"gone"`) {
		t.Errorf("fail-closed line should name the misconfigured remote, got:\n%s", out)
	}
	if strings.Contains(out, "Checkpoints sync to:") {
		t.Errorf("fail-closed status must not also print a destination line, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncHiddenWhenNoRemotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if strings.Contains(stdout.String(), "Checkpoints sync") || strings.Contains(stdout.String(), "Checkpoints NOT syncing") {
		t.Errorf("no remotes: checkpoint sync lines must be absent, got:\n%s", stdout.String())
	}
}

func TestRunStatus_CheckpointSyncHiddenWhenDisabled(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if strings.Contains(stdout.String(), "Checkpoints sync to:") {
		t.Errorf("disabled: checkpoint sync lines must be absent, got:\n%s", stdout.String())
	}
}

func TestRunStatus_CheckpointSyncCounter_GitBranchAhead(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")
	checkpointSyncTestCommit(t, "a.txt", "one")
	second := checkpointSyncTestCommit(t, "b.txt", "two")
	// Local v1 with no origin-tracking ref: every v1 commit counts as unpushed
	// (the deferred-publish reading).
	testutil.GitUpdateRef(t, ".", "refs/heads/"+paths.MetadataBranchName, second)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "2 checkpoints not yet on origin — they sync with your next 'git push origin'") {
		t.Errorf("expected unpushed counter line, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncCounterOmitted_WhenSynced(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")
	head := checkpointSyncTestCommit(t, "a.txt", "one")
	testutil.GitUpdateRef(t, ".", "refs/heads/"+paths.MetadataBranchName, head)
	testutil.GitUpdateRef(t, ".", "refs/remotes/origin/"+paths.MetadataBranchName, head)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoints sync to: origin") {
		t.Errorf("expected destination line, got:\n%s", out)
	}
	if strings.Contains(out, "not yet on") {
		t.Errorf("tracking ref equals local v1: counter must be omitted, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncDedicated_GitBranch_NoCounter(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	// Same owner ("org") as checkpoint_remote and a parseable GitHub URL, so
	// PushURL derivation succeeds locally and dedicated mode is verified.
	testutil.AddRemote(t, ".", "origin", "https://github.com/org/repo.git")
	head := checkpointSyncTestCommit(t, "a.txt", "one")
	// A local v1 branch exists, but in dedicated URL mode on the git-branch
	// backend the tracking-ref comparison would permanently read "all
	// unpushed" — the counter must be suppressed.
	testutil.GitUpdateRef(t, ".", "refs/heads/"+paths.MetadataBranchName, head)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoints sync to: dedicated checkpoint remote (org/checkpoints)") {
		t.Errorf("expected dedicated destination line with repo slug, got:\n%s", out)
	}
	// In effect is the opposite of ignored; the warning belongs only to the
	// fallback case.
	if strings.Contains(out, "is not in effect") {
		t.Errorf("dedicated mode must not warn about an ignored store, got:\n%s", out)
	}
	if strings.Contains(out, "not yet") {
		t.Errorf("dedicated + git-branch: counter must be suppressed, got:\n%s", out)
	}
}

func TestRunStatus_CheckpointSyncDedicated_GitRefs_QueueCounter(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}, "checkpoints": {"primary": {"type": "git-refs"}}}`)
	testutil.AddRemote(t, ".", "origin", "https://github.com/org/repo.git")
	checkpointSyncTestCommit(t, "a.txt", "one")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	queue := checkpoint.NewPushQueue(filepath.Join(cwd, ".git"))
	if err := queue.Enqueue("refs/entire/checkpoints/aa/bb0000000001"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.Enqueue("refs/entire/checkpoints/aa/bb0000000002"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Checkpoints sync to: dedicated checkpoint remote (org/checkpoints)") {
		t.Errorf("expected dedicated destination line, got:\n%s", out)
	}
	// Dedicated mode has no remote name to phrase the counter around.
	if !strings.Contains(out, "2 checkpoints not yet pushed") {
		t.Errorf("expected queue-length counter without a remote name, got:\n%s", out)
	}
	if strings.Contains(out, "not yet on") {
		t.Errorf("dedicated counter must not name a git remote, got:\n%s", out)
	}
}

func TestRunStatusJSON_CheckpointSync_Elected(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")
	checkpointSyncTestCommit(t, "a.txt", "one")
	second := checkpointSyncTestCommit(t, "b.txt", "two")
	testutil.GitUpdateRef(t, ".", "refs/heads/"+paths.MetadataBranchName, second)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.CheckpointSyncRemote != electedOriginRemote {
		t.Errorf("checkpoint_sync_remote = %q, want %q", result.CheckpointSyncRemote, "origin")
	}
	if result.CheckpointSyncRemoteSource != "default" {
		t.Errorf("checkpoint_sync_remote_source = %q, want %q", result.CheckpointSyncRemoteSource, "default")
	}
	if result.CheckpointSyncError != "" {
		t.Errorf("checkpoint_sync_error should be empty, got %q", result.CheckpointSyncError)
	}
	if result.UnpushedCheckpoints != 2 {
		t.Errorf("unpushed_checkpoints = %d, want 2", result.UnpushedCheckpoints)
	}
}

func TestRunStatusJSON_CheckpointSync_FailClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "gone"}}`)
	testutil.AddRemote(t, ".", "origin", "https://example.com/origin.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.CheckpointSyncRemote != "" {
		t.Errorf("checkpoint_sync_remote should be empty when unresolved, got %q", result.CheckpointSyncRemote)
	}
	if result.CheckpointSyncRemoteSource != "" {
		t.Errorf("checkpoint_sync_remote_source should be empty when unresolved, got %q", result.CheckpointSyncRemoteSource)
	}
	if !strings.Contains(result.CheckpointSyncError, `"gone"`) {
		t.Errorf("checkpoint_sync_error should name the misconfigured remote, got %q", result.CheckpointSyncError)
	}
}

func TestRunStatusJSON_CheckpointSync_Dedicated(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	testutil.AddRemote(t, ".", "origin", "https://github.com/org/repo.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.CheckpointSyncRemote != testCheckpointStoreRepo {
		t.Errorf("checkpoint_sync_remote = %q, want the org/repo slug", result.CheckpointSyncRemote)
	}
	if result.CheckpointSyncRemoteSource != "dedicated" {
		t.Errorf("checkpoint_sync_remote_source = %q, want %q", result.CheckpointSyncRemoteSource, "dedicated")
	}
	if result.UnpushedCheckpoints != 0 {
		t.Errorf("dedicated + git-branch must not report a count, got %d", result.UnpushedCheckpoints)
	}
}

// Dedicated mode is reported only when PushURL derivation succeeds (the same
// condition the pre-push gate's exemption uses). An owner mismatch between the
// elected remote and checkpoint_remote makes derivation fall back, so the next
// push uses normal single-remote sync — status must say so.
func TestRunStatus_CheckpointSyncDedicated_IneligibleFallsBackToElected(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	// Remote owner "other" != checkpoint_remote owner "org": fork detection
	// rejects the dedicated store at push time.
	testutil.AddRemote(t, ".", "origin", "https://github.com/other/repo.git")
	checkpointSyncTestCommit(t, "a.txt", "one")
	second := checkpointSyncTestCommit(t, "b.txt", "two")
	testutil.GitUpdateRef(t, ".", "refs/heads/"+paths.MetadataBranchName, second)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	out := stdout.String()
	if strings.Contains(out, "dedicated") {
		t.Errorf("ineligible derivation must not render dedicated mode, got:\n%s", out)
	}
	if !strings.Contains(out, "Checkpoints sync to: origin") {
		t.Errorf("expected normal elected-remote destination line, got:\n%s", out)
	}
	// The elected-remote counter applies in normal mode (v1 ahead, no
	// tracking ref -> all commits count).
	if !strings.Contains(out, "2 checkpoints not yet on origin") {
		t.Errorf("expected elected-remote counter in fallback mode, got:\n%s", out)
	}
	// The rejection is a log line everywhere else, so status is where a user
	// learns the store they configured is not being used.
	if !strings.Contains(out, `checkpoint_remote "org/checkpoints" is not in effect; checkpoints go to origin`) {
		t.Errorf("expected the ignored-store warning, got:\n%s", out)
	}
}

func TestRunStatusJSON_CheckpointSync_DedicatedIneligible(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	testutil.AddRemote(t, ".", "origin", "https://github.com/other/repo.git")

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.CheckpointSyncRemote != electedOriginRemote {
		t.Errorf("checkpoint_sync_remote = %q, want the elected remote %q", result.CheckpointSyncRemote, "origin")
	}
	if result.CheckpointSyncRemoteSource != "default" {
		t.Errorf("checkpoint_sync_remote_source = %q, want %q (not dedicated)", result.CheckpointSyncRemoteSource, "default")
	}
	if result.CheckpointSyncError != "" {
		t.Errorf("checkpoint_sync_error should be empty, got %q", result.CheckpointSyncError)
	}
	if result.CheckpointRemoteIgnored != testCheckpointStoreRepo {
		t.Errorf("checkpoint_remote_ignored = %q, want the configured store", result.CheckpointRemoteIgnored)
	}
}

// TestResolveCheckpointSyncDestination_UnreadableSnapshot pins the degradation
// for a failed `git remote -v`: the probe cannot judge the store, and "cannot
// tell" must not surface as the "store ignored" warning — that warning claims
// the store was rejected, which a transient read failure does not establish.
//
// Not parallel: setupTestRepo chdirs.
func TestResolveCheckpointSyncDestination_UnreadableSnapshot(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	// Remote owner "other" != store owner "org": with a readable snapshot this
	// is exactly the rejected-store state.
	testutil.AddRemote(t, ".", "origin", "https://github.com/other/repo.git")

	ctx := context.Background()
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		t.Fatalf("LoadEntireSettings() error = %v", err)
	}

	// Control: the live snapshot judges the store and reports it ignored.
	live := resolveCheckpointSyncDestination(ctx, newCheckpointStoreProbe(ctx, s, loadRemoteSnapshot(ctx)))
	if live.IgnoredStore != testCheckpointStoreRepo {
		t.Errorf("live snapshot: IgnoredStore = %q, want the configured store", live.IgnoredStore)
	}

	// An empty snapshot is what a failed `git remote -v` degrades to.
	info := resolveCheckpointSyncDestination(ctx, newCheckpointStoreProbe(ctx, s, gitremote.Snapshot{}))
	if info.IgnoredStore != "" {
		t.Errorf("unreadable snapshot: IgnoredStore = %q, want none — a transient read failure is not a rejection", info.IgnoredStore)
	}
	if info.dedicated() {
		t.Errorf("unreadable snapshot must not claim dedicated mode, got source %q", info.Source)
	}
	if info.Remote != electedOriginRemote {
		t.Errorf("Remote = %q, want plain elected-remote sync to %q", info.Remote, "origin")
	}
}

func TestRunStatusJSON_CheckpointSync_AbsentWhenNoRemotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runStatus(context.Background(), &stdout, false, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	var result statusJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.CheckpointSyncRemote != "" || result.CheckpointSyncRemoteSource != "" || result.CheckpointSyncError != "" {
		t.Errorf("no remotes: checkpoint sync JSON fields must be absent, got remote=%q source=%q err=%q",
			result.CheckpointSyncRemote, result.CheckpointSyncRemoteSource, result.CheckpointSyncError)
	}
	if strings.Contains(stdout.String(), "checkpoint_sync_remote") {
		t.Errorf("omitempty should drop empty checkpoint sync fields, got: %s", stdout.String())
	}
}

// A user who wrote external_agents into .entire/settings.json sees it there
// and has no other way to learn it is not in effect: discovery just does not
// happen, and the agent simply never appears. Detailed status renders the
// files, so it is where the discrepancy has to be named.
func TestRunStatusDetailed_ReportsRejectedExternalAgents(t *testing.T) { //nolint:paralleltest // t.Chdir
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	projectPath := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(projectPath, []byte(`{"enabled":true,"external_agents":true}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Chdir(dir)

	var out bytes.Buffer
	sty := statusStyles{colorEnabled: false, width: 80}
	if err := runStatusDetailed(t.Context(), &out, sty, projectPath,
		filepath.Join(entireDir, "settings.local.json"), true, false); err != nil {
		t.Fatalf("runStatusDetailed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "external_agents") {
		t.Errorf("status does not mention external_agents:\n%s", got)
	}
	if !strings.Contains(got, settings.EntireSettingsLocalFile) {
		t.Errorf("status does not name where the setting must live:\n%s", got)
	}
}
