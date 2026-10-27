package agent_test

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/assert"
)

func TestHomeLayout_StoresUnderAndHolds(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "home")
	layout := agent.HomeLayout{Stores: []string{"sessions", "archived/sessions"}}
	live := filepath.Join(home, "sessions")
	archived := filepath.Join(home, "archived", "sessions")

	assert.Equal(t, []string{live, archived}, layout.StoresUnder(home))

	for _, tc := range []struct {
		path string
		want bool
	}{
		{path: live, want: true},
		{path: filepath.Join(live, "2026", "10", "05", "rollout.jsonl"), want: true},
		{path: filepath.Join(archived, "rollout.jsonl"), want: true},
		{path: filepath.Join(home, "sessions-other", "rollout.jsonl")},
		{path: filepath.Join(home, "config.toml")},
		{path: home},
	} {
		assert.Equal(t, tc.want, layout.Holds(home, tc.path), tc.path)
	}
}
