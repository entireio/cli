package gitremote

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInfo_Repository(t *testing.T) {
	t.Parallel()

	widgets := Repository{Forge: ForgeGitHub, Owner: "acme", Repo: "widgets"}
	tests := []struct {
		name   string
		url    string
		want   Repository
		wantOK bool
	}{
		{"ssh", "git@github.com:acme/widgets.git", widgets, true},
		{"https", "https://github.com/acme/widgets", widgets, true},
		{"entire mirror", "entire://aws-eu-central-1.entire.io/gh/acme/widgets", widgets, true},
		{"case folded", "https://github.com/Acme/Widgets.git", widgets, true},
		{"unknown host", "https://git.example.com/acme/widgets.git", Repository{}, false},
		{"entire-native", "entire://aws-eu-central-1.entire.io/et/acme/widgets", Repository{}, false},
		{"ssh host alias", "git@github-work:acme/widgets.git", Repository{}, false},
		{"git+ssh spelling", "git+ssh://git@github.com/acme/widgets.git", widgets, true},
		{"remote helper scheme on github.com", "bogus+ssh://github.com/acme/widgets", Repository{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := ParseURL(tt.url)
			require.NoError(t, err)
			got, ok := info.Repository()
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Not parallel: InstallFakeSSH and t.Setenv change process-global state.
func TestResolveRepository_SSHHostAlias(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	testutil.InstallFakeSSH(t, map[string]string{
		"github-work":  "GitHub.com",
		"gitlab-work":  "gitlab.example.com",
		"github-proxy": "github.com.evil.example",
	})
	ctx := context.Background()
	widgets := Repository{Forge: ForgeGitHub, Owner: "acme", Repo: "widgets"}

	t.Run("scp alias resolving to github.com", func(t *testing.T) {
		got, ok := ResolveRepository(ctx, "", nil, "git@github-work:Acme/widgets.git")
		require.True(t, ok)
		assert.Equal(t, widgets, got)
	})
	t.Run("ssh:// alias resolving to github.com", func(t *testing.T) {
		got, ok := ResolveRepository(ctx, "", nil, "ssh://git@github-work/acme/widgets.git")
		require.True(t, ok)
		assert.Equal(t, widgets, got)
	})
	for _, url := range []string{
		"git@gitlab-work:acme/widgets.git",
		"git@github-proxy:acme/widgets.git",
		"git@unaliased.example:acme/widgets.git",
		"https://github-work/acme/widgets.git", // only ssh reads ssh config
	} {
		t.Run("unresolved: "+url, func(t *testing.T) {
			_, ok := ResolveRepository(ctx, "", nil, url)
			assert.False(t, ok)
		})
	}

	t.Run("custom ssh command is not consulted", func(t *testing.T) {
		t.Setenv("GIT_SSH_COMMAND", "ssh -F /dev/null")
		_, ok := ResolveRepository(ctx, "", nil, "git@github-work:acme/widgets.git")
		assert.False(t, ok, "a custom ssh command may read another config; its hostname is unknowable")
	})

	t.Run("ssh missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, ok := ResolveRepository(ctx, "", nil, "git@github-work:acme/widgets.git")
		assert.False(t, ok)
	})
}
