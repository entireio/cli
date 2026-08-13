package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// TestResolveStatusTarget_PrefersActiveContext pins the multi-core fix: status
// targets the active context's CoreURL + its session token, recording a real
// context and reading it back.
func TestResolveStatusTargetUsesCurrentLogin(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	if err := auth.RecordLogin(makeContextJWT(t, fmt.Sprintf(`{"iss":"`+testCoreURL+`","handle":"alice","exp":%d}`, exp)), ""); err != nil {
		t.Fatalf("record context: %v", err)
	}

	got, err := resolveStatusTarget(t.Context(), auth.CurrentLogin, auth.RefreshedLoginToken)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.coreURL != testCoreURL {
		t.Errorf("coreURL = %q, want the active context's CoreURL", got.coreURL)
	}
	if got.token == "" {
		t.Error("token = empty, want the active context's session token")
	}
}

// TestResolveStatusTarget_PrefersRefreshedToken pins the fix: status uses the
// refreshed login JWT for the active context, so an expired-but-refreshable
// session reports "logged in" rather than the false "re-login" the raw read
// produced. The resolver returns a token distinct from what's stored; we assert
// status carries the refreshed one.
func TestResolveStatusTarget_PrefersRefreshedToken(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	// Stored token is expired; a raw read would 401 at /me → "re-login".
	expired := time.Now().Add(-time.Hour).Unix()
	if err := auth.RecordLogin(makeContextJWT(t, fmt.Sprintf(`{"iss":"`+testCoreURL+`","handle":"alice","exp":%d}`, expired)), "entr_refresh"); err != nil {
		t.Fatalf("record context: %v", err)
	}

	refreshed := func(_ context.Context, _ *contexts.Context) (string, error) { return "refreshed-jwt", nil }
	got, err := resolveStatusTarget(t.Context(), auth.CurrentLogin, refreshed)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.token != "refreshed-jwt" {
		t.Errorf("token = %q, want the refreshed token (not the stale stored one)", got.token)
	}
	if got.coreURL != testCoreURL {
		t.Errorf("coreURL = %q, want the active context's CoreURL", got.coreURL)
	}
}

// TestResolveStatusTarget_FallsBackToStoredWhenRefreshFails pins the safety net:
// when refresh fails (revoked family, network, opaque token) status drops to the
// stored token and lets the /me probe arbitrate — rather than losing the active
// context.
func TestResolveStatusTarget_FallsBackToStoredWhenRefreshFails(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	stored := makeContextJWT(t, fmt.Sprintf(`{"iss":"`+testCoreURL+`","handle":"alice","exp":%d}`, exp))
	if err := auth.RecordLogin(stored, ""); err != nil {
		t.Fatalf("record context: %v", err)
	}

	failRefresh := func(_ context.Context, _ *contexts.Context) (string, error) {
		return "", auth.ErrNotLoggedIn
	}
	got, err := resolveStatusTarget(t.Context(), auth.CurrentLogin, failRefresh)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.token != stored {
		t.Errorf("token = %q, want the stored token as fallback", got.token)
	}
	if got.coreURL != testCoreURL {
		t.Errorf("coreURL = %q, want current login server", got.coreURL)
	}
}

// A genuine contexts.json read/parse error is surfaced by resolveStatusTarget,
// symmetric with the control-plane commands. (A missing file reads as "no
// contexts" and is not an error.)
func TestResolveStatusTarget_CorruptContextsErrors(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	if err := os.WriteFile(filepath.Join(cfgDir, "contexts.json"), []byte("{ not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt contexts.json: %v", err)
	}
	if _, err := resolveStatusTarget(t.Context(), auth.CurrentLogin, auth.RefreshedLoginToken); err == nil {
		t.Fatal("want an error when contexts.json is corrupt, got nil")
	}
}

// With no contexts at all, the target is zero-valued: status renders the
// informational "Not logged in." (exit 0) and logout no-ops — never a probe
// against any default host.
func TestResolveStatusTarget_NoContextsIsZeroTarget(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	got, err := resolveStatusTarget(t.Context(), auth.CurrentLogin, auth.RefreshedLoginToken)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.coreURL != "" || got.token != "" {
		t.Fatalf("want zero target with no contexts, got %+v", got)
	}
}

// makeContextJWT builds a JWT-shaped token (non-"none" alg) carrying the
// given claims, which is all RecordLogin needs.
func makeContextJWT(t *testing.T, payloadJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	return header + "." + enc.EncodeToString([]byte(payloadJSON)) + "." + enc.EncodeToString([]byte("sig"))
}
