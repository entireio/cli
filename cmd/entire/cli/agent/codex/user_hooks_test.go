package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// userCodexHome points CODEX_HOME at a fresh directory that exists, so
// Codex counts as in use.
func userCodexHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "codex-home")
	require.NoError(t, os.MkdirAll(home, 0o750))
	t.Setenv("CODEX_HOME", home)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	return home
}

func readUserHooksJSON(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, HooksFileName))
	require.NoError(t, err)
	return string(data)
}

// The user-level file carries every managed event, each command marked
// --scope=user/v<N>, and a second install changes nothing.
func TestEnsureUserHooks_InstallsScopedCommandsIdempotently(t *testing.T) {
	home := userCodexHome(t)
	ag := &CodexAgent{}

	install, err := ag.EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, HooksFileName), install.Path)
	assert.Equal(t, len(managedHooks), install.Changed)
	assert.False(t, install.Updated)
	content := readUserHooksJSON(t, home)
	for _, h := range managedHooks {
		assert.Contains(t, content, "entire hooks codex --scope=user/v1 "+h.verb)
	}

	install, err = ag.EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	assert.Zero(t, install.Changed, "second install")

	report := ag.InspectUserHooks(context.Background())
	assert.Equal(t, agent.UserHooksCurrent, report.State)
	assert.Equal(t, userHooksVersion, report.Version)
	require.NotEmpty(t, report.Notes, "no approval records yet: the report says to open /hooks")
	assert.Contains(t, report.Notes[0], "/hooks")
}

// Removal takes out exactly Entire's hooks and keeps the user's own.
func TestRemoveUserHooks_KeepsTheUsersHooks(t *testing.T) {
	home := userCodexHome(t)
	own := `{"theme":"dark","hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, HooksFileName), []byte(own), 0o600))
	ag := &CodexAgent{}
	_, err := ag.EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, readUserHooksJSON(t, home), "say done")

	removed, _, err := ag.RemoveUserHooks(context.Background())
	require.NoError(t, err)
	assert.True(t, removed)
	after := readUserHooksJSON(t, home)
	assert.NotContains(t, after, "entire hooks codex")
	assert.Contains(t, after, "say done")
	assert.Contains(t, after, `"theme"`)

	removed, _, err = ag.RemoveUserHooks(context.Background())
	require.NoError(t, err)
	assert.False(t, removed, "nothing left to remove")
}

// trustCodexHooks writes approval records, as Codex would, for every hook in
// hooksPath's managed events into the Codex config.
func trustCodexHooks(t *testing.T, codexHome, hooksPath string) {
	t.Helper()
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	var doc struct {
		Hooks map[string][]codexRawMatcherGroup `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	var b strings.Builder
	for _, h := range managedHooks {
		for gi, group := range doc.Hooks[h.event] {
			for hi, raw := range group.Hooks {
				hash, ok := codexHookDeclarationHash(h.event, h.label, group.Matcher, raw)
				require.True(t, ok, "hash for %s", h.event)
				fmt.Fprintf(&b, "[hooks.state.\"%s:%s:%d:%d\"]\ntrusted_hash = \"%s\"\n\n",
					tomlBasicEscape(hooksPath), h.label, gi, hi, hash)
			}
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(b.String()), 0o600))
}

// projectWithEntireHooks makes a committed repository at root with Entire's
// project Codex hooks installed.
func projectWithEntireHooks(t *testing.T, root string) {
	t.Helper()
	initCommittedRepo(t, root)
	t.Chdir(root)
	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
}

// The user-level hooks step aside only where the project's Entire hooks are
// there and approved; unapproved project hooks do not run, so then the
// user-level ones must.
func TestProjectHooksSupersede_OnlyForTrustedEntireProjectHooks(t *testing.T) {
	t.Cleanup(osroot.ResetShared)
	home := userCodexHome(t)
	root := filepath.Join(t.TempDir(), "repo")
	projectWithEntireHooks(t, root)
	ag := &CodexAgent{}
	ctx := context.Background()

	assert.False(t, ag.ProjectHooksSupersede(ctx), "project hooks not approved yet")

	trustCodexHooks(t, home, canonicalHooksPath(t, root))
	assert.True(t, ag.ProjectHooksSupersede(ctx), "approved project hooks take over")

	require.NoError(t, ag.UninstallHooks(ctx))
	assert.False(t, ag.ProjectHooksSupersede(ctx), "no Entire hooks in the project")
}

// Codex re-asks for approval once a hook changes, so an approval recorded
// for an earlier definition does not let the user-level hooks step aside.
func TestProjectHooksSupersede_StaleApprovalKeepsUserHooks(t *testing.T) {
	t.Cleanup(osroot.ResetShared)
	home := userCodexHome(t)
	root := filepath.Join(t.TempDir(), "repo")
	projectWithEntireHooks(t, root)
	hooksPath := canonicalHooksPath(t, root)
	trustCodexHooks(t, home, hooksPath)
	ag := &CodexAgent{}
	ctx := context.Background()
	require.True(t, ag.ProjectHooksSupersede(ctx))

	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	changed := strings.Replace(string(data), `"timeout": 30`, `"timeout": 31`, 1)
	require.NotEqual(t, string(data), changed)
	require.NoError(t, os.WriteFile(hooksPath, []byte(changed), 0o600))
	assert.False(t, ag.ProjectHooksSupersede(ctx), "hook changed after approval")

	configPath := filepath.Join(home, "config.toml")
	trustCodexHooks(t, home, hooksPath)
	config, err := os.ReadFile(configPath)
	require.NoError(t, err)
	unhashed := regexp.MustCompile(`(?m)^trusted_hash = .*$`).ReplaceAllString(string(config), "enabled = true")
	require.NoError(t, os.WriteFile(configPath, []byte(unhashed), 0o600))
	assert.False(t, ag.ProjectHooksSupersede(ctx), "records without a trusted_hash")
}

// A linked worktree: Codex discovers the main checkout's hooks, so that
// file decides.
func TestProjectHooksSupersede_LinkedWorktreeUsesTheDiscoveredFile(t *testing.T) {
	t.Cleanup(osroot.ResetShared)
	home := userCodexHome(t)
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	projectWithEntireHooks(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)
	require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))
	trustCodexHooks(t, home, canonicalHooksPath(t, mainRoot))
	t.Chdir(linkedRoot)

	assert.True(t, (&CodexAgent{}).ProjectHooksSupersede(context.Background()))
}

// A home directory that is itself a repository: the project hooks file
// would be the user-level one, which never supersedes itself.
func TestProjectHooksSupersede_HomeThatIsARepository(t *testing.T) {
	t.Cleanup(osroot.ResetShared)
	fakeHome := filepath.Join(t.TempDir(), "home")
	initCommittedRepo(t, fakeHome)
	t.Setenv("HOME", fakeHome)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(fakeHome, ".codex"), 0o750))
	t.Chdir(fakeHome)
	ag := &CodexAgent{}
	install, err := ag.EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	trustCodexHooks(t, filepath.Join(fakeHome, ".codex"), install.Path)

	assert.False(t, ag.ProjectHooksSupersede(context.Background()))
}

// The user-level file is valid hooks.json Codex can parse: an object of
// events, each a list of matcher groups.
func TestEnsureUserHooks_WritesCodexShapedJSON(t *testing.T) {
	home := userCodexHome(t)
	_, err := (&CodexAgent{}).EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	var doc struct {
		Hooks map[string][]MatcherGroup `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(readUserHooksJSON(t, home)), &doc))
	require.NotEmpty(t, doc.Hooks["Stop"])
}
