package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/auth-go/tokens"
	authtokenstore "github.com/entireio/auth-go/tokenstore"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/tokenstore/senclave"
)

// fakeSealer is a reversible stand-in for the Secure Enclave key.
type fakeSealer struct {
	mu      sync.Mutex
	unseals int
	reasons []string
	fail    error
	// failOn makes only the Nth unseal (1-based) fail with ErrCanceled.
	failOn int
}

const fakeSealPrefix = "FAKESEAL|"

func (f *fakeSealer) Seal(plaintext []byte) ([]byte, error) {
	return append([]byte(fakeSealPrefix), plaintext...), nil
}

func (f *fakeSealer) Unseal(ciphertext []byte, reason string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unseals++
	f.reasons = append(f.reasons, reason)
	if f.fail != nil {
		return nil, f.fail
	}
	if f.failOn > 0 && f.unseals == f.failOn {
		return nil, senclave.ErrCanceled
	}
	s := string(ciphertext)
	if !strings.HasPrefix(s, fakeSealPrefix) {
		return nil, errors.New("fake: not sealed")
	}
	return []byte(strings.TrimPrefix(s, fakeSealPrefix)), nil
}

func (f *fakeSealer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unseals
}

func (f *fakeSealer) lastReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reasons) == 0 {
		return ""
	}
	return f.reasons[len(f.reasons)-1]
}

// setSealerErrForTesting makes every sealer lookup fail with err.
func setSealerErrForTesting(t *testing.T, err error) {
	t.Helper()
	protection.mu.Lock()
	prevOpen := protection.open
	protection.cached = nil
	protection.open = func() (senclave.Sealer, error) { return nil, err }
	protection.mu.Unlock()
	t.Cleanup(func() {
		protection.mu.Lock()
		protection.open = prevOpen
		protection.cached = nil
		protection.mu.Unlock()
	})
}

// protectedFixture isolates the token store and installs a fake sealer.
func protectedFixture(t *testing.T) (*fakeSealer, *contexts.Context) {
	t.Helper()
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	fs := &fakeSealer{}
	SetSealerForTesting(t, fs)
	c := &contexts.Context{
		Name:            "prod",
		CoreURL:         "https://core.example.test/",
		Handle:          "toothbrush",
		KeychainService: tokenstore.CoreKeyringService("https://core.example.test"),
	}
	return fs, c
}

func TestSealedStore_RoundTripOnePrompt(t *testing.T) {
	fs, c := protectedFixture(t)
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}

	exp := time.Now().Add(30 * time.Minute)
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc-1", RefreshToken: "ref-1", ExpiresAt: exp}); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}
	if fs.count() != 0 {
		t.Fatalf("save must not prompt; unseals=%d", fs.count())
	}
	raw, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil || !isSealed(raw) {
		t.Fatalf("access slot not sealed: %q err=%v", raw, err)
	}
	if strings.Contains(raw, "acc-1") || strings.Contains(raw, "ref-1") {
		t.Fatalf("plaintext leaked into slot: %q", raw)
	}
	if _, err := tokenstore.Get(tokenstore.RefreshService(c.KeychainService), c.Handle); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("refresh slot should be cleared, err=%v", err)
	}

	// Another process would not have the just-sealed bundle cached.
	forgetBundles()
	store.setPromptAction("git push to cluster.example.test")
	got, err := store.LoadTokens("")
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	if got.AccessToken != "acc-1" || got.RefreshToken != "ref-1" {
		t.Fatalf("tokens mismatch: %+v", got)
	}
	if got.ExpiresAt.IsZero() || got.ExpiresAt.After(exp.Add(2*time.Second)) {
		t.Fatalf("expiry not carried: %v", got.ExpiresAt)
	}
	if fs.count() != 1 {
		t.Fatalf("one load must be one prompt; unseals=%d", fs.count())
	}
	want := "git push to cluster.example.test with Entire login toothbrush@core.example.test"
	if fs.lastReason() != want {
		t.Fatalf("reason = %q, want %q", fs.lastReason(), want)
	}
}

