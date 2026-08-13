package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

func TestRemoveCurrentLoginDeletesAllCredentials(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))

	exp := time.Now().Add(time.Hour).Unix()
	token := makeJWT(t, fmt.Sprintf(`{"iss":"https://core.example.com","handle":"alice","exp":%d}`, exp))
	if err := RecordLogin(token, testRefreshToken); err != nil {
		t.Fatal(err)
	}
	if err := contexts.Modify(cfgDir, func(f *contexts.File) (bool, error) {
		f.Contexts[0].JurisdictionAudiences = []string{"https://eu.example.io"}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.Set(tokenstore.JurisdictionService("https://eu.example.io"), "alice", "jurisdiction-token"); err != nil {
		t.Fatal(err)
	}

	if err := RemoveCurrentLogin(); err != nil {
		t.Fatal(err)
	}
	f, err := contexts.Load(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Contexts) != 0 || f.CurrentContext != "" {
		t.Fatalf("login metadata survived: %+v", f)
	}
	services := []string{
		tokenstore.CoreKeyringService("https://core.example.com"),
		tokenstore.RefreshService(tokenstore.CoreKeyringService("https://core.example.com")),
		tokenstore.JurisdictionService("https://eu.example.io"),
	}
	for _, service := range services {
		if _, err := tokenstore.Get(service, "alice"); !errors.Is(err, tokenstore.ErrNotFound) {
			t.Fatalf("credential %q survived: %v", service, err)
		}
	}
}
