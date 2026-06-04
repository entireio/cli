package checkpoint

import (
	"context"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// CommittedRefs is the committed-metadata ref topology for the active
// checkpoints_version. Resolve it once and consult it instead of re-deriving
// the topology at each read/write/mirror site.
//
// Today Primary is always the v1 branch. Opting into checkpoints_version "1.1"
// points Read and Mirror at the local-only v1.1 custom ref. A future rollout
// phase can flip the topology (v1.1 as Primary) by changing ResolveCommittedRefs.
type CommittedRefs struct {
	Primary plumbing.ReferenceName // source of truth: written first, pushed/fetched
	Read    plumbing.ReferenceName // committed reads resolve against this
	Mirror  plumbing.ReferenceName // advanced to Primary after v1 writes/fetches; empty = none (local-only, never pushed)
}

// HasMirror reports whether a mirror ref is configured.
func (r CommittedRefs) HasMirror() bool { return r.Mirror != "" }

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
	v1Branch := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	refs := CommittedRefs{Primary: v1Branch, Read: v1Branch}
	if mirrorEnabled {
		custom := plumbing.ReferenceName(paths.MetadataRefName)
		refs.Read = custom
		refs.Mirror = custom
	}
	return refs
}
