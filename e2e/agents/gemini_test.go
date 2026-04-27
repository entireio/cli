package agents

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGeminiPromptEnv_TrustsWorkspaceForHeadlessRuns(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv(geminiTrustWorkspaceEnvKey, "false")

	repoDir := filepath.Join(t.TempDir(), "repo")
	env := geminiPromptEnv(repoDir)

	if got, _ := envValue(env, geminiTrustWorkspaceEnvKey); got != "true" {
		t.Fatalf("%s = %q, want true", geminiTrustWorkspaceEnvKey, got)
	}
	if got, ok := envValue(env, "ENTIRE_TEST_TTY"); ok {
		t.Fatalf("ENTIRE_TEST_TTY = %q, want unset", got)
	}
	if got, _ := envValue(env, "HOME"); got != geminiTestHomeDir(repoDir) {
		t.Fatalf("HOME = %q, want %q", got, geminiTestHomeDir(repoDir))
	}
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix), true
		}
	}
	return "", false
}
