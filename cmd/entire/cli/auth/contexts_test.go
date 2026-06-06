package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/zalando/go-keyring"
)

// makeJWT builds a three-segment JWT-shaped string with a non-"none" alg
// (so ParseClaims accepts it) and the given payload. The signature segment
// is arbitrary — claims are parsed unverified.
func makeJWT(t *testing.T, payloadJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(payloadJSON))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"))
}

// RecordLoginContext must persist the refresh token before the access token,
// so a failed access write never commits a fresh access JWT against a stale
// refresh token left over from an earlier login.
func TestRecordLoginContext_RefreshFirstOrdering(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)

	const coreURL = "https://core.example.com"
	const handle = "alice"
	svc := tokenstore.CoreKeyringService(coreURL)
	path := filepath.Join(t.TempDir(), "tokens.json")

	// A prior login left a stale refresh token in the slot.
	seedRestore := tokenstore.UseFileBackendForTesting(path)
	if err := tokenstore.Set(tokenstore.RefreshService(svc), handle, "entr_stale"); err != nil {
		t.Fatalf("seed stale refresh: %v", err)
	}
	seedRestore()

	// Fail the access-token write only.
	failAccess := func(service, _ string) bool { return service == svc }
	restore := tokenstore.UseFailingBackendForTesting(path, failAccess)
	t.Cleanup(restore)

	exp := time.Now().Add(2 * time.Hour).Unix()
	token := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":%q,"exp":%d}`, coreURL, handle, exp))
	if _, err := RecordLoginContext(token, "entr_login_new", true); err == nil {
		t.Fatal("RecordLoginContext: want error when access write fails")
	}
	// The refresh token must already be the new one (written first), and no
	// access token may sit alongside the stale refresh token.
	if r, _ := tokenstore.Get(tokenstore.RefreshService(svc), handle); r != "entr_login_new" { //nolint:errcheck // read-back
		t.Fatalf("refresh slot = %q, want entr_login_new persisted before the access write", r)
	}
	if v, err := tokenstore.Get(svc, handle); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("access slot = %q (err=%v); a fresh access token must not be committed when its write failed", v, err)
	}
}

func TestRecordLoginContext_WritesContextAndToken(t *testing.T) {
	// Sets ENTIRE_CONFIG_DIR and swaps the keyring backend — process-global
	// state, so this test cannot run in parallel.
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const coreURL = "https://core.example.com"
	const handle = "alice"
	exp := time.Now().Add(2 * time.Hour).Unix()
	token := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":%q,"exp":%d}`, coreURL, handle, exp))

	name, err := RecordLoginContext(token, "", true)
	if err != nil {
		t.Fatalf("RecordLoginContext: %v", err)
	}
	if name != "core.example.com" {
		t.Fatalf("context name = %q, want core.example.com", name)
	}

	// Context recorded and made current.
	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load contexts: %v", err)
	}
	if f.CurrentContext != name {
		t.Fatalf("current_context = %q, want %q", f.CurrentContext, name)
	}
	c := f.Find(name)
	if c == nil {
		t.Fatalf("context %q not found", name)
	}
	if c.CoreURL != coreURL || c.Handle != handle {
		t.Fatalf("context = {CoreURL:%q Handle:%q}, want {%q %q}", c.CoreURL, c.Handle, coreURL, handle)
	}
	wantService := tokenstore.CoreKeyringService(coreURL)
	if c.KeychainService != wantService {
		t.Fatalf("KeychainService = %q, want %q", c.KeychainService, wantService)
	}

	// Token stored at the context's keychain slot, decodable with a
	// future expiry.
	encoded, err := tokenstore.Get(wantService, handle)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	gotToken, expiresAt := tokenstore.DecodeTokenWithExpiration(encoded)
	if gotToken != token {
		t.Fatalf("stored token mismatch")
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("stored expiry %s is not in the future", expiresAt)
	}
}

