package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// postTrailChangeAction POSTs a bodyless action to the selected change and
// decodes the answer into out.
func postTrailChangeAction(ctx context.Context, selected *trailWorkingContext, suffix, what string, out any) error {
	path, err := trailRepoIDPath(selected, suffix)
	if err != nil {
		return err
	}
	resp, err := selected.Client.Post(ctx, path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return err
	}
	if err := api.DecodeJSON(resp, out); err != nil {
		return fmt.Errorf("decode %s response: %w", what, err)
	}
	return nil
}

func addTrailWorkingBranchFlag(cmd *cobra.Command, mode *trailMode, branch *string) {
	cmd.Flags().StringVar(branch, "branch", "", mode.help("Branch of the trail (defaults to current); cannot be combined with a trail selector", "Select a repository branch within the trail"))
}

func trailWorkingSelectorHelp(mode *trailMode, verb string) string {
	return mode.help("If <trail> is omitted, "+verb+" the trail for the current branch (or --branch).",
		`<trail> is a project trail number, ID, or <repo>/<number>. Without one, follow the current branch's
parent. --repo and --branch select a working context; only that branch is affected.`)
}

func newTrailUpdateBranchCmd(mode *trailMode) *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "update-branch [<trail>]",
		Short: "Merge the base branch into a trail's branch",
		Long: "Merge the latest base branch into a trail's branch on the server, so it is up to date for merging. " +
			"Your local branch is not touched; pull afterwards. A branch that conflicts with its base is left unchanged. " +
			"Only Entire-native repositories are supported; for a GitHub-backed trail, merge the base locally and push.\n\n" +
			trailWorkingSelectorHelp(mode, "updates"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selected, err := mode.workingContext(cmd, selectorFromArgs(args), branch, false)
			if err != nil {
				return err
			}
			if selected.Host != nativeCloneForge {
				return fmt.Errorf("update-branch only supports Entire-native repositories; for %s, merge %s into %s locally and push",
					selected.description(), tuiutil.SanitizeTerminalLabel(trailBaseOrDefault(selected.Work.Base)), tuiutil.SanitizeTerminalLabel(changeBranchName(selected.Work)))
			}
			var out api.TrailUpdateBranchResponse
			if err := postTrailChangeAction(cmd.Context(), selected, "/update-branch", "update trail branch", &out); err != nil {
				return err
			}
			if out.MergeCommitSHA == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is already up to date with its base\n", selected.description())
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %s; its branch is now at %s\n", selected.description(), tuiutil.SanitizeTerminalLabel(out.MergeCommitSHA))
			return nil
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	return cmd
}

func trailBaseOrDefault(base string) string {
	if strings.TrimSpace(base) == "" {
		return "the base branch"
	}
	return base
}

func newTrailCIPullRequestCmd(mode *trailMode) *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "ci-pr [<trail>]",
		Short: "Create or link the GitHub pull request that runs CI",
		Long: "Create or link the GitHub pull request that backs a trail, so the repository's CI runs on its branch. " +
			"Only GitHub-backed trails with changes on an open branch have one.\n\n" + trailWorkingSelectorHelp(mode, "uses"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selected, err := mode.workingContext(cmd, selectorFromArgs(args), branch, false)
			if err != nil {
				return err
			}
			var out api.TrailCIPullRequestResponse
			if err := postTrailChangeAction(cmd.Context(), selected, "/ci-pr", "create CI pull request", &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is backed by pull request #%d\n", selected.description(), out.PRNumber)
			return nil
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	return cmd
}

func newTrailRunnerCmd(mode *trailMode) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runner",
		Short: "Inspect and start a trail's configured runners",
		Long:  "Inspect and start the runners the repository configures for trails (reviews, monitors, and other agents).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newTrailRunnerListCmd(mode), newTrailRunnerShowCmd(mode), newTrailRunnerStartCmd(mode), newTrailRunnerRerunReviewCmd(mode))
	return cmd
}

func newTrailRunnerListCmd(mode *trailMode) *cobra.Command {
	var branch string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list [<trail>]",
		Short: "List a trail's runners and their latest state",
		Long:  "List the runners configured for a trail with each one's latest state and outcome.\n\n" + trailWorkingSelectorHelp(mode, "lists"),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selected, err := mode.workingContext(cmd, selectorFromArgs(args), branch, false)
			if err != nil {
				return err
			}
			detail, err := fetchTrailDetailAtPath(cmd.Context(), selected.Client, selected.BasePath, selected.Work.Number)
			if err != nil {
				return err
			}
			runners, err := detail.DecodeRunners()
			if err != nil {
				return fmt.Errorf("read trail runners: %w", err)
			}
			if jsonOut {
				if runners == nil {
					runners = []api.TrailRunnerState{}
				}
				return printJSON(cmd.OutOrStdout(), runners)
			}
			if len(runners) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No runners on %s\n", selected.description())
				return nil
			}
			return printTable(cmd.OutOrStdout(), []string{"RUNNER", "STATE", "OUTCOME", "HEAD", "STARTED"}, runners, func(r api.TrailRunnerState) []string {
				started := ""
				if r.StartedAt != nil {
					started = r.StartedAt.Format(time.RFC3339)
				}
				return []string{tuiutil.SanitizeTerminalLabel(r.RunnerID), tuiutil.SanitizeTerminalLabel(r.State),
					tuiutil.SanitizeTerminalLabel(stringPtrValue(r.Outcome)), shortSHA(tuiutil.SanitizeTerminalLabel(stringPtrValue(r.EvaluatedAtSHA))), started}
			})
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	addTrailRunnerJSONFlag(cmd, &jsonOut)
	return cmd
}

