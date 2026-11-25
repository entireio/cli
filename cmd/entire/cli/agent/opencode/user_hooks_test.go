package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func userOpenCodeHome(t *testing.T) string {
	t.Helper()
	xdg := filepath.Join(t.TempDir(), "xdg")
	require.NoError(t, os.MkdirAll(filepath.Join(xdg, "opencode"), 0o750))
	t.Setenv(xdgConfigHomeEnvVar, xdg)
	return filepath.Join(xdg, "opencode")
}

// Install writes the user-level render to plugin/entire.ts once, inspect
// reports it current with its version, and removal takes it away.
func TestUserHooks_InstallInspectRemove(t *testing.T) {
	home := userOpenCodeHome(t)
	ag := &OpenCodeAgent{}
	ctx := context.Background()

	n, path, err := ag.EnsureUserHooks(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, filepath.Join(home, "plugin", "entire.ts"), path)
	report := ag.InspectUserHooks(ctx)
	assert.Equal(t, agent.UserHooksCurrent, report.State)
	assert.Equal(t, userHooksVersion, report.Version)

	removed, _, err := ag.RemoveUserHooks(ctx)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.NoFileExists(t, path)
}

// The project render keeps no trace of the user-level placeholders, so
// committed project plugins stay current.
func TestRenderPlugin_ProjectRenderHasNoUserLevelCode(t *testing.T) {
	t.Parallel()
	assert.NotContains(t, renderPlugin(), "__ENTIRE_USER_LEVEL")
	assert.NotContains(t, renderPlugin(), "userLevelSteppedAside")
	assert.Contains(t, renderPluginFor(true), "userLevelSteppedAside(directory, false)")
	assert.Contains(t, renderPluginFor(true), "userLevelSteppedAside(ctx.location.directory, true)")
}

// stepAsideDriver loads a plugin and reports how many hooks it registered,
// through the OpenCode 1 entry point (OPENCODE=1) or OpenCode 2's setup.
const stepAsideDriver = `
import { pathToFileURL } from "node:url"
const { default: plugin } = await import(pathToFileURL(process.argv[2]).href)
const dir = process.argv[3]
let registered = 0
if (typeof plugin === "function") {
  const out = await plugin({ directory: dir, worktree: dir, client: {}, project: {} })
  registered = Object.keys(out ?? {}).length
} else {
  const never = { [Symbol.asyncIterator]() { return { next: () => new Promise(() => {}) } } }
  await plugin.setup({
    location: { directory: dir },
    session: { hook: async () => { registered++ }, get: async () => undefined },
    tool: { hook: async () => { registered++ } },
    event: { subscribe: () => never },
  })
}
console.log(registered)
process.exit(0)
`

// The user-level plugin registers nothing where OpenCode also loads the
// project's own current Entire plugin (searching up from the directory to
// the repository root), and handles the session otherwise, on both OpenCode
// entry points: an older render may not run on this OpenCode, and OpenCode 2
// does not load the legacy plugins directory.
func TestUserLevelPlugin_StepsAsideForTheProjectPlugin(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	stale := strings.Replace(renderPlugin(), "hookCmd", "hookCmdOld", 1)
	for name, tc := range map[string]struct {
		place    string // where the project copy goes, relative to the outer dir
		content  string
		stepAway bool
		// onlyOpenCode1 limits stepAway to the OpenCode 1 entry point.
		onlyOpenCode1 bool
	}{
		"no project plugin":           {},
		"project plugin at repo root": {place: "repo/.opencode/plugin/entire.ts", content: renderPlugin(), stepAway: true},
		"CRLF project plugin":         {place: "repo/.opencode/plugin/entire.ts", content: strings.ReplaceAll(renderPlugin(), "\n", "\r\n"), stepAway: true},
		"older project plugin":        {place: "repo/.opencode/plugin/entire.ts", content: stale},
		"legacy plugins directory":    {place: "repo/.opencode/plugins/entire.ts", content: renderPlugin(), stepAway: true, onlyOpenCode1: true},
		"someone else's entire.ts":    {place: "repo/.opencode/plugin/entire.ts", content: "export default {}\n"},
		"above the repository root":   {place: ".opencode/plugin/entire.ts", content: renderPlugin()},
	} {
		for _, opencode1 := range []bool{true, false} {
			t.Run(name+"/opencode1="+strconv.FormatBool(opencode1), func(t *testing.T) {
				t.Parallel()
				outer := t.TempDir()
				start := filepath.Join(outer, "repo", "sub")
				require.NoError(t, os.MkdirAll(start, 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(outer, "repo", ".git"), 0o755))
				userPath := filepath.Join(outer, "user", "opencode", "plugin", "entire.ts")
				require.NoError(t, os.MkdirAll(filepath.Dir(userPath), 0o755))
				require.NoError(t, os.WriteFile(userPath, []byte(renderPluginFor(true)), 0o644))
				driver := filepath.Join(outer, "driver.mjs")
				require.NoError(t, os.WriteFile(driver, []byte(stepAsideDriver), 0o644))
				if tc.place != "" {
					project := filepath.Join(outer, filepath.FromSlash(tc.place))
					require.NoError(t, os.MkdirAll(filepath.Dir(project), 0o755))
					require.NoError(t, os.WriteFile(project, []byte(tc.content), 0o644))
				}
				cmd := exec.CommandContext(t.Context(), "node", "--experimental-strip-types", driver, userPath, start)
				cmd.Dir = start
				cmd.Env = append(os.Environ(), "PATH="+t.TempDir())
				cmd.Env = append(cmd.Env, "OPENCODE=")
				if opencode1 {
					cmd.Env = append(cmd.Env, "OPENCODE=1")
				}
				out, err := cmd.Output()
				var stderr string
				if ee, ok := err.(*exec.ExitError); ok { //nolint:errorlint // only for the message
					stderr = string(ee.Stderr)
				}
				require.NoError(t, err, stderr)
				registered, err := strconv.Atoi(strings.TrimSpace(string(out)))
				require.NoError(t, err, string(out))
				assert.Equal(t, tc.stepAway && (opencode1 || !tc.onlyOpenCode1), registered == 0, "registered %d hooks", registered)
			})
		}
	}
}
