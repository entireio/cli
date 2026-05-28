package checkpoint

import (
	"fmt"
	"os"
	"testing"

	_ "unsafe"

	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/go-git/v6/x/plugin/config"
)

func TestMain(m *testing.M) {
	// For tests, ensure that go-git always gets empty Configs for both
	// system and global scopes. This way the current environment does not
	// impact the tests.
	err := plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource {
		return config.NewEmpty()
	})
	if err != nil {
		panic(fmt.Errorf("failed to register config storers: %w", err))
	}

	os.Exit(m.Run())
}

//go:linkname resetPluginEntry github.com/go-git/go-git/v6/x/plugin.resetEntry
func resetPluginEntry(name plugin.Name)

// configLoaderKey mirrors the unexported name from x/plugin/plugin_config.go.
const configLoaderKey plugin.Name = "config-loader"

// useAutoConfigLoader swaps the registered ConfigLoader plugin to NewAuto (which
// reads $HOME/.gitconfig) for the duration of t, then restores NewEmpty on cleanup.
// Also sets GIT_CONFIG_NOSYSTEM=1 so NewAuto skips the host's /etc/gitconfig.
func useAutoConfigLoader(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	registerConfigLoaderForTest(t, func() error {
		return plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource { return config.NewAuto() })
	})
}

// registerConfigLoaderForTest resets the ConfigLoader plugin entry, runs register
// to install a test loader, and restores NewEmpty on cleanup. The reset is required
// because a prior plugin.Get may have frozen the entry.
func registerConfigLoaderForTest(t *testing.T, register func() error) {
	t.Helper()
	resetPluginEntry(configLoaderKey)
	if err := register(); err != nil {
		t.Fatalf("failed to register config loader: %v", err)
	}
	t.Cleanup(func() {
		resetPluginEntry(configLoaderKey)
		if err := plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource { return config.NewEmpty() }); err != nil {
			t.Fatalf("failed to restore NewEmpty config loader: %v", err)
		}
	})
}
