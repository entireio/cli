package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// mirrorCollaboratorColumns is the human table/field view of a mirror
// collaborator: the display handle, the reader/writer role, and the Entire
// account ULID (the stable identifier, shown last as the fallback when no
// handle resolves).
var mirrorCollaboratorColumns = []string{"HANDLE", colHeaderRole, "ACCOUNT"}

func mirrorCollaboratorRow(c coreapi.MirrorCollaborator) []string {
	handle := c.Handle.Or("")
	if handle == "" {
		handle = "-"
	}
	return []string{handle, c.Role, c.AccountId}
}

// newRepoAccessCmd is the `entire repo access` subtree: who can reach a
// repository. Today it holds the GitHub half only — `list`, a read-only view
// of who can pull a mirror; native grants live under `entire grant repo`.
func newRepoAccessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "access",
		Short: "Show who has access to a repository",
	}
	cmd.AddCommand(newRepoAccessListCmd())
	return requireSubcommand(cmd)
}

// newRepoAccessListCmd wires `repo access list`: a read-only view of who can
// pull a mirror. It hits the user-facing GET /mirrors/collaborators endpoint,
// which runs a LIVE GitHub-admin check against the caller's own GitHub
// identity — the caller must be a current admin of the upstream (org repo) or
// its owner (user repo). Run it as yourself, not via a break-glass
// service-account token.
//
// Grant/revoke used to live beside it, but the server sunset those endpoints
// (mirror collaboration is now managed upstream on GitHub and reconciled into
// SpiceDB), so only the read path remains.
//
// A grant is per-cell (a mirror is a per-cluster native repo with its own
// SpiceDB grant), so when a repo is mirrored on more than one cluster, pass
// --cluster to target the right placement.
func newRepoAccessListCmd() *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   cmdListRepo,
		Short: "List the users with access to a mirror (live GitHub-admin gated)",
		Long: "Lists the principals that can pull the mirror of <repo> on " +
			"the cluster named by --cluster (default " + defaultClusterHost + "), " +
			"with their reader/writer role resolved from the control plane. The " +
			"caller must be a live GitHub admin of the upstream (org repo) or its " +
			"owner (user repo).\n\n" + mirrorRepoRefHelp,
		Example: "  entire repo access list /gh/acme/widget\n" +
			"  entire repo access list /gh/acme/widget --cluster aws-eu-central-1.entire.io",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseGitHubMirrorRepoRef(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				// This verb's name says nothing about GitHub, so a native repo
				// is a reasonable thing to ask it about. Native access is
				// grants, so name the command that answers rather than
				// stopping at "unsupported".
				if declaresForge(args[0], nativeCloneForge) {
					return fmt.Errorf("%w; for an Entire repository see `entire repo grant list %s`", err, args[0])
				}
				return err
			}
			clusterHost := cluster
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid --cluster: %w", err)
			}
			return runCoreListForCluster(cmd, clusterHost, "No collaborators found.", mirrorCollaboratorColumns, mirrorCollaboratorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.MirrorCollaborator, error) {
				out, err := c.ListMirrorCollaborators(ctx, coreapi.ListMirrorCollaboratorsParams{
					Provider:    coreapi.ListMirrorCollaboratorsProviderGithub,
					Owner:       owner,
					Repo:        repo,
					ClusterHost: clusterHost,
				})
				if err != nil {
					return nil, err
				}
				return out.Collaborators, nil
			})
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", defaultClusterHost, "Cluster host the mirror is on")
	addJSONFlag(cmd)
	return cmd
}