func TestSealedStore_SaveCarriesRefreshForward(t *testing.T) {
	_, c := protectedFixture(t)
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc-1", RefreshToken: "ref-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadTokens(""); err != nil {
		t.Fatal(err)
	}
	// Server did not rotate: an empty refresh means "leave as-is".
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc-2"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadTokens("")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "acc-2" || got.RefreshToken != "ref-1" {
		t.Fatalf("refresh not carried forward: %+v", got)
	}
}

func TestSealedStore_SaveCarriesCurrentSlotRefresh(t *testing.T) {
	_, c := protectedFixture(t)
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc-1", RefreshToken: "ref-old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadTokens(""); err != nil {
		t.Fatal(err)
	}
	// Another process rotates the slot; this process has unsealed that
	// newer bundle too (as auth-go's re-read after locking would).
	sl, err := protection.sealer()
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := sealSlot(sl, tokenBundle{Issuer: c.CoreURL, Handle: c.Handle, Access: "acc-1", Refresh: "ref-new"}, 600)
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(c.KeychainService, c.Handle, rotated); err != nil {
		t.Fatal(err)
	}
	// A save without a refresh token must carry the slot's current one, not
	// the stale value from the earlier load.
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc-2"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadTokens("")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "acc-2" || got.RefreshToken != "ref-new" {
		t.Fatalf("stale refresh carried forward: %+v", got)
	}
}

func TestSealedStore_RejectsRedirectedContext(t *testing.T) {
	_, c := protectedFixture(t)
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc", RefreshToken: "ref"}); err != nil {
		t.Fatal(err)
	}
	// contexts.json edited to point the same slot at another login server.
	evil := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: "https://evil.example.test"}
	if _, err := evil.LoadTokens(""); !errors.Is(err, ErrBundleMismatch) {
		t.Fatalf("err = %v, want ErrBundleMismatch", err)
	}
	// The sealed value copied into another account's slot.
	raw, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(c.KeychainService, "someone-else", raw); err != nil {
		t.Fatal(err)
	}
	other := &contextTokenStore{service: c.KeychainService, handle: "someone-else", issuer: c.CoreURL}
	if _, err := other.LoadTokens(""); !errors.Is(err, ErrBundleMismatch) {
		t.Fatalf("handle mismatch err = %v, want ErrBundleMismatch", err)
	}
}

func TestSealedStore_DeclinedPromptIsClassified(t *testing.T) {
	fs, c := protectedFixture(t)
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc"}); err != nil {
		t.Fatal(err)
	}
	fs.fail = senclave.ErrCanceled
	forgetBundles()
	_, err := store.LoadTokens("")
	if !PromptDeclined(err) {
		t.Fatalf("err = %v, want PromptDeclined", err)
	}
	if _, err := LoginTokenForContext(c); !PromptDeclined(err) {
		t.Fatalf("LoginTokenForContext err = %v, want PromptDeclined", err)
	}
}

func TestSealedStore_PlaintextRefusedWhileProtected(t *testing.T) {
	fs, c := protectedFixture(t)
	// A plaintext slot left by an interrupted enrolment or a legacy writer
	// must not be readable without the dialog while a key is enrolled.
	if err := tokenstore.Set(c.KeychainService, c.Handle, tokenstore.EncodeTokenWithExpiration("plain-acc", 600)); err != nil {
		t.Fatal(err)
	}
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if _, err := store.LoadTokens(""); !errors.Is(err, ErrPlaintextWhileProtected) {
		t.Fatalf("LoadTokens err = %v, want ErrPlaintextWhileProtected", err)
	}
	if _, err := LoginTokenForContext(c); !errors.Is(err, ErrPlaintextWhileProtected) {
		t.Fatalf("LoginTokenForContext err = %v, want ErrPlaintextWhileProtected", err)
	}
	if fs.count() != 0 {
		t.Fatalf("refusal must not prompt; unseals=%d", fs.count())
	}
}

