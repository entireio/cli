package cli

import (
	"fmt"
	"github.com/entireio/cli/cmd/entire/cli/auth"
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

	// ENTIRE_TOKEN is isolated by ABSENCE, not by a redirected path, so it is
	// not in the block above. Left set, it outranks every stored context in
	// resolveEntireIdentityProfile, so a test driving the production identity
	// resolver sends the developer's own bearer to the host in that token's aud
	// claim — a live request to a real core from a unit test — and then fails,
	// because the resolver returns a transport error instead of the guidance
	// the test asserts. Unset, not set-to-blank: blank is "set but blank",
	// which ParseEnvToken maps to errEntireEnvTokenRejected, whose guidance also
	// carries the git-config line the tests look for — so they would pass
	// without exercising the path they exist to pin.
	if err := os.Unsetenv(auth.EnvTokenVar); err != nil {
		panic(fmt.Errorf("failed to unset %s: %w", auth.EnvTokenVar, err))
	}

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

// unsetEnv makes key absent for the duration of the test, restoring whatever it
// held — including its absence — afterwards.
//
// There is no t.Unsetenv: golang/go#52817 proposed one and it was declined as
// trivially expressible, and this pair is the workaround from that thread. The
// t.Setenv does the bookkeeping, taking the parallel guard and registering the
// restore; os.Unsetenv then makes the variable absent rather than blank.
//
// Absent and blank are not interchangeable for every reader, which is why this
// is a helper and not a t.Setenv(key, "") call. auth.ParseEnvToken is
// fail-closed and rejects a set-but-blank ENTIRE_TOKEN, so blanking it does not
// neutralise it, it makes every command that reads it fail before the test's
// own fixtures are consulted. contexts.Active reads $ENTIRE_CONTEXT as
// TrimSpace(...) != "", where blank and absent genuinely do coincide.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	os.Unsetenv(key)
}

// unsetEnv's contract is the reason six call sites could drop a hand-rolled
// LookupEnv/Cleanup restore, so pin both halves of it: the key is absent rather
// than blank while the test runs, and whatever was there — a value, or nothing
// — is back afterwards.
//
// Not parallel: t.Setenv.
func TestUnsetEnv(t *testing.T) {
	const key = "ENTIRE_TEST_UNSETENV_PROBE"

	t.Run("absent during the test, prior value restored after", func(t *testing.T) {
		t.Setenv(key, "before")
		t.Run("inner", func(t *testing.T) {
			unsetEnv(t, key)
			if v, ok := os.LookupEnv(key); ok {
				t.Fatalf("%s = %q, want absent (blank is not absent)", key, v)
			}
		})
		if v, ok := os.LookupEnv(key); !ok || v != "before" {
			t.Fatalf("%s = (%q, %v) after restore, want (%q, true)", key, v, ok, "before")
		}
	})

	t.Run("an already-absent key stays absent after restore", func(t *testing.T) {
		// Establish the absent prior state rather than assuming it. Without
		// this the subtest asserts nothing when the key happens to be exported:
		// the inner restore correctly puts the ambient value back, and this
		// subtest fails having never exercised an absent prior state at all.
		unsetEnv(t, key)
		t.Run("inner", func(t *testing.T) {
			unsetEnv(t, key)
			if v, ok := os.LookupEnv(key); ok {
				t.Fatalf("%s = %q, want absent", key, v)
			}
		})
		if v, ok := os.LookupEnv(key); ok {
			t.Fatalf("%s = %q after restore, want still absent", key, v)
		}
	})
}
