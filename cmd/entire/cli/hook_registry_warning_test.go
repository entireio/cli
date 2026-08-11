package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

// No t.Parallel in this file: the executeAgentHook tests use t.Chdir/t.Setenv.

func TestInactiveSessionStartNotice_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		reason settings.InactiveReason
		want   string
	}{
		{"active is silent", settings.InactiveReasonNone, ""},
		// Explicit repo-level disable means silence — the user already decided.
		{"repo disabled is silent", settings.InactiveReasonRepoDisabled, ""},
		{"excluded names the exclusion", settings.InactiveReasonGlobalExcluded,
			"entire: not tracking this session (repo excluded by global settings)"},
		// Global off is an equally explicit, durable answer (declined wizard,
		// enable --global never run, or disable --global): silence, never a
		// nag to re-enable.
		{"global off is silent", settings.InactiveReasonGlobalOff, ""},
	}
	for _, tc := range cases {
		if got := inactiveSessionStartNotice(tc.reason); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWarnInactiveOnSessionStart_OnlySessionStartVerb(t *testing.T) {
	t.Parallel()
	notice := "entire: not tracking this session (not a git repo)"

	var buf bytes.Buffer
	warnInactiveOnSessionStart(&buf, "session-start", notice)
	if got := buf.String(); got != notice+"\n" {
		t.Errorf("session-start notice = %q, want %q", got, notice+"\n")
	}

	// Every other hook stays silent — the notice must never repeat per hook.
	for _, hook := range []string{"stop", "user-prompt-submit", "session-end", "post-todo"} {
		var b bytes.Buffer
		warnInactiveOnSessionStart(&b, hook, notice)
		if b.Len() != 0 {
			t.Errorf("hook %q wrote a notice: %q", hook, b.String())
		}
	}

	// An empty notice (silent reason) writes nothing even on session-start.
	var b bytes.Buffer
	warnInactiveOnSessionStart(&b, "session-start", "")
	if b.Len() != 0 {
		t.Errorf("empty notice wrote output: %q", b.String())
	}
}

// newHookTestCmd builds a command shell for executeAgentHook with a captured
// stderr and an empty JSON payload on stdin.
func newHookTestCmd(t *testing.T, errBuf *bytes.Buffer) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(io.Discard)
	cmd.SetErr(errBuf)
	cmd.SetIn(strings.NewReader("{}"))
	return cmd
}

// TestExecuteAgentHook_InactiveWarnings drives the real gate in
// executeAgentHook and checks the emission matrix end to end: SessionStart
// only, correct reason text, and silence for an explicitly disabled repo.
func TestExecuteAgentHook_InactiveWarnings(t *testing.T) {
	t.Run("global on, not a git repo warns on session-start only", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(settings.ClearGlobalModeCache)
		if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(t.TempDir()) // not a git repository

		var errBuf bytes.Buffer
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agent.AgentNameClaudeCode, "session-start", false); err != nil {
			t.Fatalf("executeAgentHook() error = %v", err)
		}
		if !strings.Contains(errBuf.String(), "(not a git repo)") {
			t.Errorf("missing not-a-git-repo notice, stderr: %q", errBuf.String())
		}

		errBuf.Reset()
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agent.AgentNameClaudeCode, "stop", false); err != nil {
			t.Fatalf("executeAgentHook() error = %v", err)
		}
		if errBuf.Len() != 0 {
			t.Errorf("stop hook must stay silent, stderr: %q", errBuf.String())
		}
	})

	t.Run("global off or unconfigured, not a git repo stays silent", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(settings.ClearGlobalModeCache)
		t.Chdir(t.TempDir()) // not a git repository

		// Unconfigured tier: the user never opted in — no notice.
		var errBuf bytes.Buffer
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agent.AgentNameClaudeCode, "session-start", false); err != nil {
			t.Fatalf("executeAgentHook() error = %v", err)
		}
		if errBuf.Len() != 0 {
			t.Errorf("unconfigured tier must stay silent, stderr: %q", errBuf.String())
		}

		// Explicitly disabled tier (disable --global): equally silent — this
		// is what keeps the "inert while global tracking is off" wording of
		// disable --global truthful for left-behind user-level hooks.
		if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(`{"global":{"enabled":false}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		errBuf.Reset()
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agent.AgentNameClaudeCode, "session-start", false); err != nil {
			t.Fatalf("executeAgentHook() error = %v", err)
		}
		if errBuf.Len() != 0 {
			t.Errorf("explicitly disabled tier must stay silent, stderr: %q", errBuf.String())
		}
	})

	t.Run("no repo setup and global off stays silent", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(settings.ClearGlobalModeCache)
		if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(`{"global":{"enabled":false}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		var errBuf bytes.Buffer
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agent.AgentNameClaudeCode, "session-start", false); err != nil {
			t.Fatalf("executeAgentHook() error = %v", err)
		}
		if errBuf.Len() != 0 {
			t.Errorf("global-off must stay silent (explicit off is durable), stderr: %q", errBuf.String())
		}
	})

	t.Run("excluded repo names the exclusion", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(settings.ClearGlobalModeCache)
		content := `{"global":{"enabled":true,"exclude_paths":["` + filepath.ToSlash(resolved) + `"]}}`
		if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		var errBuf bytes.Buffer
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agent.AgentNameClaudeCode, "session-start", false); err != nil {
			t.Fatalf("executeAgentHook() error = %v", err)
		}
		if !strings.Contains(errBuf.String(), "repo excluded by global settings") {
			t.Errorf("missing excluded notice, stderr: %q", errBuf.String())
		}
	})

	t.Run("explicitly disabled repo stays silent", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		cfg := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", cfg)
		t.Cleanup(settings.ClearGlobalModeCache)
		// Global on AND repo-level disabled: the explicit veto wins and the
		// notice must not second-guess it.
		content := `{"global":{"enabled":true}}`
		if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		var errBuf bytes.Buffer
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agent.AgentNameClaudeCode, "session-start", false); err != nil {
			t.Fatalf("executeAgentHook() error = %v", err)
		}
		if errBuf.Len() != 0 {
			t.Errorf("explicitly disabled repo must stay silent, stderr: %q", errBuf.String())
		}
	})
}
