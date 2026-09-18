package cli

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// failingTokenStore installs a backend whose Set always fails, standing in
// for the locked/absent OS keyring a headless machine hits (#1036). Fault
// injection (rather than filesystem permissions) keeps the failure
// deterministic even when tests run as root, where permission bits don't
// block writes.
func failingTokenStore(t *testing.T) {
	t.Helper()
	restore := tokenstore.UseFailingBackendForTesting(
		filepath.Join(t.TempDir(), "tokens.json"),
		func(string, string) bool { return true },
	)
	t.Cleanup(restore)
}

// loginTestJWT builds a token that passes validateReceivedToken and carries
// the iss/handle claims RecordLoginContext keys on.
func loginTestJWT(t *testing.T, issuer string) string {
	t.Helper()
	exp := time.Now().Add(time.Hour).Unix()
	return makeJWT(t, `{"alg":"RS256"}`, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, issuer, exp))
}

// A login that reaches token persistence and fails there must tell headless
// users about the file token store: the default backend is the OS keyring,
// and on keyring-less machines (CI, containers, minimal server VMs) the raw
// store error gives no way forward (#1036). Both store-write sites are
// covered: the refresh-token write (refreshToken != "") fails first when a
// refresh token is present, and the login-token write is the first store
// write when there is none.
func TestPersistLogin_StoreWriteFailureIncludesHeadlessHint(t *testing.T) {
	for name, refreshToken := range map[string]string{
		"refresh-token write fails": "refresh-token",
		"login-token write fails":   "",
	} {
		t.Run(name, func(t *testing.T) {
			// Not parallel: mutates the process-global tokenstore backend and
			// env. TestMain sets ENTIRE_TOKEN_STORE=file process-wide for
			// spawned-binary isolation; blank it so this test sees the
			// default-keyring condition a real user hits.
			t.Setenv("ENTIRE_TOKEN_STORE", "")
			// The hint's suppression now also reads the remembered-preference
			// marker in the config dir, and TestMain shares one config dir
			// across the package: a marker written by another test in this
			// process would suppress the hint here.
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			// TestMain sets ENTIRE_TOKEN_STORE_PATH process-wide; with it set the
			// hint must NOT claim the choice is remembered, so blank it here to
			// test the remembered wording. The not-remembered wording has its
			// own test below.
			t.Setenv("ENTIRE_TOKEN_STORE_PATH", "")
			failingTokenStore(t)

			var out bytes.Buffer
			err := persistLogin(&out, "https://example.test", "", loginTestJWT(t, "https://example.test"), refreshToken)
			if err == nil {
				t.Fatal("persistLogin should fail when the token store rejects writes")
			}
			if !strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE=file") {
				t.Fatalf("store-write failure should point headless users at the file token store, got:\n%v", err)
			}
			if !strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE_PATH") {
				t.Fatalf("hint should mention the path override, got:\n%v", err)
			}
			if !strings.Contains(err.Error(), "remembered") {
				t.Fatalf("hint should say the choice is remembered, so users do not export the variable everywhere:\n%v", err)
			}
			if !strings.Contains(err.Error(), "=keyring entire login") {
				t.Fatalf("a sticky choice must come with the way back:\n%v", err)
			}
		})
	}
}

// When the user is already on the file backend, suggesting
// ENTIRE_TOKEN_STORE=file would be nonsense — the raw error must pass
// through without the headless hint.
func TestPersistLogin_StoreWriteFailureOnFileBackend_NoHint(t *testing.T) {
	// Not parallel: mutates the process-global tokenstore backend and env.
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	failingTokenStore(t)

	var out bytes.Buffer
	err := persistLogin(&out, "https://example.test", "", loginTestJWT(t, "https://example.test"), "refresh-token")
	if err == nil {
		t.Fatal("persistLogin should fail when the token store rejects writes")
	}
	// Assert on the hint's structural markers, not its prose: the underlying
	// store error can never contain these, so the assertion stays meaningful
	// if the hint wording changes.
	if strings.Contains(err.Error(), "=file entire login") || strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE_PATH") {
		t.Fatalf("hint must not appear when the file backend is already configured, got:\n%v", err)
	}
	if !strings.Contains(err.Error(), "save login") {
		t.Fatalf("underlying save failure should still surface, got:\n%v", err)
	}
}

// Failures unrelated to the credential store (here: a token whose issuer
// doesn't match the login server) must not carry the keyring hint — the
// file store wouldn't help.
func TestPersistLogin_NonStoreFailure_NoHint(t *testing.T) {
	// Not parallel: mutates process-global env.
	t.Setenv("ENTIRE_TOKEN_STORE", "")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	// iss mismatch with baseURL fails validateReceivedToken before any store write.
	token := makeJWT(t, `{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"https://other.test","handle":"alice","exp":%d}`, exp))

	var out bytes.Buffer
	err := persistLogin(&out, "https://example.test", "", token, "refresh-token")
	if err == nil {
		t.Fatal("persistLogin should reject a token from the wrong issuer")
	}
	if strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE") {
		t.Fatalf("non-store failure must not carry the token-store hint, got:\n%v", err)
	}
}

