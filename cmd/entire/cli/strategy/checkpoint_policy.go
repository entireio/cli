package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
)

func committedCheckpointWriteAllowed(ctx context.Context, repo *git.Repository) bool {
	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy read failed; allowing checkpoint write",
			slog.String("error", err.Error()),
		)
		return true
	}
	if !checkpointpolicy.UnsupportedWrite(state.Policy) {
		return true
	}
	warnOrLogUnsupportedCheckpointWrite(ctx, state.Policy)
	return false
}

func syncCheckpointPolicyForPrePush(ctx context.Context) bool {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy pre-push: failed to open repository; allowing checkpoint push",
			slog.String("error", err.Error()),
		)
		return true
	}
	defer repo.Close()

	target, err := checkpointpolicy.ResolveTarget(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy pre-push: failed to resolve policy remote; allowing checkpoint push",
			slog.String("error", err.Error()),
		)
		return true
	}
	state, err := checkpointpolicy.Sync(ctx, repo, target)
	if err != nil {
		warnOrLogCheckpointPolicySyncFailure(ctx, err)
		return true
	}
	if state.Source == checkpointpolicy.SourceLocalDiverged {
		warnOrLogCheckpointPolicyDiverged(ctx, state)
		return false
	}
	if !checkpointpolicy.UnsupportedWrite(state.Policy) {
		return true
	}
	warnOrLogUnsupportedCheckpointWrite(ctx, state.Policy)
	return false
}

func warnOrLogCheckpointPolicySyncFailure(ctx context.Context, err error) {
	if interactive.CanPromptInteractively() {
		fmt.Fprintf(stderrWriter, "[entire] Could not refresh checkpoint policy: %v\n", err)
		return
	}
	logging.Warn(ctx, "checkpoint policy sync failed",
		slog.String("error", err.Error()),
	)
}

func warnOrLogCheckpointPolicyDiverged(ctx context.Context, state checkpointpolicy.State) {
	if interactive.CanPromptInteractively() {
		fmt.Fprintf(
			stderrWriter,
			"[entire] Could not reconcile checkpoint policy: local checkpoint policy %s diverges from remote %s\n",
			state.Hash,
			state.RemoteHash,
		)
		return
	}
	logging.Warn(ctx, "checkpoint policy diverged; skipping checkpoint push",
		slog.String("local_hash", state.Hash.String()),
		slog.String("remote_hash", state.RemoteHash.String()),
	)
}

func warnOrLogUnsupportedCheckpointWrite(ctx context.Context, policy checkpointpolicy.Policy) {
	warning := checkpointpolicy.UpgradeWarning(versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version))
	if interactive.CanPromptInteractively() {
		fmt.Fprint(stderrWriter, warning)
		return
	}
	logging.Warn(ctx, "checkpoint write skipped by policy",
		slog.String("checkpoint_version", policy.CheckpointVersion),
		slog.String("checkpoint_min_version", policy.CheckpointMinVersion),
	)
}