func TestMigrateLegacyLoginContext_SynthesizesContext(t *testing.T) {
	// Mocks the legacy keyring + swaps the new tokenstore + sets the config
	// dir — all process-global, so not parallel.
	keyring.MockInit()
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const coreURL = "https://legacy-core.example.com"
	const handle = "bob"
	exp := time.Now().Add(time.Hour).Unix()
	legacy := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":%q,"exp":%d}`, coreURL, handle, exp))

	// Seed only the legacy single-host keyring entry.
	if err := NewStore().SaveToken(api.AuthBaseURL(), legacy); err != nil {
		t.Fatalf("seed legacy token: %v", err)
	}

	migrated, err := MigrateLegacyLoginContext()
	if err != nil {
		t.Fatalf("MigrateLegacyLoginContext: %v", err)
	}
	if !migrated {
		t.Fatal("expected migration to synthesize a context")
	}

	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load contexts: %v", err)
	}
	if got := f.ContextsForIssuer(coreURL); len(got) != 1 {
		t.Fatalf("contexts for issuer = %d, want 1", len(got))
	}

	// Idempotent: a second call is a no-op now that a context exists.
	migratedAgain, err := MigrateLegacyLoginContext()
	if err != nil {
		t.Fatalf("second MigrateLegacyLoginContext: %v", err)
	}
	if migratedAgain {
		t.Fatal("second migration should be a no-op")
	}
}

func TestLoginTokenForContext(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	c := &contexts.Context{
		Name:            "core.example.com",
		CoreURL:         "https://core.example.com",
		Handle:          "carol",
		KeychainService: tokenstore.CoreKeyringService("https://core.example.com"),
	}
	if err := tokenstore.Set(c.KeychainService, c.Handle, tokenstore.EncodeTokenWithExpiration("the-jwt", 3600)); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	got, err := LoginTokenForContext(c)
	if err != nil {
		t.Fatalf("LoginTokenForContext: %v", err)
	}
	if got != "the-jwt" {
		t.Fatalf("token = %q, want the-jwt", got)
	}

	if _, err := LoginTokenForContext(nil); err == nil {
		t.Fatal("expected error for nil context")
	}
}

func TestContextStore_PrefersCurrentContextThenLegacy(t *testing.T) {
	keyring.MockInit()
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	store := NewContextStore()

	// No context yet: falls back to the legacy entry.
	if err := store.SaveToken(api.AuthBaseURL(), "legacy-token"); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	legacyGot, err := store.GetToken(api.AuthBaseURL())
	if err != nil {
		t.Fatalf("GetToken (legacy): %v", err)
	}
	if legacyGot != "legacy-token" {
		t.Fatalf("with no context, GetToken = %q, want legacy-token", legacyGot)
	}

	// Record a context: its token now wins over the legacy entry.
	exp := time.Now().Add(time.Hour).Unix()
	ctxToken := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example.com","handle":"alice","exp":%d}`, exp))
	if _, err := RecordLoginContext(ctxToken, "", true); err != nil {
		t.Fatalf("RecordLoginContext: %v", err)
	}
	got, err := store.GetToken(api.AuthBaseURL())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if got != ctxToken {
		t.Fatal("with a current context, GetToken should return the context token")
	}
}

func TestRemoveCurrentContext(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	token := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example.com","handle":"alice","exp":%d}`, exp))
	if _, err := RecordLoginContext(token, "entr_refresh", true); err != nil {
		t.Fatalf("RecordLoginContext: %v", err)
	}
	if _, ok := CurrentContextToken(); !ok {
		t.Fatal("precondition: expected a current context token")
	}
	svc := tokenstore.CoreKeyringService("https://core.example.com")
	if r, _ := tokenstore.Get(tokenstore.RefreshService(svc), "alice"); r != "entr_refresh" { //nolint:errcheck // read-back; only the value matters
		t.Fatalf("precondition: expected refresh slot seeded, got %q", r)
	}

	if err := RemoveCurrentContext(); err != nil {
		t.Fatalf("RemoveCurrentContext: %v", err)
	}
	if _, ok := CurrentContextToken(); ok {
		t.Fatal("after RemoveCurrentContext, expected no current context token")
	}
	// Logout must scrub both slots: the access token and the long-lived
	// refresh token. A leftover refresh token would let any keyring-capable
	// process mint fresh access tokens after logout.
	if v, err := tokenstore.Get(svc, "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("access slot survived logout: value=%q err=%v", v, err)
	}
	if v, err := tokenstore.Get(tokenstore.RefreshService(svc), "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("refresh slot survived logout: value=%q err=%v", v, err)
	}

	// Idempotent: a second call with nothing current is a no-op.
	if err := RemoveCurrentContext(); err != nil {
		t.Fatalf("second RemoveCurrentContext: %v", err)
	}
}

