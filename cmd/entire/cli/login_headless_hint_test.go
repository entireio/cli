package cli

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			failingTokenStore(t)

			var out bytes.Buffer
			err := persistLogin(&out, "https://example.test", loginTestJWT(t, "https://example.test"), refreshToken)
			if err == nil {
				t.Fatal("persistLogin should fail when the token store rejects writes")
			}
			if !strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE=file") {
				t.Fatalf("store-write failure should point headless users at the file token store, got:\n%v", err)
			}
			if !strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE_PATH") {
				t.Fatalf("hint should mention the path override, got:\n%v", err)
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
	failingTokenStore(t)

	var out bytes.Buffer
	err := persistLogin(&out, "https://example.test", loginTestJWT(t, "https://example.test"), "refresh-token")
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
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	// iss mismatch with baseURL fails validateReceivedToken before any store write.
	token := makeJWT(t, `{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"https://other.test","handle":"alice","exp":%d}`, exp))

	var out bytes.Buffer
	err := persistLogin(&out, "https://example.test", token, "refresh-token")
	if err == nil {
		t.Fatal("persistLogin should reject a token from the wrong issuer")
	}
	if strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE") {
		t.Fatalf("non-store failure must not carry the token-store hint, got:\n%v", err)
	}
}
