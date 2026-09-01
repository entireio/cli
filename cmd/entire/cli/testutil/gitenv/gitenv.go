// Package gitenv isolates test git invocations from the host's git config.
// It deliberately imports nothing from this repo so that internal tests of
// low-level packages (e.g. gitrepo, which testutil itself imports) can use it
// without an import cycle; testutil re-exports it for everyone else.
package gitenv

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// emptyConfigPath returns the path to a config file suitable for use as
// GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM. We use a real file instead of
// os.DevNull because git on Windows cannot open NUL as a config file.
//
// The file is not strictly empty: it pins background maintenance off so that
// no detached `git gc`/`git maintenance` process lingers after a test holding
// an open handle on the temp repo's .git/objects. Such a lingering process
// races t.TempDir()'s deferred RemoveAll and fails the test with
// "directory not empty" (see COR-394). Suppressing it centrally keeps every
// git-shelling test that uses Isolated/IsolateProcess safe.
var emptyConfig string
var emptyConfigOnce sync.Once

// EnvHermetic, when set to a non-empty value, makes emptyConfigPath append
// per-host HTTP proxy config that routes git HTTPS transport to real external
// hosts (github.com, gitlab.com) through an unroutable loopback proxy. Any test
// whose git commands accidentally dial those hosts then fails fast (connection
// refused at 127.0.0.1:1) instead of reaching the network or prompting for
// credentials. It is opt-in per test process — the integration TestMain sets it
// — so unit test packages that don't set it are unaffected. Because
// Isolated strips all inherited GIT_CONFIG_* env, this config must live in
// the file GIT_CONFIG_GLOBAL points at (this one), not in GIT_CONFIG_* env
// entries.
//
// A dead proxy (not url.insteadOf) is used deliberately: insteadOf rewrites the
// effective URL that git reports on read, which breaks production code that
// resolves the origin URL to detect the forge (e.g. `entire trail`). The proxy
// blocks transport only, leaving the configured URL string intact, and is scoped
// per host so loopback (127.0.0.1) test servers are never proxied.
//
// Regression class: tests accidentally hitting live github.com / the macOS
// keychain (#1463, 53bc37a88).
const EnvHermetic = "ENTIRE_TEST_GIT_HERMETIC"

// hermeticGitConfig routes HTTPS transport to real external hosts through a dead
// loopback proxy. Loopback test servers (127.0.0.1) and file:// / bare-path
// remotes are not proxied, so the in-process HTTPS git server still works. Only
// HTTPS is covered — the accidental-dial regression class is HTTPS fetches; SSH
// (git@…) to a real host fails on its own without credentials.
const hermeticGitConfig = "[http \"https://github.com/\"]\n" +
	"\tproxy = http://127.0.0.1:1\n" +
	"[http \"https://gitlab.com/\"]\n" +
	"\tproxy = http://127.0.0.1:1\n"

func emptyConfigPath() string {
	emptyConfigOnce.Do(func() {
		f, err := os.CreateTemp("", "git-isolation-config-*")
		if err != nil {
			panic("create git isolation config: " + err.Error())
		}
		content := "[gc]\n\tauto = 0\n\tautoDetach = false\n[maintenance]\n\tauto = false\n[fetch]\n\twriteCommitGraph = false\n"
		if os.Getenv(EnvHermetic) != "" {
			content += hermeticGitConfig
		}
		_, err = f.WriteString(content)
		if err != nil {
			panic("write git isolation config: " + err.Error())
		}
		if err := f.Close(); err != nil {
			panic("close git isolation config: " + err.Error())
		}
		emptyConfig = f.Name()
	})
	return emptyConfig
}

// Isolated returns os.Environ() with git isolation variables set.
// This prevents user/system git config (global gitignore, aliases, etc.) from
// affecting test behavior. Use this for any exec.Command that runs git or the
// CLI binary in integration tests.
//
// See https://git-scm.com/docs/git#Documentation/git.txt-GITCONFIGGLOBAL
//
// Every inherited GIT_CONFIG_* entry is filtered out — including
// GIT_CONFIG_PARAMETERS and the indexed KEY_/VALUE_ pairs that can inject
// `git -c` overrides — so our explicit isolation overrides take effect
// regardless of parent env.
func Isolated() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+2)
	for _, e := range env {
		if isGitConfigEnv(e) {
			continue
		}
		filtered = append(filtered, e)
	}
	return append(filtered,
		"GIT_CONFIG_GLOBAL="+emptyConfigPath(), // Isolate from user's global git config (e.g. global gitignore)
		"GIT_CONFIG_SYSTEM="+emptyConfigPath(), // Isolate from system git config
	)
}

// IsolateProcess applies the same git config isolation to the current
// process. Use this in tests that exercise production code paths which invoke
// git with os.Environ(). All inherited GIT_CONFIG_* variables are cleared
// before the isolation overrides are set, so values such as
// GIT_CONFIG_PARAMETERS or indexed KEY_/VALUE_ overrides cannot leak into
// child git invocations.
func IsolateProcess(t *testing.T) {
	t.Helper()

	for _, e := range os.Environ() {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			t.Setenv(key, "")
		}
	}

	t.Setenv("GIT_CONFIG_GLOBAL", emptyConfigPath())
	t.Setenv("GIT_CONFIG_SYSTEM", emptyConfigPath())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
}

// IsolateMain is IsolateProcess for a TestMain, which has no *testing.T to
// restore through: the isolation is set process-wide for the whole run via
// os.Setenv and inherited by every spawned binary and git hook. Inherited
// GIT_CONFIG_* overrides are neutralized (GIT_CONFIG_COUNT=0 disables the
// indexed KEY_/VALUE_ pairs) rather than unset.
func IsolateMain() {
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":     emptyConfigPath(),
		"GIT_CONFIG_SYSTEM":     emptyConfigPath(),
		"GIT_CONFIG_NOSYSTEM":   "1",
		"GIT_CONFIG_COUNT":      "0",
		"GIT_CONFIG_PARAMETERS": "",
	} {
		if err := os.Setenv(k, v); err != nil {
			panic("set " + k + ": " + err.Error())
		}
	}
}

// Run runs one git command in dir with an isolated git config, failing the
// test on error and returning combined output.
func Run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:noctx // test helper, no context needed
	cmd.Dir = dir
	cmd.Env = Isolated()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %q: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func isGitConfigEnv(e string) bool {
	return strings.HasPrefix(e, "GIT_CONFIG_")
}
