package gitremote

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		url      string
		wantInfo *Info
		wantErr  bool
	}{
		{
			name:     "SSH SCP format",
			url:      "git@github.com:org/repo.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "SSH SCP without .git",
			url:      "git@github.com:org/repo",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "HTTPS format",
			url:      "https://github.com/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolHTTPS, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "HTTPS without .git",
			url:      "https://github.com/org/repo",
			wantInfo: &Info{Protocol: ProtocolHTTPS, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "SSH protocol format",
			url:      "ssh://git@github.com/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "HTTPS with non-standard port",
			url:      "https://git.example.com:8443/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolHTTPS, Host: "git.example.com", Port: "8443", Owner: "org", Repo: "repo"},
		},
		{
			name:     "SSH protocol with non-standard port",
			url:      "ssh://git@git.example.com:2222/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "git.example.com", Port: "2222", Owner: "org", Repo: "repo"},
		},
		{
			name:     "HTTPS standard port not appended",
			url:      "https://github.com/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolHTTPS, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "entire:// gh prefix preserved as forge",
			url:      "entire://entirehost/gh/entireio/cli",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "gh", Owner: "entireio", Repo: "cli"},
		},
		{
			name:     "entire:// non-gh prefix preserved as forge",
			url:      "entire://abc/jk/myproject/repo",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "abc", Forge: "jk", Owner: "myproject", Repo: "repo"},
		},
		{
			name:     "entire:// regional host with gh forge",
			url:      "entire://aws-us-east-2.entire.io/gh/entirehq/entiredb",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "aws-us-east-2.entire.io", Forge: "gh", Owner: "entirehq", Repo: "entiredb"},
		},
		{
			name:     "entire:// with .git suffix",
			url:      "entire://entirehost/gh/entireio/cli.git",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "gh", Owner: "entireio", Repo: "cli"},
		},
		{
			name:     "unmapped host has empty forge",
			url:      "git@gitlab.com:org/repo.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "gitlab.com", Owner: "org", Repo: "repo"},
		},
		{
			// git dispatches git+ssh:// and ssh+git:// to ssh (verified on git
			// 2.54.0: GIT_SSH_COMMAND runs for both), so Protocol must report
			// the transport rather than the spelling in .git/config.
			name:     "git+ssh alias reports the ssh transport",
			url:      "git+ssh://git@github.com/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "ssh+git alias reports the ssh transport",
			url:      "ssh+git://git@git.example.com:2222/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "git.example.com", Port: "2222", Owner: "org", Repo: "repo"},
		},
		{
			// An unrecognized scheme is a remote helper to git (`git ls-remote
			// bogus+ssh://…` → "remote helper 'bogus+ssh' aborted session"),
			// so it stays itself and keeps failing closed downstream.
			name:     "unknown scheme alias is not normalized",
			url:      "bogus+ssh://git@github.com/org/repo.git",
			wantInfo: &Info{Protocol: "bogus+ssh", Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:    "empty string",
			url:     "",
			wantErr: true,
		},
		{
			name:    "no path",
			url:     "https://github.com",
			wantErr: true,
		},
		{
			// A crafted SCP-style origin must not smuggle a newline through
			// owner/repo into plain-text consumers like `entire agent-help`.
			name:    "SCP with embedded newline rejected",
			url:     "git@github.com:org/repo\nINJECTED",
			wantErr: true,
		},
		{
			name:    "SCP with embedded ANSI escape rejected",
			url:     "git@github.com:org/repo\x1b[31mEVIL",
			wantErr: true,
		},
		{
			name:    "SCP with embedded carriage return rejected",
			url:     "git@github.com:org/re\rpo",
			wantErr: true,
		},
		{
			// `.git` is decoration on a native path too: `/et/p/foo` and
			// `/et/p/foo.git` address one repository, spelled without it.
			name:     "entire:// native drops a .git suffix",
			url:      "entire://entirehost/et/audit1/foo.git",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "et", Owner: "audit1", Repo: "foo"},
		},
		{
			name:     "entire:// native without a suffix is unchanged",
			url:      "entire://entirehost/et/audit1/foo",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "et", Owner: "audit1", Repo: "foo"},
		},
		{
			name:     "entire:// native drops only one .git",
			url:      "entire://entirehost/et/audit1/foo.git.git",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "et", Owner: "audit1", Repo: "foo.git"},
		},
		{
			name:     "entire:// mirror drops only one .git",
			url:      "entire://entirehost/gh/entireio/cli.git.git",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "gh", Owner: "entireio", Repo: "cli.git"},
		},
		{
			// The SCP branch used to trim before calling splitOwnerRepo, which
			// trims too; one trim, one site.
			name:     "SCP drops only one .git",
			url:      "git@github.com:org/repo.git.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo.git"},
		},
		{
			// Case is not part of the suffix. Entire's data plane cuts it
			// with EqualFold, so a `.GIT` remote resolves over the wire;
			// a case-sensitive cut here would report a different repo name
			// than the server the very same URL reaches.
			name:     "entire:// native drops an uppercase .GIT",
			url:      "entire://entirehost/et/audit1/foo.GIT",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "et", Owner: "audit1", Repo: "foo"},
		},
		{
			name:     "entire:// mirror drops a mixed-case .Git",
			url:      "entire://entirehost/gh/entireio/cli.Git",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "gh", Owner: "entireio", Repo: "cli"},
		},
		{
			name:     "HTTPS drops an uppercase .GIT",
			url:      "https://github.com/org/repo.GIT",
			wantInfo: &Info{Protocol: ProtocolHTTPS, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			name:     "SCP drops a mixed-case .Git",
			url:      "git@github.com:org/repo.Git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Forge: "gh", Owner: "org", Repo: "repo"},
		},
		{
			// Still exactly one cut, whatever the cases involved.
			name:     "entire:// native drops only the last suffix, whatever its case",
			url:      "entire://entirehost/et/audit1/foo.GIT.git",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "et", Owner: "audit1", Repo: "foo.GIT"},
		},
		{
			// A longer dotted extension merely starts with the suffix; it
			// is part of the name and must survive in every case.
			name:     "entire:// native keeps a .gitignore name",
			url:      "entire://entirehost/et/audit1/foo.gitignore",
			wantInfo: &Info{Protocol: ProtocolEntire, Host: "entirehost", Forge: "et", Owner: "audit1", Repo: "foo.gitignore"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := ParseURL(tt.url)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantInfo.Protocol, info.Protocol)
			assert.Equal(t, tt.wantInfo.Host, info.Host)
			assert.Equal(t, tt.wantInfo.Port, info.Port)
			assert.Equal(t, tt.wantInfo.Forge, info.Forge)
			assert.Equal(t, tt.wantInfo.Owner, info.Owner)
			assert.Equal(t, tt.wantInfo.Repo, info.Repo)
		})
	}
}

