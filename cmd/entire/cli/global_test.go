package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/go-git/v6/x/plugin/config"
)

func TestMain(m *testing.M) {
	// Register a default ConfigSource so tests that call ConfigScoped
	// (directly or indirectly via Commit/CreateTag) don't fail with
	// "no config loader registered".
	err := plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource {
		return config.NewEmpty()
	})
	if err != nil {
		panic(fmt.Errorf("failed to register config storers: %w", err))
	}

	os.Exit(m.Run())
}
