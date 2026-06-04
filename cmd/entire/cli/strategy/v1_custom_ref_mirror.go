package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ErrPrimaryMetadataMissing is returned by MirrorCommittedMetadataRef when the
// primary ref does not exist yet. Callers can match this sentinel to
// distinguish "expected on first use" from a real read failure, and from a
// SetReference NotFound on the mirror itself.
var ErrPrimaryMetadataMissing = errors.New("primary metadata ref missing")

// MirrorCommittedMetadataRef points the committed-metadata mirror at the primary
// ref's tip. No-op when the topology has no mirror.
func MirrorCommittedMetadataRef(ctx context.Context, repo *git.Repository, refs checkpoint.CommittedRefs) error {
	if !refs.HasMirror() {
		return nil
	}

	primaryRef, err := repo.Reference(refs.Primary, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("primary metadata ref %s missing: %w", refs.Primary, ErrPrimaryMetadataMissing)
		}
		return fmt.Errorf("read primary metadata ref %s: %w", refs.Primary, err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refs.Mirror, primaryRef.Hash())); err != nil {
		return fmt.Errorf("set mirror ref %s to %s: %w", refs.Mirror, primaryRef.Hash(), err)
	}

	logging.Debug(ctx, "committed-ref mirror updated",
		slog.String("ref", refs.Mirror.String()),
		slog.String("hash", primaryRef.Hash().String()))
	return nil
}

// MirrorCommittedMetadataRefBestEffort mirrors committed metadata for callers
// where mirror failure must not affect the primary operation.
//
// The mirror runs under context.WithoutCancel so a parent deadline that is
// already near-expired (e.g. the 2-minute fetch budget) cannot silently fail
// settings.Load and skip the mirror with no log. Trace/value context is
// preserved; only cancellation is detached. The mirror itself is short.
func MirrorCommittedMetadataRefBestEffort(ctx context.Context, repo *git.Repository) {
	ctx = context.WithoutCancel(ctx)

	refs := checkpoint.ResolveCommittedRefs(ctx)
	if !refs.HasMirror() {
		return
	}

	if err := MirrorCommittedMetadataRef(ctx, repo, refs); err != nil {
		if errors.Is(err, ErrPrimaryMetadataMissing) {
			// No primary metadata ref yet — nothing to mirror. Expected on first use.
			logging.Debug(ctx, "committed-ref mirror skipped: primary metadata ref unavailable",
				slog.String("error", err.Error()))
			return
		}
		logging.Warn(ctx, "committed-ref mirror failed",
			slog.String("ref", refs.Mirror.String()),
			slog.String("error", err.Error()))
		return
	}
}
