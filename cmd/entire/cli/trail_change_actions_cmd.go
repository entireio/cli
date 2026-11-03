package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

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
	var jsonOut bool
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
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
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
	addTrailActionJSONFlag(cmd, &jsonOut)
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
	var jsonOut bool
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
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is backed by pull request #%d\n", selected.description(), out.PRNumber)
			return nil
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	addTrailActionJSONFlag(cmd, &jsonOut)
	return cmd
}

// newTrailRerunReviewCmd re-reviews the selected change: every reviewer at
// once through the change's review-rerun, or one project agent with --agent.
func newTrailRerunReviewCmd(mode *trailMode) *cobra.Command {
	var branch, agentID string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "rerun-review [<trail>]",
		Short: "Review a trail's current head again",
		Long: "Review a trail's current head again: the project's review agents start as a push starts them, and " +
			"the repository's own configured reviewers re-run. Runs that already cover the head are kept rather than " +
			"repeated. --agent <id> runs one project agent instead ('entire project agent list' names them). " +
			"Runs spend the project's agent budget. Follow the results with 'entire trail finding'.\n\n" +
			trailWorkingSelectorHelp(mode, "reviews"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID = strings.TrimSpace(agentID)
			if cmd.Flags().Changed("agent") && !looksLikeULID(agentID) {
				return fmt.Errorf("--agent must be an agent ID (see 'entire project agent list'), got %q", tuiutil.SanitizeTerminalLabel(agentID))
			}
			selected, err := mode.workingContext(cmd, selectorFromArgs(args), branch, false)
			if err != nil {
				return err
			}
			if agentID != "" {
				return runTrailAgentReview(cmd, selected, agentID, jsonOut)
			}
			var out api.TrailReviewRerunResponse
			if err := postTrailChangeAction(cmd.Context(), selected, "/review-rerun", "re-run review", &out); err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Requested a new review of %s; follow it with 'entire trail finding'\n", selected.description())
			return nil
		},
	}
	addTrailWorkingBranchFlag(cmd, mode, &branch)
	cmd.Flags().StringVar(&agentID, "agent", "", "Run only this project agent (its ID from 'entire project agent list')")
	addTrailActionJSONFlag(cmd, &jsonOut)
	return cmd
}

// runTrailAgentReview starts one project agent on the selected change. Agents
// belong to the project, so the run goes to the project's route, naming the
// trail and change it reviews.
func runTrailAgentReview(cmd *cobra.Command, selected *trailWorkingContext, agentID string, jsonOut bool) error {
	if selected.Target == nil {
		return errors.New("--agent needs a project trail; this trail belongs to no project")
	}
	t := selected.Target
	path := "/api/v1/" + url.PathEscape(t.Host) + "/" + url.PathEscape(t.Project) + "/agents/" + url.PathEscape(agentID) + "/runs"
	body := api.AgentRunRequest{Target: api.AgentRunTarget{TrailID: selected.Parent.ID, ChangeID: selected.Work.ID}}
	var run api.AgentRun
	if err := t.Client.CreateProjectConfig(cmd.Context(), path, body, &run); err != nil {
		return fmt.Errorf("run agent %s: %w", tuiutil.SanitizeTerminalLabel(agentID), err)
	}
	if jsonOut {
		return printJSON(cmd.OutOrStdout(), run)
	}
	name := run.Agent.Name
	if name == "" {
		name = agentID
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Started %s on %s (run %s, %s)\n", tuiutil.SanitizeTerminalLabel(name), selected.description(),
		tuiutil.SanitizeTerminalLabel(run.ID), tuiutil.SanitizeTerminalLabel(run.Status))
	return nil
}

func addTrailActionJSONFlag(cmd *cobra.Command, jsonOut *bool) {
	cmd.Flags().BoolVar(jsonOut, "json", false, "Output as JSON")
}
