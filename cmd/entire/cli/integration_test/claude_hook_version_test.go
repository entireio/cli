//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeHookVersionCommands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"old", []string{"TEST_CLAUDE_VERSION_OUTPUT=2.1.72 (Claude Code)"}, "2.1.78 or later"},
		{"minimum", []string{"TEST_CLAUDE_VERSION_OUTPUT=2.1.78 (Claude Code)"}, ""},
		{"unreadable", []string{"TEST_CLAUDE_VERSION_OUTPUT=unknown"}, "cannot read Claude Code version"},
		{"probe_failure", []string{"TEST_CLAUDE_VERSION_FAIL=1"}, "cannot check Claude Code version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, command := range []struct {
				name string
				args []string
			}{
				{"enable", []string{"enable", "--agent", "claude-code", "--telemetry=false"}},
				{"agent_add", []string{"agent", "add", "claude-code"}},
				{"agent_add_force", []string{"agent", "add", "claude-code", "--force"}},
			} {
				t.Run(command.name, func(t *testing.T) {
					t.Parallel()
					env := NewTestEnv(t)
					env.InitRepo()
					env.ExtraEnv = tc.env
					const original = `{"model":"user-choice"}`
					env.WriteFile(".claude/settings.json", original)
					output, err := env.RunCLIWithError(command.args...)
					data, readErr := os.ReadFile(filepath.Join(env.RepoDir, ".claude", "settings.json"))
					require.NoError(t, readErr)
					if tc.want != "" {
						require.Error(t, err, output)
						require.Contains(t, output, tc.want)
						require.Equal(t, []byte(original), data) //nolint:testifylint // Refusal must preserve exact bytes, including formatting.
					} else {
						require.NoError(t, err, output)
						require.Contains(t, string(data), "StopFailure")
						require.Contains(t, string(data), "user-choice")
					}
				})
			}
		})
	}
}