func TestInfo_CanonicalHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{"direct https github", "https://github.com/org/repo.git", "github.com"},
		{"direct ssh github", "git@github.com:org/repo.git", "github.com"},
		{"entire mirror maps forge to host", "entire://aws-us-east-2.entire.io/gh/org/repo", "github.com"},
		{"unknown forge falls back to host", "git@ghe.corp.example.com:org/repo.git", "ghe.corp.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := ParseURL(tt.url)
			require.NoError(t, err)
			assert.Equal(t, tt.want, info.CanonicalHost())
		})
	}
}

// TestInfo_UpstreamHost pins the distinction CanonicalHost's fallback hides: on
// an entire:// remote, Host is a cluster, so "CanonicalHost returned something"
// is not evidence that a forge host is known. ParseURL preserves any non-empty
// forge token, so an unrecognized one is indistinguishable from a mirror until
// a caller asks this.
func TestInfo_UpstreamHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		url       string
		want      string
		wantKnown bool
	}{
		{name: "mirror maps to its forge host", url: "entire://aws-us-east-2.entire.io/gh/org/repo", want: "github.com", wantKnown: true},
		{name: "direct github maps too", url: "https://github.com/org/repo.git", want: "github.com", wantKnown: true},
		{name: "native has no upstream", url: "entire://aws-us-east-2.entire.io/et/proj/repo", wantKnown: false},
		{name: "unrecognized forge has no upstream", url: "entire://aws-us-east-2.entire.io/jk/proj/repo", wantKnown: false},
		{name: "unmapped direct host has no forge to map", url: "git@ghe.corp.example.com:org/repo.git", wantKnown: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := ParseURL(tt.url)
			require.NoError(t, err)
			got, known := info.UpstreamHost()
			assert.Equal(t, tt.wantKnown, known)
			assert.Equal(t, tt.want, got)
			if !known {
				assert.Equal(t, info.Host, info.CanonicalHost(),
					"CanonicalHost must fall back to Host, which is what makes the fallback unsafe to read as a forge host")
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "HTTPS no creds",
			url:  "https://github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "HTTPS with token",
			url:  "https://x-token:ghp_abc123@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "HTTPS with query token",
			url:  "https://github.com/org/repo.git?token=secret",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "SSH SCP-style returned as-is",
			url:  "git@github.com:org/repo.git",
			want: "git@github.com:org/repo.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, RedactURL(tt.url))
		})
	}
}

