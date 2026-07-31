package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// testCoreURL is the login server every context in this file is recorded
// against; seedLoginWithJurisdictionTokens keys its keyring slots off it.
const testCoreURL = "https://core.example.com"

// seedAccountWithJurisdictionTokens records a login context for `handle`
// against testCoreURL, notes `audiences` on it, and files a jurisdiction access
// token in each of those keyring slots under that handle — the state
// git-remote-entire leaves behind after a few git operations. Returns the
// context name and the core keyring service its login tokens live in.
func seedAccountWithJurisdictionTokens(t *testing.T, handle string, audiences ...string) (name, coreService string) {
	t.Helper()

	exp := time.Now().Add(time.Hour).Unix()
	token := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":%q,"exp":%d}`, testCoreURL, handle, exp))
	name, err := RecordLoginContext(token, testRefreshToken, true)
	if err != nil {
		t.Fatalf("RecordLoginContext(%s): %v", handle, err)
	}

	for _, audience := range audiences {
		if err := RememberJurisdictionAudience(name, audience); err != nil {
			t.Fatalf("RememberJurisdictionAudience(%q): %v", audience, err)
		}
		if err := tokenstore.Set(tokenstore.JurisdictionService(audience), handle, "juri-jwt"); err != nil {
			t.Fatalf("seed jurisdiction token for %q: %v", audience, err)
		}
	}
	return name, tokenstore.CoreKeyringService(testCoreURL)
}

// seedLoginWithJurisdictionTokens is seedAccountWithJurisdictionTokens for the
// single-account tests, which all use handle "alice".
func seedLoginWithJurisdictionTokens(t *testing.T, audiences ...string) (name, coreService string) {
	t.Helper()
	return seedAccountWithJurisdictionTokens(t, "alice", audiences...)
}

// TestRemoveContext_DeletesJurisdictionTokens pins the logout contract for
// data-plane credentials: the jurisdiction access tokens git-remote-entire
// filed are bearers for every repo the account can reach, with an 8h
// server-side TTL, so logout must delete them alongside the login slots rather
// than leave them usable on the machine.
func TestRemoveContext_DeletesJurisdictionTokens(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))

	name, coreService := seedLoginWithJurisdictionTokens(t,
		"https://eu.example.io", "https://au.example.io/")

	if err := RemoveContext(name); err != nil {
		t.Fatalf("RemoveContext: %v", err)
	}

	for _, audience := range []string{"https://eu.example.io", "https://au.example.io/"} {
		svc := tokenstore.JurisdictionService(audience)
		if v, err := tokenstore.Get(svc, "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
			t.Fatalf("jurisdiction token for %q survived logout: value=%q err=%v", audience, v, err)
		}
	}
	if v, err := tokenstore.Get(coreService, "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("access slot survived logout: value=%q err=%v", v, err)
	}
	if v, err := tokenstore.Get(tokenstore.RefreshService(coreService), "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("refresh slot survived logout: value=%q err=%v", v, err)
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load contexts: %v", err)
	}
	if f.Find(name) != nil {
		t.Fatalf("context %q should have been removed", name)
	}
}

// TestRemoveContext_LeavesOtherAccountsJurisdictionTokens pins the scope of the
// sweep: jurisdiction slots are keyed by (audience, handle), and logout only
// deletes the audiences recorded on the context it is removing, under that
// context's own handle. Two accounts sharing a jurisdiction have separate slots,
// so logging one out must leave the other's data-plane token — and its
// bookkeeping — intact.
func TestRemoveContext_LeavesOtherAccountsJurisdictionTokens(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))

	// Both accounts have a token for the same jurisdiction, plus one audience
	// only bob ever reached.
	const shared = "https://eu.example.io"
	const bobOnly = "https://au.example.io"
	aliceName, _ := seedAccountWithJurisdictionTokens(t, "alice", shared)
	bobName, _ := seedAccountWithJurisdictionTokens(t, "bob", shared, bobOnly)

	if err := RemoveContext(aliceName); err != nil {
		t.Fatalf("RemoveContext(%s): %v", aliceName, err)
	}

	if v, err := tokenstore.Get(tokenstore.JurisdictionService(shared), "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("alice's jurisdiction token survived her logout: value=%q err=%v", v, err)
	}
	for _, audience := range []string{shared, bobOnly} {
		if _, err := tokenstore.Get(tokenstore.JurisdictionService(audience), "bob"); err != nil {
			t.Fatalf("bob's jurisdiction token for %q was deleted by alice's logout: %v", audience, err)
		}
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load contexts: %v", err)
	}
	bob := f.Find(bobName)
	if bob == nil {
		t.Fatalf("context %q was removed by alice's logout", bobName)
	}
	if !slices.Equal(bob.JurisdictionAudiences, []string{shared, bobOnly}) {
		t.Fatalf("bob's recorded audiences = %v, want [%s %s]", bob.JurisdictionAudiences, shared, bobOnly)
	}
}

// TestRemoveCurrentContext_DeletesJurisdictionTokens covers the default
// `entire logout` path (active context, not selected by name).
func TestRemoveCurrentContext_DeletesJurisdictionTokens(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))

	const audience = "https://eu.example.io"
	seedLoginWithJurisdictionTokens(t, audience)

	if err := RemoveCurrentContext(); err != nil {
		t.Fatalf("RemoveCurrentContext: %v", err)
	}
	if v, err := tokenstore.Get(tokenstore.JurisdictionService(audience), "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("jurisdiction token survived logout: value=%q err=%v", v, err)
	}
}

