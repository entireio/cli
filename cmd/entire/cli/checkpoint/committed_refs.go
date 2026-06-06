package checkpoint

import (
	"context"
	"slices"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// CommittedRefs is the committed-metadata ref topology for the active
// checkpoints_version. Resolve it once and consult it instead of re-deriving
// the topology at each read/write/mirror/push/fetch site.
type CommittedRefs struct {
	Primary plumbing.ReferenceName   // source of truth: written and pushed
	Read    plumbing.ReferenceName   // committed reads resolve against this
	Mirror  plumbing.ReferenceName   // advanced after Primary writes/fetches; "" disables
	Push    []plumbing.ReferenceName // refs PrePush advances on the user's remote
}

// HasMirror reports whether a mirror ref is configured.
func (r CommittedRefs) HasMirror() bool { return r.Mirror != "" }

// DefaultV1Refs returns the v1-only topology used by callers that have no
// resolver context (typically tests and the resolver's default branch).
func DefaultV1Refs() CommittedRefs {
	v1Branch := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	return CommittedRefs{
		Primary: v1Branch,
		Read:    v1Branch,
		Push:    []plumbing.ReferenceName{v1Branch},
	}
}

// PrimaryFetchableFromOrigin reports whether Primary has an origin-tracking
// shadow — i.e. whether bootstrap-from-origin paths can fetch it. Only branch
// refs in Push get a refs/remotes/origin/<name> shadow; non-branch refs are
// pushed without remote-tracking.
func (r CommittedRefs) PrimaryFetchableFromOrigin() bool {
	return r.Primary.IsBranch() && slices.Contains(r.Push, r.Primary)
}

// ReadBootstrappableFromOrigin reports whether reads can be bootstrapped from
// origin: true when reads target Primary and Primary is fetchable from origin.
func (r CommittedRefs) ReadBootstrappableFromOrigin() bool {
	return r.Read == r.Primary && r.PrimaryFetchableFromOrigin()
}

// PrimaryAsRead returns a copy of r with Read pinned to Primary.
func (r CommittedRefs) PrimaryAsRead() CommittedRefs {
	r.Read = r.Primary
	return r
}

// ResolveCommittedRefs returns the topology for the settings on disk, falling
// back to v1-branch-only when settings cannot be loaded.
func ResolveCommittedRefs(ctx context.Context) CommittedRefs {
	return committedRefsFor(settings.MirrorsToV1CustomRef(ctx))
}

// ResolveCommittedRefsFromSettings returns the topology for an already-loaded
// settings object, honoring an injected EntireSettings rather than disk (e.g.
// attach). Nil resolves to v1-branch-only.
func ResolveCommittedRefsFromSettings(s *settings.EntireSettings) CommittedRefs {
	return committedRefsFor(s != nil && s.MirrorsToV1CustomRef())
}

func committedRefsFor(mirrorEnabled bool) CommittedRefs {
	refs := DefaultV1Refs()
	if mirrorEnabled {
		custom := plumbing.ReferenceName(paths.MetadataRefName)
		refs.Read = custom
		refs.Mirror = custom
		// Push v1.1 alongside v1 so it exists on the remote before a later
		// milestone flips it to Primary.
		refs.Push = append(refs.Push, custom)
	}
	return refs
}
