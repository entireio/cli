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
	err := plugin.Register(plugin.ConfigLoader(), config.NewEmpty)
	if err != nil {
		panic(fmt.Errorf("failed to register config storers: %w", err))
	}

	os.Exit(m.Run())
}

//go:linkname resetPluginEntry github.com/go-git/go-git/v6/x/plugin.resetEntry
func resetPluginEntry(name plugin.Name)
