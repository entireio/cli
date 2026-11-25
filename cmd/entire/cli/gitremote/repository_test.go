package gitremote

import (
	"context"
	"os"
	"path/filepath"
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

func TestResolveSSH(t *testing.T) {
	t.Parallel()
	// configEnv returns a hermetic env whose global gitconfig holds content.
	configEnv := func(t *testing.T, content string, extra ...string) []string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "gitconfig")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		env := []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + path,
		}
		return append(env, extra...)
	}
	tests := []struct {
		name   string
		config string
		extra  []string
		want   SSHInvocation
		wantOK bool
	}{
		{name: "nothing set", want: SSHInvocation{Command: "ssh"}, wantOK: true},
		{
			name:   "GIT_SSH_COMMAND beats core.sshCommand",
			config: "[core]\n\tsshCommand = from-config\n",
			extra:  []string{"GIT_SSH_COMMAND=from-env"},
			want:   SSHInvocation{Command: "from-env"},
			wantOK: true,
		},
		{
			name:   "core.sshCommand beats GIT_SSH, last value wins",
			config: "[core]\n\tsshCommand = first\n\tsshCommand = second\n",
			extra:  []string{"GIT_SSH=/usr/bin/plink"},
			want:   SSHInvocation{Command: "second"},
			wantOK: true,
		},
		{
			name:   "GIT_SSH is quoted and kept as the program",
			extra:  []string{`GIT_SSH=C:\PuTTY\plink.exe`},
			want:   SSHInvocation{Command: `'C:\PuTTY\plink.exe'`, Program: `C:\PuTTY\plink.exe`},
			wantOK: true,
		},
		{
			name:   "GIT_SSH_VARIANT beats ssh.variant",
			config: "[ssh]\n\tvariant = simple\n",
			extra:  []string{"GIT_SSH_VARIANT=plink"},
			want:   SSHInvocation{Command: "ssh", Variant: "plink", HasVariant: true},
			wantOK: true,
		},
		{
			name:   "ssh.variant from config",
			config: "[ssh]\n\tvariant = putty\n",
			want:   SSHInvocation{Command: "ssh", Variant: "putty", HasVariant: true},
			wantOK: true,
		},
		{
			name:   "unreadable config is not ok",
			config: "[core\n\tsshCommand = my-ssh\n",
			want:   SSHInvocation{Command: "ssh"},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ResolveSSH(context.Background(), t.TempDir(), configEnv(t, tt.config, tt.extra...))
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
