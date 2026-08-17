package settings

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// resolvedRoot returns dir in the symlink-resolved slash form trust path keys
// use (macOS reports temp dirs as /var/... while git resolves /private/var/...).
func resolvedRoot(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(resolved)
}

// newTrustTestRepo initializes an isolated repo, points the user settings dir
// at a fresh location, and enters the repo. Callers use t.Setenv/t.Chdir via
// this helper, so trust tests cannot be parallel.
func newTrustTestRepo(t *testing.T) (dir, cfg string) {
	t.Helper()
	dir = newGlobalTestRepo(t)
	cfg = t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	t.Chdir(dir)
	return dir, cfg
}

func writeRepoSettings(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".entire", name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// addLinkedWorktree seeds dir with a commit (worktree add needs a HEAD) and
// checks out a linked worktree at path, using the isolated-git-config runner.
func addLinkedWorktree(t *testing.T, dir, path string) {
	t.Helper()
	testutil.WriteFile(t, dir, "seed.txt", "seed")
	testutil.GitAdd(t, dir, "seed.txt")
	testutil.GitCommit(t, dir, "seed")
	addTestRemote(t, dir, "worktree", "add", path)
}

func TestRepoTrustIdentity(t *testing.T) {
	cases := []struct {
		name     string
		remotes  [][]string
		wantKeys []string
		wantPath bool
	}{
		{"https origin yields one key", [][]string{
			{"remote", "add", "origin", "https://github.com/Acme/Widgets.git"},
		}, []string{"github.com/acme/widgets"}, false},
		{"multi-URL origin yields a key per configured URL", [][]string{
			{"remote", "add", "origin", "git@gitlab.example.com:team/proj.git"},
			{"remote", "set-url", "--add", "origin", "https://github.com/acme/widgets.git"},
		}, []string{"gitlab.example.com/team/proj", "github.com/acme/widgets"}, false},
		{"entire mirror origin keys by the forge host", [][]string{
			{"remote", "add", "origin", "entire://aws-us-east-2.entire.io/gh/Acme/Widgets.git"},
		}, []string{"github.com/acme/widgets"}, false},
		{"insteadOf shorthand keys by the shorthand", [][]string{
			{"remote", "add", "origin", "gh:acme/widgets"},
		}, []string{"gh/acme/widgets"}, false},
		{"unnormalizable origin falls back to path identity", [][]string{
			{"remote", "add", "origin", "file:///srv/git/secret.git"},
		}, nil, true},
		// One unkeyable URL among normalizable ones flips the WHOLE identity
		// to path — a multi-URL push delivers refs to every URL, so an origin
		// identity with partial keys would fail open on the URL that cannot
		// carry consent.
		{"mixed normalizable and unnormalizable URLs fall back to path identity", [][]string{
			{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
			{"remote", "set-url", "--add", "origin", "file:///srv/git/secret.git"},
		}, nil, true},
		{"no origin falls back to path identity", nil, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := newTrustTestRepo(t)
			for _, args := range c.remotes {
				addTestRemote(t, dir, args...)
			}
			id, err := RepoTrustIdentity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if c.wantPath {
				if want := resolvedRoot(t, dir); id.Path != want {
					t.Errorf("Path = %q, want symlink-resolved worktree root %q", id.Path, want)
				}
				if len(id.OriginKeys) != 0 {
					t.Errorf("a path identity must not also carry origin keys, got %v", id.OriginKeys)
				}
				return
			}
			if !slices.Equal(id.OriginKeys, c.wantKeys) {
				t.Errorf("OriginKeys = %v, want %v", id.OriginKeys, c.wantKeys)
			}
			if id.Path != "" {
				t.Errorf("an origin identity must not also carry a path, got %q", id.Path)
			}
		})
	}

	t.Run("outside a git repository errors so callers fail closed", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		t.Chdir(t.TempDir())
		if _, err := RepoTrustIdentity(t.Context()); err == nil {
			t.Fatal("an unresolvable worktree must be an error, not a guessed identity")
		}
	})
}

