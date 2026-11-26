package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

func newOrgTeamMemberCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "member",
		Short: "Manage a team's members",
	}
	cmd.AddCommand(newTeamMemberListCmd(), newTeamMemberAddCmd(), newTeamMemberRemoveCmd())
	return requireSubcommand(cmd)
}

var teamMemberColumns = []string{"MEMBER", colHeaderName, colHeaderStatus}

// teamMemberRow leaves STATUS blank for an active member.
func teamMemberRow(m coreapi.TeamMember) []string {
	status := ""
	if m.Status != orgMembershipActive {
		status = m.Status
	}
	return []string{teamMemberName(m), orDash(granteeDisplayName(m.DisplayName)), orDash(status)}
}

// teamMemberName is the member as the member commands take it, or the best
// name Core sent when it has no handle to show.
func teamMemberName(m coreapi.TeamMember) string {
	if h := m.Handle.Or(""); h != "" {
		return displayGranteeName(h)
	}
	if u := m.Username.Or(""); u != "" {
		return u
	}
	return m.AccountID
}

func newTeamMemberListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list <org> <team>",
		Short:   "List a team's members",
		Long:    "List a team's members. STATUS shows an org membership that is not active.",
		Example: "  entire org team member list acme platform",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, "No members found.", teamMemberColumns, teamMemberRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.TeamMember, error) {
				orgID, team, err := resolveOrgTeam(ctx, c, args[0], args[1])
				if err != nil {
					return nil, err
				}
				return pagedList(ctx, listTeamMembers(c, orgID, team.ID))
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func listTeamMembers(c *coreapi.Client, orgID, teamID string) func(context.Context, coreapi.OptString) ([]coreapi.TeamMember, coreapi.OptString, error) {
	return func(ctx context.Context, cursor coreapi.OptString) ([]coreapi.TeamMember, coreapi.OptString, error) {
		out, err := c.ListTeamMembers(ctx, coreapi.ListTeamMembersParams{OrgId: orgID, TeamID: teamID, Cursor: cursor, PerPage: coreapi.NewOptInt64(teamPageSize)})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextCursor, nil
	}
}

// teamMemberChange is the --json record of one add or remove; Core answers
// both with 204.
type teamMemberChange struct {
	OrgID     string `json:"org_id"`
	TeamID    string `json:"team_id"`
	AccountID string `json:"account_id"`
	Member    string `json:"member"`
}

func newTeamMemberAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <org> <team> [user]",
		Short: "Add a user to a team",
		Long: "Add a user (addressed as provider:handle, e.g. github:alice) to a team. " +
			"Only an active member of the org can join one of its teams. " +
			"Omit the user on a terminal to choose from the org's active members who are not on the team.",
		Example: "  entire org team member add acme platform github:alice\n" +
			"  entire org team member add acme platform",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			orgRef, teamName := args[0], args[1]
			user := ""
			if len(args) == 3 {
				user = args[2]
				if err := ensureMemberIsHandle(user); err != nil {
					return err
				}
			}
			if user == "" && !interactive.CanPromptInteractively() {
				return fmt.Errorf("no user given; pass one as provider:handle, e.g. entire org team member add %s %s github:alice", orgRef, teamName)
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, team, err := resolveOrgTeam(ctx, c, orgRef, teamName)
				if err != nil {
					return err
				}
				var picked []grantCandidate
				if user != "" {
					accountID, err := resolveHandleAccount(ctx, c, user)
					if err != nil {
						return err
					}
					picked = []grantCandidate{{ref: accountID, label: displayGranteeName(user), byID: true}}
				} else if picked, err = pickTeamMembers(ctx, cmd, c, orgRef, orgID, team); err != nil {
					return err
				}
				return addTeamMembers(ctx, cmd, c, orgRef, orgID, team, picked, user != "")
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// ensureMemberIsHandle refuses a ULID or a team as a team member.
func ensureMemberIsHandle(user string) error {
	if _, ok := teamGranteeName(user); ok {
		return fmt.Errorf("%s is a team; a team member is a user like github:alice", user)
	}
	return ensureGranteeIsHandle(user)
}

// resolveHandleAccount resolves a provider-qualified handle to its account ULID.
func resolveHandleAccount(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	provider, handle, err := parseGranteeHandle(ref)
	if err != nil {
		return "", err
	}
	id, err := c.ResolveHandle(ctx, coreapi.ResolveHandleParams{Provider: provider, Handle: handle})
	if err != nil {
		if isCoreNotFound(err) {
			return "", fmt.Errorf("no %s identity for handle %q", provider, identityFor(provider).displayHandle(handle))
		}
		return "", err
	}
	if id.AccountId == "" {
		return "", fmt.Errorf("handle %q resolved to no account", ref)
	}
	return id.AccountId, nil
}

// teamMemberPicker is the seam the add picker sits behind, as grantPicker is.
var teamMemberPicker = func(cmd *cobra.Command, pt grantPickerTarget, candidates []grantCandidate) ([]grantCandidate, error) {
	return pickGrantees(cmd, pt, "Add", "Select members to add to "+pt.describe(), candidates)
}

// pickTeamMembers offers the org's active members who are not on the team.
// An empty pool reports why and adds nobody.
func pickTeamMembers(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, orgRef, orgID string, team coreapi.Team) ([]grantCandidate, error) {
	pt := grantPickerTarget{noun: cmdTeam, ref: team.Name}
	// Unbounded: the team's members are the filter.
	onTeam, err := pagedList(ctx, listTeamMembers(c, orgID, team.ID))
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(onTeam))
	for _, m := range onTeam {
		held[m.AccountID] = true
	}
	members, partial, err := boundedList(ctx, coreListFetchBudget, listOrgMembers(c, orgID))
	if err != nil {
		return nil, err
	}
	var candidates []grantCandidate
	for _, m := range members {
		handle, ok := grantableMember(m)
		if !ok || held[m.AccountId] {
			continue
		}
		cand := memberCandidate(handle, m)
		cand.ref, cand.byID = m.AccountId, true
		candidates = append(candidates, cand)
	}
	window := listWindow{scanned: len(members), partial: partial}
	if len(candidates) == 0 {
		if window.partial {
			return nil, fmt.Errorf("none of the first %d members of org %s can be added, and it has more; pass a user as provider:handle", window.scanned, orgRef)
		}
		reportNothingToAdd(cmd, fmt.Sprintf("every active member of org %s is already on team %s", orgRef, team.Name))
		return nil, nil
	}
	if window.partial {
		pt.poolNote = partialPoolNote(window, "members of org "+orgRef)
	}
	return teamMemberPicker(cmd, pt, candidates)
}

// addTeamMembers adds each picked account. A picker run moves past a refusal
// that is about one person, as grantEach does.
func addTeamMembers(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, orgRef, orgID string, team coreapi.Team, picked []grantCandidate, single bool) error {
	added := make([]teamMemberChange, 0, len(picked))
	failed := 0
	for _, p := range picked {
		err := c.AddTeamMember(ctx, coreapi.AddTeamMemberParams{OrgId: orgID, TeamID: team.ID, AccountID: p.ref})
		if err != nil {
			fatal := grantFailureIsFatal(err)
			err = teamMemberErr(err, orgRef, team.Name, p.label)
			if single {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s was not added to team %s: %v\n", p.label, team.Name, renderCoreError(err))
			failed++
			if fatal {
				break
			}
			continue
		}
		added = append(added, teamMemberChange{OrgID: orgID, TeamID: team.ID, AccountID: p.ref, Member: p.label})
		if !jsonRequested(cmd) {
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Added %s to team %s\n", p.label, team.Name)
		}
	}
	if jsonRequested(cmd) {
		var err error
		if single {
			err = printJSON(cmd.OutOrStdout(), added[0])
		} else {
			err = printJSON(cmd.OutOrStdout(), added)
		}
		if err != nil {
			return err
		}
	}
	if failed > 0 {
		return NewSilentError(fmt.Errorf("%d of %d users were not added to team %s", len(picked)-len(added), len(picked), team.Name))
	}
	return nil
}

// teamMemberErr turns Core's member refusals into what to do next.
func teamMemberErr(err error, orgRef, team, member string) error {
	switch coreapi.APIErrorType(err) {
	case problemTypeInvitationRequired:
		return fmt.Errorf("%s is not an active member of org %s; invite them to the org first: entire org invite send %s --account %s", member, orgRef, orgRef, member)
	case problemTypeTeamDirectoryManaged:
		return fmt.Errorf("team %s is managed by your directory; change its members in your identity provider", team)
	}
	return err
}

// teamMemberRemoveConfirmed is the seam the removal prompt sits behind.
var teamMemberRemoveConfirmed = func(cmd *cobra.Command, member, team string) (bool, error) {
	return confirmPrompt(cmd, "Removal", fmt.Sprintf("Remove %s from team %s?", member, team), "", nil)
}

func newTeamMemberRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <org> <team> <user>",
		Short: "Remove a user from a team",
		Long: "Remove a user (addressed as provider:handle) from a team. " +
			"A terminal asks for confirmation; pass --yes to skip it. Removing someone who is not on the team succeeds.",
		Example: "  entire org team member remove acme platform github:alice",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			orgRef, teamName, user := args[0], args[1], args[2]
			if err := ensureMemberIsHandle(user); err != nil {
				return err
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, team, err := resolveOrgTeam(ctx, c, orgRef, teamName)
				if err != nil {
					return err
				}
				accountID, err := resolveHandleAccount(ctx, c, user)
				if err != nil {
					return err
				}
				label := displayGranteeName(user)
				if !forceRequested(cmd) && interactive.CanPromptInteractively() {
					proceed, err := teamMemberRemoveConfirmed(cmd, label, team.Name)
					if err != nil || !proceed {
						return err
					}
				}
				params := coreapi.RemoveTeamMemberParams{OrgId: orgID, TeamID: team.ID, AccountID: accountID}
				if err := c.RemoveTeamMember(ctx, params); err != nil {
					return teamMemberErr(err, orgRef, team.Name, label)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed %s from team %s\n", label, team.Name)
				return nil
			})
		},
	}
	addYesFlag(cmd)
	return cmd
}