// Not parallel: uses t.Chdir()
func TestResolveRemoteRepo(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		originURL string
		wantForge string
		wantOwner string
		wantRepo  string
	}{
		{
			name:      "SSH SCP format",
			originURL: "git@github.com:acme/my-app.git",
			wantForge: "gh",
			wantOwner: "acme",
			wantRepo:  "my-app",
		},
		{
			name:      "HTTPS format",
			originURL: "https://github.com/acme/my-app.git",
			wantForge: "gh",
			wantOwner: "acme",
			wantRepo:  "my-app",
		},
		{
			name:      "entire:// with gh forge in path",
			originURL: "entire://aws-us-east-2.entire.io/gh/acme/my-app",
			wantForge: "gh",
			wantOwner: "acme",
			wantRepo:  "my-app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)

			testutil.RunGit(t, repoDir, "remote", "add", "origin", tt.originURL)

			t.Chdir(repoDir)

			forge, owner, repo, err := ResolveRemoteRepo(ctx, "origin")
			require.NoError(t, err)
			assert.Equal(t, tt.wantForge, forge)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

// Not parallel: uses t.Chdir()
func TestResolveRemoteRepo_MissingRemote(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	_, _, _, err := ResolveRemoteRepo(context.Background(), "origin")
	assert.Error(t, err)
}

// TestIsForgePathToken pins the forge-token set and, more importantly, the two
// deliberate boundaries around it. `et` belongs because it is a real entire://
// path segment — leaving it out is what made a native URL try to dial a cluster
// named "et" — while the legacy `git` prefix stays out because `entire repo
// clone` cannot act on a /git/ ref, and hostnames stay out because a caller
// parsing forge/owner/repo needs them to fail.
func TestIsForgePathToken(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"gh", "et"} {
		assert.True(t, IsForgePathToken(forge), "%q should be a path forge", forge)
		assert.NotEmpty(t, ForgePathLabels(forge), "%q should label its path segments", forge)
	}
	for _, forge := range []string{"git", "github.com", "gitlab.com", "gl", "GH", "", "/gh"} {
		assert.False(t, IsForgePathToken(forge), "%q should not be a path forge", forge)
		assert.Empty(t, ForgePathLabels(forge), "%q should have no labels", forge)
	}
}

// TestForgePathLabelsNameTheRightSegments guards the reason the labels exist:
// a mirror is addressed by owner and a native repo by project, so the two
// forges must not share a spelling.
func TestForgePathLabelsNameTheRightSegments(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "<owner>/<repo>", ForgePathLabels("gh"))
	assert.Equal(t, "<project>/<repo>", ForgePathLabels("et"))
}