func addTrailRunnerJSONFlag(cmd *cobra.Command, jsonOut *bool) {
	cmd.Flags().BoolVar(jsonOut, "json", false, "Output as JSON")
}

// trailRunnerArgs splits [<trail>] <runner-id>.
func trailRunnerArgs(args []string) (selector, runnerID string, err error) {
	runnerID = strings.TrimSpace(args[len(args)-1])
	if runnerID == "" {
		return "", "", errors.New("runner ID must not be empty")
	}
	return selectorFromArgs(args[:len(args)-1]), runnerID, nil
}

func newTrailRunnerShowCmd(mode *trailMode) *cobra.Command {
	var branch string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show [<trail>] <runner-id>",
		Short: "Show a runner's latest run and result",
		Long:  "Show a runner's latest run on a trail and the result it reported. 'entire trail runner list' names the runners.\n\n" + trailWorkingSelectorHelp(mode, "shows"),
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, runnerID, err := trailRunnerArgs(args)
			if err != nil {
				return err
			}
			selected, err := mode.workingContext(cmd, selector, branch, false)
			if err != nil {
				return err
			}
			path, err := trailRepoIDPath(selected, "/runs/"+url.PathEscape(runnerID))
			if err != nil {
				return err
			}
			resp, err := selected.Client.Get(cmd.Context(), path)
			if err != nil {
				return fmt.Errorf("read runner result: %w", err)
			}
			defer resp.Body.Close()
			if err := checkTrailResponse(resp); err != nil {
				return err
			}
			var out api.TrailRunnerLatestResponse
			if err := api.DecodeJSON(resp, &out); err != nil {
				return fmt.Errorf("decode runner result: %w", err)
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
			}
			printTrailRunnerLatest(cmd, runnerID, selected, out)
			return nil
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	addTrailRunnerJSONFlag(cmd, &jsonOut)
	return cmd
}

func printTrailRunnerLatest(cmd *cobra.Command, runnerID string, selected *trailWorkingContext, out api.TrailRunnerLatestResponse) {
	w := cmd.OutOrStdout()
	name := tuiutil.SanitizeTerminalLabel(runnerID)
	if out.Run == nil {
		fmt.Fprintf(w, "Runner %s has not run on %s\n", name, selected.description())
		return
	}
	run := out.Run
	fmt.Fprintf(w, "Runner:  %s\n", name)
	fmt.Fprintf(w, "Run:     %s\n", tuiutil.SanitizeTerminalLabel(run.ID))
	fmt.Fprintf(w, "Status:  %s\n", tuiutil.SanitizeTerminalLabel(run.Status))
	if sha := stringPtrValue(run.HeadSHA); sha != "" {
		fmt.Fprintf(w, "Head:    %s\n", tuiutil.SanitizeTerminalLabel(sha))
	}
	fmt.Fprintf(w, "Updated: %s\n", tuiutil.SanitizeTerminalLabel(run.UpdatedAt))
	if msg := stringPtrValue(run.ErrorMessage); msg != "" {
		fmt.Fprintf(w, "Error:   %s\n", tuiutil.SanitizeTerminalText(msg))
	}
	if out.Result != nil {
		fmt.Fprintf(w, "Result:  %s (%s); pass --json for its data\n", tuiutil.SanitizeTerminalLabel(out.Result.Type), tuiutil.SanitizeTerminalLabel(out.Result.Adapter))
	}
}

func newTrailRunnerStartCmd(mode *trailMode) *cobra.Command {
	var branch string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "start [<trail>] <runner-id>",
		Short: "Start a configured runner on a trail's current head",
		Long: "Start a configured runner against a trail's current head. A runner already running, or a completed " +
			"review that already covers the head, starts nothing; the output says which. Runs use the repository's " +
			"agent budget. 'entire trail runner list' names the runners.\n\n" + trailWorkingSelectorHelp(mode, "starts it on"),
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, runnerID, err := trailRunnerArgs(args)
			if err != nil {
				return err
			}
			selected, err := mode.workingContext(cmd, selector, branch, false)
			if err != nil {
				return err
			}
			var out api.TrailRunnerLaunchResponse
			if err := postTrailChangeAction(cmd.Context(), selected, "/runs/"+url.PathEscape(runnerID), "start runner", &out); err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
			}
			name, run := tuiutil.SanitizeTerminalLabel(runnerID), tuiutil.SanitizeTerminalLabel(out.RunID)
			switch out.Outcome {
			case "already_in_progress":
				fmt.Fprintf(cmd.OutOrStdout(), "Runner %s is already running on %s (run %s)\n", name, selected.description(), run)
			case "already_complete":
				fmt.Fprintf(cmd.OutOrStdout(), "Runner %s already completed for the current head of %s (run %s); nothing started\n", name, selected.description(), run)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "Started runner %s on %s (run %s)\n", name, selected.description(), run)
			}
			return nil
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	addTrailRunnerJSONFlag(cmd, &jsonOut)
	return cmd
}

func newTrailRunnerRerunReviewCmd(mode *trailMode) *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "rerun-review [<trail>]",
		Short: "Re-run the configured review on a trail's current head",
		Long: "Re-run the repository's configured review runners against a trail's current head. " +
			"Runs use the repository's agent budget.\n\n" + trailWorkingSelectorHelp(mode, "re-runs it on"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selected, err := mode.workingContext(cmd, selectorFromArgs(args), branch, false)
			if err != nil {
				return err
			}
			var out api.TrailReviewRerunResponse
			if err := postTrailChangeAction(cmd.Context(), selected, "/review-rerun", "re-run review", &out); err != nil {
				return err
			}
			if out.Launched == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No review runners started on %s\n", selected.description())
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Started %d review runner(s) on %s\n", out.Launched, selected.description())
			return nil
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	return cmd
}
