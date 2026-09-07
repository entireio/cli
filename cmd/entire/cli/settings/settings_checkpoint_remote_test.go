package settings

import (
	"context"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCheckpointRemote_NotConfigured(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_EmptyStrategyOptions(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{},
	}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_StructuredGithub(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "org/checkpoints",
			},
		},
	}
	config := s.GetCheckpointRemote()
	require.NotNil(t, config)
	assert.Equal(t, "github", config.Provider)
	assert.Equal(t, "org/checkpoints", config.Repo)
}

func TestGetCheckpointRemote_MissingProvider(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": map[string]any{
				"repo": "org/checkpoints",
			},
		},
	}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_MissingRepo(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
			},
		},
	}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_RepoWithoutSlash(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "just-a-name",
			},
		},
	}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_StructuredGenericGit(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "git",
				"url":      "https://vc.hub.msg.team/org/checkpoints-repo.git",
			},
		},
	}
	config := s.GetCheckpointRemote()
	require.NotNil(t, config)
	assert.Equal(t, "git", config.Provider)
	assert.Equal(t, "https://vc.hub.msg.team/org/checkpoints-repo.git", config.URL)
	assert.Empty(t, config.Repo)
}

func TestGetCheckpointRemote_GenericGitSSHURL(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "git",
				"url":      "git@git.example.com:org/checkpoints-repo.git",
			},
		},
	}
	config := s.GetCheckpointRemote()
	require.NotNil(t, config)
	assert.Equal(t, "git@git.example.com:org/checkpoints-repo.git", config.URL)
}

func TestGetCheckpointRemote_GenericGitMissingURL(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "git",
			},
		},
	}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_GenericGitInvalidURL(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"not a url",
		"justahost", // no scheme, no owner/repo path
		"https://",  // no host, no path
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()
			s := &EntireSettings{
				StrategyOptions: map[string]any{
					"checkpoint_remote": map[string]any{
						"provider": "git",
						"url":      rawURL,
					},
				},
			}
			assert.Nil(t, s.GetCheckpointRemote(), "url %q should be rejected", rawURL)
		})
	}
}

func TestGetCheckpointRemote_JSONRoundTrip_GenericGit(t *testing.T) {
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	testutil.InitRepo(t, tmpDir)

	settingsJSON := `{
		"enabled": true,
		"strategy_options": {
			"checkpoint_remote": {
				"provider": "git",
				"url": "https://vc.hub.msg.team/org/checkpoints-repo.git"
			}
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsJSON), 0o644))

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	require.NoError(t, err)
	config := s.GetCheckpointRemote()
	require.NotNil(t, config)
	assert.Equal(t, "git", config.Provider)
	assert.Equal(t, "https://vc.hub.msg.team/org/checkpoints-repo.git", config.URL)
}

func TestCheckpointRemoteConfig_Target(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "org/checkpoints", (&CheckpointRemoteConfig{Provider: "github", Repo: "org/checkpoints"}).Target())
	assert.Equal(t, "https://git.example.com/org/checkpoints.git", (&CheckpointRemoteConfig{Provider: "git", URL: "https://git.example.com/org/checkpoints.git"}).Target())
}

func TestGetCheckpointRemote_LegacyStringIgnored(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": "git@github.com:org/checkpoints.git",
		},
	}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_WrongType(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": 42,
		},
	}
	assert.Nil(t, s.GetCheckpointRemote())
}

func TestGetCheckpointRemote_JSONRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	testutil.InitRepo(t, tmpDir)

	settingsJSON := `{
		"enabled": true,
		"strategy_options": {
			"checkpoint_remote": {
				"provider": "github",
				"repo": "org/checkpoints"
			}
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsJSON), 0o644))

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	require.NoError(t, err)
	config := s.GetCheckpointRemote()
	require.NotNil(t, config)
	assert.Equal(t, "github", config.Provider)
	assert.Equal(t, "org/checkpoints", config.Repo)
}

func TestGetCheckpointRemote_CoexistsWithPushSessions(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"push_sessions": false,
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "org/checkpoints",
			},
		},
	}
	config := s.GetCheckpointRemote()
	require.NotNil(t, config)
	assert.Equal(t, "org/checkpoints", config.Repo)
	assert.True(t, s.IsPushSessionsDisabled())
}

func TestCheckpointRemoteConfig_Owner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		repo string
		want string
	}{
		{"standard", "org/checkpoints", "org"},
		{"nested", "org/sub/repo", "org"},
		{"no slash", "just-name", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &CheckpointRemoteConfig{Provider: "github", Repo: tt.repo}
			assert.Equal(t, tt.want, c.Owner())
		})
	}
}

func TestHasCheckpointRemoteKey(t *testing.T) {
	t.Parallel()

	assert.False(t, (&EntireSettings{}).HasCheckpointRemoteKey(), "nil strategy options")
	assert.False(t, (&EntireSettings{StrategyOptions: map[string]any{}}).HasCheckpointRemoteKey(), "empty strategy options")
	assert.True(t, (&EntireSettings{StrategyOptions: map[string]any{
		"checkpoint_remote": map[string]any{"provider": "github", "repo": "org/repo"},
	}}).HasCheckpointRemoteKey(), "well-formed entry")
	// The reason this method exists: a malformed entry still counts as
	// present even though GetCheckpointRemote rejects it.
	assert.True(t, (&EntireSettings{StrategyOptions: map[string]any{
		"checkpoint_remote": map[string]any{"provider": "github"},
	}}).HasCheckpointRemoteKey(), "malformed entry still counts as present")
	assert.True(t, (&EntireSettings{StrategyOptions: map[string]any{
		"checkpoint_remote": nil,
	}}).HasCheckpointRemoteKey(), "null entry still counts as present")
}
