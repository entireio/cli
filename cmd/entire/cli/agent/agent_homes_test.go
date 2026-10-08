package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// homeStubAgent is a HomeLayoutProvider whose active home is home.
type homeStubAgent struct {
	mockBaseAgent

	home string
}

func (h *homeStubAgent) SessionHome() (string, error) { return h.home, nil }
func (h *homeStubAgent) HomeLayout() HomeLayout {
	return HomeLayout{Stores: []string{"sessions"}}
}

// homesTestAgent is mockBaseAgent's type, under which homeStubAgent's homes
// are recorded.
const homesTestAgent types.AgentType = "Mock"

// The tests below set ENTIRE_CONFIG_DIR, so none of them runs in parallel.

// isolateAgentHomes points the per-user config directory at a fresh directory
// and returns it.
func isolateAgentHomes(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	return dir
}

// newHome creates a directory to use as an agent home and returns its
// canonical path.
func newHome(t *testing.T) string {
	t.Helper()

	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return home
}

func knownHomes(t *testing.T) []string {
	t.Helper()

	homes, err := KnownAgentHomes(homesTestAgent)
	require.NoError(t, err)
	return homes
}

// registeredHomes returns the registry's entries for homesTestAgent as stored,
// bypassing KnownAgentHomes' filtering.
func registeredHomes(t *testing.T, configDir string) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(configDir, agentHomesFileName))
	require.NoError(t, err)
	var file agentHomesFile
	require.NoError(t, json.Unmarshal(data, &file))
	return file.Homes[homesTestAgent]
}

func TestRememberAgentHome_ListsMostRecentlyUsedFirst(t *testing.T) {
	isolateAgentHomes(t)
	first, second := newHome(t), newHome(t)

	require.NoError(t, RememberAgentHome(homesTestAgent, first))
	require.NoError(t, RememberAgentHome(homesTestAgent, second))
	assert.Equal(t, []string{second, first}, knownHomes(t))

	require.NoError(t, RememberAgentHome(homesTestAgent, first))
	assert.Equal(t, []string{first, second}, knownHomes(t))

	other, err := KnownAgentHomes("Other")
	require.NoError(t, err)
	assert.Empty(t, other)
}

