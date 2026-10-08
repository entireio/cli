package agent_test

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/assert"
)

func TestHomeLayout_StoresUnderAndStoreContaining(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "home")
	layout := agent.HomeLayout{Stores: []string{"sessions", "archived/sessions"}}
	live := filepath.Join(home, "sessions")
	archived := filepath.Join(home, "archived", "sessions")

	assert.Equal(t, []string{live, archived}, layout.StoresUnder(home))

	for _, tc := range []struct {
		path      string
		wantStore string
	}{
		{path: live, wantStore: live},
		{path: filepath.Join(live, "2026", "10", "05", "rollout.jsonl"), wantStore: live},
		{path: filepath.Join(archived, "rollout.jsonl"), wantStore: archived},
		{path: filepath.Join(home, "sessions-other", "rollout.jsonl")},
		{path: filepath.Join(home, "config.toml")},
		{path: home},
	} {
		store, ok := layout.StoreContaining(home, tc.path)
		assert.Equal(t, tc.wantStore != "", ok, tc.path)
		assert.Equal(t, tc.wantStore, store, tc.path)
	}
}
