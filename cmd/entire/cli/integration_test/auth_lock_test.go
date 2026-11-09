//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

func TestAuthToken_LockDirectoryIsolation(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"xdg-set", "xdg-unset"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			configDir := t.TempDir()
			tokensPath := filepath.Join(t.TempDir(), "tokens.json")
			refreshedJWT := fakeLoginJWT("https://core.example")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != pathOAuthToken || r.FormValue("refresh_token") != "old-refresh" {
					t.Errorf("unexpected refresh request: %s", r.URL.Path)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				writeJSON(t, w, http.StatusOK, map[string]any{
					"access_token": refreshedJWT, "refresh_token": "new-refresh",
					"token_type": "Bearer", "expires_in": 3600,
				})
			}))
			defer server.Close()

			service := tokenstore.CoreKeyringService(server.URL)
			if err := contexts.Save(configDir, &contexts.File{
				CurrentContext: "test",
				Contexts:       []*contexts.Context{{Name: "test", CoreURL: server.URL, Handle: "alice", KeychainService: service}},
			}); err != nil {
				t.Fatal(err)
			}
			expiredJWT := fakeJWT(server.URL, 1)
			data, err := json.Marshal(map[string]map[string]string{
				service:                            {"alice": expiredJWT + tokenstore.TokenExpirationSeparator + "0"},
				tokenstore.RefreshService(service): {"alice": "old-refresh"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tokensPath, data, 0o600); err != nil {
				t.Fatal(err)
			}

			// HOME, USERPROFILE and LocalAppData all point at home, so both
			// Entire's cache and auth-go's own default would land under it.
			cacheHome := t.TempDir()
			wantDir := filepath.Join(home, ".cache", "entire", "auth-locks")
			cmd := execx.NonInteractive(t.Context(), getTestBinary(), "auth", "token", "--insecure-http-auth")
			cmd.Dir = home
			cmd.Env = slices.DeleteFunc(testutil.GitIsolatedEnv(), func(entry string) bool {
				return strings.HasPrefix(entry, "XDG_CACHE_HOME=")
			})
			cmd.Env = append(cmd.Env,
				"HOME="+home, "USERPROFILE="+home, "LocalAppData="+home,
				"ENTIRE_CONFIG_DIR="+configDir, "ENTIRE_TOKEN_STORE=file", "ENTIRE_TOKEN_STORE_PATH="+tokensPath,
			)
			if mode == "xdg-set" {
				wantDir = filepath.Join(cacheHome, "entire", "auth-locks")
				cmd.Env = append(cmd.Env, "XDG_CACHE_HOME="+cacheHome)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("auth token: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != refreshedJWT {
				t.Fatalf("auth token returned %q, want refreshed token", got)
			}
			locks := append(lockFiles(t, home), lockFiles(t, cacheHome)...)
			if len(locks) != 1 || filepath.Dir(locks[0]) != wantDir {
				t.Fatalf("lock files = %v, want exactly one in %s", locks, wantDir)
			}
		})
	}
}

func lockFiles(t *testing.T, root string) []string {
	t.Helper()
	var locks []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".lock") {
			locks = append(locks, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return locks
}
