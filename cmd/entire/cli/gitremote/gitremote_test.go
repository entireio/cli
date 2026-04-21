package gitremote

import (
	"context"
	"os/exec"
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
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Owner: "org", Repo: "repo"},
		},
		{
			name:     "SSH SCP without .git",
			url:      "git@github.com:org/repo",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Owner: "org", Repo: "repo"},
		},
		{
			name:     "HTTPS format",
			url:      "https://github.com/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolHTTPS, Host: "github.com", Owner: "org", Repo: "repo"},
		},
		{
			name:     "HTTPS without .git",
			url:      "https://github.com/org/repo",
			wantInfo: &Info{Protocol: ProtocolHTTPS, Host: "github.com", Owner: "org", Repo: "repo"},
		},
		{
			name:     "SSH protocol format",
			url:      "ssh://git@github.com/org/repo.git",
			wantInfo: &Info{Protocol: ProtocolSSH, Host: "github.com", Owner: "org", Repo: "repo"},
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
			assert.Equal(t, tt.wantInfo.Owner, info.Owner)
			assert.Equal(t, tt.wantInfo.Repo, info.Repo)
		})
	}
}

func TestExtractOwnerFromRemoteURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{"SSH", "git@github.com:org/repo.git", "org"},
		{"HTTPS", "https://github.com/org/repo.git", "org"},
		{"invalid", "not-a-url", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ExtractOwnerFromRemoteURL(tt.url))
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
		wantHost  string
		wantOwner string
		wantRepo  string
	}{
		{
			name:      "SSH SCP format",
			originURL: "git@github.com:acme/my-app.git",
			wantHost:  "github.com",
			wantOwner: "acme",
			wantRepo:  "my-app",
		},
		{
			name:      "HTTPS format",
			originURL: "https://github.com/acme/my-app.git",
			wantHost:  "github.com",
			wantOwner: "acme",
			wantRepo:  "my-app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)

			cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", tt.originURL)
			cmd.Dir = repoDir
			cmd.Env = testutil.GitIsolatedEnv()
			require.NoError(t, cmd.Run())

			t.Chdir(repoDir)

			host, owner, repo, err := ResolveRemoteRepo(ctx, "origin")
			require.NoError(t, err)
			assert.Equal(t, tt.wantHost, host)
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
