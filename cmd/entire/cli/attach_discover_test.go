package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// nativeSessionListFixtureEntry mirrors the (assumed, per entireio/cli#1992)
// shape of one row of `opencode session list --format json`: session ID,
// title, working directory, and last-update time in epoch milliseconds. It is
// a standalone copy of agent/opencode's unexported nativeSessionListEntry —
// this test builds the JSON OpenCode is documented to emit, it does not reach
// into the opencode package's internals.
type nativeSessionListFixtureEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	UpdatedAt int64  `json:"updatedAt"`
}

// stubOpenCodeSessionList puts a fake `opencode` binary on PATH (ahead of the
// real PATH, so `git` and other tools setupAttachTestRepo needs stay
// resolvable) that answers any invocation with fixtureJSON on stdout. This is
// the same technique agent/opencode/cli_commands_test.go uses for `opencode
// export` — a real subprocess exec, not a hand-mocked Go function — so the
// discovery test below exercises the actual production call path
// (runAttachDiscoverSessionID -> ListNativeSessions -> runOpenCodeSessionList
// -> exec.Command("opencode", ...)) rather than a stand-in.
func stubOpenCodeSessionList(t *testing.T, fixtureJSON []byte) {
	t.Helper()
	if runtime.GOOS == windowsGOOS {
		t.Skip("stub opencode is a shell script")
	}

	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "fixture.json"), fixtureJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ndir=$(dirname \"$0\")\ncat \"$dir/fixture.json\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestAttachDiscover_OpenCodeListsUntrackedSessionsScopedToWorktree is the
// mandated real test for entireio/cli#1992: `entire session attach --agent
// opencode` with NO session ID must list OpenCode's own untracked sessions
// for the current worktree, in non-interactive mode (go test always reports
// non-interactive — see interactive.CanPromptInteractively), without any
// hand-mocked Go list standing in for the discovery function.
//
// This is genuinely new capability, not a regression fix: before this change
// `entire session attach --agent opencode` with no ID just printed usage help
// (see TestAttach_MissingSessionID, which still covers that behavior for
// agents that don't implement discovery).
func TestAttachDiscover_OpenCodeListsUntrackedSessionsScopedToWorktree(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("paths.WorktreeRoot: %v", err)
	}

	now := time.Now()
	fixture := []nativeSessionListFixtureEntry{
		// In scope: directory is exactly the worktree root.
		{ID: "ses_untracked_recent", Title: "docs: example", Directory: worktreeRoot, UpdatedAt: now.Add(-1 * time.Hour).UnixMilli()},
		// In scope: directory is below the worktree root ("a directory below
		// it", per the issue's scoping rule).
		{ID: "ses_untracked_subdir", Title: "fix bug in parser", Directory: filepath.Join(worktreeRoot, "pkg", "sub"), UpdatedAt: now.Add(-48 * time.Hour).UnixMilli()},
		// Out of scope: an unrelated project directory. Must be excluded.
		{ID: "ses_outside_worktree", Title: "unrelated project work", Directory: filepath.Join(t.TempDir(), "unrelated-project"), UpdatedAt: now.UnixMilli()},
		// In scope by directory, but already tracked by Entire. Must be
		// excluded — the issue is explicit that tracked sessions don't belong
		// in this picker.
		{ID: "ses_already_tracked", Title: "already tracked session", Directory: worktreeRoot, UpdatedAt: now.UnixMilli()},
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	stubOpenCodeSessionList(t, raw)

	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, &session.State{SessionID: "ses_already_tracked", StartedAt: now}); err != nil {
		t.Fatal(err)
	}

	cmd := newAttachCmd()
	cmd.SetArgs([]string{"--agent", "opencode"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("attach --agent opencode (no session ID) failed: %v", err)
	}

	output := out.String()
	t.Logf("discovery output:\n%s", output)

	for _, want := range []string{"ses_untracked_recent", "docs: example", "ses_untracked_subdir", "fix bug in parser"} {
		if !strings.Contains(output, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, output)
		}
	}
	for _, notWant := range []string{"ses_outside_worktree", "unrelated project work", "ses_already_tracked"} {
		if strings.Contains(output, notWant) {
			t.Errorf("expected output to exclude %q (out of scope or already tracked), got:\n%s", notWant, output)
		}
	}
	// Non-interactive fallback (Agent-Safe CLI Fallbacks convention): a plain
	// list plus the exact command to attach one directly, no TUI required.
	if !strings.Contains(output, "entire session attach <session-id> --agent opencode") {
		t.Errorf("expected non-interactive fallback to print the direct-attach command, got:\n%s", output)
	}
}

// TestAttachDiscover_NoUntrackedSessionsPrintsMessage covers the empty case:
// every native session is either out of scope or already tracked, so nothing
// is listed and no session is picked.
func TestAttachDiscover_NoUntrackedSessionsPrintsMessage(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("paths.WorktreeRoot: %v", err)
	}

	fixture := []nativeSessionListFixtureEntry{
		{ID: "ses_elsewhere", Title: "some other project", Directory: filepath.Join(t.TempDir(), "elsewhere"), UpdatedAt: time.Now().UnixMilli()},
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	stubOpenCodeSessionList(t, raw)
	_ = worktreeRoot

	cmd := newAttachCmd()
	cmd.SetArgs([]string{"--agent", "opencode"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("attach --agent opencode (no session ID) failed: %v", err)
	}

	if !strings.Contains(out.String(), "No untracked opencode sessions found for this worktree.") {
		t.Errorf("expected the no-results message, got:\n%s", out.String())
	}
}

// TestAttachCmd_ExplicitOpenCodeSessionIDStillWorks is the regression guard
// for entireio/cli#1992: the pre-existing "manual `opencode session list` +
// explicit-ID `entire session attach <id> --agent opencode`" path must keep
// working unchanged now that the command also accepts zero session-ID
// arguments. Exercises the full cobra command (not just runAttach directly),
// so it also covers the new arg-count check in newAttachCmd's RunE.
func TestAttachCmd_ExplicitOpenCodeSessionIDStillWorks(t *testing.T) {
	setupAttachTestRepo(t)
	t.Setenv("ENTIRE_TEST_OPENCODE_MOCK_EXPORT", "1")

	sessionID := "test-attach-cmd-opencode-explicit"
	repoRoot := mustGetwd(t)
	tmpDir := filepath.Join(repoRoot, ".entire", "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatal(err)
	}
	export := `{
  "info": {"id": "test-attach-cmd-opencode-explicit", "title": "regression: explicit id"},
  "messages": [
    {
      "info": {"id": "msg_u1", "role": "user", "time": {"created": 1767225600000}},
      "parts": [{"type": "text", "text": "Do the thing"}]
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(tmpDir, sessionID+".json"), []byte(export), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newAttachCmd()
	cmd.SetArgs([]string{sessionID, "--agent", "opencode"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("attach <session-id> --agent opencode failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Attached session "+sessionID) {
		t.Errorf("expected explicit-ID attach to succeed, got:\n%s", output)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.LastCheckpointID.IsEmpty() {
		t.Fatal("expected a checkpoint to be recorded for the explicitly-attached session")
	}
}
