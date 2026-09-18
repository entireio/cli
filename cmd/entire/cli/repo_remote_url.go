package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newRepoRemoteURLCmd() *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   "url <repo>",
		Short: "Print an Entire repository's git remote URL",
		Long: "Resolve an Entire-native /et/<project>/<repo> ref or a GitHub mirror " +
			"/gh/<owner>/<repo> ref to its entire:// URL. A full entire:// URL is " +
			"passed through without a lookup.\n\n" +
			"Prints only the URL and a newline to stdout, suitable for shell substitution. " +
			"Works from any directory. Native repos resolve to their home cluster; " +
			"--cluster is accepted only for /gh/ mirror refs — it is rejected on a " +
			"native ref, which has exactly one home cluster, and ignored for a full " +
			"entire:// URL, which already names its cluster. " +
			"For mirrors on multiple clusters, prompts for a placement interactively; " +
			"pass --cluster to choose non-interactively.",
		Example: "  entire repo remote url /et/project/example\n" +
			"  git remote add entire \"$(entire repo remote url /et/project/example)\"\n" +
			"  entire repo remote url /gh/entirehq/entire-api --cluster aws-us-east-2.entire.io",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Set inside RunE, as repo clone does, so cobra still prints usage
			// for an arg or flag error and suppresses it only once the command
			// is genuinely running.
			cmd.SilenceUsage = true
			// passthroughNeedsHost is true: this URL is printed for the user to
			// paste into `git remote add`, so nothing downstream would catch a
			// malformed one.
			url, err := resolveRepoRemoteURL(cmd, args[0], cluster, remoteURLPlacementPicker(), true)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), url); err != nil {
				return fmt.Errorf("write remote URL: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "Cluster host to use when the repo is mirrored on more than one (may belong to another auth context)")
	return cmd
}

// remoteURLPlacementPicker is `repo remote url`'s wording for selectPlacement.
// See clonePlacementPicker for why a verb supplies a value rather than its own
// selection function.
func remoteURLPlacementPicker() placementPicker {
	return placementPicker{
		selector: clusterSelectorFlag,
		title:    "This repo is mirrored on more than one cluster — pick a remote",
		action:   "Select remote",
	}
}