func TestRememberAgentHome_RecordsTheCanonicalHome(t *testing.T) {
	isolateAgentHomes(t)
	home := newHome(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	require.NoError(t, RememberAgentHome(homesTestAgent, alias))
	assert.Equal(t, []string{home}, knownHomes(t))
}

func TestRememberAgentHome_RejectsRelativeAndMissingHomes(t *testing.T) {
	configDir := isolateAgentHomes(t)

	require.Error(t, RememberAgentHome(homesTestAgent, "relative/home"))
	require.Error(t, RememberAgentHome(homesTestAgent, filepath.Join(t.TempDir(), "missing")))
	assert.NoFileExists(t, filepath.Join(configDir, agentHomesFileName))
}

func TestRememberAgentHome_DropsHomesThatNoLongerExist(t *testing.T) {
	configDir := isolateAgentHomes(t)
	gone, kept, latest := newHome(t), newHome(t), newHome(t)
	require.NoError(t, RememberAgentHome(homesTestAgent, gone))
	require.NoError(t, RememberAgentHome(homesTestAgent, kept))
	require.NoError(t, os.Remove(gone))

	require.NoError(t, RememberAgentHome(homesTestAgent, latest))
	assert.Equal(t, []string{latest, kept}, registeredHomes(t, configDir))
}

func TestRememberAgentHome_KeepsAtMostMaxHomesPerAgent(t *testing.T) {
	configDir := isolateAgentHomes(t)
	homes := make([]string, maxHomesPerAgent+1)
	for i := range homes {
		homes[i] = newHome(t)
		require.NoError(t, RememberAgentHome(homesTestAgent, homes[i]))
	}

	got := registeredHomes(t, configDir)
	require.Len(t, got, maxHomesPerAgent)
	assert.Equal(t, homes[len(homes)-1], got[0])
	assert.NotContains(t, got, homes[0], "the least recently used home is evicted")
}

func TestRememberAgentHome_LeavesAnUnusableRegistryUntouched(t *testing.T) {
	for _, tt := range []struct{ name, contents string }{
		{name: "malformed", contents: "{not json"},
		{name: "unsupported version", contents: `{"version": 99, "homes": {}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configDir := isolateAgentHomes(t)
			registry := filepath.Join(configDir, agentHomesFileName)
			require.NoError(t, os.WriteFile(registry, []byte(tt.contents), 0o600))

			require.Error(t, RememberAgentHome(homesTestAgent, newHome(t)))
			data, err := os.ReadFile(registry)
			require.NoError(t, err)
			assert.Equal(t, tt.contents, string(data))

			_, err = KnownAgentHomes(homesTestAgent)
			require.Error(t, err)
		})
	}
}

func TestKnownAgentHomes_WithoutConfigDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("ENTIRE_CONFIG_DIR", missing)

	homes, err := KnownAgentHomes(homesTestAgent)
	require.NoError(t, err)
	assert.Empty(t, homes)
	assert.NoDirExists(t, missing, "reading the registry must not create the config directory")
}

func TestResolveTrustedHome(t *testing.T) {
	isolateAgentHomes(t)
	active, recorded, unknown := newHome(t), newHome(t), newHome(t)
	require.NoError(t, RememberAgentHome(homesTestAgent, recorded))
	provider := &homeStubAgent{home: active}

	t.Run("active home", func(t *testing.T) {
		got, err := ResolveTrustedHome(provider, active)
		require.NoError(t, err)
		assert.Equal(t, active, got)
	})
	t.Run("recorded home", func(t *testing.T) {
		got, err := ResolveTrustedHome(provider, recorded)
		require.NoError(t, err)
		assert.Equal(t, recorded, got)
	})
	t.Run("unknown home", func(t *testing.T) {
		_, err := ResolveTrustedHome(provider, unknown)
		require.ErrorIs(t, err, ErrUntrustedAgentHome)
	})
	t.Run("relative home", func(t *testing.T) {
		_, err := ResolveTrustedHome(provider, "relative")
		require.Error(t, err)
	})
}

func TestResolveTrustedHome_ReturnsTheSpellingItChecked(t *testing.T) {
	isolateAgentHomes(t)
	recorded := newHome(t)
	require.NoError(t, RememberAgentHome(homesTestAgent, recorded))
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(recorded, alias); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	provider := &homeStubAgent{home: newHome(t)}

	t.Run("alias of a recorded home", func(t *testing.T) {
		got, err := ResolveTrustedHome(provider, alias)
		require.NoError(t, err)
		assert.Equal(t, alias, got)
	})
	t.Run("unclean spelling", func(t *testing.T) {
		got, err := ResolveTrustedHome(provider, filepath.Join(recorded, "sessions")+string(filepath.Separator)+"..")
		require.NoError(t, err)
		assert.Equal(t, recorded, got)
	})
}

func TestResolveTrustedHome_TrustsTheActiveHomeDespiteAnUnusableRegistry(t *testing.T) {
	configDir := isolateAgentHomes(t)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, agentHomesFileName), []byte("{not json"), 0o600))
	active := newHome(t)
	provider := &homeStubAgent{home: active}

	got, err := ResolveTrustedHome(provider, active)
	require.NoError(t, err)
	assert.Equal(t, active, got)

	_, err = ResolveTrustedHome(provider, newHome(t))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUntrustedAgentHome, "an unreadable registry is reported, not mistaken for an untrusted home")
}

func TestResolveTrustedHome_ComparesTheActiveHomeAsCleaned(t *testing.T) {
	configDir := isolateAgentHomes(t)
	// No registry to fall back on: only the active-home comparison can trust it.
	require.NoError(t, os.WriteFile(filepath.Join(configDir, agentHomesFileName), []byte("{not json"), 0o600))
	base := newHome(t)
	elsewhere := newHome(t)
	require.NoError(t, os.Mkdir(filepath.Join(elsewhere, "child"), 0o700))
	if err := os.Symlink(filepath.Join(elsewhere, "child"), filepath.Join(base, "link")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	// Resolving the raw spelling follows link to elsewhere; cleaning it gives
	// base, which is how the session records the active home.
	active := base + string(filepath.Separator) + "link" + string(filepath.Separator) + ".."

	got, err := ResolveTrustedHome(&homeStubAgent{home: active}, base)
	require.NoError(t, err)
	assert.Equal(t, base, got)
}
