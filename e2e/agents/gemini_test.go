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

func TestGeminiIsTransientError_RecognizesAbortedTurn(t *testing.T) {
	t.Parallel()
	g := &Gemini{}
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"invalid stream", "[ERROR] Invalid stream: The model returned an empty response or malformed tool call.", true},
		{"malformed tool call phrase", "something empty response or malformed tool call happened", true},
		{"existing transient pattern still matched", "Error: RESOURCE_EXHAUSTED", true},
		{"unrelated error", "fatal: not a git repository", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := g.IsTransientError(Output{Stderr: tc.stderr}, nil)
			if got != tc.want {
				t.Fatalf("IsTransientError(stderr=%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

func TestGeminiAbortedTurn(t *testing.T) {
	t.Parallel()
	if !geminiAbortedTurn("[ERROR] Invalid stream: ...") {
		t.Fatal("expected Invalid stream to be detected as aborted turn")
	}
	if geminiAbortedTurn("clean run, no errors") {
		t.Fatal("did not expect a clean run to be detected as aborted turn")
	}
}

func TestGeminiModel(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("E2E_GEMINI_MODEL", "")
		if got := geminiModel(); got != geminiDefaultModel {
			t.Fatalf("geminiModel() = %q, want default %q", got, geminiDefaultModel)
		}
	})
	t.Run("honors override", func(t *testing.T) {
		t.Setenv("E2E_GEMINI_MODEL", "gemini-2.5-pro")
		if got := geminiModel(); got != "gemini-2.5-pro" {
			t.Fatalf("geminiModel() = %q, want override gemini-2.5-pro", got)
		}
	})
}
