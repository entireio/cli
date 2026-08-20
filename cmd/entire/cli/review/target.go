package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const envReviewFindingsWorktree = "ENTIRE_REVIEW_FINDINGS_WORKTREE"

// TargetWorktree describes the checkout prepared for a targeted review.
// Created is false when the branch was already checked out and that existing
// worktree is being reused.
type TargetWorktree struct {
	Path    string
	Created bool
}

type reviewWorktreeRunner func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error

func runTargetReview(ctx context.Context, cmd *cobra.Command, target string, childArgs []string, cleanupWorktree, modeSelected bool, deps Deps) error {
	if modeSelected {
		return errors.New("--target can only be used when running a review")
	}
	if deps.PrepareTarget == nil {
		return errors.New("review target checkout is unavailable")
	}
	callerWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve caller worktree: %w", err)
	}
	prepared, err := deps.PrepareTarget(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), target)
	if err != nil {
		return err
	}
	env := []string{envReviewFindingsWorktree + "=" + callerWorktree}
	if err := runReviewInWorktree(ctx, deps.RunInWorktree, prepared.Path, childArgs, env, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return wrapReviewSilentError(deps.NewSilentError, err)
	}
	return finishTargetReview(ctx, cmd, prepared, cleanupWorktree, deps.RemoveTarget)
}

func reviewTargetChildArgs(cmd *cobra.Command, positional []string) []string {
	args := make([]string, 0, len(positional)+cmd.Flags().NFlag()+1)
	args = append(args, "review")
	args = append(args, positional...)
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name == "target" || flag.Name == "cleanup-worktree" {
			return
		}
		args = append(args, "--"+flag.Name+"="+flag.Value.String())
	})
	return args
}

func finishTargetReview(ctx context.Context, cmd *cobra.Command, target TargetWorktree, cleanupWorktree bool, removeTarget func(context.Context, string) error) error {
	out := cmd.OutOrStdout()
	if !target.Created {
		if cleanupWorktree {
			fmt.Fprintf(out, "Kept reused worktree at %s.\n", target.Path)
		}
		return nil
	}

	remove := cleanupWorktree
	if !remove && reviewCommandIsInteractive(cmd) {
		form := newAccessibleForm(huh.NewGroup(
			huh.NewConfirm().
				Title("Remove the temporary review worktree?").
				Description(target.Path).
				Value(&remove),
		))
		// The review itself already completed. Cancelling this optional prompt
		// leaves remove=false, keeping the worktree without turning success into
		// failure.
		runOptionalCleanupPrompt(ctx, form)
	}
	if !remove {
		fmt.Fprintf(out, "Kept worktree at %s.\n", target.Path)
		return nil
	}
	if removeTarget == nil {
		return errors.New("review target cleanup is unavailable")
	}
	if err := removeTarget(ctx, target.Path); err != nil {
		return fmt.Errorf("remove review worktree %s: %w", target.Path, err)
	}
	fmt.Fprintf(out, "Removed temporary review worktree %s.\n", target.Path)
	return nil
}

func runOptionalCleanupPrompt(ctx context.Context, form *huh.Form) {
	if err := form.RunWithContext(ctx); err != nil {
		return
	}
}

func runReviewInWorktree(ctx context.Context, runner reviewWorktreeRunner, worktreeRoot string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if strings.TrimSpace(worktreeRoot) == "" {
		return errors.New("review target checkout returned an empty worktree path")
	}
	if runner != nil {
		return runner(ctx, worktreeRoot, args, env, stdin, stdout, stderr)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve entire executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = worktreeRoot
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("target review context: %w", ctx.Err())
		}
		// The child writes its own command error to stderr. The caller wraps this
		// as SilentError so the parent preserves failure without printing a
		// duplicate "exit status 1" line.
		return fmt.Errorf("target review process: %w", err)
	}
	return nil
}
