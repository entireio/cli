package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/spf13/cobra"
)

type policyCheckpointOptions struct {
	version    string
	minVersion string
	force      bool
}

func newPolicyCheckpointCmd() *cobra.Command {
	var opts policyCheckpointOptions
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Inspect and update checkpoint policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPolicyCheckpoint(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.version, "checkpoint-version", "", "Set the checkpoint version written by this repository")
	cmd.Flags().StringVar(&opts.minVersion, "checkpoint-min-version", "", "Set the minimum checkpoint version required by this repository")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Allow checkpoint policy version downgrades")
	return cmd
}

func runPolicyCheckpoint(cmd *cobra.Command, opts policyCheckpointOptions) error {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return NewSilentError(err)
	}
	repo, err := gitrepo.OpenCurrent(ctx)
	if err != nil {
		return policyCheckpointError("open repository", err)
	}
	defer repo.Close()

	target, err := checkpointpolicy.ResolveTarget(ctx)
	if err != nil {
		return policyCheckpointError("resolve checkpoint policy remote", err)
	}

	var state checkpointpolicy.State
	if hasPolicyCheckpointUpdate(opts) {
		state, err = checkpointpolicy.Update(ctx, repo, target, checkpointpolicy.UpdateOptions{
			CheckpointVersion:    opts.version,
			CheckpointMinVersion: opts.minVersion,
			Force:                opts.force,
		})
		if err != nil {
			return policyCheckpointError("update checkpoint policy", err)
		}
		if err := checkpointpolicy.Push(ctx, target); err != nil {
			return policyCheckpointError("push checkpoint policy", err)
		}
		state.Source = checkpointpolicy.SourceRemote
	} else {
		state, err = checkpointpolicy.Sync(ctx, repo, target)
		if err != nil {
			return policyCheckpointError("sync checkpoint policy", err)
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "checkpoint_version: %s\n", state.Policy.CheckpointVersion)
	fmt.Fprintf(cmd.OutOrStdout(), "checkpoint_min_version: %s\n", state.Policy.CheckpointMinVersion)
	fmt.Fprintf(cmd.OutOrStdout(), "source: %s\n", state.Source)
	return nil
}

func hasPolicyCheckpointUpdate(opts policyCheckpointOptions) bool {
	return opts.version != "" || opts.minVersion != ""
}

func policyCheckpointError(message string, err error) error {
	wrapped := fmt.Errorf("%s: %w", message, err)
	if errors.Is(wrapped, context.Canceled) {
		return NewSilentError(wrapped)
	}
	return wrapped
}
