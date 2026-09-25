package gitenv

import (
	"os"
	"testing"
)

func TestIsolateRepository_RestoresSelectors(t *testing.T) {
	// Both levels mutate the environment and must remain serial.
	keys := []string{"GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"}
	for _, key := range keys {
		t.Setenv(key, "inherited-selector")
	}
	t.Run("isolated", func(t *testing.T) {
		IsolateRepository(t)
		for _, key := range keys {
			if value, ok := os.LookupEnv(key); ok {
				t.Errorf("%s = %q, want absent", key, value)
			}
		}
		if os.Getenv("GIT_CONFIG_GLOBAL") != emptyConfigPath() {
			t.Error("global Git config was not isolated")
		}
	})
	for _, key := range keys {
		if value := os.Getenv(key); value != "inherited-selector" {
			t.Errorf("%s = %q after cleanup, want inherited selector", key, value)
		}
	}
}

// TestUnsetGlobalConfig_LeavesTheVariableAbsent pins the distinction the helper
// exists for. Git and go-git both read an EMPTY GIT_CONFIG_GLOBAL as "no global
// config at all", so emptying the variable is not the same as removing it: a
// refactor that keeps only t.Setenv(key, "") and drops the os.Unsetenv would
// disable global config outright rather than hand resolution back to $HOME, and
// every caller that points HOME at a fixture would silently read nothing.
func TestUnsetGlobalConfig_LeavesTheVariableAbsent(t *testing.T) {
	// Cannot t.Parallel(): mutates GIT_CONFIG_GLOBAL process-wide via t.Setenv.
	t.Setenv("GIT_CONFIG_GLOBAL", emptyConfigPath())

	t.Run("removes an inherited value", func(t *testing.T) {
		UnsetGlobalConfig(t)
		if value, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); ok {
			t.Fatalf("GIT_CONFIG_GLOBAL = %q, want absent", value)
		}
	})

	// The subtest's t.Setenv cleanup must put the inherited value back, or a
	// later test in this process inherits the hole this one punched.
	if value, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); !ok || value != emptyConfigPath() {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q (set=%t) after cleanup, want %q", value, ok, emptyConfigPath())
	}
}
