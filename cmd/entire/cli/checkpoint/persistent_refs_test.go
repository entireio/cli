package checkpoint

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
)

// Not parallel: uses t.Chdir() to exercise on-disk settings being ignored.
func TestResolveCommittedRefs(t *testing.T) {
	v1 := v1BranchRef()
	tests := []struct {
		name    string
		version string
		want    PersistentRefs
	}{
		{"unset", "", PersistentRefs{Primary: v1, Read: v1, Push: []plumbing.ReferenceName{v1}}},
		{"checkpoints version ignored", `"1.1"`, PersistentRefs{Primary: v1, Read: v1, Push: []plumbing.ReferenceName{v1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			writeSettings(t, dir, tt.version)
			assert.Equal(t, tt.want, ResolveRefs(context.Background()))
		})
	}
}

func TestDefaultV1Refs(t *testing.T) {
	t.Parallel()
	v1 := v1BranchRef()
	assert.Equal(t, PersistentRefs{
		Primary: v1,
		Read:    v1,
		Push:    []plumbing.ReferenceName{v1},
	}, DefaultV1Refs())
}

func TestCommittedRefs_PrimaryFetchableFromOrigin(t *testing.T) {
	t.Parallel()
	v1 := v1BranchRef()
	otherBranch := plumbing.NewBranchReferenceName("entire/checkpoints/other")
	tests := []struct {
		name string
		refs PersistentRefs
		want bool
	}{
		{"v1 in push", PersistentRefs{Primary: v1, Push: []plumbing.ReferenceName{v1}}, true},
		{"primary not in push", PersistentRefs{Primary: otherBranch, Push: []plumbing.ReferenceName{v1}}, false},
		{"empty push", PersistentRefs{Primary: v1, Push: nil}, false},
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
	v1 := v1BranchRef()
	otherBranch := plumbing.NewBranchReferenceName("entire/checkpoints/other")
	tests := []struct {
		name string
		refs PersistentRefs
		want bool
	}{
		{"v1-only: reads target fetchable primary", PersistentRefs{Primary: v1, Read: v1, Push: []plumbing.ReferenceName{v1}}, true},
		{"reads target primary but primary not pushed", PersistentRefs{Primary: v1, Read: v1, Push: nil}, false},
		{"reads target a different ref", PersistentRefs{Primary: v1, Read: otherBranch, Push: []plumbing.ReferenceName{v1}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.refs.ReadBootstrappableFromOrigin())
		})
	}
}
