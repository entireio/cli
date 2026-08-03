package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
)

func readLocalCheckpointPolicy(ctx context.Context, repo *git.Repository) (checkpointpolicy.Policy, error) {
	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		return checkpointpolicy.Policy{}, err
	}
	return state.Policy, nil
}

func checkpointPolicyAllowsGitHook(ctx context.Context, repo *git.Repository) bool {
	policy, err := readLocalCheckpointPolicy(ctx, repo)
	if err != nil {
		warnOrLogCheckpointPolicyReadFailure(ctx, err)
		return false
	}
	if checkpointpolicy.CanSatisfyPolicy(policy) {
		return true
	}
	warnOrLogCheckpointPolicyUpgrade(ctx, policy)
	return false
}

func syncCheckpointPolicyForPrePush(ctx context.Context, repo *git.Repository, ps pushSettings) {
	dir, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy pre-push: failed to resolve worktree root; allowing checkpoint push",
			slog.String("error", err.Error()),
		)
		return
	}
	target := checkpointpolicy.Target{Remote: ps.pushTarget(), Dir: dir}
	state, err := checkpointpolicy.Sync(ctx, repo, target)
	if err != nil {
		warnOrLogCheckpointPolicySyncFailure(ctx, err)
		return
	}
	if state.Source == checkpointpolicy.SourceLocalDiverged {
		warnOrLogCheckpointPolicyDiverged(ctx, state)
		return
	}
}

func warnOrLogCheckpointPolicyReadFailure(ctx context.Context, err error) {
	if interactive.CanPromptInteractively() {
		fmt.Fprintf(stderrWriter, "[entire] Could not read checkpoint policy; skipping Entire checkpoint work: %v\n", err)
		return
	}
	logging.Warn(ctx, "checkpoint policy read failed; skipping checkpoint work",
		slog.String("error", err.Error()),
	)
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
	logging.Warn(ctx, "checkpoint policy diverged; allowing checkpoint push",
		slog.String("local_hash", state.Hash.String()),
		slog.String("remote_hash", state.RemoteHash.String()),
	)
}

func warnIfCheckpointPolicyNeedsUpgrade(ctx context.Context, policy checkpointpolicy.Policy) {
	if checkpointpolicy.CanSatisfyPolicy(policy) {
		return
	}
	warnOrLogCheckpointPolicyUpgrade(ctx, policy)
}

func warnOrLogCheckpointPolicyUpgrade(ctx context.Context, policy checkpointpolicy.Policy) {
	warning := checkpointpolicy.UnsupportedPolicyMessage(policy, versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version))
	if interactive.CanPromptInteractively() {
		fmt.Fprint(stderrWriter, warning)
		return
	}
	logging.Warn(ctx, "checkpoint policy requires newer CLI",
		slog.String("checkpoint_version", policy.CheckpointVersion),
		slog.String("checkpoint_min_version", policy.CheckpointMinVersion),
	)
}