func TestSealContext_ClearsStrayRefreshWhenAlreadySealed(t *testing.T) {
	fs, c := protectedFixture(t)
	sl, err := protection.sealer()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sealSlot(sl, tokenBundle{Issuer: c.CoreURL, Handle: c.Handle, Access: "acc", Refresh: "ref"}, 600)
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(c.KeychainService, c.Handle, enc); err != nil {
		t.Fatal(err)
	}
	// An earlier run sealed the access slot but died before clearing this.
	if err := tokenstore.Set(tokenstore.RefreshService(c.KeychainService), c.Handle, "stale-plaintext"); err != nil {
		t.Fatal(err)
	}
	done, err := sealContext(sl, c)
	if err != nil || done {
		t.Fatalf("sealContext = %v, %v; want already-sealed no-op", done, err)
	}
	if _, err := tokenstore.Get(tokenstore.RefreshService(c.KeychainService), c.Handle); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("stray plaintext refresh survived, err=%v", err)
	}
	if fs.count() != 0 {
		t.Fatalf("cleanup must not prompt; unseals=%d", fs.count())
	}
}

func TestSealedStore_ProtectionOffUsesPlaintext(t *testing.T) {
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	SetSealerForTesting(t, nil)
	store := &contextTokenStore{service: "entire-core:https://core.example.test", handle: "h", issuer: "https://core.example.test"}
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc", RefreshToken: "ref"}); err != nil {
		t.Fatal(err)
	}
	raw, err := tokenstore.Get(store.service, store.handle)
	if err != nil || isSealed(raw) || !strings.HasPrefix(raw, "acc|") {
		t.Fatalf("expected plaintext slot, got %q err=%v", raw, err)
	}
	if TokensProtected() {
		t.Fatal("TokensProtected should be false")
	}
}

func TestSealedStore_MissingSlotIsNotLoggedIn(t *testing.T) {
	_, c := protectedFixture(t)
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if _, err := store.LoadTokens(""); !errors.Is(err, authtokenstore.ErrNotFound) {
		t.Fatalf("err = %v, want auth-go ErrNotFound", err)
	}
}

func TestRecordLoginContext_SealsWhenProtected(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	fs, _ := protectedFixture(t)

	// A plaintext refresh token from before protection was turned on must
	// not survive a re-login: it would be usable without the dialog.
	service := tokenstore.CoreKeyringService("https://core.example.test")
	if err := tokenstore.Set(tokenstore.RefreshService(service), "toothbrush", "stale-plaintext-refresh"); err != nil {
		t.Fatal(err)
	}
	token := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example.test","handle":"toothbrush","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	if _, err := RecordLoginContext(token, "refresh-1", true); err != nil {
		t.Fatalf("RecordLoginContext: %v", err)
	}
	raw, err := tokenstore.Get(service, "toothbrush")
	if err != nil || !isSealed(raw) {
		t.Fatalf("login not sealed: %q err=%v", raw, err)
	}
	if _, err := tokenstore.Get(tokenstore.RefreshService(service), "toothbrush"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("refresh slot should be empty, err=%v", err)
	}
	if fs.count() != 0 {
		t.Fatalf("login must not prompt; unseals=%d", fs.count())
	}
	b, _, err := openSealedSlot(raw, "https://core.example.test", "toothbrush", "test")
	if err != nil || b.Access != token || b.Refresh != "refresh-1" {
		t.Fatalf("bundle = %+v err=%v", b, err)
	}
}

