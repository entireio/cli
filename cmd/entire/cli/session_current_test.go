package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/cobra"
)

func TestSessionCurrent_NoSessionsPrintsHint(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	cmd := newSessionCurrentCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	if !strings.Contains(stdout.String(), "No active session") {
		t.Errorf("expected 'No active session' hint, got: %q", stdout.String())
	}
}

func TestSessionCurrent_JSONPrintsCurrentSessionInfo(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	now := time.Now().UTC().Truncate(time.Second)
	state := &strategy.SessionState{
		SessionID:           "session-current-json",
		AgentType:           agent.AgentTypeClaudeCode,
		ModelName:           "claude-sonnet",
		WorktreePath:        dir,
		StartedAt:           now.Add(-time.Hour),
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
		SessionTurnCount:    2,
		StepCount:           3,
		LastPrompt:          "inspect the current command",
		FilesTouched:        []string{"cmd/current.go"},
	}
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	cmd := newSessionCurrentCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	var got sessionInfoJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", stdout.String(), err)
	}
	if got.SessionID != state.SessionID {
		t.Fatalf("session_id = %q, want %q", got.SessionID, state.SessionID)
	}
	if got.Checkpoints != state.StepCount {
		t.Fatalf("checkpoints = %d, want %d", got.Checkpoints, state.StepCount)
	}
	if got.WorktreePath != dir {
		t.Fatalf("worktree_path = %q, want %q", got.WorktreePath, dir)
	}
}

func TestSessionCurrent_NotARepoErrors(t *testing.T) {
	// CWD-mutating; cannot run in parallel.
	dir := t.TempDir() // not initialized as git repo
	t.Chdir(dir)

	cmd := newSessionCurrentCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SilenceErrors = true
	cmd.SetArgs(nil)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when not in a git repository, got nil")
	}
}

// Compile-time check that newSessionCurrentCmd returns a *cobra.Command (a
// trivial sanity guard so an accidental return-type change is caught here
// rather than at the wiring site in sessions.go).
var _ *cobra.Command = newSessionCurrentCmd()
