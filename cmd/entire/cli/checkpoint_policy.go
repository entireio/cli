package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

type checkpointPolicyOptions struct {
	version    string
	minVersion string
	force      bool
}

const (
	checkpointVersionFlag    = "checkpoint-version"
	checkpointMinVersionFlag = "checkpoint-min-version"
)

func newCheckpointPolicyCmd() *cobra.Command {
	var opts checkpointPolicyOptions
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Inspect and update checkpoint policy",
		Long: `Inspect and update checkpoint policy.

checkpoint_version is a checkpoint-data write guard.
If no policy is configured, Entire uses the CLI default.
If another client configures a checkpoint_version this CLI cannot write,
commands that create checkpoint data fail until the CLI is upgraded. Other commands warn and
continue. Set checkpoint_version to "" to inherit the CLI default.

checkpoint_min_version is an upgrade nudge and checkpoint-data write guard.
Clients that cannot read that version warn users to upgrade. Commands that
create checkpoint data fail until the CLI is upgraded. Other commands warn
and continue. Set checkpoint_min_version to "" to inherit the CLI default.

Unsetting a field still uses the normal downgrade guard. If inheriting the
default would lower the field's effective version, pass --force to allow it.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCheckpointPolicy(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.version, checkpointVersionFlag, "", `Set checkpoint_version. Use "" to inherit the CLI default; --force may be required`)
	cmd.Flags().StringVar(&opts.minVersion, checkpointMinVersionFlag, "", `Set checkpoint_min_version. Use "" to inherit the CLI default; --force may be required`)
	cmd.Flags().BoolVar(&opts.force, "force", false, "Allow checkpoint policy version downgrades")
	return cmd
}

func runCheckpointPolicy(cmd *cobra.Command, opts checkpointPolicyOptions) error {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return NewSilentError(err)
	}
	repo, err := gitrepo.OpenCurrent(ctx)
	if err != nil {
		return checkpointPolicyError("open repository", err)
	}
	defer repo.Close()

	readTargets, pushTarget, err := resolveCheckpointPolicyTargets(ctx)
	if err != nil {
		return checkpointPolicyError("resolve checkpoint policy remote", err)
	}

	var state checkpointpolicy.State
	checkpointVersionSet := cmd.Flags().Changed(checkpointVersionFlag)
	checkpointMinVersionSet := cmd.Flags().Changed(checkpointMinVersionFlag)
	if hasCheckpointPolicyUpdate(checkpointVersionSet, checkpointMinVersionSet) {
		if pushTarget == nil {
			return checkpointPolicyError("resolve checkpoint policy push remote",
				errors.New("cannot resolve the checkpoint sync remote to push the policy to"))
		}
		state, err = checkpointpolicy.Update(ctx, repo, *pushTarget, checkpointpolicy.UpdateOptions{
			CheckpointVersion:       opts.version,
			CheckpointVersionSet:    checkpointVersionSet,
			CheckpointMinVersion:    opts.minVersion,
			CheckpointMinVersionSet: checkpointMinVersionSet,
			Force:                   opts.force,
		})
		if err != nil {
			return checkpointPolicyError("update checkpoint policy", err)
		}
		if err := checkpointpolicy.Push(ctx, *pushTarget); err != nil {
			return checkpointPolicyError("push checkpoint policy", err)
		}
		state.Source = checkpointpolicy.SourceRemote
	} else {
		state, err = checkpointpolicy.SyncFrom(ctx, repo, readTargets)
		if err != nil {
			return checkpointPolicyError("sync checkpoint policy", err)
		}
	}

	effectivePolicy := checkpointpolicy.Normalize(state.Policy)
	fmt.Fprintf(cmd.OutOrStdout(), "checkpoint_version: %s\n", formatCheckpointVersionPolicyValue(state.Policy.CheckpointVersion, effectivePolicy.CheckpointVersion))
	fmt.Fprintf(cmd.OutOrStdout(), "checkpoint_min_version: %s\n", formatCheckpointPolicyValue(state.Policy.CheckpointMinVersion, effectivePolicy.CheckpointMinVersion))
	fmt.Fprintf(cmd.OutOrStdout(), "source: %s\n", state.Source)
	return nil
}

func hasCheckpointPolicyUpdate(checkpointVersionSet, checkpointMinVersionSet bool) bool {
	return checkpointVersionSet || checkpointMinVersionSet
}

// resolveCheckpointPolicyTargets splits the checkpoint policy remotes into the
// READ candidates (iterated in order: elected sync remote, then the legacy
// origin tier — the legacy tier is read-only and marked SkipLocalUpdate so its
// baseline never advances the local policy ref) and the single PUSH target
// (the elected sync remote only; nil when the election fails or elects
// nothing). A configured checkpoint_remote is a dedicated store: it is both
// the sole read target and the push target, unchanged from the historical
// behavior. The elected remote is resolved explicitly rather than inferred
// from the read chain's first entry, which can be the fail-open origin.
func resolveCheckpointPolicyTargets(ctx context.Context) ([]checkpointpolicy.Target, *checkpointpolicy.Target, error) {
	if remote.Configured(ctx) {
		target, err := checkpointpolicy.ResolveTarget(ctx)
		if err != nil {
			return nil, nil, err
		}
		return []checkpointpolicy.Target{target}, &target, nil
	}

	dir, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve worktree root: %w", err)
	}

	// One resolver call yields both the chain and the election, so the
	// read-only marking below cannot disagree with the push target.
	resolution := strategy.CheckpointReadRemotesWithElection(ctx)
	if len(resolution.Candidates) == 0 {
		if resolution.ElectionErr != nil {
			return nil, nil, fmt.Errorf("no checkpoint policy remote available: %w", resolution.ElectionErr)
		}
		return nil, nil, errors.New("no git remotes configured to resolve the checkpoint policy from")
	}
	readTargets := make([]checkpointpolicy.Target, 0, len(resolution.Candidates))
	for _, name := range resolution.Candidates {
		readTargets = append(readTargets, checkpointpolicy.Target{
			Remote:          name,
			Dir:             dir,
			SkipLocalUpdate: name != resolution.ElectedName,
		})
	}

	var pushTarget *checkpointpolicy.Target
	if resolution.ElectedName != "" {
		pushTarget = &checkpointpolicy.Target{Remote: resolution.ElectedName, Dir: dir}
	}
	return readTargets, pushTarget, nil
}

func formatCheckpointPolicyValue(configured, effective string) string {
	if configured == "" {
		return effective + " (default)"
	}
	return configured
}

func formatCheckpointVersionPolicyValue(configured, effective string) string {
	if configured != "" && checkpointpolicy.UnsupportedWrite(checkpointpolicy.Policy{
		CheckpointVersion:    configured,
		CheckpointMinVersion: checkpointpolicy.DefaultCheckpointVersion(),
	}) {
		return configured + " (unsupported)"
	}
	return formatCheckpointPolicyValue(configured, effective)
}

func checkpointPolicyError(message string, err error) error {
	wrapped := fmt.Errorf("%s: %w", message, err)
	if errors.Is(wrapped, context.Canceled) {
		return NewSilentError(wrapped)
	}
	return wrapped
}
