package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// newDoctorMigrateToULIDCmd builds the hidden `entire doctor migrate-to-ulid`
// test-tooling command. It converts a git-branch + hex-id repo into one that
// looks as if it had used git-refs + ULIDs from the start: every checkpoint is
// re-identified as a ULID stored under refs/entire/checkpoints/<shard>/<ulid>,
// the Entire-Checkpoint commit trailers are rewritten hex → ULID (rewriting
// history), and the entire/* branches (v1 + shadow) are dropped.
//
// It is intended for producing validation repos from a COPY of a real repo, not
// for use on a repo you care about — it rewrites history irreversibly (aside from
// git's refs/original backup).
func newDoctorMigrateToULIDCmd() *cobra.Command {
	var apply bool

	cmd := &cobra.Command{
		Use:    "migrate-to-ulid",
		Short:  "Test tooling: rewrite a branch+hex repo to look like it always used refs+ULIDs",
		Hidden: true,
		Long: `Convert a git-branch + hex-id checkpoint repo into a git-refs + ULID one,
rewriting history so it reads as if it had used refs+ULIDs from the start:

  1. Re-identify every checkpoint on entire/checkpoints/v1 as a fresh ULID
     (timestamped from the checkpoint's original creation time) stored under
     refs/entire/checkpoints/<shard>/<ulid>, re-stamping the embedded
     checkpoint_id in its metadata.
  2. Rewrite the Entire-Checkpoint commit trailers hex -> ULID across all local
     branches (except the entire/* infra branches) via git filter-branch. This
     REWRITES HISTORY (commit SHAs change).
  3. Delete the entire/checkpoints/v1 branch and the per-worktree shadow branches
     so only the ULID refs remain.

Intended for a THROWAWAY COPY of a repo (to stage a validation repo for testing
commands and the UI). It rewrites history irreversibly apart from git's
refs/original/ backup. Runs a dry-run preview unless --yes is given.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			repoRoot, err := paths.WorktreeRoot(ctx)
			if err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run this from inside the repo you want to convert.")
				return NewSilentError(errors.New("not a git repository"))
			}
			repo, err := git.PlainOpen(repoRoot)
			if err != nil {
				return fmt.Errorf("open repository: %w", err)
			}

			// Single walk: a dry-run for the preview, or the real migration under
			// --yes. Running a dry pass and then a real one would mint different
			// (random) ULIDs and print a mapping that didn't match what was written.
			result, err := cpkg.MigrateBranchHexToULIDRefs(ctx, repo, !apply)
			if err != nil {
				return fmt.Errorf("migrate checkpoints to ULID refs: %w", err)
			}
			userBranches, err := listRewritableBranches(ctx, repoRoot)
			if err != nil {
				return fmt.Errorf("list branches: %w", err)
			}
			infraBranches, err := listInfraBranches(ctx, repoRoot)
			if err != nil {
				return fmt.Errorf("list entire/* branches: %w", err)
			}

			printMigrateToULIDPlan(out, result, userBranches, infraBranches)

			if len(result.Mapping) == 0 {
				fmt.Fprintln(out, "\nNothing to migrate (no legacy-hex checkpoints found).")
				return nil
			}
			if !apply {
				fmt.Fprintln(out, "\nDry run. Re-run with --yes to apply (this rewrites history).")
				return nil
			}
			fmt.Fprintf(out, "\nWrote %d ULID checkpoint ref(s).\n", len(result.Mapping))

			// Rewrite the Entire-Checkpoint trailers across the user branches.
			if len(userBranches) > 0 {
				if err := rewriteCheckpointTrailers(ctx, repoRoot, result.Mapping, userBranches); err != nil {
					return fmt.Errorf("rewrite commit trailers: %w", err)
				}
				fmt.Fprintf(out, "Rewrote Entire-Checkpoint trailers on %d branch(es).\n", len(userBranches))
			}

			// 3. Drop the entire/* infra branches so only the ULID refs remain.
			dropped := deleteBranches(ctx, repoRoot, infraBranches)
			if dropped > 0 {
				fmt.Fprintf(out, "Deleted %d entire/* branch(es) (v1 + shadow).\n", dropped)
			}

			fmt.Fprintln(out, "\nDone. The repo now uses ULID refs only. Force-push branches and push the")
			fmt.Fprintln(out, "checkpoint refs to your test remote to validate commands and the UI.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&apply, "yes", false, "Apply the migration (rewrites history); without it, only a dry-run preview is printed")
	return cmd
}

// printMigrateToULIDPlan prints the dry-run/pre-apply plan.
func printMigrateToULIDPlan(out io.Writer, preview cpkg.ULIDMigrateResult, userBranches, infraBranches []string) {
	fmt.Fprintf(out, "Checkpoints on entire/checkpoints/v1: %d\n", preview.Total)
	fmt.Fprintf(out, "  to re-identify hex -> ULID: %d\n", len(preview.Mapping))
	if preview.Skipped > 0 {
		fmt.Fprintf(out, "  already ULID (skipped):     %d\n", preview.Skipped)
	}
	const sample = 5
	for i, m := range preview.Mapping {
		if i >= sample {
			fmt.Fprintf(out, "    ... and %d more\n", len(preview.Mapping)-sample)
			break
		}
		fmt.Fprintf(out, "    %s -> %s\n", m.OldID, m.NewID)
	}
	fmt.Fprintf(out, "Branches whose trailers will be rewritten: %d\n", len(userBranches))
	for _, b := range userBranches {
		fmt.Fprintf(out, "    %s\n", b)
	}
	fmt.Fprintf(out, "entire/* branches to delete: %d\n", len(infraBranches))
	for _, b := range infraBranches {
		fmt.Fprintf(out, "    %s\n", b)
	}
}

// listRewritableBranches returns the local branch refs to rewrite: every
// refs/heads/* except the entire/* infra branches.
func listRewritableBranches(ctx context.Context, repoRoot string) ([]string, error) {
	refs, err := forEachRef(ctx, repoRoot, "refs/heads")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range refs {
		if strings.HasPrefix(r, "refs/heads/entire/") {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// listInfraBranches returns the entire/* branches (v1 + per-worktree shadow).
func listInfraBranches(ctx context.Context, repoRoot string) ([]string, error) {
	return forEachRef(ctx, repoRoot, "refs/heads/entire")
}

// forEachRef lists full ref names under the given prefix.
func forEachRef(ctx context.Context, repoRoot, prefix string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "for-each-ref", "--format=%(refname)", prefix)
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref %s: %w", prefix, err)
	}
	var refs []string
	scanner := bufio.NewScanner(strings.NewReader(string(stdout)))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}

// rewriteCheckpointTrailers remaps the Entire-Checkpoint trailer values hex ->
// ULID across the given branches using git filter-branch with a sed --msg-filter.
func rewriteCheckpointTrailers(ctx context.Context, repoRoot string, mapping []cpkg.ULIDMapping, branches []string) error {
	sedScript, err := os.CreateTemp("", "entire-ulid-map-*.sed")
	if err != nil {
		return fmt.Errorf("create sed map: %w", err)
	}
	defer func() { _ = os.Remove(sedScript.Name()) }()
	var b strings.Builder
	for _, m := range mapping {
		// hex and ULID contain no '/', so '/' is a safe s/// delimiter. Anchor on
		// the trailer key so only checkpoint-id occurrences are remapped.
		fmt.Fprintf(&b, "s/Entire-Checkpoint: %s/Entire-Checkpoint: %s/g\n", m.OldID, m.NewID)
	}
	if _, err := sedScript.WriteString(b.String()); err != nil {
		_ = sedScript.Close()
		return fmt.Errorf("write sed map: %w", err)
	}
	if err := sedScript.Close(); err != nil {
		return fmt.Errorf("close sed map: %w", err)
	}

	args := []string{"-C", repoRoot, "filter-branch", "-f", "--msg-filter", "sed -f '" + sedScript.Name() + "'", "--"}
	args = append(args, branches...)
	cmd := exec.CommandContext(ctx, "git", args...)
	// filter-branch prints a discouraged-use warning to stderr and exits non-zero
	// on that alone unless squelched; it is the right tool for a one-shot rewrite.
	cmd.Env = append(os.Environ(), "FILTER_BRANCH_SQUELCH_WARNING=1")
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git filter-branch: %w\n%s", err, strings.TrimSpace(string(outBytes)))
	}
	return nil
}

// deleteBranches deletes the given branch refs, returning how many were removed.
// Failures are tolerated (a missing branch is fine) so cleanup is best-effort.
func deleteBranches(ctx context.Context, repoRoot string, refs []string) int {
	deleted := 0
	for _, ref := range refs {
		cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "update-ref", "-d", ref)
		if err := cmd.Run(); err == nil {
			deleted++
		}
	}
	return deleted
}
