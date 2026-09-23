//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestEnable_NoIdentityNoTerminal_FailsFast spawns the real binary with no
// controlling terminal in a repo with no git identity, and asserts `entire
// enable` refuses immediately instead of starting a device-code login.
//
// The unit tests for this path all inject canPrompt, so none of them exercise
// the production wiring. That mattered: the first version of this feature
// gated on IsKnownUnattended, which is false under Claude Code, Codex, and any
// headless non-CI context, and the resulting device-code flow blocked in
// waitForApproval for up to 15 minutes on a code nobody would read. Every unit
// test passed. Only spawning the binary without a TTY reproduces it.
//
// The deadline is the assertion: a pass must come from a fast refusal, not from
// a test that happens to outlive the wait.
func TestEnable_NoIdentityNoTerminal_FailsFast(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	// InitRepo configures a local identity; the bug needs it absent.
	for _, key := range []string{"user.name", "user.email"} {
		unset := exec.CommandContext(t.Context(), "git", "config", "--local", "--unset-all", key)
		unset.Dir = dir
		// Exit 5 means the key was not set, which is the state we want anyway.
		unset.Run() //nolint:errcheck // see above
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	start := time.Now()
	cmd := execx.NonInteractive(ctx, getTestBinary(), "enable", "--agent", "claude-code")
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		t.Fatalf("enable did not return within %v — it is waiting on something (device login?):\n%s", elapsed, out)
	}
	if err == nil {
		t.Fatalf("enable succeeded with no git identity and no terminal; want a refusal:\n%s", out)
	}
	// Well inside any device-code wait, so a regression cannot pass by being slow.
	if elapsed > 30*time.Second {
		t.Errorf("enable took %v to refuse; expected an immediate failure", elapsed)
	}
	text := string(out)
	if !strings.Contains(text, "git config --global user.name") {
		t.Errorf("output does not offer the direct git config fix:\n%s", text)
	}
	if strings.Contains(text, "Device code:") {
		t.Errorf("a device-code login was started where it cannot be completed:\n%s", text)
	}
}
