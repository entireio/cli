package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

// No t.Parallel in this file: the executeAgentHook tests use t.Chdir/t.Setenv,
// and the notice-delivery tests swap os.Stdout to observe the agent
// hook-response channel.

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

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. Agent hook-response writers (the notice's primary delivery
// channel) write to the process stdout, not to a cobra stream, so tests must
// observe it here.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = pw
	defer func() { os.Stdout = old }()
	outCh := make(chan string)
	go func() {
		var buf bytes.Buffer
		if _, cpErr := io.Copy(&buf, pr); cpErr != nil {
			t.Errorf("reading captured stdout: %v", cpErr)
		}
		outCh <- buf.String()
	}()
	fn()
	os.Stdout = old
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	return <-outCh
}

// TestWarnInactiveOnSessionStart_DeliveryChannel pins where the notice goes:
// through the agent's hook-response channel when it has one (Claude Code's
// JSON systemMessage on stdout — no built-in agent surfaces raw hook stderr
// to the user), and to stderr only as the fallback for agents without that
// capability (e.g. Cursor).
func TestWarnInactiveOnSessionStart_DeliveryChannel(t *testing.T) {
	notice := "entire: not tracking this session (not a git repo)"

	var errBuf bytes.Buffer
	out := captureStdout(t, func() {
		warnInactiveOnSessionStart(&errBuf, agent.AgentNameClaudeCode, "session-start", notice)
	})
	if !strings.Contains(out, `"systemMessage"`) || !strings.Contains(out, "not a git repo") {
		t.Errorf("response-writer agent: stdout = %q, want JSON systemMessage carrying the notice", out)
	}
	if errBuf.Len() != 0 {
		t.Errorf("response-writer agent must not also write stderr: %q", errBuf.String())
	}

	// Agents without a hook-response channel keep the plain stderr line.
	errBuf.Reset()
	out = captureStdout(t, func() {
		warnInactiveOnSessionStart(&errBuf, agent.AgentNameCursor, "session-start", notice)
	})
	if got := errBuf.String(); got != notice+"\n" {
		t.Errorf("fallback stderr notice = %q, want %q", got, notice+"\n")
	}
	if out != "" {
		t.Errorf("fallback agent wrote stdout: %q", out)
	}
}

