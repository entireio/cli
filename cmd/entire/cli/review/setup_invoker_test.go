package review_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review"
)

func TestDetectInvokingAgent_ReadsEnvSentinels(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Setenv to drive env-var
	// behavior; Go's testing package panics on Setenv + Parallel.

	cases := []struct {
		envKey string
		want   string
	}{
		{"CLAUDE_CODE", "claude-code"},
		{"CODEX", "codex"},
		{"GEMINI_CLI", "gemini-cli"},
		{"COPILOT_CLI", "copilot-cli"},
		{"PI_CODING_AGENT", "pi"},
	}
	for _, c := range cases {
		t.Run(c.envKey, func(t *testing.T) {
			// Clear other sentinels so the test doesn't see drift from
			// the parent shell.
			for _, k := range []string{"CLAUDE_CODE", "CODEX", "GEMINI_CLI", "COPILOT_CLI", "PI_CODING_AGENT"} {
				t.Setenv(k, "")
			}
			t.Setenv(c.envKey, "1")
			got := review.DetectInvokingAgent()
			if got != c.want {
				t.Errorf("env %s=1 → DetectInvokingAgent() = %q, want %q", c.envKey, got, c.want)
			}
		})
	}
}

func TestDetectInvokingAgent_NoSentinelReturnsEmpty(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE", "CODEX", "GEMINI_CLI", "COPILOT_CLI", "PI_CODING_AGENT"} {
		t.Setenv(k, "")
	}
	if got := review.DetectInvokingAgent(); got != "" {
		t.Errorf("with no sentinels, got %q, want \"\"", got)
	}
}
