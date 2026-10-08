package testutil

import (
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// AssertLegacyHookReplaced pins the migration contract for agents whose config is
// JSON: a hook command written by an older version must be REPLACED by a plain
// install, not joined by the current one.
//
// It installs, rewrites the installed command into legacyCmd to simulate a config
// an older version wrote, installs again with force=false, and asserts the legacy
// command is gone and the current one appears exactly once. Two hooks would both
// fire, and for the removed local-dev mode the legacy one ran a script inside the
// working tree — so "replaced" and "added alongside" are very different outcomes.
//
// install must perform a non-force InstallHooks for the agent under test.
func AssertLegacyHookReplaced(t *testing.T, configPath, currentCmd, legacyCmd string, install func()) {
	t.Helper()

	install()

	raw := readConfig(t, configPath)
	seeded := strings.Replace(raw, encodeJSONStringBody(t, currentCmd), encodeJSONStringBody(t, legacyCmd), 1)
	if seeded == raw {
		t.Fatalf("could not find the installed command in %s to seed the legacy hook.\nlooking for: %s\ngot:\n%s", configPath, currentCmd, raw)
	}
	writeConfig(t, configPath, seeded)

	install()

	after := readConfig(t, configPath)
	if got := strings.Count(after, encodeJSONStringBody(t, legacyCmd)); got != 0 {
		t.Errorf("legacy hook survived a non-force install (%d occurrences) in %s:\n%s", got, configPath, after)
	}
	if got := strings.Count(after, encodeJSONStringBody(t, currentCmd)); got != 1 {
		t.Errorf("current hook appears %d times in %s, want exactly 1:\n%s", got, configPath, after)
	}
}

// AssertStaleHookDroppedAlongsideCurrent covers the state a machine lands in when
// a legacy hook and the current hook are both present: nothing needs *adding*, so
// an install that only writes when it added something would silently leave the
// legacy hook on disk.
//
// seedBoth must write a config containing both currentCmd and legacyCmd; it needs
// per-agent format knowledge, which is why it is a callback rather than a string
// substitution.
func AssertStaleHookDroppedAlongsideCurrent(t *testing.T, configPath, currentCmd, legacyCmd string, seedBoth, install func()) {
	t.Helper()

	seedBoth()
	before := readConfig(t, configPath)
	if !strings.Contains(before, encodeJSONStringBody(t, legacyCmd)) || !strings.Contains(before, encodeJSONStringBody(t, currentCmd)) {
		t.Fatalf("seed must contain both the legacy and the current command, got:\n%s", before)
	}

	install()

	after := readConfig(t, configPath)
	if strings.Contains(after, encodeJSONStringBody(t, legacyCmd)) {
		t.Errorf("legacy hook survived alongside the current one in %s:\n%s", configPath, after)
	}
	if got := strings.Count(after, encodeJSONStringBody(t, currentCmd)); got != 1 {
		t.Errorf("current hook appears %d times in %s, want exactly 1:\n%s", got, configPath, after)
	}
}

// encodeJSONStringBody renders s the way it appears inside a JSON string on disk,
// without the surrounding quotes. Hook commands contain characters that must be
// escaped (`"` in the legacy launcher, `>` in the production wrapper), and these
// configs are written with the no-HTML-escape marshaller — so a plain
// json.Marshal would escape `>` as \u003e and never match.
func encodeJSONStringBody(t *testing.T, s string) string {
	t.Helper()

	b, err := jsonutil.MarshalWithNoHTMLEscape(s)
	if err != nil {
		t.Fatalf("failed to encode %q: %v", s, err)
	}
	return string(b[1 : len(b)-1])
}

func readConfig(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return string(data)
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

// Legacy hook command shapes, for seeding a config an older Entire would have
// written. They live in this test-only package deliberately: the agent package
// keeps its recognized set unexported so production code cannot name — and
// therefore cannot install — a command that runs Entire from the working tree.
// Construction is available only to tests.
//
// suffix is the part after the launcher, e.g. "hooks cursor stop".
const (
	legacyGitRevParseLauncher      = `"$(git rev-parse --show-toplevel)"/scripts/entire-dev`
	legacyClaudeProjectDirLauncher = "${CLAUDE_PROJECT_DIR}/scripts/entire-dev"
)

// LegacyLocalDevCommand builds the local-dev hook command for agents that
// resolved the repo root by shelling out to git.
func LegacyLocalDevCommand(suffix string) string {
	return legacyGitRevParseLauncher + " " + suffix
}

// LegacyClaudeProjectDirCommand builds the local-dev hook command for Claude
// Code, which had ${CLAUDE_PROJECT_DIR} and so did not need git.
func LegacyClaudeProjectDirCommand(suffix string) string {
	return legacyClaudeProjectDirLauncher + " " + suffix
}