func TestWarnInactiveOnSessionStart_OnlySessionStartVerb(t *testing.T) {
	notice := "entire: not tracking this session (not a git repo)"

	// Every other hook stays silent on both channels — the notice must never
	// repeat per hook.
	for _, hook := range []string{"stop", "user-prompt-submit", "session-end", "post-todo"} {
		var b bytes.Buffer
		out := captureStdout(t, func() {
			warnInactiveOnSessionStart(&b, agent.AgentNameClaudeCode, hook, notice)
		})
		if b.Len() != 0 || out != "" {
			t.Errorf("hook %q wrote a notice: stderr=%q stdout=%q", hook, b.String(), out)
		}
	}

	// An empty notice (silent reason) writes nothing even on session-start.
	var b bytes.Buffer
	out := captureStdout(t, func() {
		warnInactiveOnSessionStart(&b, agent.AgentNameClaudeCode, "session-start", "")
	})
	if b.Len() != 0 || out != "" {
		t.Errorf("empty notice wrote output: stderr=%q stdout=%q", b.String(), out)
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

// setUserGlobalSettings points ENTIRE_CONFIG_DIR at a fresh temp dir and, when
// content is non-empty, writes it as the user-level settings file.
func setUserGlobalSettings(t *testing.T, content string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Cleanup(settings.ClearGlobalModeCache)
	if content == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(cfg, settings.UserSettingsFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runAgentHook drives executeAgentHook (asserting it exits clean — the notice
// must never fail the hook) and returns what reached each delivery channel:
// the process stdout (the hook-response channel) and the command stderr.
func runAgentHook(t *testing.T, agentName types.AgentName, hookName string) (stdout, stderr string) {
	t.Helper()
	var errBuf bytes.Buffer
	out := captureStdout(t, func() {
		if err := executeAgentHook(newHookTestCmd(t, &errBuf), agentName, hookName, false); err != nil {
			t.Errorf("executeAgentHook() error = %v", err)
		}
	})
	return out, errBuf.String()
}

// expectSystemMessageNotice asserts the notice arrived as a Claude Code JSON
// systemMessage on stdout and did not leak to stderr.
func expectSystemMessageNotice(t *testing.T, stdout, stderr, want string) {
	t.Helper()
	if !strings.Contains(stdout, `"systemMessage"`) || !strings.Contains(stdout, want) {
		t.Errorf("missing %q on the hook-response channel, stdout: %q", want, stdout)
	}
	if stderr != "" {
		t.Errorf("response-writer agent must not write the notice to stderr: %q", stderr)
	}
}

// expectSilence asserts nothing reached either delivery channel.
func expectSilence(t *testing.T, stdout, stderr, context string) {
	t.Helper()
	if stdout != "" || stderr != "" {
		t.Errorf("%s must stay silent, stderr: %q stdout: %q", context, stderr, stdout)
	}
}

// TestExecuteAgentHook_InactiveWarnings drives the real gate in
// executeAgentHook and checks the emission matrix end to end: SessionStart
// only, correct reason text, delivery via the agent's hook-response channel
// (stdout), and silence for an explicitly disabled repo.
func TestExecuteAgentHook_InactiveWarnings(t *testing.T) {
	t.Run("global on, not a git repo warns on session-start only", func(t *testing.T) {
		setUserGlobalSettings(t, `{"global":{"enabled":true}}`)
		t.Chdir(t.TempDir()) // not a git repository

		stdout, stderr := runAgentHook(t, agent.AgentNameClaudeCode, "session-start")
		expectSystemMessageNotice(t, stdout, stderr, "(not a git repo)")

		stdout, stderr = runAgentHook(t, agent.AgentNameClaudeCode, "stop")
		expectSilence(t, stdout, stderr, "stop hook")
	})

	t.Run("global on, not a git repo, agent without response writer falls back to stderr", func(t *testing.T) {
		setUserGlobalSettings(t, `{"global":{"enabled":true}}`)
		t.Chdir(t.TempDir()) // not a git repository

		stdout, stderr := runAgentHook(t, agent.AgentNameCursor, "session-start")
		if !strings.Contains(stderr, "(not a git repo)") {
			t.Errorf("missing stderr-fallback notice, stderr: %q", stderr)
		}
		if stdout != "" {
			t.Errorf("fallback agent wrote stdout: %q", stdout)
		}
	})

	t.Run("global off or unconfigured, not a git repo stays silent", func(t *testing.T) {
		// Unconfigured tier: the user never opted in — no notice.
		setUserGlobalSettings(t, "")
		t.Chdir(t.TempDir()) // not a git repository

		stdout, stderr := runAgentHook(t, agent.AgentNameClaudeCode, "session-start")
		expectSilence(t, stdout, stderr, "unconfigured tier")

		// Explicitly disabled tier (disable --global): equally silent — this
		// is what keeps the "inert while global tracking is off" wording of
		// disable --global truthful for left-behind user-level hooks.
		setUserGlobalSettings(t, `{"global":{"enabled":false}}`)
		stdout, stderr = runAgentHook(t, agent.AgentNameClaudeCode, "session-start")
		expectSilence(t, stdout, stderr, "explicitly disabled tier")
	})

	t.Run("no repo setup and global off stays silent", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		setUserGlobalSettings(t, `{"global":{"enabled":false}}`)
		t.Chdir(dir)

		stdout, stderr := runAgentHook(t, agent.AgentNameClaudeCode, "session-start")
		expectSilence(t, stdout, stderr, "global-off (explicit off is durable)")
	})

	t.Run("excluded repo names the exclusion", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		setUserGlobalSettings(t, `{"global":{"enabled":true,"exclude_paths":["`+filepath.ToSlash(resolved)+`"]}}`)
		t.Chdir(dir)

		stdout, stderr := runAgentHook(t, agent.AgentNameClaudeCode, "session-start")
		expectSystemMessageNotice(t, stdout, stderr, "repo excluded by global settings")
	})

	t.Run("explicitly disabled repo stays silent", func(t *testing.T) {
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		// Global on AND repo-level disabled: the explicit veto wins and the
		// notice must not second-guess it.
		setUserGlobalSettings(t, `{"global":{"enabled":true}}`)
		if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		stdout, stderr := runAgentHook(t, agent.AgentNameClaudeCode, "session-start")
		expectSilence(t, stdout, stderr, "explicitly disabled repo")
	})
}
