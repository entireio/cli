package strategy

import (
	"fmt"
	"io"
	"os"
	"testing"

	_ "unsafe"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/go-git/v6/x/plugin/config"
)

func TestMain(m *testing.M) {
	opfPrePushProgressWriter = io.Discard

	// The ConfigLoader plugin below only isolates go-git's IN-PROCESS config
	// reads. The push and checkpoint-remote paths under test also shell out to
	// git, and those children read the developer's ~/.gitconfig unless the whole
	// process is isolated. That is not cosmetic: a host with transfer.fsckObjects
	// set makes `git fetch` hand the objects to index-pack instead of
	// unpack-objects, so the fetched commit lands in a new packfile that the
	// already-open go-git repository never indexes — the empty-orphan heal then
	// reports "object not found" and silently keeps the orphan (ENCLI-378). Set
	// process-wide (not per-test) so it also covers spawned binaries and git
	// hooks, and so t.Parallel tests are not excluded. Mirrors the e2e TestMains.
	gitenv.IsolateMain()

	// Turn start writes the per-user session-home index (session/home_index.go)
	// under userdirs' cache dir, and an exported XDG_CACHE_HOME outranks the
	// in-process testdirs fallback. Point it at a throwaway dir so tests never
	// write into a developer's real cache. Tests that assert on the index set
	// their own per-test dir on top of this.
	cacheDir, err := os.MkdirTemp("", "entire-strategy-test-cache-*")
	if err != nil {
		panic(fmt.Errorf("failed to create test cache dir: %w", err))
	}
	os.Setenv("XDG_CACHE_HOME", cacheDir)

	// Register a default ConfigSource so tests that call ConfigScoped
	// (directly or indirectly via Commit/CreateTag) don't fail with
	// "no config loader registered".
	err = plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource {
		return config.NewEmpty()
	})
	if err != nil {
		panic(fmt.Errorf("failed to register config storers: %w", err))
	}

	code := m.Run()
	_ = os.RemoveAll(cacheDir)
	os.Exit(code)
}

//go:linkname resetPluginEntry github.com/go-git/go-git/v6/x/plugin.resetEntry
func resetPluginEntry(name plugin.Name)

// configLoaderKey mirrors the unexported name from x/plugin/plugin_config.go.
const configLoaderKey plugin.Name = "config-loader"

// useAutoConfigLoader swaps the registered ConfigLoader plugin to NewAuto (which
// reads $HOME/.gitconfig) for the duration of t, then restores NewEmpty on cleanup.
// Also sets GIT_CONFIG_NOSYSTEM=1 so NewAuto skips the host's /etc/gitconfig, and
// unsets GIT_CONFIG_GLOBAL so it resolves the caller's $HOME rather than the
// isolation file TestMain points that variable at — see gitenv.UnsetGlobalConfig.
func useAutoConfigLoader(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitenv.UnsetGlobalConfig(t)
	resetPluginEntry(configLoaderKey)
	if err := plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource { return config.NewAuto() }); err != nil {
		t.Fatalf("failed to register NewAuto config loader: %v", err)
	}
	t.Cleanup(func() {
		resetPluginEntry(configLoaderKey)
		if err := plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource { return config.NewEmpty() }); err != nil {
			t.Fatalf("failed to restore NewEmpty config loader: %v", err)
		}
	})
}
