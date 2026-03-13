package settings

import (
	"context"
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
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755))

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
