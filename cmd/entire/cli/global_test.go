package cli

import (
	"fmt"
	"os"
	"path/filepath"
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

	// keyring.MockInit only covers in-process credential access. Several tests
	// in this package spawn the real entire binary (or a git hook that invokes
	// it), and testing.Testing() is false in that child — so the internal
	// testdirs fallback and the in-memory keyring mock don't apply there, and
	// the child's tokenstore default backend reaches the developer's real OS
	// keychain. Set the file-backed token store and isolated config/cache dirs
	// process-wide so spawned children inherit them. Mirrors the integration
	// and e2e TestMains.
	isolationDir, err := os.MkdirTemp("", "entire-cli-test-*")
	if err != nil {
		panic(fmt.Errorf("failed to create test isolation dir: %w", err))
	}
	os.Setenv("ENTIRE_TOKEN_STORE", "file")
	os.Setenv("ENTIRE_TOKEN_STORE_PATH", filepath.Join(isolationDir, "tokenstore.json"))
	os.Setenv("ENTIRE_TEST_AUTH_STORE_FILE", filepath.Join(isolationDir, "auth-tokens.json"))
	os.Setenv("ENTIRE_CONFIG_DIR", filepath.Join(isolationDir, "config"))
	os.Setenv("XDG_CACHE_HOME", filepath.Join(isolationDir, "cache"))

	// Register a default ConfigSource so tests that call ConfigScoped
	// (directly or indirectly via Commit/CreateTag) don't fail with
	// "no config loader registered".
	if regErr := plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource {
		return config.NewEmpty()
	}); regErr != nil {
		panic(fmt.Errorf("failed to register config storers: %w", regErr))
	}

	code := m.Run()
	_ = os.RemoveAll(isolationDir)
	os.Exit(code)
}