func TestRemoveCurrentContext_DoesNotSwitchToAnother(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	if _, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":"https://a.example.com","handle":"alice","exp":%d}`, exp)), "", true); err != nil {
		t.Fatalf("record a: %v", err)
	}
	active, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":"https://b.example.com","handle":"alice","exp":%d}`, exp)), "", true)
	if err != nil {
		t.Fatalf("record b: %v", err)
	}

	// Logging out of the active context must NOT silently switch to the
	// surviving one — current_context is cleared.
	if err := RemoveCurrentContext(); err != nil {
		t.Fatalf("RemoveCurrentContext: %v", err)
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.CurrentContext != "" {
		t.Fatalf("current_context = %q after logout, want empty (not switched)", f.CurrentContext)
	}
	if f.Find(active) != nil {
		t.Fatalf("active context %q should have been removed", active)
	}
	if len(f.Contexts) != 1 {
		t.Fatalf("want the other context to survive; got %d contexts", len(f.Contexts))
	}
}

func TestRemoveContext(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	first, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":"https://a.example.com","handle":"alice","exp":%d}`, exp)), "entr_a", true)
	if err != nil {
		t.Fatalf("record a: %v", err)
	}
	active, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":"https://b.example.com","handle":"alice","exp":%d}`, exp)), "entr_b", true)
	if err != nil {
		t.Fatalf("record b: %v", err)
	}

	// Remove the non-current context by name: it must disappear (both slots)
	// while the active context and current_context pointer are untouched.
	if err := RemoveContext(first); err != nil {
		t.Fatalf("RemoveContext: %v", err)
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.Find(first) != nil {
		t.Fatalf("context %q should have been removed", first)
	}
	if f.CurrentContext != active {
		t.Fatalf("current_context = %q, want the untouched active context %q", f.CurrentContext, active)
	}
	svcA := tokenstore.CoreKeyringService("https://a.example.com")
	if v, err := tokenstore.Get(svcA, "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("access slot survived RemoveContext: value=%q err=%v", v, err)
	}
	if v, err := tokenstore.Get(tokenstore.RefreshService(svcA), "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("refresh slot survived RemoveContext: value=%q err=%v", v, err)
	}

	// Idempotent: removing a name that no longer exists is a no-op.
	if err := RemoveContext(first); err != nil {
		t.Fatalf("second RemoveContext: %v", err)
	}
}

func TestSetCurrentContext(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	// Two contexts from two cores; the second becomes current on login.
	exp := time.Now().Add(time.Hour).Unix()
	if _, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":"https://a.example.com","handle":"alice","exp":%d}`, exp)), "", true); err != nil {
		t.Fatalf("record a: %v", err)
	}
	if _, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":"https://b.example.com","handle":"alice","exp":%d}`, exp)), "", true); err != nil {
		t.Fatalf("record b: %v", err)
	}

	all, current, err := Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d contexts, want 2", len(all))
	}
	if current != "b.example.com" {
		t.Fatalf("current = %q, want b.example.com (most recent login)", current)
	}

	// Switch back to the first.
	if err := SetCurrentContext("a.example.com"); err != nil {
		t.Fatalf("SetCurrentContext: %v", err)
	}
	_, current, err = Contexts()
	if err != nil {
		t.Fatalf("Contexts after switch: %v", err)
	}
	if current != "a.example.com" {
		t.Fatalf("after switch, current = %q, want a.example.com", current)
	}

	// Unknown context errors.
	if err := SetCurrentContext("nope"); err == nil {
		t.Fatal("expected error switching to unknown context")
	}
}