func TestCheckpointEgressAllowed(t *testing.T) {
	t.Run("repo-level setup passes even with checkpoint_remote configured", func(t *testing.T) {
		// Pins the "checkpoint_remote is moot" invariant: a checkpoint_remote
		// can only live inside repo-level settings, and repo-level setup is
		// explicit consent that passes the gate before any remote-specific
		// logic could matter.
		dir, _ := newTrustTestRepo(t)
		writeRepoSettings(t, dir, "settings.json",
			`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/widgets"}}}`)
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("repo-level setup is explicit consent; the gate must pass")
		}
	})

	t.Run("repo-level settings.local.json alone passes", func(t *testing.T) {
		dir, _ := newTrustTestRepo(t)
		writeRepoSettings(t, dir, "settings.local.json",
			`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/widgets"}}}`)
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("`enable --local` setup is explicit consent; the gate must pass")
		}
	})

	t.Run("trust_all passes", func(t *testing.T) {
		_, cfg := newTrustTestRepo(t)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trust_all":true}}`)
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("trust_all must allow egress")
		}
	})

	t.Run("all origin keys trusted passes", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "git@gitlab.example.com:team/proj.git")
		addTestRemote(t, dir, "remote", "set-url", "--add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg,
			`{"global":{"enabled":true,"trusted_origins":["gitlab.example.com/team/proj","github.com/acme/widgets"]}}`)
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("an identity with every key trusted must pass")
		}
	})

	t.Run("a trusted subset of a multi-URL origin holds", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "git@gitlab.example.com:team/proj.git")
		addTestRemote(t, dir, "remote", "set-url", "--add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("consent must cover ALL configured origin URLs, not a subset")
		}
	})

	t.Run("a mixed origin holds even with its normalizable key trusted", func(t *testing.T) {
		// The mixed-origins rule: one unkeyable URL flips the identity to
		// path, so the trusted key for the normalizable URL must not open
		// egress — otherwise the file:/// destination syncs without consent.
		dir, cfg := newTrustTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		addTestRemote(t, dir, "remote", "set-url", "--add", "origin", "file:///srv/git/secret.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("an origin with an unkeyable URL is a path identity; its normalizable key must not grant egress")
		}
	})

	t.Run("trusted path identity passes", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_paths":["`+resolvedRoot(t, dir)+`"]}}`)
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("a no-origin repo whose root is trusted must pass")
		}
	})

	t.Run("untrusted path identity holds", func(t *testing.T) {
		_, cfg := newTrustTestRepo(t)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_paths":["/srv/other"]}}`)
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("a path identity not in trusted_paths must hold")
		}
	})

	t.Run("a trusted path does not cover an origin-keyed identity", func(t *testing.T) {
		// Identity exclusivity: a path-trusted repo that later gains a real
		// origin has a new egress destination and must re-ask — the lists are
		// consulted per identity side, never as a union.
		dir, cfg := newTrustTestRepo(t)
		root := resolvedRoot(t, dir)
		addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_paths":["`+root+`"]}}`)
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("path trust must not cover a repo whose identity is its origin keys")
		}
	})

	t.Run("a linked worktree needs its own trusted path", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		wtB := filepath.Join(t.TempDir(), "wtb")
		addLinkedWorktree(t, dir, wtB)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_paths":["`+resolvedRoot(t, dir)+`"]}}`)
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("the trusted worktree itself must pass")
		}
		t.Chdir(wtB)
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("trusting worktree A's root must not open egress from linked worktree B")
		}
	})

	t.Run("unreadable user settings hold", func(t *testing.T) {
		_, cfg := newTrustTestRepo(t)
		// settings.json as a DIRECTORY exercises the read-error branch portably.
		if err := os.MkdirAll(filepath.Join(cfg, UserSettingsFileName), 0o755); err != nil {
			t.Fatal(err)
		}
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("an unreadable trust store must hold egress (fail closed)")
		}
	})
}

