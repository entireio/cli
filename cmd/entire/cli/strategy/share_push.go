package strategy

import (
	"context"

	"github.com/go-git/go-git/v6"
)

// ShareCheckpointResult reports what PushSharedCheckpoints delivered.
type ShareCheckpointResult struct {
	// Pushed counts the refs confirmed on the remote. Both backends report a
	// real count — git-refs the per-checkpoint refs it drained from the push
	// queue, git-branch the v1 ref — and both treat "nothing landed" as an
	// error, so a nil error with Pushed > 0 is the only shape that means the
	// checkpoint is shareable.
	Pushed int
	// PushDisabled reports push_sessions=false in settings: nothing left the
	// machine, and the checkpoint stays local. Distinct from an error, because
	// it is a configured choice rather than a failure.
	PushDisabled bool
}

// PushSharedCheckpoints delivers locally-written checkpoints to the checkpoint
// remote for `entire session share`, dispatching on the configured primary
// backend the same way the pre-push hook does.
//
// The backend fork lives here rather than in the CLI because pushSettings
// already resolves which primary is configured, and because the two paths are
// not interchangeable: git-refs pushes the per-checkpoint refs named in the
// push queue, git-branch pushes the single entire/checkpoints/v1 branch.
//
// Both halves use the strict entry point for their backend rather than the
// hook's. PrePush is fail-soft by contract — nil when the sync-remote gate
// skips delivery, nil when a ref is refused — and sharing must never print a
// resume command for a checkpoint that never reached the remote.
//
// Both run the OPF gate before anything is sent. Callers must have run
// EnsureRedactionConfigured first: unconfigured redaction reads as "OPF off"
// and would wave un-OPF'd checkpoint content through.
func (s *ManualCommitStrategy) PushSharedCheckpoints(ctx context.Context, repo *git.Repository, remote string) (ShareCheckpointResult, error) {
	ps := resolvePushSettings(ctx, remote)
	if ps.pushDisabled {
		return ShareCheckpointResult{PushDisabled: true}, nil
	}

	if ps.primaryIsRefs {
		pushed, pushDisabled, err := PushQueuedCheckpointRefs(ctx, repo, remote)
		return ShareCheckpointResult{Pushed: pushed, PushDisabled: pushDisabled}, err
	}

	pushed, pushDisabled, err := PushCheckpointBranch(ctx, remote)
	return ShareCheckpointResult{Pushed: pushed, PushDisabled: pushDisabled}, err
}
