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
var mirrorCollaboratorColumns = []string{"HANDLE", "ROLE", "ACCOUNT"}

func mirrorCollaboratorRow(c coreapi.MirrorCollaborator) []string {
	handle := c.Handle.Or("")
	if handle == "" {
		handle = "-"
	}
	return []string{handle, c.Role, c.AccountId}
}

// newRepoMirrorCollaboratorsCmd wires `repo mirror collaborators list`: a
// read-only view of who can pull a mirror. It hits the user-facing
// GET /mirrors/collaborators endpoint, which runs a LIVE GitHub-admin check
// against the caller's own GitHub identity — the caller must be a current
// admin of the upstream (org repo) or its owner (user repo). Run it as
// yourself, not via a break-glass service-account token.
//
// Grant/revoke used to live here too, but the server sunset those endpoints
// (mirror collaboration is now managed upstream on GitHub and reconciled
// into SpiceDB), so only the read path remains.
//
// The cluster-host is an optional trailing positional, defaulting to
// defaultClusterHost. A grant is per-cell (a mirror is a per-cluster native
// repo with its own SpiceDB grant), so when a repo is mirrored on more than
// one cluster, pass the cluster explicitly to target the right placement.
func newRepoMirrorCollaboratorsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collaborators",
		Short: "List the users with access to a mirror (live GitHub-admin gated)",
	}
	cmd.AddCommand(newRepoMirrorCollaboratorsListCmd())
	return cmd
}

func newRepoMirrorCollaboratorsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <github-url> [cluster-host]",
		Short: "List the users with access to a mirror",
		Long: "Lists the principals that can pull the mirror of <github-url> on " +
			"the target cluster, with their reader/writer role resolved from the " +
			"control plane. The caller must be a live GitHub admin of the upstream " +
			"(org repo) or its owner (user repo). The cluster-host defaults to " +
			defaultClusterHost + " when omitted.",
		Example: "  entire repo mirror collaborators list github.com/acme/widget",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseGitHubURL(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid <github-url>: %w", err)
			}
			clusterHost := clusterArgAt(args, 1)
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid [cluster-host]: %w", err)
			}
			return runCoreListForCluster(cmd, clusterHost, mirrorCollaboratorColumns, mirrorCollaboratorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.MirrorCollaborator, error) {
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
}
