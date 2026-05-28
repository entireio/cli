package checkpoint

import (
	"context"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// RefDisplayName produces a short, log-friendly name for a metadata ref by
// stripping the refs/heads/ or refs/entire/ prefix. Use this for user-facing
// messages so legacy v1 ("entire/checkpoints/v1") and 1.1
// ("checkpoints/v1.1") both display naturally. Returns the input unchanged
// when neither prefix matches.
func RefDisplayName(ref plumbing.ReferenceName) string {
	s := string(ref)
	for _, prefix := range []string{"refs/heads/", "refs/entire/"} {
		if strings.HasPrefix(s, prefix) {
			return strings.TrimPrefix(s, prefix)
		}
	}
	return s
}

// MetadataRef returns the plumbing.ReferenceName for v1 metadata storage,
// resolved from settings.
//
// Legacy v1 repos return the branch ref (refs/heads/entire/checkpoints/v1).
// checkpoints_version 1.1 repos return the custom ref
// (refs/entire/checkpoints/v1.1). Falls back to the legacy branch ref when
// settings cannot be loaded.
//
// 1.1 repos start with an empty custom ref — prior history on the legacy
// branch is NOT automatically reachable from the new ref. Users who want
// the old checkpoints under 1.1 can run, once:
//
//	git update-ref refs/entire/checkpoints/v1.1 refs/heads/entire/checkpoints/v1
func MetadataRef(ctx context.Context) plumbing.ReferenceName {
	if settings.UsesCustomMetadataRef(ctx) {
		return plumbing.ReferenceName(paths.MetadataRefName)
	}
	return plumbing.NewBranchReferenceName(paths.MetadataBranchName)
}

// MetadataTrackingRef returns the plumbing.ReferenceName for the origin
// remote-tracking counterpart of the v1 metadata ref. Use this for code
// paths that are explicitly about the origin tracking ref (doctor,
// resume promotion, EnsureMetadataBranch's origin check, fetch from
// origin). For the push hook (which can push to any remote name), use
// MetadataTrackingRefForRemote with the actual push remote.
//
// For legacy v1: refs/remotes/origin/entire/checkpoints/v1.
// For 1.1: refs/entire/remotes/origin/checkpoints/v1.1.
//
// The 1.1 tracking ref is intentionally NOT the same as the local ref —
// mapping a fetched ref to itself would clobber local writes on every
// fetch. The separate namespace preserves local commits the way the
// standard refs/heads/* ↔ refs/remotes/origin/* mapping does.
func MetadataTrackingRef(ctx context.Context) plumbing.ReferenceName {
	return MetadataTrackingRefForRemote(ctx, "origin")
}

// MetadataTrackingRefForRemote returns the local remote-tracking ref for
// the v1 metadata ref under a specific remote name. The push hook uses
// this so non-origin pushes (e.g. `git push upstream`) compare against
// the right tracking ref, not always origin's.
//
// For legacy v1: refs/remotes/<remoteName>/entire/checkpoints/v1.
// For 1.1: refs/entire/remotes/<remoteName>/checkpoints/v1.1.
//
// Note: for 1.1, only the origin refspec is installed by `entire enable`
// (see installMetadataRefspec). Tracking refs for non-origin remotes will
// not be populated by `git fetch` until a user installs the equivalent
// refspec by hand. The push hook treats a missing tracking ref as
// "needs push" — safe but suboptimal.
func MetadataTrackingRefForRemote(ctx context.Context, remoteName string) plumbing.ReferenceName {
	if settings.UsesCustomMetadataRef(ctx) {
		return plumbing.ReferenceName(paths.BuildMetadataTrackingRef(remoteName))
	}
	return plumbing.NewRemoteReferenceName(remoteName, paths.MetadataBranchName)
}
