package checkpoint

import (
	"context"
	"slices"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// PersistentRefs is the committed-metadata ref topology.
type PersistentRefs struct {
	Primary plumbing.ReferenceName
	Read    plumbing.ReferenceName
	Push    []plumbing.ReferenceName
}

// DefaultV1Refs returns the v1-only topology.
func DefaultV1Refs() PersistentRefs {
	v1Branch := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	return PersistentRefs{
		Primary: v1Branch,
		Read:    v1Branch,
		Push:    []plumbing.ReferenceName{v1Branch},
	}
}

// PrimaryFetchableFromOrigin reports whether Primary has an origin-tracking shadow.
func (r PersistentRefs) PrimaryFetchableFromOrigin() bool {
	return r.Primary.IsBranch() && slices.Contains(r.Push, r.Primary)
}

// ReadBootstrappableFromOrigin reports whether reads can be bootstrapped from
// origin: true when reads target Primary and Primary is fetchable from origin.
func (r PersistentRefs) ReadBootstrappableFromOrigin() bool {
	return r.Read == r.Primary && r.PrimaryFetchableFromOrigin()
}

// PrimaryAsRead returns a copy of r with Read pinned to Primary.
func (r PersistentRefs) PrimaryAsRead() PersistentRefs {
	r.Read = r.Primary
	return r
}

// ResolveRefs returns the committed metadata topology.
func ResolveRefs(_ context.Context) PersistentRefs {
	return DefaultV1Refs()
}
