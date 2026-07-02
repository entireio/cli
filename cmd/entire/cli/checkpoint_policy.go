package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
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

checkpoint_version selects the checkpoint metadata format used for new writes.
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

	target, err := checkpointpolicy.ResolveTarget(ctx)
	if err != nil {
		return checkpointPolicyError("resolve checkpoint policy remote", err)
	}

	var state checkpointpolicy.State
	checkpointVersionSet := cmd.Flags().Changed(checkpointVersionFlag)
	checkpointMinVersionSet := cmd.Flags().Changed(checkpointMinVersionFlag)
	if hasCheckpointPolicyUpdate(checkpointVersionSet, checkpointMinVersionSet) {
		state, err = checkpointpolicy.Update(ctx, repo, target, checkpointpolicy.UpdateOptions{
			CheckpointVersion:       opts.version,
			CheckpointVersionSet:    checkpointVersionSet,
			CheckpointMinVersion:    opts.minVersion,
			CheckpointMinVersionSet: checkpointMinVersionSet,
			Force:                   opts.force,
		})
		if err != nil {
			return checkpointPolicyError("update checkpoint policy", err)
		}
		if err := checkpointpolicy.Push(ctx, target); err != nil {
			return checkpointPolicyError("push checkpoint policy", err)
		}
		state.Source = checkpointpolicy.SourceRemote
	} else {
		state, err = checkpointpolicy.Sync(ctx, repo, target)
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
