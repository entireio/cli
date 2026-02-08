package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

func TestRunStatus_Enabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runStatus(&stdout, false); err != nil {
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
	if err := runStatus(&stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Disabled") {
		t.Errorf("Expected output to show 'Disabled', got: %s", stdout.String())
	}
}

func TestRunStatus_NotSetUp(t *testing.T) {
	setupTestRepo(t)

	var stdout bytes.Buffer
	if err := runStatus(&stdout, false); err != nil {
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
	if err := runStatus(&stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "✕ not a git repository") {
		t.Errorf("Expected output to show '✕ not a git repository', got: %s", stdout.String())
	}
}

func TestRunStatus_LocalSettingsOnly(t *testing.T) {
	setupTestRepo(t)
	writeLocalSettings(t, `{"strategy": "auto-commit", "enabled": true}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show effective status first
	if !strings.Contains(output, "Enabled (auto-commit)") {
		t.Errorf("Expected output to show effective 'Enabled (auto-commit)', got: %s", output)
	}
	// Should show per-file details
	if !strings.Contains(output, "Local, enabled") {
		t.Errorf("Expected output to show 'Local, enabled', got: %s", output)
	}
	if strings.Contains(output, "Project,") {
		t.Errorf("Should not show Project settings when only local exists, got: %s", output)
	}
}

func TestRunStatus_BothProjectAndLocal(t *testing.T) {
	setupTestRepo(t)
	// Project: enabled=true, strategy=manual-commit
	// Local: enabled=false, strategy=auto-commit
	// Detailed mode shows effective status first, then each file separately
	writeSettings(t, `{"strategy": "manual-commit", "enabled": true}`)
	writeLocalSettings(t, `{"strategy": "auto-commit", "enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show effective status first (local overrides project)
	if !strings.Contains(output, "Disabled (auto-commit)") {
		t.Errorf("Expected output to show effective 'Disabled (auto-commit)', got: %s", output)
	}
	// Should show both settings separately
	if !strings.Contains(output, "Project, enabled (manual-commit)") {
		t.Errorf("Expected output to show 'Project, enabled (manual-commit)', got: %s", output)
	}
	if !strings.Contains(output, "Local, disabled (auto-commit)") {
		t.Errorf("Expected output to show 'Local, disabled (auto-commit)', got: %s", output)
	}
}

func TestRunStatus_BothProjectAndLocal_Short(t *testing.T) {
	setupTestRepo(t)
	// Project: enabled=true, strategy=manual-commit
	// Local: enabled=false, strategy=auto-commit
	// Short mode shows merged/effective settings
	writeSettings(t, `{"strategy": "manual-commit", "enabled": true}`)
	writeLocalSettings(t, `{"strategy": "auto-commit", "enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show merged/effective state (local overrides project)
	if !strings.Contains(output, "Disabled (auto-commit)") {
		t.Errorf("Expected output to show 'Disabled (auto-commit)', got: %s", output)
	}
}

func TestRunStatus_ShowsStrategy(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"strategy": "auto-commit", "enabled": true}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "(auto-commit)") {
		t.Errorf("Expected output to show strategy '(auto-commit)', got: %s", output)
	}
}

func TestRunStatus_ShowsManualCommitStrategy(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"strategy": "manual-commit", "enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout, true); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show effective status first
	if !strings.Contains(output, "Disabled (manual-commit)") {
		t.Errorf("Expected output to show effective 'Disabled (manual-commit)', got: %s", output)
	}
	// Should show per-file details
	if !strings.Contains(output, "Project, disabled (manual-commit)") {
		t.Errorf("Expected output to show 'Project, disabled (manual-commit)', got: %s", output)
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
	store, err := session.NewStateStore()
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	recentInteraction := now.Add(-5 * time.Minute)

	// Create active sessions
	states := []*session.State{
		{
			SessionID:         "abc-1234-session",
			WorktreePath:      "/Users/test/repo",
			StartedAt:         now.Add(-2 * time.Hour),
			LastInteractionAt: &recentInteraction,
			FirstPrompt:       "Fix auth bug in login flow",
			AgentType:         agent.AgentType("Claude Code"),
		},
		{
			SessionID:    "def-5678-session",
			WorktreePath: "/Users/test/repo",
			StartedAt:    now.Add(-15 * time.Minute),
			FirstPrompt:  "Add dark mode support for the entire application and all components",
			AgentType:    agent.AgentType("Cursor"),
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
	writeActiveSessions(&buf)

	output := buf.String()

	// Should contain "Active Sessions:" header
	if !strings.Contains(output, "Active Sessions:") {
		t.Errorf("Expected 'Active Sessions:' header, got: %s", output)
	}

	// Should contain worktree paths
	if !strings.Contains(output, "/Users/test/repo") {
		t.Errorf("Expected worktree path '/Users/test/repo', got: %s", output)
	}
	if !strings.Contains(output, "/Users/test/repo/.worktrees/3") {
		t.Errorf("Expected worktree path '/Users/test/repo/.worktrees/3', got: %s", output)
	}

	// Should contain agent labels
	if !strings.Contains(output, "[Claude Code]") {
		t.Errorf("Expected agent label '[Claude Code]', got: %s", output)
	}
	if !strings.Contains(output, "[Cursor]") {
		t.Errorf("Expected agent label '[Cursor]', got: %s", output)
	}
	// Session without AgentType should show unknown placeholder
	if !strings.Contains(output, "[(unknown)]") {
		t.Errorf("Expected '[(unknown)]' for missing agent type, got: %s", output)
	}

	// Should contain truncated session IDs
	if !strings.Contains(output, "abc-123") {
		t.Errorf("Expected truncated session ID 'abc-123', got: %s", output)
	}

	// Should contain first prompts on indented second line
	if !strings.Contains(output, "\"Fix auth bug in login flow\"") {
		t.Errorf("Expected first prompt text in quotes, got: %s", output)
	}

	// Session without FirstPrompt should NOT show a prompt line
	// (no more "(unknown)" in quotes for missing prompts)
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "ghi-901") {
			// The line with the no-prompt session should not have a prompt
			if strings.Contains(line, "\"") {
				t.Errorf("Session without prompt should not show quoted text on first line, got: %s", line)
			}
		}
	}

	// Should show "active X ago" for session with LastInteractionAt that differs from StartedAt
	if !strings.Contains(output, "active 5m ago") {
		t.Errorf("Expected 'active 5m ago' for session with LastInteractionAt, got: %s", output)
	}

	// Session started 15m ago with no LastInteractionAt should NOT show "active" text
	// Find the Cursor session line and verify no "active" in it
	for _, line := range lines {
		if strings.Contains(line, "[Cursor]") {
			if strings.Contains(line, "active") {
				t.Errorf("Session without LastInteractionAt should not show 'active', got: %s", line)
			}
		}
	}
}

func TestWriteActiveSessions_ActiveTimeOmittedWhenClose(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore()
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	now := time.Now()
	// LastInteractionAt is only 30 seconds after StartedAt — should be omitted
	startedAt := now.Add(-10 * time.Minute)
	lastInteraction := startedAt.Add(30 * time.Second)

	state := &session.State{
		SessionID:         "close-time-session",
		WorktreePath:      "/Users/test/repo",
		StartedAt:         startedAt,
		LastInteractionAt: &lastInteraction,
		FirstPrompt:       "test prompt",
		AgentType:         agent.AgentType("Claude Code"),
	}

	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	writeActiveSessions(&buf)

	output := buf.String()
	if strings.Contains(output, "active") {
		t.Errorf("Expected no 'active' when LastInteractionAt is close to StartedAt, got: %s", output)
	}
}

func TestWriteActiveSessions_NoSessions(t *testing.T) {
	setupTestRepo(t)

	var buf bytes.Buffer
	writeActiveSessions(&buf)

	// Should produce no output when there are no sessions
	if buf.Len() != 0 {
		t.Errorf("Expected empty output with no sessions, got: %s", buf.String())
	}
}

func TestWriteActiveSessions_EndedSessionsExcluded(t *testing.T) {
	setupTestRepo(t)

	store, err := session.NewStateStore()
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
	writeActiveSessions(&buf)

	// Should produce no output when all sessions are ended
	if buf.Len() != 0 {
		t.Errorf("Expected empty output with only ended sessions, got: %s", buf.String())
	}
}
