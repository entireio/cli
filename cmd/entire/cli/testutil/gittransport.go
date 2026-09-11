package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// GitTransportRealGitEnv is the variable the shim execs through. Callers set it
// to the real git binary; the shim never resolves git from PATH itself, since
// PATH is where it has just been installed.
const GitTransportRealGitEnv = "CHECKPOINT_TEST_GIT"

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
