package checkpointpolicy

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v6"
)

type UpdateOptions struct {
	CheckpointVersion string
	Force             bool
}

func Update(ctx context.Context, repo *git.Repository, target Target, opts UpdateOptions) (State, error) {
	baseline, err := updateBaseline(ctx, repo, target)
	if err != nil {
		return State{}, err
	}

	policy := baseline.Policy
	policy.CheckpointVersion = opts.CheckpointVersion

	if err := rejectDowngrades(baseline.Policy, policy, opts); err != nil {
		return State{}, err
	}
	if err := ValidatePolicy(policy); err != nil {
		return State{}, err
	}

	hash, err := WriteLocal(ctx, repo, baseline.Hash, policy)
	if err != nil {
		return State{}, err
	}
	return State{
		Policy:     policy,
		Source:     SourceLocal,
		Hash:       hash,
		RemoteHash: baseline.RemoteHash,
	}, nil
}

func updateBaseline(ctx context.Context, repo *git.Repository, target Target) (State, error) {
	local, err := ReadLocal(ctx, repo)
	if err != nil {
		return State{}, err
	}

	baseline, remoteFound, err := remoteBaseline(ctx, repo, target, local)
	if err != nil {
		return State{}, err
	}
	if !remoteFound || local.Hash == baseline.Hash {
		return baseline, nil
	}
	if local.Hash.IsZero() {
		return baseline, nil
	}
	localAncestor, err := isAncestorOf(ctx, repo, local.Hash, baseline.Hash)
	if err != nil {
		return State{}, err
	}
	if localAncestor {
		return baseline, nil
	}
	baselineAncestor, err := isAncestorOf(ctx, repo, baseline.Hash, local.Hash)
	if err != nil {
		return State{}, err
	}
	if baselineAncestor {
		local.RemoteHash = baseline.RemoteHash
		return local, nil
	}
	return State{}, fmt.Errorf("local checkpoint policy %s diverges from remote %s; push or reconcile the policy before updating", local.Hash, baseline.RemoteHash)
}

func rejectDowngrades(before, after Policy, opts UpdateOptions) error {
	if opts.Force {
		return nil
	}
	return rejectCheckpointVersionDowngrade(Normalize(before).CheckpointVersion, Normalize(after).CheckpointVersion)
}

func rejectCheckpointVersionDowngrade(beforeRaw, afterRaw string) error {
	before, err := resolveCheckpointVersionSelector(beforeRaw, supportedCheckpointVersions)
	if err != nil {
		return fmt.Errorf("checkpoint_version existing value %q: %w; pass --force to replace it", beforeRaw, err)
	}
	after, err := resolveCheckpointVersionSelector(afterRaw, supportedCheckpointVersions)
	if err != nil {
		return fmt.Errorf("checkpoint_version: %w", err)
	}
	if after.version.LessThan(before.version) {
		return fmt.Errorf("would downgrade checkpoint_version from %q to %q; pass --force to allow this", beforeRaw, afterRaw)
	}
	return nil
}