func TestTrustCurrentRepo(t *testing.T) {
	t.Run("multi-URL origin writes every key and opens the gate", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "git@gitlab.example.com:team/proj.git")
		addTestRemote(t, dir, "remote", "set-url", "--add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		id, err := TrustCurrentRepo(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(id.OriginKeys) != 2 {
			t.Fatalf("expected a key per configured URL, got %v", id.OriginKeys)
		}
		us, err := LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"gitlab.example.com/team/proj", "github.com/acme/widgets"}
		if !slices.Equal(us.Global.TrustedOrigins, want) {
			t.Fatalf("trusted_origins = %v, want ALL configured URL keys %v", us.Global.TrustedOrigins, want)
		}
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("the gate must open immediately after trusting")
		}
	})

	t.Run("no-origin repo records path trust once", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		for range 2 { // second call pins idempotency (no duplicate entry)
			id, err := TrustCurrentRepo(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if want := resolvedRoot(t, dir); id.Path != want {
				t.Fatalf("identity Path = %q, want %q", id.Path, want)
			}
		}
		us, err := LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(us.Global.TrustedPaths, []string{resolvedRoot(t, dir)}) {
			t.Fatalf("trusting twice must record the root exactly once, got %v", us.Global.TrustedPaths)
		}
		if !CheckpointEgressAllowed(t.Context()) {
			t.Fatal("the gate must open immediately after trusting")
		}
	})

	t.Run("origin trust prunes a stale path entry for the root", func(t *testing.T) {
		// The anti-resurrection prune: without it, revoking the origin keys
		// later and then removing the origin would fall back to this stale
		// path entry and silently re-open egress.
		dir, cfg := newTrustTestRepo(t)
		root := resolvedRoot(t, dir)
		addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_paths":["`+root+`","/srv/other"]}}`)
		if _, err := TrustCurrentRepo(t.Context()); err != nil {
			t.Fatal(err)
		}
		us, err := LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(us.Global.TrustedPaths, []string{"/srv/other"}) {
			t.Fatalf("trusted_paths = %v, want this root pruned and other entries kept", us.Global.TrustedPaths)
		}
	})

	t.Run("trusting twice does not duplicate origin keys", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		for range 2 {
			if _, err := TrustCurrentRepo(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		us, err := LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(us.Global.TrustedOrigins, []string{"github.com/acme/widgets"}) {
			t.Fatalf("trusting twice must not duplicate keys, got %v", us.Global.TrustedOrigins)
		}
	})

	t.Run("unconfigured global tier errors without materializing the block", func(t *testing.T) {
		newTrustTestRepo(t)
		if _, err := TrustCurrentRepo(t.Context()); err == nil {
			t.Fatal("trust with no global block must error, not invent one")
		}
		us, err := LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if us.Global != nil {
			t.Fatal("a failed trust write must not materialize the global block (it would suppress the ask-once global-enable question)")
		}
	})
}

func TestRevokeCurrentRepo(t *testing.T) {
	t.Run("revoke then remove origin keeps the gate closed", func(t *testing.T) {
		// The resurrection regression: revoke must ALSO drop any trusted_paths
		// entry for this root, or removing the origin later flips the identity
		// to path and the leftover entry silently re-opens egress.
		dir, cfg := newTrustTestRepo(t)
		root := resolvedRoot(t, dir)
		addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg,
			`{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"],"trusted_paths":["`+root+`"]}}`)
		id, err := RevokeCurrentRepo(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// The returned identity names what was revoked (revoke messaging
		// depends on it).
		if !slices.Equal(id.OriginKeys, []string{"github.com/acme/widgets"}) {
			t.Fatalf("revoked identity = %+v, want the origin keys", id)
		}
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("the revoked origin identity must hold")
		}
		addTestRemote(t, dir, "remote", "remove", "origin")
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("after the origin is removed, a stale path entry must not resurrect trust")
		}
	})

	t.Run("path revoke removes only this root's entry", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		root := resolvedRoot(t, dir)
		writeUserSettings(t, cfg,
			`{"global":{"enabled":true,"trusted_origins":["github.com/other/repo"],"trusted_paths":["`+root+`","/srv/other"]}}`)
		if _, err := RevokeCurrentRepo(t.Context()); err != nil {
			t.Fatal(err)
		}
		us, err := LoadUserSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(us.Global.TrustedPaths, []string{"/srv/other"}) {
			t.Fatalf("trusted_paths = %v, want only this root's entry removed", us.Global.TrustedPaths)
		}
		if !slices.Equal(us.Global.TrustedOrigins, []string{"github.com/other/repo"}) {
			t.Fatalf("revoke must never touch other repos' origin keys, got %v", us.Global.TrustedOrigins)
		}
		if CheckpointEgressAllowed(t.Context()) {
			t.Fatal("the revoked path identity must hold")
		}
	})

	t.Run("revoking an untrusted repo is a no-op", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		for range 2 {
			if _, err := RevokeCurrentRepo(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestCurrentTrustSource(t *testing.T) {
	t.Run("trust_all", func(t *testing.T) {
		_, cfg := newTrustTestRepo(t)
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trust_all":true}}`)
		if got := CurrentTrustSource(t.Context()); got != TrustSourceAll {
			t.Fatalf("CurrentTrustSource = %q, want %q", got, TrustSourceAll)
		}
	})

	t.Run("repo", func(t *testing.T) {
		dir, cfg := newTrustTestRepo(t)
		addTestRemote(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		writeUserSettings(t, cfg, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
		if got := CurrentTrustSource(t.Context()); got != TrustSourceRepo {
			t.Fatalf("CurrentTrustSource = %q, want %q", got, TrustSourceRepo)
		}
	})

	t.Run("none", func(t *testing.T) {
		_, cfg := newTrustTestRepo(t)
		writeUserSettings(t, cfg, `{"global":{"enabled":true}}`)
		if got := CurrentTrustSource(t.Context()); got != TrustSourceNone {
			t.Fatalf("CurrentTrustSource = %q, want %q", got, TrustSourceNone)
		}
	})
}
