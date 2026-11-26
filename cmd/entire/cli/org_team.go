package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// Teams are addressed by name within their org. Core keys every team route by
// ULID, so each command resolves the name through the org's team list.

const (
	problemTypeTeamNameInUse        = "https://entire.com/errors/team/name-in-use"
	problemTypeTeamDirectoryManaged = "https://entire.com/errors/team/directory-managed"
	problemTypeInvitationRequired   = "https://entire.com/errors/invitation/required"
)

// teamPageSize is Core's per_page maximum for the team listings.
const teamPageSize = 100

func newOrgTeamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdTeam,
		Short: "Manage an organization's teams",
		Long: "Manage an organization's teams. A team is addressed by name within its org. " +
			"Grant a team access with `entire project grant add <project> team:<name>` or `entire repo grant add <repo> team:<name>`.",
	}
	cmd.AddCommand(newOrgTeamListCmd(), newOrgTeamCreateCmd(), newOrgTeamRenameCmd(), newOrgTeamDeleteCmd(), newOrgTeamMemberCmd())
	return requireSubcommand(cmd)
}

var teamColumns = []string{colHeaderName, "MANAGED BY", colHeaderCreated}

func teamRow(t coreapi.Team) []string {
	managedBy := "-"
	if t.DirectoryManaged {
		managedBy = "directory"
	}
	return []string{t.Name, managedBy, t.CreatedAt.Format("2006-01-02")}
}

func newOrgTeamListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list <org>",
		Short:   "List an organization's teams",
		Long:    "List an organization's teams. The org is addressed by name or ULID. Teams that directory sync manages are read-only here.",
		Example: "  entire org team list acme",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, "No teams found.", teamColumns, teamRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Team, error) {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return listAllTeams(ctx, c, orgID)
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newOrgTeamCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "create <org> <name>",
		Short:   "Create a team",
		Long:    "Create a team in an organization. A name holds lowercase letters, digits and '-', and is unique in the org.",
		Example: "  entire org team create acme platform",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			orgRef, name := args[0], args[1]
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				orgID, err := resolveOrgRef(ctx, c, orgRef)
				if err != nil {
					return "", nil, err
				}
				team, err := c.CreateTeam(ctx, &coreapi.CreateTeamInputBody{Name: name}, coreapi.CreateTeamParams{OrgId: orgID})
				if err != nil {
					return "", nil, teamErr(err, orgRef, name)
				}
				return fmt.Sprintf("✓ Created team %s in org %s", team.Name, orgRef), team, nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newOrgTeamRenameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rename <org> <team> <new-name>",
		Short:   "Rename a team",
		Long:    "Rename a team. Grants and members stay with the team.",
		Example: "  entire org team rename acme platform infra",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			orgRef, name, newName := args[0], args[1], args[2]
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				orgID, team, err := resolveOrgTeam(ctx, c, orgRef, name)
				if err != nil {
					return "", nil, err
				}
				body := &coreapi.UpdateTeamInputBody{Name: coreapi.NewOptString(newName)}
				renamed, err := c.UpdateTeam(ctx, body, coreapi.UpdateTeamParams{OrgId: orgID, TeamID: team.ID})
				if err != nil {
					return "", nil, teamErr(err, orgRef, newName)
				}
				return fmt.Sprintf("✓ Renamed team %s to %s in org %s", team.Name, renamed.Name, orgRef), renamed, nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newOrgTeamDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete <org> <team>",
		Short:   "Delete a team",
		Long:    "Delete a team with its members and grants. A terminal asks for confirmation; pass --force to skip it, which a non-interactive run must.",
		Example: "  entire org team delete acme platform",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			orgRef, name := args[0], args[1]
			var orgID string
			return runControlPlaneDelete(cmd, "org "+orgRef+" team", name,
				func(ctx context.Context, c *coreapi.Client) (resolvedRef, error) {
					id, team, err := resolveOrgTeam(ctx, c, orgRef, name)
					orgID = id
					return resolvedRef{ID: team.ID, Name: team.Name}, err
				},
				func(ctx context.Context, c *coreapi.Client, teamID string) error {
					return teamErr(c.DeleteTeam(ctx, coreapi.DeleteTeamParams{OrgId: orgID, TeamID: teamID}), orgRef, name)
				})
		},
	}
	addForceFlag(cmd)
	return cmd
}

// listAllTeams walks every page of an org's teams.
func listAllTeams(ctx context.Context, c *coreapi.Client, orgID string) ([]coreapi.Team, error) {
	return pagedList(ctx, func(ctx context.Context, cursor coreapi.OptString) ([]coreapi.Team, coreapi.OptString, error) {
		out, err := c.ListTeams(ctx, coreapi.ListTeamsParams{OrgId: orgID, Cursor: cursor, PerPage: coreapi.NewOptInt64(teamPageSize)})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Teams, out.NextCursor, nil
	})
}

// findTeam finds a team by name in an org; ok is false on no match.
func findTeam(ctx context.Context, c *coreapi.Client, orgID, name string) (team coreapi.Team, ok bool, err error) {
	teams, err := listAllTeams(ctx, c, orgID)
	if err != nil {
		return coreapi.Team{}, false, fmt.Errorf("list teams: %w", err)
	}
	for _, t := range teams {
		if strings.EqualFold(t.Name, name) {
			return t, true, nil
		}
	}
	return coreapi.Team{}, false, nil
}

// resolveOrgTeam resolves an org ref and a team name in it.
func resolveOrgTeam(ctx context.Context, c *coreapi.Client, orgRef, name string) (string, coreapi.Team, error) {
	orgID, err := resolveOrgRef(ctx, c, orgRef)
	if err != nil {
		return "", coreapi.Team{}, err
	}
	team, ok, err := findTeam(ctx, c, orgID, name)
	if err != nil {
		return "", coreapi.Team{}, err
	}
	if !ok {
		return "", coreapi.Team{}, fmt.Errorf("no team named %q in org %s (run `entire org team list %s` to see team names)", name, orgRef, orgRef)
	}
	return orgID, team, nil
}

// teamErr turns Core's team refusals into what to do next. name is the team
// the request named.
func teamErr(err error, orgRef, name string) error {
	if err == nil {
		return nil
	}
	switch coreapi.APIErrorType(err) {
	case problemTypeTeamNameInUse:
		return fmt.Errorf("org %s already has a team named %s", orgRef, name)
	case problemTypeTeamDirectoryManaged:
		return fmt.Errorf("team %s is managed by your directory; change it in your identity provider", name)
	}
	return err
}
