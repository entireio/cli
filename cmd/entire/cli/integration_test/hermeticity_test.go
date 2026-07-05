//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestHermeticityGuard_ExternalHostFailsFast proves the TestMain hermeticity
// tripwire fires: a git command that dials a real external host is redirected to
// an unroutable loopback address and fails fast, without reaching the network or
// prompting for credentials. Regression class: tests accidentally hitting live
// github.com / the macOS keychain (#1463, 53bc37a88).
func TestHermeticityGuard_ExternalHostFailsFast(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// ls-remote against a public-looking github URL must be refused immediately
	// by the per-host http.<url>.proxy entries pointing at the dead loopback
	// address (see testutil.hermeticGitConfig), not hang on DNS/network or
	// block on a credential prompt.
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "https://github.com/example/example")
	cmd.Env = testutil.GitIsolatedEnv()

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected ls-remote to fail under the hermeticity guard, but it succeeded:\n%s", out)
	}
	if ctx.Err() != nil {
		t.Fatalf("ls-remote did not fail fast (timed out after %s); the guard should refuse it immediately:\n%s", elapsed, out)
	}
	// The redirect target is the loopback refusal address, confirming the rewrite
	// (not a real github.com dial) produced the failure.
	if !strings.Contains(string(out), "127.0.0.1") {
		t.Errorf("expected failure to mention the loopback redirect target 127.0.0.1, got:\n%s", out)
	}
}
