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

// newRepoMirrorCollaboratorsCmd wires `repo mirror collaborators add|remove|
// list`: per-mirror access management. These hit the user-facing
// /mirrors/collaborators endpoints, which run a LIVE GitHub-admin check
// against the caller's own GitHub identity — the caller must be a current
// admin of the upstream (org repo) or its owner (user repo). Run them as
// yourself, not via a break-glass service-account token.
//
// The cluster-host is an optional trailing positional, defaulting to
// defaultClusterHost like the sibling create/remove. A grant is per-cell (a
// mirror is a per-cluster native repo with its own SpiceDB grant), so when a
// repo is mirrored on more than one cluster, pass the cluster explicitly to
// target the right placement.
func newRepoMirrorCollaboratorsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collaborators",
		Short: "Grant or revoke a user's access to a mirror (live GitHub-admin gated)",
	}
	cmd.AddCommand(newRepoMirrorCollaboratorsAddCmd())
	cmd.AddCommand(newRepoMirrorCollaboratorsRemoveCmd())
	cmd.AddCommand(newRepoMirrorCollaboratorsListCmd())
	return cmd
}

// parseMirrorRole maps the --role flag to the generated enum, rejecting
// unknown values at the CLI boundary so the user gets a clear message
// instead of a server 422. admin/manage are not grantable here — mirror
// collaboration is data access only.
func parseMirrorRole(s string) (coreapi.GrantMirrorCollaboratorInputBodyRole, error) {
	switch s {
	case "reader":
		return coreapi.GrantMirrorCollaboratorInputBodyRoleReader, nil
	case "writer":
		return coreapi.GrantMirrorCollaboratorInputBodyRoleWriter, nil
	default:
		return "", fmt.Errorf("invalid --role %q: must be \"reader\" or \"writer\"", s)
	}
}

func newRepoMirrorCollaboratorsAddCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "add <github-url> <handle> [cluster-host]",
		Short: "Grant a user reader/writer access to a mirror",
		Long: "Grants the user identified by <handle> reader (pull) or writer " +
			"(pull+push) access to the mirror of <github-url> on the target " +
			"cluster. <handle> must be QUALIFIED (e.g. github:alice). The caller " +
			"must be a live GitHub admin of the upstream (org repo) or its owner " +
			"(user repo). A grantee with no GitHub identity can still hold reader " +
			"(pull works without push-through). The cluster-host defaults to " +
			defaultClusterHost + " when omitted.",
		Example: "  entire repo mirror collaborators add github.com/acme/widget github:alice --role writer\n" +
			"  entire repo mirror collaborators add github.com/acme/widget github:alice eu-west-1.entire.io --role reader",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseGitHubURL(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid <github-url>: %w", err)
			}
			handle := args[1]
			r, err := parseMirrorRole(role)
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			clusterHost := clusterArgAt(args, 2)
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid [cluster-host]: %w", err)
			}
			return runCoreForCluster(cmd, clusterHost, func(ctx context.Context, c *coreapi.Client) error {
				granted, err := c.GrantMirrorCollaborator(ctx, &coreapi.GrantMirrorCollaboratorInputBody{
					Provider:    coreapi.GrantMirrorCollaboratorInputBodyProviderGithub,
					Owner:       owner,
					Repo:        repo,
					ClusterHost: clusterHost,
					Handle:      handle,
					Role:        r,
				})
				if err != nil {
					return err
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), granted)
				}
				cmd.Printf("Granted %s %s on github.com/%s/%s (%s)\n", handle, role, owner, repo, clusterHost)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "grant level: reader (pull) or writer (pull+push)")
	markRequired(cmd, "role")
	return cmd
}

func newRepoMirrorCollaboratorsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <github-url> <handle> [cluster-host]",
		Short: "Revoke a user's access to a mirror",
		Long: "Revokes the grant held by <handle> (qualified, e.g. github:alice) " +
			"on the mirror of <github-url> on the target cluster. Same live " +
			"GitHub-admin gate as `add`. Reports an error if no such grant " +
			"exists. The cluster-host defaults to " + defaultClusterHost +
			" when omitted.",
		Example: "  entire repo mirror collaborators remove github.com/acme/widget github:alice",
		Args:    cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseGitHubURL(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid <github-url>: %w", err)
			}
			handle := args[1]
			clusterHost := clusterArgAt(args, 2)
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid [cluster-host]: %w", err)
			}
			return runCoreForCluster(cmd, clusterHost, func(ctx context.Context, c *coreapi.Client) error {
				if err := c.RevokeMirrorCollaborator(ctx, coreapi.RevokeMirrorCollaboratorParams{
					Provider:    coreapi.RevokeMirrorCollaboratorProviderGithub,
					Owner:       owner,
					Repo:        repo,
					ClusterHost: clusterHost,
					Handle:      handle,
				}); err != nil {
					return err
				}
				cmd.Printf("Revoked %s on github.com/%s/%s (%s)\n", handle, owner, repo, clusterHost)
				return nil
			})
		},
	}
}

func newRepoMirrorCollaboratorsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <github-url> [cluster-host]",
		Short: "List the users with access to a mirror",
		Long: "Lists the principals that can pull the mirror of <github-url> on " +
			"the target cluster, with their reader/writer role resolved from the " +
			"control plane. Same live GitHub-admin gate as `add`/`remove`. The " +
			"cluster-host defaults to " + defaultClusterHost + " when omitted.",
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
