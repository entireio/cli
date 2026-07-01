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
	version string
	force   bool
}

const (
	checkpointVersionFlag = "checkpoint-version"
)

func newCheckpointPolicyCmd() *cobra.Command {
	var opts checkpointPolicyOptions
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Inspect and update checkpoint policy",
		Long: `Inspect and update checkpoint policy.

checkpoint_version selects the checkpoint metadata format used for new writes.
If no policy is configured, Entire uses the CLI default.
If another client configures a checkpoint_version expression this CLI cannot satisfy,
commands that create checkpoint data fail until the CLI is upgraded. Other commands warn and
continue. Set checkpoint_version to "" to inherit the CLI default.

Unsetting checkpoint_version still uses the normal downgrade guard. If inheriting
the default would lower the effective version, pass --force to allow it.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCheckpointPolicy(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.version, checkpointVersionFlag, "", `Set checkpoint_version. Use "" to inherit the CLI default; --force may be required`)
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
	if checkpointVersionSet {
		state, err = checkpointpolicy.Update(ctx, repo, target, checkpointpolicy.UpdateOptions{
			CheckpointVersion: opts.version,
			Force:             opts.force,
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
	fmt.Fprintf(cmd.OutOrStdout(), "source: %s\n", state.Source)
	return nil
}

func formatCheckpointVersionPolicyValue(configured, effective string) string {
	if configured == "" {
		return effective + " (default)"
	}
	if err := checkpointpolicy.ValidateCheckpointVersionSelector(configured); err != nil {
		return configured + " (unsupported)"
	}
	return configured
}

func checkpointPolicyError(message string, err error) error {
	wrapped := fmt.Errorf("%s: %w", message, err)
	if errors.Is(wrapped, context.Canceled) {
		return NewSilentError(wrapped)
	}
	return wrapped
}
