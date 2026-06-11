package checkpoint

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
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
		{"unset", "", CommittedRefs{Primary: v1, Read: v1, Push: []plumbing.ReferenceName{v1}}},
		{"opted in", `"1.1"`, CommittedRefs{Primary: v1, Read: custom, Mirror: custom, Push: []plumbing.ReferenceName{v1, custom}}},
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
		{"nil", nil, CommittedRefs{Primary: v1, Read: v1, Push: []plumbing.ReferenceName{v1}}},
		{"empty", &settings.EntireSettings{}, CommittedRefs{Primary: v1, Read: v1, Push: []plumbing.ReferenceName{v1}}},
		{"opted in", version("1.1"), CommittedRefs{Primary: v1, Read: custom, Mirror: custom, Push: []plumbing.ReferenceName{v1, custom}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ResolveCommittedRefsFromSettings(tt.settings))
		})
	}
}

func TestDefaultV1Refs(t *testing.T) {
	t.Parallel()
	v1 := v1BranchRef()
	assert.Equal(t, CommittedRefs{
		Primary: v1,
		Read:    v1,
		Push:    []plumbing.ReferenceName{v1},
	}, DefaultV1Refs())
}

func TestCommittedRefs_PrimaryFetchableFromOrigin(t *testing.T) {
	t.Parallel()
	v1, custom := v1BranchRef(), customRef()
	tests := []struct {
		name string
		refs CommittedRefs
		want bool
	}{
		{"v1 in push", CommittedRefs{Primary: v1, Push: []plumbing.ReferenceName{v1}}, true},
		// Non-branch v1.1 alongside v1 in Push must not change v1's fetchability.
		{"v1 primary, v1.1 also pushed", CommittedRefs{Primary: v1, Push: []plumbing.ReferenceName{v1, custom}}, true},
		{"primary not in push", CommittedRefs{Primary: custom, Push: []plumbing.ReferenceName{v1}}, false},
		{"empty push", CommittedRefs{Primary: v1, Push: nil}, false},
		{"non-branch primary in push", CommittedRefs{Primary: custom, Push: []plumbing.ReferenceName{custom}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.refs.PrimaryFetchableFromOrigin())
		})
	}
}

func TestCommittedRefs_ReadBootstrappableFromOrigin(t *testing.T) {
	t.Parallel()
	v1, custom := v1BranchRef(), customRef()
	tests := []struct {
		name string
		refs CommittedRefs
		want bool
	}{
		{"v1-only: reads target fetchable primary", CommittedRefs{Primary: v1, Read: v1, Push: []plumbing.ReferenceName{v1}}, true},
		// v1.1 is pushed but is a non-branch ref (no origin shadow) and
		// Read != Primary, so reads still can't bootstrap from origin.
		{"v1.1 pushed but reads target mirror", CommittedRefs{Primary: v1, Read: custom, Mirror: custom, Push: []plumbing.ReferenceName{v1, custom}}, false},
		{"reads target primary but primary not pushed", CommittedRefs{Primary: v1, Read: v1, Push: nil}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.refs.ReadBootstrappableFromOrigin())
		})
	}
}
