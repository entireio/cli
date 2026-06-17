package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/go-git/v6/x/plugin/config"
	"github.com/zalando/go-keyring"
)

func TestMain(m *testing.M) {
	// Route the OS keyring to an in-memory mock for the whole package. The
	// default tokenstore backend is the real OS keychain, so any test that
	// reaches a credential path without UseFileBackendForTesting — or in the
	// window after such a test restores the backend — would otherwise read the
	// developer's real keychain and trigger a macOS unlock prompt. Mirrors the
	// auth subpackage's TestMain.
	keyring.MockInit()

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
