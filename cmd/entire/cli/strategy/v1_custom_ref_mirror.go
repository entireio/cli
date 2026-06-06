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
// primary ref does not exist yet. Callers match this sentinel to distinguish
// "expected on first use" from a real read failure, and from a SetReference
// NotFound on the mirror itself.
var ErrPrimaryMetadataMissing = errors.New("primary metadata ref missing")

// AdvanceCommittedPrimary points refs.Primary at hash and best-effort-advances
// refs.Mirror. Mirror failure does not abort the primary operation.
func AdvanceCommittedPrimary(ctx context.Context, repo *git.Repository, refs checkpoint.CommittedRefs, hash plumbing.Hash) error {
	if err := repo.Storer.SetReference(plumbing.NewHashReference(refs.Primary, hash)); err != nil {
		return fmt.Errorf("set primary metadata ref %s to %s: %w", refs.Primary, hash, err)
	}
	setMirrorToBestEffort(ctx, repo, refs, hash)
	return nil
}

// AdvanceLocalRef points ref at hash, advancing the mirror best-effort when
// ref is refs.Primary.
func AdvanceLocalRef(ctx context.Context, repo *git.Repository, refs checkpoint.CommittedRefs, ref plumbing.ReferenceName, hash plumbing.Hash) error {
	if ref == refs.Primary {
		return AdvanceCommittedPrimary(ctx, repo, refs, hash)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(ref, hash)); err != nil {
		return fmt.Errorf("set ref %s to %s: %w", ref, hash, err)
	}
	return nil
}

// MirrorStatus classifies the committed-metadata mirror's relationship to the
// primary metadata ref.
type MirrorStatus int

const (
	MirrorNotConfigured  MirrorStatus = iota // topology has no mirror (v1 mode)
	MirrorOK                                 // mirror == primary tip
	MirrorNoMetadata                         // neither primary nor mirror exists yet (fresh repo)
	MirrorMissing                            // primary exists, mirror ref absent
	MirrorBehind                             // mirror is an ancestor of the primary tip
	MirrorDiverged                           // mirror is not an ancestor of the primary tip
	MirrorPrimaryMissing                     // mirror exists but the primary ref it mirrors is gone
)

// String returns the status label shared by doctor and bundle output.
func (s MirrorStatus) String() string {
	switch s {
	case MirrorNotConfigured:
		return "NOT CONFIGURED"
	case MirrorOK:
		return "OK"
	case MirrorNoMetadata:
		return "NO METADATA"
	case MirrorMissing:
		return "MISSING"
	case MirrorBehind:
		return "STALE"
	case MirrorDiverged:
		return "DIVERGED"
	case MirrorPrimaryMissing:
		return "V1 BRANCH MISSING"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(s))
	}
}

// MirrorDiagnosis is the mirror's state plus the resolved topology for repair.
type MirrorDiagnosis struct {
	Status  MirrorStatus
	Refs    checkpoint.CommittedRefs
	Primary plumbing.Hash // zero when the primary ref is missing
	Mirror  plumbing.Hash // zero when the mirror ref is missing
}

// DiagnoseCommittedMetadataMirror classifies the mirror ref against the local
// primary metadata ref. Read-only; repair is the caller's decision.
func DiagnoseCommittedMetadataMirror(ctx context.Context, repo *git.Repository) (MirrorDiagnosis, error) {
	refs := checkpoint.ResolveCommittedRefs(ctx)
	diag := MirrorDiagnosis{Status: MirrorNotConfigured, Refs: refs}
	if !refs.HasMirror() {
		return diag, nil
	}

	mirrorRef, err := repo.Reference(refs.Mirror, true)
	switch {
	case errors.Is(err, plumbing.ErrReferenceNotFound):
		// Mirror absent — diag.Mirror stays zero.
	case err != nil:
		return diag, fmt.Errorf("read mirror ref %s: %w", refs.Mirror, err)
	default:
		diag.Mirror = mirrorRef.Hash()
	}

	primaryRef, err := repo.Reference(refs.Primary, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			if diag.Mirror.IsZero() {
				diag.Status = MirrorNoMetadata // fresh repo: nothing committed yet
			} else {
				diag.Status = MirrorPrimaryMissing // mirror outlived its primary
			}
			return diag, nil
		}
		return diag, fmt.Errorf("read primary metadata ref %s: %w", refs.Primary, err)
	}
	diag.Primary = primaryRef.Hash()

	switch {
	case diag.Mirror.IsZero():
		diag.Status = MirrorMissing
		return diag, nil
	case diag.Mirror == diag.Primary:
		diag.Status = MirrorOK
		return diag, nil
	}

	mirrorCommit, err := repo.CommitObject(diag.Mirror)
	if err != nil {
		return diag, fmt.Errorf("read mirror commit %s: %w", diag.Mirror, err)
	}
	primaryCommit, err := repo.CommitObject(diag.Primary)
	if err != nil {
		return diag, fmt.Errorf("read primary commit %s: %w", diag.Primary, err)
	}
	isAncestor, err := mirrorCommit.IsAncestor(primaryCommit)
	if err != nil {
		return diag, fmt.Errorf("check mirror ancestry against %s: %w", refs.Primary, err)
	}
	if isAncestor {
		diag.Status = MirrorBehind
	} else {
		diag.Status = MirrorDiverged
	}
	return diag, nil
}

// MirrorCommittedMetadataRef points refs.Mirror at refs.Primary's tip. No-op
// when the topology has no mirror.
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
	return setMirrorTo(ctx, repo, refs, primaryRef.Hash())
}

func setMirrorTo(ctx context.Context, repo *git.Repository, refs checkpoint.CommittedRefs, hash plumbing.Hash) error {
	if err := repo.Storer.SetReference(plumbing.NewHashReference(refs.Mirror, hash)); err != nil {
		return fmt.Errorf("set mirror ref %s to %s: %w", refs.Mirror, hash, err)
	}
	logging.Debug(ctx, "committed-ref mirror updated",
		slog.String("ref", refs.Mirror.String()),
		slog.String("hash", hash.String()))
	return nil
}

func setMirrorToBestEffort(ctx context.Context, repo *git.Repository, refs checkpoint.CommittedRefs, hash plumbing.Hash) {
	if !refs.HasMirror() {
		return
	}
	// Detach cancellation so a parent deadline near expiry doesn't silently
	// drop the mirror write; the mirror itself is short.
	ctx = context.WithoutCancel(ctx)
	if err := setMirrorTo(ctx, repo, refs, hash); err != nil {
		logging.Warn(ctx, "committed-ref mirror failed",
			slog.String("ref", refs.Mirror.String()),
			slog.String("error", err.Error()))
	}
}

func mirrorCommittedMetadataRefBestEffort(ctx context.Context, repo *git.Repository, refs checkpoint.CommittedRefs) {
	if !refs.HasMirror() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := MirrorCommittedMetadataRef(ctx, repo, refs); err != nil {
		if errors.Is(err, ErrPrimaryMetadataMissing) {
			logging.Debug(ctx, "committed-ref mirror skipped: primary metadata ref unavailable",
				slog.String("error", err.Error()))
			return
		}
		logging.Warn(ctx, "committed-ref mirror failed",
			slog.String("ref", refs.Mirror.String()),
			slog.String("error", err.Error()))
	}
}
