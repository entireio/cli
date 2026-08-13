package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

const testRefreshToken = "entr_refresh"

func makeJWT(t *testing.T, payloadJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(payloadJSON))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"))
}

func setupLoginTest(t *testing.T) string {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	return cfgDir
}

func TestRecordLoginStoresOneCurrentLogin(t *testing.T) {
	cfgDir := setupLoginTest(t)
	exp := time.Now().Add(time.Hour).Unix()
	token := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example.com","handle":"alice","exp":%d}`, exp))

	if err := RecordLogin(token, testRefreshToken); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}

	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatalf("load login: %v", err)
	}
	if f.CurrentContext != "current" || len(f.Contexts) != 1 {
		t.Fatalf("stored login = %+v", f)
	}
	login := f.Contexts[0]
	if login.CoreURL != "https://core.example.com" || login.Handle != "alice" {
		t.Fatalf("login = %+v", login)
	}
	encoded, err := tokenstore.Get(login.KeychainService, login.Handle)
	if err != nil {
		t.Fatalf("read access token: %v", err)
	}
	access, _ := tokenstore.DecodeTokenWithExpiration(encoded)
	if access != token {
		t.Fatalf("access token mismatch")
	}
	refresh, err := tokenstore.Get(tokenstore.RefreshService(login.KeychainService), login.Handle)
	if err != nil || refresh != testRefreshToken {
		t.Fatalf("refresh token = %q, err=%v", refresh, err)
	}
	if _, err := contexts.FilePath(cfgDir); err != nil {
		t.Fatal(err)
	}
}

func TestRecordLoginReplacesPreviousLogin(t *testing.T) {
	cfgDir := setupLoginTest(t)
	exp := time.Now().Add(time.Hour).Unix()
	first := makeJWT(t, fmt.Sprintf(`{"iss":"https://a.example.com","handle":"alice","exp":%d}`, exp))
	second := makeJWT(t, fmt.Sprintf(`{"iss":"https://b.example.com","handle":"bob","exp":%d}`, exp))
	if err := RecordLogin(first, "refresh-a"); err != nil {
		t.Fatal(err)
	}
	oldService := tokenstore.CoreKeyringService("https://a.example.com")
	if err := RecordLogin(second, "refresh-b"); err != nil {
		t.Fatal(err)
	}

	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Contexts) != 1 || f.Contexts[0].CoreURL != "https://b.example.com" || f.Contexts[0].Handle != "bob" {
		t.Fatalf("current login = %+v", f)
	}
	if _, err := tokenstore.Get(oldService, "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("old access token survived: %v", err)
	}
	if _, err := tokenstore.Get(tokenstore.RefreshService(oldService), "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("old refresh token survived: %v", err)
	}
}

func TestRecordLoginRejectsMissingClaims(t *testing.T) {
	setupLoginTest(t)
	if err := RecordLogin(makeJWT(t, `{"handle":"alice"}`), ""); err == nil {
		t.Fatal("expected missing issuer error")
	}
	if err := RecordLogin(makeJWT(t, `{"iss":"https://core.example.com"}`), ""); err == nil {
		t.Fatal("expected missing identity error")
	}
}

func TestLocalIdentityCacheKeyCurrentLogin(t *testing.T) {
	cfgDir := setupLoginTest(t)
	t.Setenv(EnvTokenVar, "")
	login := &contexts.Context{Name: "current", CoreURL: "https://core.example.com/", Handle: "alice", KeychainService: "entire-core:https://core.example.com"}
	if err := contexts.Save(cfgDir, &contexts.File{CurrentContext: login.Name, Contexts: []*contexts.Context{login}}); err != nil {
		t.Fatal(err)
	}
	got, err := LocalIdentityCacheKey()
	if err != nil {
		t.Fatal(err)
	}
	if want := "login|https://core.example.com|alice|entire-core:https://core.example.com"; got != want {
		t.Fatalf("cache key = %q, want %q", got, want)
	}
}

func TestLocalIdentityCacheKeyEnvToken(t *testing.T) {
	token := makeJWT(t, `{"iss":"https://core.example.com/","sub":"svc-1","handle":"robot","aud":"https://api.example.com"}`)
	t.Setenv(EnvTokenVar, token)
	got, err := LocalIdentityCacheKey()
	if err != nil {
		t.Fatal(err)
	}
	if want := "env|https://core.example.com|svc-1|robot|https://api.example.com"; got != want {
		t.Fatalf("cache key = %q, want %q", got, want)
	}
	if strings.Contains(got, token) {
		t.Fatal("cache key contains raw token")
	}
}
