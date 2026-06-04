package checkpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// What counts as opting in is the parsing concern of
// settings.MirrorsToV1CustomRef (covered by its own tests); the resolver only
// sees the resulting boolean. These cases cover the two topologies the
// resolver produces.
// Not parallel: uses t.Chdir() so settings.Load resolves the test repo.
func TestResolveCommittedRefs(t *testing.T) {
	v1, custom := v1BranchRef(), customRef()
	tests := []struct {
		name    string
		version string // checkpoints_version value; "" omits it
		want    CommittedRefs
	}{
		{"unset", "", CommittedRefs{Primary: v1, Read: v1}},
		{"opted in", `"1.1"`, CommittedRefs{Primary: v1, Read: custom, Mirror: custom}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			writeSettings(t, dir, tt.version)
			assert.Equal(t, tt.want, ResolveCommittedRefs(context.Background()))
		})
	}
}

func TestResolveCommittedRefsFromSettings(t *testing.T) {
	t.Parallel()
	v1, custom := v1BranchRef(), customRef()
	version := func(v string) *settings.EntireSettings {
		return &settings.EntireSettings{StrategyOptions: map[string]any{"checkpoints_version": v}}
	}
	tests := []struct {
		name     string
		settings *settings.EntireSettings
		want     CommittedRefs
	}{
		{"nil", nil, CommittedRefs{Primary: v1, Read: v1}},
		{"empty", &settings.EntireSettings{}, CommittedRefs{Primary: v1, Read: v1}},
		{"opted in", version("1.1"), CommittedRefs{Primary: v1, Read: custom, Mirror: custom}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ResolveCommittedRefsFromSettings(tt.settings))
		})
	}
}