func TestEnableDisableProtection_RewritesSlots(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	fs, c := protectedFixture(t)
	if err := contexts.Save(cfgDir, &contexts.File{CurrentContext: c.Name, Contexts: []*contexts.Context{c}}); err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(c.KeychainService, c.Handle, tokenstore.EncodeTokenWithExpiration("acc", 900)); err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(tokenstore.RefreshService(c.KeychainService), c.Handle, "ref"); err != nil {
		t.Fatal(err)
	}

	sealed, created, err := EnableProtection(cfgDir)
	if err != nil {
		t.Fatalf("EnableProtection: %v", err)
	}
	if created || len(sealed) != 1 || sealed[0] != "prod" {
		t.Fatalf("sealed=%v created=%v", sealed, created)
	}
	raw, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil || !isSealed(raw) {
		t.Fatalf("slot not sealed: %q err=%v", raw, err)
	}
	// Idempotent.
	if again, _, err := EnableProtection(cfgDir); err != nil || len(again) != 0 {
		t.Fatalf("second enable sealed=%v err=%v", again, err)
	}

	// `auth unprotect` runs in a fresh process with nothing cached.
	forgetBundles()
	unsealed, dropped, err := DisableProtection(cfgDir)
	if err != nil {
		t.Fatalf("DisableProtection: %v", err)
	}
	if len(unsealed) != 1 || dropped != nil || fs.count() != 1 {
		t.Fatalf("unsealed=%v dropped=%v prompts=%d", unsealed, dropped, fs.count())
	}
	raw, err = tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil || !strings.HasPrefix(raw, "acc|") {
		t.Fatalf("slot not plaintext: %q err=%v", raw, err)
	}
	ref, err := tokenstore.Get(tokenstore.RefreshService(c.KeychainService), c.Handle)
	if err != nil || ref != "ref" {
		t.Fatalf("refresh = %q err=%v", ref, err)
	}
}

func TestSealedStore_OnePromptPerProcess(t *testing.T) {
	fs, c := protectedFixture(t)
	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "acc-1", RefreshToken: "ref-1"}); err != nil {
		t.Fatal(err)
	}
	// Same process that sealed it: no prompt at all.
	if _, err := store.LoadTokens(""); err != nil {
		t.Fatal(err)
	}
	if fs.count() != 0 {
		t.Fatalf("load after own save prompted %d times", fs.count())
	}

	// A fresh process: the first read prompts, every later read of the
	// same slot (other token managers, auth-go's re-read) does not.
	forgetBundles()
	for i := range 3 {
		other := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
		if _, err := other.LoadTokens(""); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
	}
	if _, err := LoginTokenForContext(c); err != nil {
		t.Fatal(err)
	}
	if fs.count() != 1 {
		t.Fatalf("expected exactly one prompt, got %d", fs.count())
	}

	// A slot rotated by another process has new ciphertext: prompt again.
	sl, err := protection.sealer()
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := sealSlot(sl, tokenBundle{Issuer: c.CoreURL, Handle: c.Handle, Access: "acc-2", Refresh: "ref-2"}, 600)
	if err != nil {
		t.Fatal(err)
	}
	forgetBundles()
	if err := tokenstore.Set(c.KeychainService, c.Handle, rotated); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadTokens("")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "acc-2" || fs.count() != 2 {
		t.Fatalf("rotated slot: access=%q prompts=%d", got.AccessToken, fs.count())
	}
}

