package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var (
	// gitSubcommandRe, transportSpellingRe and envNameRe bound what may reach
	// the generated script. Shell metacharacters — `;` `)` `$` `` ` `` `|` `&`
	// `<` `>` quotes, whitespace and newlines — are absent from all three by
	// construction, which is the property that makes the generation safe.
	gitSubcommandRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// A remote name or a URL: git's own remote-name charset plus the URL
	// punctuation these fixtures use. No userinfo `@:` password syntax is
	// needed, but `@` and `:` are allowed for scp-style and port forms.
	transportSpellingRe = regexp.MustCompile(`^[A-Za-z0-9._~:/@+-]+$`)
	envNameRe           = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// GitTransportRealGitEnv is the variable the shim execs through. Callers set it
// to the real git binary; the shim never resolves git from PATH itself, since
// PATH is where it has just been installed.
const GitTransportRealGitEnv = "CHECKPOINT_TEST_GIT"

// validateShimInputs refuses anything that would not survive interpolation
// into the generated script.
//
// Every argument reaches a bash script that is then made executable and placed
// first on PATH, so an unvalidated one is arbitrary command execution, not a
// cosmetic problem: a `case` pattern ends at the first unquoted `)` and a
// command at the first `;`. The code this helper replaced carried all three
// values as literals and could not be fed anything; making it shared made it
// feedable, and these allowlists are what keep the two equivalent.
//
// Deliberately narrower than what git accepts. A fixture needing something
// outside them should write its own script rather than widen these.
func validateShimInputs(subcommands []string, rewrites map[string]string) error {
	for _, sub := range subcommands {
		if !gitSubcommandRe.MatchString(sub) {
			return fmt.Errorf("subcommand %q is not a plain git subcommand; it would be interpolated into a shell pattern", sub)
		}
	}
	for spelling, envName := range rewrites {
		if !transportSpellingRe.MatchString(spelling) {
			return fmt.Errorf("rewrite key %q is not a remote name or URL; it would be interpolated into a shell pattern", spelling)
		}
		if !envNameRe.MatchString(envName) {
			return fmt.Errorf("rewrite value %q is not an environment variable name; it would be interpolated as a shell expansion", envName)
		}
	}
	return nil
}

// GitTransportShim writes a `git` wrapper into a fresh temp bin dir and returns
// that dir, for tests that must exercise checkpoint remote resolution against
// local repositories without disturbing the forge identities it votes on.
//
// Only the arguments of the named subcommands are rewritten, so `git remote
// get-url` and friends keep reporting the real https URLs — which is the whole
// point: checkpoint ownership checks compare the OWNERS of those URLs, and a
// fixture that rewrote them everywhere would make every identity agree and the
// topology under test disappear.
//
// rewrites maps a spelling as it appears on the command line — a URL, or a bare
// remote name — to the NAME of an environment variable holding the local path
// it should resolve to. The indirection is load-bearing: the wrapper reads the
// variable when git runs, so a test can repoint one with t.Setenv after the
// shim exists (pointing a remote at a missing repository to force a transport
// failure, say).
//
// Skips on Windows, which has no bash to run the wrapper.
func GitTransportShim(t *testing.T, subcommands []string, rewrites map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("transport mapping uses a bash git wrapper")
	}
	if len(subcommands) == 0 || len(rewrites) == 0 {
		t.Fatalf("GitTransportShim needs at least one subcommand and one rewrite, got %d and %d",
			len(subcommands), len(rewrites))
	}

	if err := validateShimInputs(subcommands, rewrites); err != nil {
		t.Fatalf("GitTransportShim: %v", err)
	}

	// Sorted so the generated script is identical run to run; a fixture that
	// varies with map iteration order is one that fails intermittently.
	spellings := make([]string, 0, len(rewrites))
	for spelling := range rewrites {
		spellings = append(spellings, spelling)
	}
	sort.Strings(spellings)

	var arms strings.Builder
	for _, spelling := range spellings {
		fmt.Fprintf(&arms, "          %s) args[$i]=\"$%s\" ;;\n", spelling, rewrites[spelling])
	}

	script := fmt.Sprintf(`#!/bin/bash
args=("$@")
for arg in "$@"; do
  case "$arg" in
    %s)
      for i in "${!args[@]}"; do
        case "${args[$i]}" in
%s        esac
      done
      break ;;
  esac
done
exec "$%s" "${args[@]}"
`, strings.Join(subcommands, "|"), arms.String(), GitTransportRealGitEnv)

	binDir := t.TempDir()
	WriteFile(t, binDir, "git", script)
	// Executable by design — it is a wrapper git must be able to run from
	// PATH, which gosec's "0600 or less" rule cannot express.
	if err := os.Chmod(filepath.Join(binDir, "git"), 0o755); err != nil { //nolint:gosec // a PATH shim must be executable
		t.Fatalf("chmod git shim: %v", err)
	}
	return binDir
}
