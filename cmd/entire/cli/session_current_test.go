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

// clearCallerSessionEnv unsets every agent's caller-session variable, derived
// from the registry so it cannot rot when an agent is added.
//
// Without it these tests inherit the session of whichever agent is running
// `go test` — `session current` would correctly identify that real session and
// every fixture-based assertion below would fail on a contributor's machine
// while passing on CI. Tests that want the caller tier set their variable
// after calling this.
func clearCallerSessionEnv(t *testing.T) {
	t.Helper()
	for _, name := range agent.CallerSessionEnvVars() {
		t.Setenv(name, "")
	}
}

func TestSessionCurrent_NoSessionsPrintsHint(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

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

// Machine-readable modes must keep stdout parseable: with --json and no
// active session, the hint text goes to stderr and the command exits
// non-zero, instead of printing prose to stdout with exit 0 (which crashed
// downstream JSON parsers in the review runner sandboxes).
func TestSessionCurrent_JSONNoSessionErrorsWithCleanStdout(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	for _, flag := range []string{"--json", "--transcript"} {
		cmd := newSessionCurrentCmd()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetContext(context.Background())
		cmd.SetArgs([]string{flag})

		err := cmd.Execute()
		if err == nil {
			t.Errorf("%s: expected non-zero exit when no session exists", flag)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: stdout must stay clean for parsers, got: %q", flag, stdout.String())
		}
		if !strings.Contains(stderr.String(), "No active session") {
			t.Errorf("%s: expected 'No active session' on stderr, got: %q", flag, stderr.String())
		}
	}
}

func TestSessionCurrent_JSONPrintsCurrentSessionInfo(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

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

// The caller tier end to end: an agent's published session ID names the
// session even though a different one is the most recent thing in the store.
// This is the reported bug's shape — before the tier, the foreign session came
// back indistinguishable from a real answer.
func TestSessionCurrent_JSONIdentifiesTheCallersOwnSession(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-2 * time.Hour)
	mine := &strategy.SessionState{
		SessionID:           "caller-owned-session",
		AgentType:           agent.AgentTypeCodex,
		WorktreePath:        "/some/other/worktree",
		StartedAt:           older,
		LastInteractionTime: &older,
		Phase:               session.PhaseIdle,
	}
	theirs := &strategy.SessionState{
		SessionID:           "most-recent-elsewhere",
		AgentType:           agent.AgentTypeClaudeCode,
		WorktreePath:        "/a/third/worktree",
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
	}
	for _, state := range []*strategy.SessionState{mine, theirs} {
		if err := strategy.SaveSessionState(context.Background(), state); err != nil {
			t.Fatalf("SaveSessionState(%q): %v", state.SessionID, err)
		}
	}
	t.Setenv("CODEX_SESSION_ID", mine.SessionID)

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
	if got.SessionID != mine.SessionID {
		t.Errorf("session_id = %q, want the caller's own %q", got.SessionID, mine.SessionID)
	}
	if got.Resolution != string(strategy.ResolutionCallerEnv) {
		t.Errorf("resolution = %q, want %q", got.Resolution, strategy.ResolutionCallerEnv)
	}
}

// The weakest tier stays available but must announce itself on stderr while
// keeping stdout a clean envelope — an agent parsing stdout needs the
// "resolution" field, and a human needs the sentence.
func TestSessionCurrent_CrossWorktreeFallbackIsLabelledAndWarned(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	now := time.Now().UTC().Truncate(time.Second)
	state := &strategy.SessionState{
		SessionID:           "only-elsewhere",
		AgentType:           agent.AgentTypeClaudeCode,
		WorktreePath:        "/some/other/worktree",
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
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
	if got.Resolution != string(strategy.ResolutionOtherWorktree) {
		t.Errorf("resolution = %q, want %q", got.Resolution, strategy.ResolutionOtherWorktree)
	}
	if !strings.Contains(stderr.String(), "not this command's caller") {
		t.Errorf("expected a stderr warning naming the risk, got: %q", stderr.String())
	}
}

// An agent named its session and Entire has no state for it. The command
// reports that fact — it must not fall through and answer with the unrelated
// session that happens to be in the store.
func TestSessionCurrent_UntrackedCallerIsDiagnosedNotSubstituted(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	now := time.Now().UTC().Truncate(time.Second)
	decoy := &strategy.SessionState{
		SessionID:           "unrelated-session",
		AgentType:           agent.AgentTypeClaudeCode,
		WorktreePath:        dir,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
	}
	if err := strategy.SaveSessionState(context.Background(), decoy); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
	t.Setenv("PI_SESSION_ID", "no-state-for-me")

	cmd := newSessionCurrentCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "no-state-for-me") {
		t.Errorf("expected the caller's session ID in the output, got: %q", out)
	}
	if strings.Contains(out, decoy.SessionID) {
		t.Errorf("substituted an unrelated session for the untracked caller: %q", out)
	}
}

// Several untracked claims must not be narrated as one identified session.
// The resolver reports the first of them so the diagnosis survives, but the
// command previously said "This command is running inside X session Y" — an
// arbitrary pick stated as fact.
func TestSessionCurrent_UntrackedAmbiguousClaimsAreNotStatedAsFact(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	t.Setenv("CLAUDE_CODE_SESSION_ID", "untracked-outer")
	t.Setenv("CODEX_SESSION_ID", "untracked-inner")

	cmd := newSessionCurrentCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "is running inside") {
		t.Errorf("stated an arbitrary pick as the caller: %q", out)
	}
	if !strings.Contains(out, "could not be determined") {
		t.Errorf("expected the output to say which session is running it is unknown, got: %q", out)
	}
}