func TestDisableProtection_PartialFailureRollsBack(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	fs, a := protectedFixture(t)
	b := &contexts.Context{
		Name:            "staging",
		CoreURL:         "https://staging.example.test",
		Handle:          "toothbrush",
		KeychainService: tokenstore.CoreKeyringService("https://staging.example.test"),
	}
	if err := contexts.Save(cfgDir, &contexts.File{CurrentContext: a.Name, Contexts: []*contexts.Context{a, b}}); err != nil {
		t.Fatal(err)
	}
	sl, err := protection.sealer()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*contexts.Context{a, b} {
		enc, err := sealSlot(sl, tokenBundle{Issuer: c.CoreURL, Handle: c.Handle, Access: "acc-" + c.Name, Refresh: "ref-" + c.Name}, 600)
		if err != nil {
			t.Fatal(err)
		}
		if err := tokenstore.Set(c.KeychainService, c.Handle, enc); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh `auth unprotect` process; the user cancels the second dialog.
	forgetBundles()
	fs.failOn = 2
	unsealed, _, err := DisableProtection(cfgDir)
	if err == nil || !PromptDeclined(err) {
		t.Fatalf("DisableProtection err = %v, want declined prompt", err)
	}
	if len(unsealed) != 0 {
		t.Fatalf("partial failure must report nothing unsealed, got %v", unsealed)
	}
	// The first context was unsealed and must be sealed again, with no
	// further prompt and no stray plaintext refresh token.
	raw, err := tokenstore.Get(a.KeychainService, a.Handle)
	if err != nil || !isSealed(raw) {
		t.Fatalf("first context not re-sealed: %q err=%v", raw, err)
	}
	if _, err := tokenstore.Get(tokenstore.RefreshService(a.KeychainService), a.Handle); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("plaintext refresh left behind, err=%v", err)
	}
	if fs.count() != 2 {
		t.Fatalf("expected exactly the two unseal attempts, got %d", fs.count())
	}
	if !TokensProtected() {
		t.Fatal("key must remain enrolled after a failed unprotect")
	}
	// Both logins still load, each behind the dialog as before.
	fs.failOn = 0
	for _, c := range []*contexts.Context{a, b} {
		store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
		got, err := store.LoadTokens("")
		if err != nil || got.AccessToken != "acc-"+c.Name || got.RefreshToken != "ref-"+c.Name {
			t.Fatalf("context %s after rollback: %+v err=%v", c.Name, got, err)
		}
	}
}

func TestDisableProtection_DropsUnusableKey(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	fs, c := protectedFixture(t)
	if err := contexts.Save(cfgDir, &contexts.File{CurrentContext: c.Name, Contexts: []*contexts.Context{c}}); err != nil {
		t.Fatal(err)
	}
	sl, err := protection.sealer()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sealSlot(sl, tokenBundle{Issuer: c.CoreURL, Handle: c.Handle, Access: "acc", Refresh: "ref"}, 600)
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(c.KeychainService, c.Handle, enc); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(cfgDir, ProtectedKeyFile)
	if err := os.WriteFile(keyPath, []byte("blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The blob is there but will not load: a config dir synced from a Mac
	// to a machine without an enclave, or a damaged file.
	setSealerErrForTesting(t, fmt.Errorf("%w: %w", ErrKeyUnusable, senclave.ErrUnsupported))
	forgetBundles()

	store := &contextTokenStore{service: c.KeychainService, handle: c.Handle, issuer: c.CoreURL}
	if _, err := store.LoadTokens(""); !errors.Is(err, ErrKeyUnusable) {
		t.Fatalf("LoadTokens err = %v, want ErrKeyUnusable", err)
	}
	if err := store.SaveTokens("", tokens.TokenSet{AccessToken: "new"}); !errors.Is(err, ErrKeyUnusable) {
		t.Fatalf("SaveTokens err = %v, want ErrKeyUnusable", err)
	}

	unsealed, dropped, err := DisableProtection(cfgDir)
	if err != nil {
		t.Fatalf("DisableProtection: %v", err)
	}
	if len(unsealed) != 0 || len(dropped) != 1 || dropped[0] != "prod" {
		t.Fatalf("unsealed=%v dropped=%v", unsealed, dropped)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key blob still present, err=%v", err)
	}
	if fs.count() != 0 {
		t.Fatalf("an unusable key must not prompt, got %d", fs.count())
	}
	// The sealed slot is left for `entire login` to overwrite.
	if raw, err := tokenstore.Get(c.KeychainService, c.Handle); err != nil || !isSealed(raw) {
		t.Fatalf("slot = %q err=%v", raw, err)
	}
}

func TestPromptAction_FromContext(t *testing.T) {
	t.Parallel()
	ctx := WithPromptAction(context.Background(), "git push to h")
	if got := promptActionFrom(ctx); got != "git push to h" {
		t.Fatalf("got %q", got)
	}
	if got := promptActionFrom(context.Background()); !strings.HasPrefix(got, "entire") {
		t.Fatalf("default action %q", got)
	}
}
