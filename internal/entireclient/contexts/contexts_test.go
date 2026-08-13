package contexts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

func setupStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	return dir
}

func testFile() *contexts.File {
	login := &contexts.Context{Name: "current", CoreURL: "https://eu.auth.entire.io", Handle: "alice", KeychainService: "entire-core:https://eu.auth.entire.io"}
	return &contexts.File{CurrentContext: login.Name, Contexts: []*contexts.Context{login}}
}

func TestLoadMissingReturnsLoggedOut(t *testing.T) {
	dir := setupStore(t)
	f, err := contexts.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.CurrentContext != "" || len(f.Contexts) != 0 {
		t.Fatalf("load = %+v", f)
	}
}

func TestSaveStoresMetadataOutsideContextsJSON(t *testing.T) {
	dir := setupStore(t)
	if err := contexts.Save(dir, testFile()); err != nil {
		t.Fatal(err)
	}
	path, err := contexts.FilePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy contexts.json should not exist: %v", err)
	}
	got, err := contexts.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentContext != "current" || len(got.Contexts) != 1 || got.Contexts[0].Handle != "alice" {
		t.Fatalf("load = %+v", got)
	}
}

func TestModifyUpdatesCredentialMetadata(t *testing.T) {
	dir := setupStore(t)
	if err := contexts.Save(dir, testFile()); err != nil {
		t.Fatal(err)
	}
	if err := contexts.Modify(dir, func(f *contexts.File) (bool, error) {
		f.Contexts[0].Handle = "bob"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := contexts.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Contexts[0].Handle != "bob" {
		t.Fatalf("handle = %q", got.Contexts[0].Handle)
	}
}

func TestLoadMigratesOnlyCurrentLegacyContext(t *testing.T) {
	dir := setupStore(t)
	legacy := &contexts.File{
		CurrentContext: "eu",
		Contexts: []*contexts.Context{
			{Name: "us", CoreURL: "https://us.auth.entire.io", Handle: "alice"},
			{Name: "eu", CoreURL: "https://eu.auth.entire.io", Handle: "alice"},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path, err := contexts.FilePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set("old-us", "alice", "old-access"); err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(tokenstore.RefreshService("old-us"), "alice", "old-refresh"); err != nil {
		t.Fatal(err)
	}
	legacy.Contexts[0].KeychainService = "old-us"
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := contexts.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Contexts) != 1 || got.Contexts[0].Name != "eu" {
		t.Fatalf("migrated = %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy contexts.json was not removed: %v", err)
	}
	if _, err := tokenstore.Get("old-us", "alice"); err == nil {
		t.Fatal("non-current legacy access token survived migration")
	}
	if _, err := tokenstore.Get(tokenstore.RefreshService("old-us"), "alice"); err == nil {
		t.Fatal("non-current legacy refresh token survived migration")
	}
}
