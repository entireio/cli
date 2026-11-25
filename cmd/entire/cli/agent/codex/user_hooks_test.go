package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	n, path, err := ag.EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, HooksFileName), path)
	assert.Equal(t, len(managedHooks), n)
	content := readUserHooksJSON(t, home)
	for _, h := range managedHooks {
		assert.Contains(t, content, "entire hooks codex --scope=user/v1 "+h.verb)
	}

	n, _, err = ag.EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	assert.Zero(t, n, "second install")

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
	_, _, err := ag.EnsureUserHooks(context.Background(), nil)
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

// trustCodexHooks writes approval records for every Entire event in
// hooksPath into the Codex config.
func trustCodexHooks(t *testing.T, codexHome, hooksPath string) {
	t.Helper()
	var b strings.Builder
	for _, h := range managedHooks {
		fmt.Fprintf(&b, "[hooks.state.\"%s:%s:0:0\"]\ntrusted_hash = \"x\"\n\n", tomlBasicEscape(hooksPath), h.label)
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
	require.NoError(t, os.MkdirAll(filepath.Join(fakeHome, ".codex"), 0o750))
	t.Chdir(fakeHome)
	ag := &CodexAgent{}
	_, path, err := ag.EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	trustCodexHooks(t, filepath.Join(fakeHome, ".codex"), path)

	assert.False(t, ag.ProjectHooksSupersede(context.Background()))
}

// The user-level file is valid hooks.json Codex can parse: an object of
// events, each a list of matcher groups.
func TestEnsureUserHooks_WritesCodexShapedJSON(t *testing.T) {
	home := userCodexHome(t)
	_, _, err := (&CodexAgent{}).EnsureUserHooks(context.Background(), nil)
	require.NoError(t, err)
	var doc struct {
		Hooks map[string][]MatcherGroup `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(readUserHooksJSON(t, home)), &doc))
	require.NotEmpty(t, doc.Hooks["Stop"])
}
