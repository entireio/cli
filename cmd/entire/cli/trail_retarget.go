package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/spf13/cobra"
)

func newTrailRetargetCmd(mode *trailMode) *cobra.Command {
	var branch, base string
	cmd := &cobra.Command{
		Use:   "retarget [<trail>] --base <branch>",
		Short: "Change a trail's target base branch",
		Long: "Change a trail's target base branch, for example after its previous target was merged.\n\n" + mode.help(
			"If <trail> is omitted, targets the trail for the current branch (or --branch).",
			"<trail> is a project trail number, ID, or <repo>/<number>. Without one, follow the current branch's parent. --repo and --branch select one working context; only that branch's target is changed.") + "\n\nThis updates Trail metadata through the API. It does not rebase, check out, fetch, or push local Git branches. The selected work must have a linked branch and must not be closed or merged. The old target need not exist. An unchanged target is a no-op.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base = strings.TrimPrefix(strings.TrimSpace(base), "refs/heads/")
			if base == "" {
				return errors.New("--base is required and must name a non-empty target branch")
			}
			// Reflog expressions refer to local checkout history, not a remote target.
			if strings.Contains(base, "@{") {
				return fmt.Errorf("invalid branch name %q", base)
			}
			if err := ValidateBranchName(cmd.Context(), base); err != nil {
				return err
			}
			selected, err := mode.workingContext(cmd, selectorFromArgs(args), branch, false)
			if err != nil {
				return err
			}
			if selected.Work.Status == "merged" || selected.Work.Status == "closed" {
				return fmt.Errorf("cannot change the target of %s work", selected.Work.Status)
			}
			if selected.Work.Branch == "" {
				return errors.New("trail work must have a linked branch to change its target")
			}
			if strings.TrimPrefix(selected.Work.Branch, "refs/heads/") == base {
				return errors.New("trail cannot target its own branch")
			}
			if strings.TrimPrefix(selected.Work.Base, "refs/heads/") == base {
				fmt.Fprintf(cmd.OutOrStdout(), "%s already targets %s\n", selected.description(), tuiutil.SanitizeTerminalLabel(base))
				return nil
			}
			if err := sendTrailPatch(cmd.Context(), selected.Client, trailNumberPathForBase(selected.BasePath, selected.Work.Number), api.TrailUpdateRequest{Base: &base}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Retargeted %s to %s\n", selected.description(), tuiutil.SanitizeTerminalLabel(base))
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "New target base branch (required)")
	cmd.Flags().StringVar(&branch, "branch", "", mode.help("Branch of the trail (defaults to current); cannot be combined with a trail selector", "Select a repository branch within the trail"))
	return cmd
}