// TestCanonicalHostIgnoresPathForges keeps the two forge maps apart: pathForges
// gained "et", but a native repo has no upstream forge host, so CanonicalHost
// must still fall back to the cluster host rather than invent one.
func TestCanonicalHostIgnoresPathForges(t *testing.T) {
	t.Parallel()
	native := &Info{Host: "aws-us-east-2.entire.io", Forge: "et", Owner: "paul", Repo: "dogbark"}
	assert.Equal(t, "aws-us-east-2.entire.io", native.CanonicalHost())
	mirror := &Info{Host: "aws-us-east-2.entire.io", Forge: "gh", Owner: "entireio", Repo: "cli"}
	assert.Equal(t, "github.com", mirror.CanonicalHost())
}

// TestParseURL_EncodedControlCharIsRejectedOnEveryForge pins the shared
// control-character chokepoint on the entire:// branch. A literal control
// character in the raw URL never gets this far: net/url.Parse scans the
// still-encoded string up front and rejects it before ParseURL sees a path at
// all. Percent-encoding is the bypass — url.Parse only inspects the raw bytes,
// so "%0A" sails through and only becomes a real newline once u.Path is
// decoded. splitOwnerRepo's guard is the only thing stopping that decoded
// escape from reaching owner/repo and, from there, plain-text consumers like
// `entire agent-help`.
func TestParseURL_EncodedControlCharIsRejectedOnEveryForge(t *testing.T) {
	t.Parallel()
	_, err := ParseURL("entire://entirehost/et/audit1/foo%0A.git")
	require.Error(t, err)
	require.Contains(t, err.Error(), "control character")
}

// TestParseURL_RejectsDotOnlySegments pins that stripping cannot MANUFACTURE a
// dot-only name: "..git" trims to "." and "...git" trims to "..", neither of
// which addresses a repo and both of which are path-traversal shapes if a
// caller ever joins them. The trim runs on every forge, so every forge can
// manufacture one. The /gh/ ref grammar guards this case already
// (parseMirrorCloneRef's dotOnlyRe); this is the URL half, which
// ResolveRemoteRepo actually uses.
func TestParseURL_RejectsDotOnlySegments(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{
		"entire://entirehost/gh/acme/..git",  // trims to "."
		"entire://entirehost/gh/acme/...git", // trims to ".."
		"entire://entirehost/gh/../app",      // typed, not manufactured
		"entire://entirehost/et/acme/..git",  // native trims too, so it can manufacture one
		"entire://entirehost/et/acme/..",     // typed on a native path
		// A trailing separator used to carry ".." past this guard: "../" is not
		// dot-only as spelled. Separators are stripped first, so it is again.
		"entire://entirehost/gh/acme/../",
		"entire://entirehost/gh/acme/..//",
		"entire://entirehost/gh/acme/..git/",
		"entire://entirehost/et/acme/../",
	} {
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()
			_, err := ParseURL(rawURL)
			require.Error(t, err)
		})
	}
}

// TestParseURL_TrailingSeparatorsAreStrippedBeforeTheSuffix pins the order
// git_url_basename uses: trailing separators, then one `.git`. A pasted clone
// URL routinely carries a trailing slash, and trimming in the other order would
// leave the suffix in the name — the exact spelling this CLI is supposed to
// treat as an alias.
func TestParseURL_TrailingSeparatorsAreStrippedBeforeTheSuffix(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		url      string
		wantRepo string
	}{
		{url: "entire://entirehost/et/audit1/foo.git/", wantRepo: "foo"},
		{url: "entire://entirehost/et/audit1/foo/", wantRepo: "foo"},
		{url: "entire://entirehost/et/audit1/foo.git//", wantRepo: "foo"},
		{url: "entire://entirehost/gh/entireio/cli.git/", wantRepo: "cli"},
		{url: "https://github.com/entireio/cli.git/", wantRepo: "cli"},
		{url: "git@github.com:entireio/cli.git/", wantRepo: "cli"},
		// Only the trailing run goes; an interior separator still splits.
		{url: "entire://entirehost/et/audit1/a/b.git/", wantRepo: "a/b"},
	} {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			info, err := ParseURL(tt.url)
			require.NoError(t, err)
			require.Equal(t, tt.wantRepo, info.Repo)
		})
	}
}
