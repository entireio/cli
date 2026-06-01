package strategy

import (
	"context"
	"log/slog"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// mirrorMetadataToV1CustomRef advances the v1 custom ref
// (refs/entire/checkpoints/v1.1) to the v1 metadata branch's current commit
// when checkpoints_version "1.1" is opted in.
//
// v1 stays the source of truth; the v1 custom ref is a local-only mirror
// sharing v1's exact commit — nothing reads it and it is never pushed. Call
// only after a successful v1 committed write; a mirror failure must not affect
// that write, so problems are logged, not returned.
func mirrorMetadataToV1CustomRef(ctx context.Context, repo *git.Repository) {
	if !settings.MirrorsToV1CustomRef(ctx) {
		return
	}

	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		// No v1 metadata branch yet — nothing to mirror. Expected on first use.
		logging.Debug(ctx, "v1 custom-ref mirror skipped: v1 metadata branch unavailable",
			slog.String("error", err.Error()))
		return
	}

	v1CustomRef := plumbing.ReferenceName(paths.MetadataRefName)
	if err := SafelyAdvanceLocalRef(ctx, repo, v1CustomRef, v1Ref.Hash()); err != nil {
		logging.Warn(ctx, "v1 custom-ref mirror failed",
			slog.String("ref", paths.MetadataRefName),
			slog.String("error", err.Error()))
		return
	}

	logging.Debug(ctx, "v1 custom-ref mirror updated",
		slog.String("ref", paths.MetadataRefName),
		slog.String("hash", v1Ref.Hash().String()))
}
