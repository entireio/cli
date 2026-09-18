//go:build integration

package integration

import (
	"context"
	"fmt"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestMain builds the CLI binary once before running all tests.
func TestMain(m *testing.M) {
	// Build binary once to a temp directory
	tmpDir, err := os.MkdirTemp("", "entire-integration-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir for binary: %v\n", err)
		os.Exit(1)
	}

	testBinaryPath = filepath.Join(tmpDir, "entire")
	if runtime.GOOS == "windows" {
		testBinaryPath += ".exe"
	}

	// Route every spawned CLI away from the developer's real ~/.config/entire
	// (contexts.json, version_check.json), ~/.cache/entire (discovery caches),
	// and OS keychain. testing.Testing() is false in the subprocess, so the
	// internal/testdirs fallback cannot protect it — isolation must come from
	// the environment, which children inherit because all integration env
	// building starts from os.Environ() (testutil.GitIsolatedEnv).
	//
	// GIT_TERMINAL_PROMPT=0 and ENTIRE_TEST_GIT_HERMETIC form the hermeticity
	// tripwire: the latter makes GitIsolatedEnv's global git config route HTTPS
	// transport to real external hosts (github.com, gitlab.com) through a dead
	// loopback proxy, so any test whose git commands accidentally dial the network
	// fails fast instead of reaching it or prompting for credentials (regressions
	// #1463, 53bc37a88). The config lives in the file because GitIsolatedEnv strips
	// inherited GIT_CONFIG_* env; it proxies transport only (not url.insteadOf, which
	// would corrupt origin-URL forge detection) and leaves loopback servers untouched.
	isolation := map[string]string{
		"ENTIRE_CONFIG_DIR":           filepath.Join(tmpDir, "entire-config"),
		"XDG_CACHE_HOME":              filepath.Join(tmpDir, "entire-cache"),
		"ENTIRE_TOKEN_STORE":          "file",
		"ENTIRE_TOKEN_STORE_PATH":     filepath.Join(tmpDir, "entire-tokens.json"),
		"ENTIRE_TEST_AUTH_STORE_FILE": filepath.Join(tmpDir, "entire-auth-tokens.json"),
		"GIT_TERMINAL_PROMPT":         "0",
		testutil.EnvGitHermetic:       "1",
		// Plugin fixtures are file:// repos, which the shipped allowlist
		// refuses — nothing in production needs a local git remote, and a
		// security allowlist should not be widened for test convenience.
		// testing.Testing() is false in the spawned binary, so it is told here.
		"ENTIRE_TEST_ALLOW_FILE_REMOTES": "1",
	}
	for k, v := range isolation {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "failed to set %s: %v\n", k, err)
			os.RemoveAll(tmpDir)
			os.Exit(1)
		}
	}

	// Unset the agents' caller-session variables, for the same reason as the
	// config isolation above and with the same mechanism: the developer running
	// `mise run test:integration` is usually inside an agent, that agent
	// publishes its session ID into this process's environment, and every
	// spawned `entire` inherits it — so the binary under test resolves the
	// developer's real session as its caller and stamps it into hooks that are
	// supposed to have no session at all. Not covered by the map above because
	// isolation here means absence, not a redirected path. The list is static
	// rather than registry-derived precisely so a harness whose binary does
	// not link every agent still clears every variable — see
	// agent.callerSessionEnvVars.
	for _, name := range agent.CallerSessionEnvVars() {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintf(os.Stderr, "failed to unset %s: %v\n", name, err)
			os.RemoveAll(tmpDir)
			os.Exit(1)
		}
	}

	// Same shape, same reason: absence, not a redirected path. ENTIRE_TOKEN
	// outranks stored contexts in the identity resolver, and gitenv.Isolated()
	// filters only GIT_CONFIG_*, so it reaches the spawned binary too — a test
	// asserting the no-identity guidance would instead get a transport error
	// from the host in the developer's token aud.
	if err := os.Unsetenv(auth.EnvTokenVar); err != nil {
		fmt.Fprintf(os.Stderr, "failed to unset %s: %v\n", auth.EnvTokenVar, err)
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	moduleRoot := findModuleRoot()
	buildCmd := exec.CommandContext(context.Background(), "go", "build", "-o", testBinaryPath, ".")
	buildCmd.Dir = filepath.Join(moduleRoot, "cmd", "entire")

	buildOutput, err := buildCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build CLI binary: %v\nOutput: %s\n", err, buildOutput)
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	// Warm the binary before any test deadline is running. First exec of a
	// freshly built ~65MB binary costs ~0.6s idle on darwin/arm64 (page-in plus
	// codesign validation, once per newly written file) and several seconds
	// under this suite's own parallel -race load, against 0.03s once warm. The
	// build is immediately above, so otherwise the first test to spawn the
	// binary pays that inside its own timeout — and the victim is whoever the
	// scheduler starts first. TestExternalCommand_SigintReachesPlugin allows 3s
	// for its plugin to signal ready, which is the budget that actually broke.
	//
	// A failed warm-up only restores that previous behaviour, so it warns
	// rather than exiting: an optimization must not become a new way for the
	// whole package to fail. The bound is here for the same reason — a hang
	// would otherwise stall the package until `go test -timeout` panics it,
	// pointing at TestMain rather than at the cause.
	warmCtx, cancelWarm := context.WithTimeout(context.Background(), 60*time.Second)
	warmCmd := execx.NonInteractive(warmCtx, testBinaryPath, "--version")
	if warmOutput, warmErr := warmCmd.CombinedOutput(); warmErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not warm CLI binary: %v\nOutput: %s\n", warmErr, warmOutput)
	}
	cancelWarm()

	// Run tests
	code := m.Run()

	// Cleanup
	os.RemoveAll(tmpDir)
	os.Exit(code)
}
