package settings

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// resolvedRoot returns dir in the symlink-resolved slash form trust path keys use.
func resolvedRoot(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(resolved)
}

// newTrustTestRepo initializes an isolated repo with a fresh user settings dir
// and enters it. Not parallel-safe: t.Setenv/t.Chdir.
func newTrustTestRepo(t *testing.T) (dir, cfg string) {
	t.Helper()
	dir = newGlobalTestRepo(t)
	cfg = t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Chdir(dir)
	return dir, cfg
}

// TestCheckpointEgressAllowed drives the gate across the identity rules:
// fetch AND push URL keys all required, unkeyable configured URLs hold,
// exclusive identity sides, every failure holds.
func TestCheckpointEgressAllowed(t *testing.T) {
	multiURLOrigin := [][]string{
		{"remote", "add", "origin", "git@gitlab.example.com:team/proj.git"},
		{"remote", "set-url", "--add", "origin", "https://github.com/Acme/Widgets.git"},
	}
	normalOriginWithPushurl := [][]string{
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
		{"config", "remote.origin.pushurl", "git@gitlab.example.com:team/proj.git"},
	}
	for _, tc := range []struct {
		name            string
		remotes         [][]string
		userSettings    string // %ROOT% expands to the resolved worktree root
		repoSetup       bool
		unreadableStore bool
		want            bool
	}{
		// checkpoint_remote is moot: it can only live in repo-level settings.
		{"committed repo settings never grant consent", nil, "", true, false, false},
		{"trust_all passes", nil, `{"global":{"enabled":true,"trust_all":true}}`, false, false, true},
		{"a trusted subset of a multi-URL origin holds", multiURLOrigin,
			`{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`, false, false, false},
		{"all origin keys trusted passes", multiURLOrigin,
			`{"global":{"enabled":true,"trusted_origins":["gitlab.example.com/team/proj","github.com/acme/widgets"]}}`, false, false, true},
		// Egress follows the pushurl (pushurl-replaces-url): its key is required.
		{"the fetch-URL key alone does not cover a differing pushurl", normalOriginWithPushurl,
			`{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`, false, false, false},
		{"fetch-URL and pushurl keys together pass", normalOriginWithPushurl,
			`{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets","gitlab.example.com/team/proj"]}}`, false, false, true},
		// A configured but unkeyable URL holds; path identity is only for repos
		// without an origin.
		{"unnormalizable pushurl holds even when path trusted", [][]string{
			{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
			{"config", "remote.origin.pushurl", "file:///srv/git/secret.git"}},
			`{"global":{"enabled":true,"trusted_paths":["%ROOT%"]}}`, false, false, false},
		{"a mixed origin holds even with its normalizable key trusted", [][]string{
			{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
			{"remote", "set-url", "--add", "origin", "file:///srv/git/secret.git"}},
			`{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`, false, false, false},
		// Identity exclusivity: gaining an origin re-asks.
		{"a trusted path does not cover an origin-keyed identity",
			[][]string{{"remote", "add", "origin", "https://github.com/acme/widgets.git"}},
			`{"global":{"enabled":true,"trusted_paths":["%ROOT%"]}}`, false, false, false},
		{"trusted path identity passes with no origin", nil, `{"global":{"enabled":true,"trusted_paths":["%ROOT%"]}}`, false, false, true},
		// settings.json as a DIRECTORY exercises the read-error branch portably.
		{"unreadable user settings hold (fail closed)", nil, "", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg := newTrustTestRepo(t)
			for _, args := range tc.remotes {
				addTestRemote(t, dir, args...)
			}
			if tc.repoSetup {
				if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
					t.Fatal(err)
				}
				repoSettings := `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/widgets"}}}`
				if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(repoSettings), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.unreadableStore {
				if err := os.MkdirAll(filepath.Join(cfg, UserSettingsFileName), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.userSettings != "" {
				writeUserSettings(t, cfg, strings.ReplaceAll(tc.userSettings, "%ROOT%", resolvedRoot(t, dir)))
			}
			if got := CheckpointEgressAllowed(t.Context()); got != tc.want {
				t.Fatalf("CheckpointEgressAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckpointEgressAllowed_IncidentalRepoSettingsRequireGlobalTrust(t *testing.T) {
	dir, cfg := newTrustTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"investigate":{"max_turns":4}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)

	if CheckpointEgressAllowed(t.Context()) {
		t.Fatal("an incidental repo settings file must not grant checkpoint egress")
	}
	if !RepoUntrustedEnrolled(t.Context()) {
		t.Fatal("an incidental repo settings file must retain the globally enrolled trust hold")
	}

	writeUserSettings(t, cfg, `{"global":{"enabled":true,"trust_all":true}}`)
	if !CheckpointEgressAllowed(t.Context()) {
		t.Fatal("trust_all must grant egress when repo settings contain no activation intent")
	}

	if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"enabled":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if CheckpointEgressAllowed(t.Context()) {
		t.Fatal("malformed repo settings must fail closed even when trust_all is enabled")
	}
}

// TestTrustAndRevokeCurrentRepo: trusting writes all origin keys once and
// revoke removes the CLI-recorded keys without guessing at hand edits.
func TestTrustAndRevokeCurrentRepo(t *testing.T) {
	dir, cfg := newTrustTestRepo(t)
	root := resolvedRoot(t, dir)
	addTestRemote(t, dir, "remote", "add", "origin", "git@gitlab.example.com:team/proj.git")
	addTestRemote(t, dir, "remote", "set-url", "--add", "origin", "https://github.com/acme/widgets.git")
	writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_paths":["`+root+`","/srv/other"]}}`)
	for range 2 { // second call pins idempotency (no duplicate keys)
		if _, err := TrustCurrentRepo(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	us, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"gitlab.example.com/team/proj", "github.com/acme/widgets"}
	if !slices.Equal(us.Global.TrustedOrigins, wantKeys) {
		t.Fatalf("trusted_origins = %v, want ALL configured URL keys %v once", us.Global.TrustedOrigins, wantKeys)
	}
	if !slices.Equal(us.Global.TrustedPaths, []string{root, "/srv/other"}) {
		t.Fatalf("trusted_paths = %v, want hand-authored entries preserved", us.Global.TrustedPaths)
	}
	if !CheckpointEgressAllowed(t.Context()) {
		t.Fatal("the gate must open immediately after trusting")
	}

	id, err := RevokeCurrentRepo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(id.OriginKeys, wantKeys) {
		t.Fatalf("revoked identity = %+v, want the origin keys", id)
	}
	if CheckpointEgressAllowed(t.Context()) {
		t.Fatal("the revoked origin identity must hold")
	}
	addTestRemote(t, dir, "remote", "remove", "origin")
	if !CheckpointEgressAllowed(t.Context()) {
		t.Fatal("revoke must preserve a hand-authored path grant")
	}
}

func TestTrustGrantAndRevoke_TracksOnlyCLIOwnedIdentityHistory(t *testing.T) {
	dir, cfg := newTrustTestRepo(t)
	root := resolvedRoot(t, dir)
	writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_paths":["/srv/hand-authored"],"trusted_origins":["example.com/hand/authored"]}}`)

	// path
	if _, err := TrustCurrentRepo(t.Context()); err != nil {
		t.Fatal(err)
	}
	// origin A
	addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/one.git")
	if _, err := TrustCurrentRepo(t.Context()); err != nil {
		t.Fatal(err)
	}
	// path again
	addTestRemote(t, dir, "remote", "remove", "origin")
	if _, err := TrustCurrentRepo(t.Context()); err != nil {
		t.Fatal(err)
	}
	// origin B
	addTestRemote(t, dir, "remote", "add", "origin", "https://gitlab.example.com/team/two.git")
	if _, err := TrustCurrentRepo(t.Context()); err != nil {
		t.Fatal(err)
	}

	us, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(us.Global.TrustedPaths, root) || slices.Contains(us.Global.TrustedOrigins, "github.com/acme/one") {
		t.Fatalf("stale CLI grants survived transition: %+v", us.Global)
	}
	if !slices.Contains(us.Global.TrustedOrigins, "gitlab.example.com/team/two") {
		t.Fatalf("current CLI grant missing: %+v", us.Global)
	}

	if _, err := RevokeCurrentRepo(t.Context()); err != nil {
		t.Fatal(err)
	}
	us, err = LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(us.Global.TrustedPaths, []string{"/srv/hand-authored"}) ||
		!slices.Equal(us.Global.TrustedOrigins, []string{"example.com/hand/authored"}) {
		t.Fatalf("revoke changed hand-authored trust: %+v", us.Global)
	}
}

func TestCheckpointEgressAllowed_GlobalOffDeniesTrustAll(t *testing.T) {
	_, cfg := newTrustTestRepo(t)
	writeUserSettings(t, cfg, `{"global":{"enabled":false,"trust_all":true}}`)
	if CheckpointEgressAllowed(t.Context()) {
		t.Fatal("trust_all must not override a disabled global tier")
	}
}

func TestTrustAllRepos_UnconfiguredTierErrorsWithoutMaterializingGlobal(t *testing.T) {
	newTrustTestRepo(t)
	if err := TrustAllRepos(t.Context()); err == nil {
		t.Fatal("trust_all with no global block must error, not invent one")
	}
	us, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if us.Global != nil {
		t.Fatal("a failed trust_all write must not materialize the global block")
	}
}
