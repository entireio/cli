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
