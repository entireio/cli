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

	// localReadOnly pins reads to the local Read ref with no remote-tracking
	// tiers at all — set via PrimaryAsLocalRead. Guards (attach's
	// local-presence gates) that must answer "would the next LOCAL write see
	// this checkpoint?" need it: the ordinary read chain selects per
	// checkpoint across local and remote-tracking trees, so a checkpoint
	// present only on a remote would otherwise read as present and a write
	// based on the stale local branch would clobber the remote on push.
	localReadOnly bool
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

// PrimaryFetchableFromRemote reports whether Primary has a remote-tracking
// shadow on whichever remote carries checkpoint data (the caller supplies the
// remote-tracking ref).
func (r PersistentRefs) PrimaryFetchableFromRemote() bool {
	return r.Primary.IsBranch() && slices.Contains(r.Push, r.Primary)
}

// ReadBootstrappableFromRemote reports whether reads can be bootstrapped from
// whichever remote carries checkpoint data (the caller supplies the
// remote-tracking ref): true when reads target Primary and Primary is
// fetchable from a remote, and the topology was not pinned local-only.
func (r PersistentRefs) ReadBootstrappableFromRemote() bool {
	return !r.localReadOnly && r.Read == r.Primary && r.PrimaryFetchableFromRemote()
}

// PrimaryAsRead returns a copy of r with Read pinned to Primary.
func (r PersistentRefs) PrimaryAsRead() PersistentRefs {
	r.Read = r.Primary
	return r
}

// PrimaryAsLocalRead returns a copy of r with Read pinned to Primary and
// every remote read tier disabled — see the localReadOnly field for when a
// caller needs this instead of PrimaryAsRead.
func (r PersistentRefs) PrimaryAsLocalRead() PersistentRefs {
	r.Read = r.Primary
	r.localReadOnly = true
	return r
}

// ResolveRefs returns the committed metadata topology.
func ResolveRefs(_ context.Context) PersistentRefs {
	return DefaultV1Refs()
}
