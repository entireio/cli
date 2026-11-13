package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestIsPromptSyncDisabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want bool
	}{
		{name: "nil options", opts: nil, want: false},
		{name: "absent", opts: map[string]any{"push_sessions": false}, want: false},
		{name: "true", opts: map[string]any{SyncPromptsOptionKey: true}, want: false},
		{name: "false", opts: map[string]any{SyncPromptsOptionKey: false}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &EntireSettings{StrategyOptions: tt.opts}
			assert.Equal(t, tt.want, s.IsPromptSyncDisabled())
		})
	}
}

func writeSyncPromptsSettings(t *testing.T, base, local string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(base), 0o644))
	if local != "" {
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(local), 0o644))
	}
	return dir
}

func TestLoad_SyncPromptsLocalOverride(t *testing.T) {
	t.Parallel()
	dir := writeSyncPromptsSettings(t,
		`{"enabled": true}`,
		`{"strategy_options": {"sync_prompts": false}}`)

	s, err := Load(WithWorktreeRoot(context.Background(), dir))
	require.NoError(t, err)
	assert.True(t, s.IsPromptSyncDisabled(), "a personal settings.local.json must be able to opt out")
}

func TestLoad_SyncPromptsRejectsNonBoolean(t *testing.T) {
	t.Parallel()
	dir := writeSyncPromptsSettings(t,
		`{"enabled": true, "strategy_options": {"sync_prompts": "false"}}`, "")

	_, err := Load(WithWorktreeRoot(context.Background(), dir))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strategy_options.sync_prompts must be a boolean")
}

func TestLoadPromptSyncDisabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		base  string
		local string
		want  bool
	}{
		{name: "absent", base: `{"enabled": true}`, want: false},
		{name: "base false", base: `{"strategy_options": {"sync_prompts": false}}`, want: true},
		{name: "base true", base: `{"strategy_options": {"sync_prompts": true}}`, want: false},
		{name: "local overrides base", base: `{"strategy_options": {"sync_prompts": false}}`, local: `{"strategy_options": {"sync_prompts": true}}`, want: false},
		{name: "local opts out", base: `{"enabled": true}`, local: `{"strategy_options": {"sync_prompts": false}}`, want: true},
		{name: "local without key falls back to base", base: `{"strategy_options": {"sync_prompts": false}}`, local: `{"log_level": "debug"}`, want: true},
		{name: "unrelated invalid field ignored", base: `{"summary_generation": "bad", "strategy_options": {"sync_prompts": true}}`, want: false},
		{name: "non-boolean fails closed", base: `{"strategy_options": {"sync_prompts": "false"}}`, want: true},
		{name: "syntax error fails closed", base: `{"strategy_options": {`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := writeSyncPromptsSettings(t, tt.base, tt.local)
			got, err := LoadPromptSyncDisabled(WithWorktreeRoot(context.Background(), dir))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
