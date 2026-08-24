package gitremote

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestParseRemoteVerbose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		out   string
		names []string
		want  map[string]RemoteURLs
	}{
		{
			name:  "fetch and push of one remote",
			out:   "origin\thttps://example.com/a.git (fetch)\norigin\thttps://example.com/a.git (push)\n",
			names: []string{"origin"},
			want:  map[string]RemoteURLs{"origin": {Fetch: "https://example.com/a.git", Push: []string{"https://example.com/a.git"}}},
		},
		{
			// Several push URLs keep git's order: the first is where the
			// git-refs backend sends checkpoints, so order is load-bearing.
			name: "several push URLs keep their order",
			out: "two\thttps://example.com/a.git (fetch)\n" +
				"two\thttps://example.com/a.git (push)\n" +
				"two\thttps://example.com/b.git (push)\n",
			names: []string{"two"},
			want: map[string]RemoteURLs{"two": {
				Fetch: "https://example.com/a.git",
				Push:  []string{"https://example.com/a.git", "https://example.com/b.git"},
			}},
		},
		{
			// A remote configured with only a pushurl: git prints an empty
			// fetch field for it.
			name:  "remote with no fetch URL",
			out:   "pushonly\t\npushonly\thttps://example.com/p.git (push)\n",
			names: []string{"pushonly"},
			want:  map[string]RemoteURLs{"pushonly": {Push: []string{"https://example.com/p.git"}}},
		},
		{
			name:  "no remotes",
			out:   "",
			names: nil,
			want:  map[string]RemoteURLs{},
		},
		{
			name: "remotes are sorted, not in git's listing order",
			out: "zeta\thttps://example.com/z.git (fetch)\nzeta\thttps://example.com/z.git (push)\n" +
				"alpha\thttps://example.com/a.git (fetch)\nalpha\thttps://example.com/a.git (push)\n",
			names: []string{"alpha", "zeta"},
			want: map[string]RemoteURLs{
				"alpha": {Fetch: "https://example.com/a.git", Push: []string{"https://example.com/a.git"}},
				"zeta":  {Fetch: "https://example.com/z.git", Push: []string{"https://example.com/z.git"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snap := parseRemoteVerbose(tt.out)
			if !slices.Equal(snap.Names(), tt.names) {
				t.Errorf("Names() = %v, want %v", snap.Names(), tt.names)
			}
			for name, want := range tt.want {
				got, ok := snap.Get(name)
				if !ok {
					t.Errorf("Get(%q) reported the remote as absent", name)
					continue
				}
				if got.Fetch != want.Fetch {
					t.Errorf("Get(%q).Fetch = %q, want %q", name, got.Fetch, want.Fetch)
				}
				if !slices.Equal(got.Push, want.Push) {
					t.Errorf("Get(%q).Push = %v, want %v", name, got.Push, want.Push)
				}
			}
			if _, ok := snap.Get("absent"); ok {
				t.Error("Get() reported an unconfigured remote as present")
			}
		})
	}
}

// TestSnapshotMatchesGetURL pins the equivalence the Snapshot rests on: the
// values it reads from one `git remote -v` are the values GetRemoteURL and
// GetPushURLs would return per remote. Everything derived from a Snapshot
// instead of those calls is only correct while this holds.
//
// insteadOf is the case most likely to break it — a rewrite that one command
// applies and the other does not would silently change which repository a
// checkpoint URL is derived from. Not parallel: chdir, because GetPushURLs has
// no dir-aware variant.
func TestSnapshotMatchesGetURL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Chdir(dir)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "-q")
	// A fetch rewrite (applied by both commands) and a push rewrite (applied by
	// neither at report time) — both directions of the risk.
	run("config", "url.https://github.com/.insteadOf", "gh:")
	run("config", "url.ssh://git@github.com/.pushInsteadOf", "https://github.com/")
	run("remote", "add", "origin", "gh:me/app.git")
	run("remote", "add", "two", "https://github.com/me/two.git")
	run("remote", "set-url", "--add", "--push", "two", "https://github.com/me/two.git")
	run("remote", "set-url", "--add", "--push", "two", "https://github.com/me/two-mirror.git")

	snap, err := LoadSnapshot(ctx, dir)
	if err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}

	for _, name := range []string{"origin", "two"} {
		got, ok := snap.Get(name)
		if !ok {
			t.Fatalf("Snapshot is missing remote %q", name)
		}
		wantFetch, err := GetRemoteURL(ctx, name)
		if err != nil {
			t.Fatalf("GetRemoteURL(%q) error = %v", name, err)
		}
		if got.Fetch != wantFetch {
			t.Errorf("remote %q: snapshot fetch URL = %q, GetRemoteURL = %q", name, got.Fetch, wantFetch)
		}
		wantPush, err := GetPushURLs(ctx, name)
		if err != nil {
			t.Fatalf("GetPushURLs(%q) error = %v", name, err)
		}
		if !slices.Equal(got.Push, wantPush) {
			t.Errorf("remote %q: snapshot push URLs = %v, GetPushURLs = %v", name, got.Push, wantPush)
		}
	}

	// The rewrite really was in play, or this test proves nothing.
	if origin, _ := snap.Get("origin"); !strings.HasPrefix(origin.Fetch, "https://github.com/") {
		t.Errorf("insteadOf was not applied; fetch URL = %q, so the equivalence above went untested", origin.Fetch)
	}
	if snap.OriginURL() == "" {
		t.Error("OriginURL() is empty for a repo that has an origin")
	}
}