// With ENTIRE_TOKEN_STORE_PATH set the marker is never written (it cannot
// carry a path), so the hint must not promise that the choice is remembered;
// it says why and tells the user to keep both variables set. This mirrors
// the fallback notice's own branch in tokenstore.
func TestPersistLogin_HintDoesNotPromiseMemoryWhenPathIsOverridden(t *testing.T) {
	// Not parallel: mutates env and the process-global token store backend.
	t.Setenv("ENTIRE_TOKEN_STORE", "")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_TOKEN_STORE_PATH", filepath.Join(t.TempDir(), "tokens.json"))
	failingTokenStore(t)

	var out bytes.Buffer
	err := persistLogin(&out, "https://example.test", "", loginTestJWT(t, "https://example.test"), "refresh-token")
	if err == nil {
		t.Fatal("persistLogin should fail when the token store rejects writes")
	}
	if !strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE=file") {
		t.Fatalf("hint should still point at the file store, got:\n%v", err)
	}
	if strings.Contains(err.Error(), "The choice is remembered") {
		t.Fatalf("hint must not promise a memory the marker will not keep while ENTIRE_TOKEN_STORE_PATH is set:\n%v", err)
	}
	if !strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE_PATH is set, so the choice is not remembered") {
		t.Fatalf("hint should say why the choice is not remembered:\n%v", err)
	}
}

// When the Linux fallback already tried the file store and it failed as well,
// the hint must not recommend the store that just failed; the way forward is
// a writable location for it, named through the path override. The flat
// two-%w error here is a stand-in: in production ErrFileStoreFailed sits
// inside the token store's three-%w error and ErrCredentialStoreWrite is
// added a level above by auth's credStoreWriteError, and errors.Is descends
// through both levels; that nesting cannot be built here because the wrapper
// type is unexported.
func TestWithHeadlessStoreHint_BothStoresFailedPointsAtThePathOverride(t *testing.T) {
	// Not parallel: FileBackendSelected() reads env and the config-dir marker.
	t.Setenv("ENTIRE_TOKEN_STORE", "")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	err := fmt.Errorf("store refresh token: %w; %w: permission denied", auth.ErrCredentialStoreWrite, tokenstore.ErrFileStoreFailed)
	got := withHeadlessStoreHint(err)
	if strings.Contains(got.Error(), "=file entire login") {
		t.Fatalf("hint must not suggest the file store that just failed, got:\n%v", got)
	}
	if !strings.Contains(got.Error(), tokenstore.PathEnvVar) {
		t.Fatalf("hint should point at the path override as the way forward, got:\n%v", got)
	}
	if !errors.Is(got, auth.ErrCredentialStoreWrite) {
		t.Fatalf("underlying error must stay reachable, got:\n%v", got)
	}
}

// Every login onto the file store says where the tokens went. The fallback's
// own notice prints only on the first adoption, so a later login onto the
// remembered file store used to write bearer tokens to disk without a word.
func TestPersistLogin_OnTheFileStoreSaysWhereTokensWent(t *testing.T) {
	// Not parallel: mutates env and the process-global token store backend.
	path := filepath.Join(t.TempDir(), "tokens.json")
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	t.Setenv("ENTIRE_TOKEN_STORE_PATH", path)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(path)
	t.Cleanup(restore)

	var out bytes.Buffer
	if err := persistLogin(&out, "https://example.test", "", loginTestJWT(t, "https://example.test"), "refresh-token"); err != nil {
		t.Fatalf("persistLogin() = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "Tokens are stored in "+path) {
		t.Fatalf("stdout should say where the tokens went:\n%s", out.String())
	}
}

// With the keyring selected there is nothing to add: the platform keystore is
// where a user expects tokens, and the line would only be noise.
func TestPersistLogin_OnTheKeyringSaysNothingAboutTheFile(t *testing.T) {
	// Not parallel: mutates env and the process-global token store backend.
	t.Setenv("ENTIRE_TOKEN_STORE", "")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	var out bytes.Buffer
	if err := persistLogin(&out, "https://example.test", "", loginTestJWT(t, "https://example.test"), "refresh-token"); err != nil {
		t.Fatalf("persistLogin() = %v, want nil", err)
	}
	if strings.Contains(out.String(), "Tokens are stored in") {
		t.Fatalf("a keyring login must not claim the tokens are in a file:\n%s", out.String())
	}
}
