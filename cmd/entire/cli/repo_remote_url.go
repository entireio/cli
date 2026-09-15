package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newRepoRemoteURLCmd() *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   "remote-url <repo>",
		Short: "Print an Entire repository's git remote URL",
		Long: "Resolve an Entire-native /et/<project>/<repo> ref or a GitHub mirror " +
			"/gh/<owner>/<repo> ref to its entire:// URL. A full entire:// URL is " +
			"passed through without a lookup.\n\n" +
			"Prints only the URL and a newline to stdout, suitable for shell substitution. " +
			"Works from any directory. Native repos resolve to their home cluster; " +
			"--cluster applies only to mirror refs and is ignored for full URLs. " +
			"For mirrors on multiple clusters, prompts for a placement interactively; " +
			"pass --cluster to choose non-interactively.",
		Example: "  entire repo remote-url /et/project/example\n" +
			"  git remote add entire \"$(entire repo remote-url /et/project/example)\"\n" +
			"  entire repo remote-url /gh/entirehq/entire-api --cluster aws-us-east-2.entire.io",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, err := resolveRepoRemoteURL(cmd, args[0], cluster, selectRemoteURLTarget)
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

func selectRemoteURLTarget(cmd *cobra.Command, placements []coreapi.ResolvedPlacement, cluster string) (coreapi.ResolvedPlacement, error) {
	return selectPlacement(cmd, placements, cluster, placementPicker{
		selector: clusterSelectorFlag,
		title:    "This repo is mirrored on more than one cluster — pick a remote",
		action:   "Select remote",
	})
}
