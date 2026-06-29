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

func newCheckpointPolicyCmd() *cobra.Command {
	var opts checkpointPolicyOptions
	cmd := &cobra.Command{
		Use:    "policy",
		Short:  "Inspect and update checkpoint policy",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCheckpointPolicy(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.version, "checkpoint-version", "", "Set the checkpoint version written by this repository")
	cmd.Flags().StringVar(&opts.minVersion, "checkpoint-min-version", "", "Set the minimum checkpoint version required by this repository")
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
	if hasCheckpointPolicyUpdate(opts) {
		state, err = checkpointpolicy.Update(ctx, repo, target, checkpointpolicy.UpdateOptions{
			CheckpointVersion:    opts.version,
			CheckpointMinVersion: opts.minVersion,
			Force:                opts.force,
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

	fmt.Fprintf(cmd.OutOrStdout(), "checkpoint_version: %s\n", state.Policy.CheckpointVersion)
	fmt.Fprintf(cmd.OutOrStdout(), "checkpoint_min_version: %s\n", state.Policy.CheckpointMinVersion)
	fmt.Fprintf(cmd.OutOrStdout(), "source: %s\n", state.Source)
	return nil
}

func hasCheckpointPolicyUpdate(opts checkpointPolicyOptions) bool {
	return opts.version != "" || opts.minVersion != ""
}

func checkpointPolicyError(message string, err error) error {
	wrapped := fmt.Errorf("%s: %w", message, err)
	if errors.Is(wrapped, context.Canceled) {
		return NewSilentError(wrapped)
	}
	return wrapped
}
