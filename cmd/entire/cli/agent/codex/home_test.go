package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every CODEX_HOME reader must agree with resolveCodexHome: a blank value is
// unset for all of them, and a relative value is refused by all of them, so
// session-dir resolution, trust inspection and the user-wide hook-root check
// cannot name three different directories for one environment.
func TestCodexHomeReadersAgree(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("blank counts as unset everywhere", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "  ")
		if got, want := codexConfigPath(), filepath.Join(home, ".codex", "config.toml"); got != want {
			t.Errorf("codexConfigPath() = %q, want %q", got, want)
		}
		got, err := resolveCodexHome()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".codex"); got != want {
			t.Errorf("resolveCodexHome() = %q, want %q", got, want)
		}
	})

	t.Run("relative is refused everywhere", func(t *testing.T) {
		t.Setenv("CODEX_HOME", filepath.Join("relative", "codex"))
		if _, err := resolveCodexHome(); err == nil || !strings.Contains(err.Error(), "CODEX_HOME") {
			t.Fatalf("resolveCodexHome() error = %v; want a refusal naming CODEX_HOME", err)
		}
		if got := codexConfigPath(); got != "" {
			t.Errorf("codexConfigPath() = %q, want empty when the home is refused", got)
		}
		if isUserHookRoot(t.TempDir()) {
			t.Error("isUserHookRoot() = true for an arbitrary dir while CODEX_HOME is refused; want false")
		}
	})
}