// A tracked winner alongside an unplaceable claim is reported with its
// envelope intact — an agent needs the resolution field — but warned about on
// stderr, because the pair was never ordered.
func TestSessionCurrent_AmbiguousTrackedWinnerIsLabelledAndWarned(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	now := time.Now().UTC().Truncate(time.Second)
	tracked := &strategy.SessionState{
		SessionID:           "outer-tracked",
		AgentType:           agent.AgentTypeClaudeCode,
		WorktreePath:        dir,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
	}
	if err := strategy.SaveSessionState(context.Background(), tracked); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-tracked")
	t.Setenv("CODEX_SESSION_ID", "inner-untracked")

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
	if got.Resolution != string(strategy.ResolutionCallerAmbiguous) {
		t.Errorf("resolution = %q, want %q", got.Resolution, strategy.ResolutionCallerAmbiguous)
	}
	if !strings.Contains(stderr.String(), "could be ordered") {
		t.Errorf("expected a stderr warning about the unordered claims, got: %q", stderr.String())
	}
}

// The help promises the output always says which question it answered. The
// worktree tier is the most common one and used to print no line at all,
// making that promise false in exactly the case a user hits most.
func TestSessionCurrent_TextModeLabelsTheWorktreeTier(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	now := time.Now().UTC().Truncate(time.Second)
	state := &strategy.SessionState{
		SessionID:           "worktree-tier-session",
		AgentType:           agent.AgentTypeClaudeCode,
		WorktreePath:        dir,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
	}
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	cmd := newSessionCurrentCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resolved:") {
		t.Errorf("text mode omitted the resolution the help promises, got: %q", stdout.String())
	}
}

// `session info <id>` names its session outright, so there is nothing to
// explain and the line must stay absent.
func TestSessionInfo_TextModeHasNoResolutionLine(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	clearCallerSessionEnv(t)

	now := time.Now().UTC().Truncate(time.Second)
	state := &strategy.SessionState{
		SessionID:           "named-outright",
		AgentType:           agent.AgentTypeClaudeCode,
		WorktreePath:        dir,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
	}
	if err := strategy.SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	var stdout bytes.Buffer
	cmd := newInfoCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"named-outright"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(stdout.String(), "Resolved:") {
		t.Errorf("session info explained a resolution it was handed, got: %q", stdout.String())
	}
}