func TestRecordLoginContext_SameCoreDifferentHandlesCoexist(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const coreURL = "https://core.example.com"
	exp := time.Now().Add(time.Hour).Unix()

	aliceName, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, coreURL, exp)), "", true)
	if err != nil {
		t.Fatalf("record alice: %v", err)
	}
	bobName, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"bob","exp":%d}`, coreURL, exp)), "", true)
	if err != nil {
		t.Fatalf("record bob: %v", err)
	}

	// Two distinct contexts for the same core — bob must not clobber alice.
	if aliceName == bobName {
		t.Fatalf("both logins got the same context name %q", aliceName)
	}
	if aliceName != "core.example.com" {
		t.Fatalf("first login name = %q, want bare host core.example.com", aliceName)
	}
	if bobName != "bob@core.example.com" {
		t.Fatalf("second login name = %q, want bob@core.example.com", bobName)
	}

	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := f.ContextsForIssuer(coreURL); len(got) != 2 {
		t.Fatalf("contexts for issuer = %d, want 2", len(got))
	}
	if a := f.Find(aliceName); a == nil || a.Handle != "alice" {
		t.Fatalf("alice context lost or wrong handle: %+v", a)
	}

	// Re-login as alice updates her context in place (no third entry).
	again, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, coreURL, exp)), "", true)
	if err != nil {
		t.Fatalf("re-login alice: %v", err)
	}
	if again != aliceName {
		t.Fatalf("re-login produced new name %q, want %q", again, aliceName)
	}
	reloaded, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Contexts) != 2 {
		t.Fatalf("re-login created a duplicate; want 2 contexts")
	}
}

func TestMigrateLegacyLoginContext_PreservesCurrentContext(t *testing.T) {
	keyring.MockInit()
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()

	// An existing, active context for one core.
	active, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":"https://active.example.com","handle":"alice","exp":%d}`, exp)), "", true)
	if err != nil {
		t.Fatalf("seed active context: %v", err)
	}

	// A legacy keyring login for a *different* core, not yet migrated.
	legacy := makeJWT(t, fmt.Sprintf(`{"iss":"https://legacy.example.com","handle":"alice","exp":%d}`, exp))
	if err := NewStore().SaveToken(api.AuthBaseURL(), legacy); err != nil {
		t.Fatalf("seed legacy token: %v", err)
	}

	migrated, err := MigrateLegacyLoginContext()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatal("expected migration to run")
	}

	// Migration recorded the legacy context but must NOT have switched away
	// from the already-active one.
	_, current, err := Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if current != active {
		t.Fatalf("current_context = %q, want unchanged %q after migration", current, active)
	}
}

func TestMigrateLegacyLoginContext_DifferentHandleSameCore(t *testing.T) {
	keyring.MockInit()
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const coreURL = "https://core.example.com"
	exp := time.Now().Add(time.Hour).Unix()

	// contexts.json already has alice@core (e.g. from another CLI).
	if _, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, coreURL, exp)), "", true); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// The legacy keyring token is bob on the *same* core — migration must
	// still run (issuer-only dedup would wrongly skip it).
	bob := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"bob","exp":%d}`, coreURL, exp))
	if err := NewStore().SaveToken(api.AuthBaseURL(), bob); err != nil {
		t.Fatalf("seed legacy bob: %v", err)
	}

	migrated, err := MigrateLegacyLoginContext()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatal("expected bob to be migrated despite alice@core existing")
	}

	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := f.ContextsForIssuer(coreURL); len(got) != 2 {
		t.Fatalf("contexts for issuer = %d, want 2 (alice + bob)", len(got))
	}
}

func TestRecordLoginContext_RejectsTokenWithoutIssuer(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	token := makeJWT(t, `{"handle":"alice"}`)
	if _, err := RecordLoginContext(token, "", true); err == nil {
		t.Fatal("expected error for token without iss claim, got nil")
	}
}
