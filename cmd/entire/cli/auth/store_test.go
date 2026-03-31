package auth

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/zalando/go-keyring"
)

func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

func TestStoreSaveAndGetToken(t *testing.T) {
	// Not parallel: go-keyring's mock provider uses an unprotected map.
	store := NewStoreWithService("test-save-get")

	if err := store.SaveToken("https://entire.io", "prod-token"); err != nil {
		t.Fatalf("SaveToken() error = %v", err)
	}

	got, err := store.GetToken("https://entire.io")
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != "prod-token" {
		t.Fatalf("GetToken() = %q, want %q", got, "prod-token")
	}
}

func TestStoreGetToken_NotFound(t *testing.T) {
	// Not parallel: go-keyring's mock provider uses an unprotected map.
	store := NewStoreWithService("test-not-found")

	got, err := store.GetToken("https://missing.example.com")
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != "" {
		t.Fatalf("GetToken() = %q, want empty string", got)
	}
}

func TestStoreSaveToken_PreservesOtherBaseURLs(t *testing.T) {
	// Not parallel: go-keyring's mock provider uses an unprotected map.
	store := NewStoreWithService("test-preserve")

	if err := store.SaveToken("https://entire.io", "prod-token"); err != nil {
		t.Fatalf("SaveToken(prod) error = %v", err)
	}

	if err := store.SaveToken("http://localhost:8787", "local-token"); err != nil {
		t.Fatalf("SaveToken(local) error = %v", err)
	}

	prod, err := store.GetToken("https://entire.io")
	if err != nil {
		t.Fatalf("GetToken(prod) error = %v", err)
	}
	if prod != "prod-token" {
		t.Fatalf("prod token = %q, want %q", prod, "prod-token")
	}

	local, err := store.GetToken("http://localhost:8787")
	if err != nil {
		t.Fatalf("GetToken(local) error = %v", err)
	}
	if local != "local-token" {
		t.Fatalf("local token = %q, want %q", local, "local-token")
	}
}

func TestStoreSaveToken_RejectsEmptyToken(t *testing.T) {
	// Not parallel: go-keyring's mock provider uses an unprotected map.
	store := NewStoreWithService("test-empty")

	if err := store.SaveToken("https://entire.io", ""); err == nil {
		t.Fatal("SaveToken() with empty token should fail")
	}

	if err := store.SaveToken("https://entire.io", "   "); err == nil {
		t.Fatal("SaveToken() with whitespace token should fail")
	}
}

func TestStoreSaveToken_TrimsWhitespace(t *testing.T) {
	// Not parallel: go-keyring's mock provider uses an unprotected map.
	store := NewStoreWithService("test-trim")

	if err := store.SaveToken("https://entire.io", "  my-token  "); err != nil {
		t.Fatalf("SaveToken() error = %v", err)
	}

	got, err := store.GetToken("https://entire.io")
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != "my-token" {
		t.Fatalf("GetToken() = %q, want %q (whitespace should be trimmed)", got, "my-token")
	}
}

func TestStoreDeleteToken(t *testing.T) {
	// Not parallel: go-keyring's mock provider uses an unprotected map.
	store := NewStoreWithService("test-delete")

	if err := store.SaveToken("https://entire.io", "tok"); err != nil {
		t.Fatalf("SaveToken() error = %v", err)
	}

	if err := store.DeleteToken("https://entire.io"); err != nil {
		t.Fatalf("DeleteToken() error = %v", err)
	}

	got, err := store.GetToken("https://entire.io")
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != "" {
		t.Fatalf("GetToken() after delete = %q, want empty", got)
	}
}

func TestStoreDeleteToken_NotFoundIsNoop(t *testing.T) {
	// Not parallel: go-keyring's mock provider uses an unprotected map.
	store := NewStoreWithService("test-delete-noop")

	if err := store.DeleteToken("https://nonexistent.example.com"); err != nil {
		t.Fatalf("DeleteToken() on missing key error = %v", err)
	}
}

func TestLookupCurrentToken(t *testing.T) {
	t.Setenv(api.BaseURLEnvVar, "http://localhost:8787")

	store := NewStore()
	if err := store.SaveToken("http://localhost:8787", "local-token"); err != nil {
		t.Fatalf("SaveToken() error = %v", err)
	}

	got, err := LookupCurrentToken()
	if err != nil {
		t.Fatalf("LookupCurrentToken() error = %v", err)
	}
	if got != "local-token" {
		t.Fatalf("LookupCurrentToken() = %q, want %q", got, "local-token")
	}
}