// TestRemoveContext_SkipsBlankRecordedAudience covers a hand-edited or
// corrupted contexts.json: a blank audience would resolve to the bare service
// prefix, so it must be skipped rather than looked up, and it must not stop the
// real slots from being deleted.
func TestRemoveContext_SkipsBlankRecordedAudience(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	path := filepath.Join(t.TempDir(), "tokens.json")
	seedRestore := tokenstore.UseFileBackendForTesting(path)

	const audience = "https://eu.example.io"
	name, _ := seedLoginWithJurisdictionTokens(t, audience)

	// Corrupt the record the way only an editor could.
	if err := contexts.Modify(cfgDir, func(f *contexts.File) (bool, error) {
		f.Find(name).JurisdictionAudiences = []string{"", "  ", audience}
		return true, nil
	}); err != nil {
		t.Fatalf("seed blank audiences: %v", err)
	}
	seedRestore()

	// Any lookup of the bare prefix is the bug this guards against.
	blank := tokenstore.JurisdictionService("")
	t.Cleanup(tokenstore.UseObservingBackendForTesting(path, func(op, service, _ string) {
		if service == blank {
			t.Errorf("%s on the bare jurisdiction prefix %q", op, service)
		}
	}))

	if err := RemoveContext(name); err != nil {
		t.Fatalf("RemoveContext: %v", err)
	}
	if v, err := tokenstore.Get(tokenstore.JurisdictionService(audience), "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("real jurisdiction token survived logout: value=%q err=%v", v, err)
	}
}

// TestRemoveContext_JurisdictionDeleteFailureAbortsLogout extends the existing
// keychain-delete contract to the jurisdiction slots: a failed delete must
// surface and leave the context entry in place for a retry, never report
// success over a surviving data-plane bearer.
func TestRemoveContext_JurisdictionDeleteFailureAbortsLogout(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	path := filepath.Join(t.TempDir(), "tokens.json")
	seedRestore := tokenstore.UseFileBackendForTesting(path)

	const audience = "https://eu.example.io"
	name, coreService := seedLoginWithJurisdictionTokens(t, audience)
	seedRestore()

	jurisdictionSvc := tokenstore.JurisdictionService(audience)
	failJurisdictionDelete := func(service, _ string) bool { return service == jurisdictionSvc }
	t.Cleanup(tokenstore.UseFailingDeleteBackendForTesting(path, failJurisdictionDelete))

	if err := RemoveContext(name); err == nil {
		t.Fatal("RemoveContext: want error when the jurisdiction-slot delete fails")
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load contexts: %v", err)
	}
	c := f.Find(name)
	if c == nil {
		t.Fatal("context entry was removed despite the failed credential delete")
	}
	if !slices.Contains(c.JurisdictionAudiences, audience) {
		t.Fatalf("recorded audiences = %v, want %q retained for the retry", c.JurisdictionAudiences, audience)
	}
	// The access slot is deleted after the jurisdiction slots, so the abort
	// must have left it alone.
	if _, err := tokenstore.Get(coreService, "alice"); err != nil {
		t.Fatalf("access slot should be untouched by the aborted logout: %v", err)
	}
}

func TestRememberJurisdictionAudience(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))

	exp := time.Now().Add(time.Hour).Unix()
	name, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, testCoreURL, exp)), testRefreshToken, true)
	if err != nil {
		t.Fatalf("RecordLoginContext: %v", err)
	}

	// Recorded once, trailing slash trimmed so the audience matches the
	// keyring service name the writer and logout both derive.
	if err := RememberJurisdictionAudience(name, "https://eu.example.io/"); err != nil {
		t.Fatalf("first record: %v", err)
	}
	// Idempotent: the same audience (in either spelling) doesn't duplicate.
	if err := RememberJurisdictionAudience(name, "https://eu.example.io"); err != nil {
		t.Fatalf("duplicate record: %v", err)
	}
	if err := RememberJurisdictionAudience(name, "https://au.example.io"); err != nil {
		t.Fatalf("second audience: %v", err)
	}

	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load contexts: %v", err)
	}
	got := f.Find(name).JurisdictionAudiences
	want := []string{"https://eu.example.io", "https://au.example.io"}
	if !slices.Equal(got, want) {
		t.Fatalf("recorded audiences = %v, want %v", got, want)
	}

	// A context that isn't there can't be recorded against — the caller must
	// not then persist a token no logout could find.
	if err := RememberJurisdictionAudience("nope", "https://eu.example.io"); err == nil {
		t.Fatal("want error for an unknown context")
	}
	if err := RememberJurisdictionAudience(name, "  "); err == nil {
		t.Fatal("want error for a blank audience")
	}
}

// TestRecordLoginContext_ReloginKeepsJurisdictionAudiences guards the upsert:
// re-logging in replaces the context entry, but the jurisdiction tokens in the
// keychain (keyed by audience + handle, not by login session) survive it — so
// dropping the list would strand them beyond any future logout.
func TestRecordLoginContext_ReloginKeepsJurisdictionAudiences(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))

	const audience = "https://eu.example.io"
	name, _ := seedLoginWithJurisdictionTokens(t, audience)

	exp := time.Now().Add(2 * time.Hour).Unix()
	again, err := RecordLoginContext(makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, testCoreURL, exp)), testRefreshToken, true)
	if err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if again != name {
		t.Fatalf("re-login produced context %q, want %q", again, name)
	}

	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load contexts: %v", err)
	}
	if got := f.Find(name).JurisdictionAudiences; !slices.Equal(got, []string{audience}) {
		t.Fatalf("recorded audiences after re-login = %v, want [%s]", got, audience)
	}
}
