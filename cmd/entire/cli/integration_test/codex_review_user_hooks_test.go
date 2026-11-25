//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// An isolated Codex reviewer registers Entire's hooks as session flags, and
// the user's own Codex config still loads. ReviewUserHookStateOverride must
// disable exactly the Entire handlers there, keyed the way Codex keys them,
// for a symlinked CODEX_HOME and for a symlinked default ~/.codex.
func TestCodexAppServerHooksList_ReviewDisablesUserEntireHooks(t *testing.T) {
	t.Parallel()
	codexPath := requireCodexAppServer(t)

	// An empty group and a non-command handler still take positions in
	// Codex's numbering; the Entire handler's key must count them.
	const hooksJSON = `{"hooks":{"Stop":[
  {"hooks":[]},
  {"matcher":"x","hooks":[{"type":"command","command":"echo user-first"}]},
  {"hooks":[{"type":"prompt","prompt":"p"},{"type":"command","command":"echo user-mine"},{"type":"command","command":"entire hooks codex stop"}]}
]}}`
	const configTOML = `[features]
hooks = true

[[hooks.SessionStart]]
hooks = [{ type = "command", command = "echo user-toml" }, { type = "command", command = "entire hooks codex session-start" }]
`
	userCommands := map[string]bool{"echo user-first": true, "echo user-mine": true, "echo user-toml": true}
	entireCommands := map[string]bool{"entire hooks codex stop": true, "entire hooks codex session-start": true}

	for _, tc := range []struct {
		name       string
		isOverride bool
	}{
		{"symlinked CODEX_HOME", true},
		{"symlinked default home", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			testutil.InitRepo(t, repo)
			target := filepath.Join(base, "target-codex")
			require.NoError(t, os.Mkdir(target, 0o700))
			testutil.WriteFile(t, target, codex.HooksFileName, hooksJSON)
			testutil.WriteFile(t, target, "config.toml", configTOML)

			var env []string
			var home string
			if tc.isOverride {
				home = filepath.Join(base, "codex-link")
				require.NoError(t, os.Symlink(target, home))
				env = []string{"CODEX_HOME=" + home}
			} else {
				fakeHome := filepath.Join(base, "home")
				require.NoError(t, os.Mkdir(fakeHome, 0o700))
				home = filepath.Join(fakeHome, ".codex")
				require.NoError(t, os.Symlink(target, home))
				env = []string{"HOME=" + fakeHome, "CODEX_HOME="}
			}

			args := codex.ReviewUserHookStateOverride(t.Context(), home, tc.isOverride)
			require.Len(t, args, 2, "want one -c hooks.state override")

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			result := listCodexHooksWith(ctx, t, codexPath, repo, env, args)
			require.Len(t, result.Data, 1)
			require.Empty(t, result.Data[0].Errors)

			seen := map[string]bool{}
			for _, hook := range result.Data[0].Hooks {
				switch {
				case entireCommands[hook.Command]:
					require.False(t, hook.Enabled, "Entire hook %s (%s) should be disabled", hook.Key, hook.Command)
				case userCommands[hook.Command]:
					require.True(t, hook.Enabled, "user hook %s (%s) should stay enabled", hook.Key, hook.Command)
				default:
					continue
				}
				seen[hook.Command] = true
			}
			require.Len(t, seen, len(userCommands)+len(entireCommands), "hooks/list result: %+v", result)
		})
	}
}
