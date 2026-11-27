package cli

import (
	"context"
	"fmt"
	"strings"

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

// teamMemberChange is the --json record of one add.
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
					accountID, err := memberAccount(ctx, c, user)
					if err != nil {
						return err
					}
					picked = []grantCandidate{{ref: accountID, label: displayGranteeName(user), byID: true, handle: user}}
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

// memberAccount resolves a typed member, already checked as a handle, to its
// account ULID.
func memberAccount(ctx context.Context, c *coreapi.Client, user string) (string, error) {
	id, err := resolveAccountRef(ctx, c, user)
	if isCoreNotFound(err) {
		return "", fmt.Errorf("no identity for handle %q", user)
	}
	return id, err
}

// teamMemberPicker is the seam the add picker sits behind, as grantPicker is.
var teamMemberPicker = func(cmd *cobra.Command, pt grantPickerTarget, candidates []grantCandidate) ([]grantCandidate, error) {
	return pickGrantees(cmd, pt, "Add", "Select members to add to "+pt.describe(), candidates)
}

// pickTeamMembers offers the org's active members who are not on the team.
// An empty pool reports why and adds nobody.
func pickTeamMembers(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, orgRef, orgID string, team coreapi.Team) ([]grantCandidate, error) {
	pool, err := teamMemberPool(ctx, c, orgID, team.ID)
	if err != nil {
		return nil, err
	}
	if len(pool.candidates) == 0 {
		if pool.window.partial {
			return nil, fmt.Errorf("none of the first %d members of org %s can be added, and it has more; pass a user as provider:handle", pool.window.scanned, orgRef)
		}
		reportNothingToAdd(cmd, emptyTeamPoolReason(pool, orgRef, team.Name))
		return nil, nil
	}
	pt := grantPickerTarget{noun: cmdTeam, ref: team.Name}
	if pool.window.partial {
		pt.poolNote = partialPoolNote(pool.window, "members of org "+orgRef)
	}
	return teamMemberPicker(cmd, pt, pool.candidates)
}

// teamMemberPool is the org's addressable members who are not on the team.
func teamMemberPool(ctx context.Context, c *coreapi.Client, orgID, teamID string) (memberPool, error) {
	// Unbounded: the team's members are the filter.
	onTeam, err := pagedList(ctx, listTeamMembers(c, orgID, teamID))
	if err != nil {
		return memberPool{}, err
	}
	held := make(map[string]bool, len(onTeam))
	for _, m := range onTeam {
		held[m.AccountID] = true
	}
	members, partial, err := boundedList(ctx, coreListFetchBudget, listOrgMembers(c, orgID))
	if err != nil {
		return memberPool{}, err
	}
	pool := memberPool{window: listWindow{scanned: len(members), partial: partial}}
	for _, m := range members {
		handle, ok := grantableMember(m)
		// Core refuses a service account as a team member.
		if !ok || strings.HasPrefix(m.AccountId, saIDPrefix) {
			continue
		}
		pool.addressable++
		if held[m.AccountId] {
			continue
		}
		cand := memberCandidate(handle, m)
		cand.ref, cand.byID = m.AccountId, true
		pool.candidates = append(pool.candidates, cand)
	}
	return pool, nil
}

// emptyTeamPoolReason says why a complete pool is empty, as the grant
// picker's empty states do.
func emptyTeamPoolReason(pool memberPool, orgRef, team string) string {
	switch {
	case pool.window.scanned == 0:
		return fmt.Sprintf("org %s has no members to choose from", orgRef)
	case pool.addressable == 0:
		return fmt.Sprintf("no member of org %s can be added to team %s: none is active with a handle", orgRef, team)
	default:
		return fmt.Sprintf("every active member of org %s is already on team %s", orgRef, team)
	}
}

// addTeamMembers adds each picked account. A picker run moves past a refusal
// that is about one person, as grantEach does.
func addTeamMembers(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, orgRef, orgID string, team coreapi.Team, picked []grantCandidate, single bool) error {
	added := make([]teamMemberChange, 0, len(picked))
	failed := 0
	var teamRefusal error
	for _, p := range picked {
		err := c.AddTeamMember(ctx, coreapi.AddTeamMemberParams{OrgId: orgID, TeamID: team.ID, AccountID: p.ref})
		if err != nil {
			// A directory-managed team refuses every add, so it ends the batch.
			if single || coreapi.APIErrorType(err) == problemTypeTeamDirectoryManaged {
				teamRefusal = teamMemberErr(err, orgRef, team.Name, p.label)
				break
			}
			fatal := grantFailureIsFatal(err)
			fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s was not added to team %s: %v\n", p.label, team.Name, renderCoreError(teamMemberErr(err, orgRef, team.Name, p.label)))
			failed++
			if fatal {
				break
			}
			continue
		}
		member := displayGranteeName(p.handle)
		added = append(added, teamMemberChange{OrgID: orgID, TeamID: team.ID, AccountID: p.ref, Member: member})
		if !jsonRequested(cmd) {
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Added %s to team %s\n", member, team.Name)
		}
	}
	if err := emitTeamMemberJSON(cmd, added, single); err != nil {
		return err
	}
	if teamRefusal != nil {
		return teamRefusal
	}
	if failed > 0 {
		return NewSilentError(fmt.Errorf("%d of %d users were not added to team %s", len(picked)-len(added), len(picked), team.Name))
	}
	return nil
}

// emitTeamMemberJSON prints the added members under --json: one object for a
// typed user, an array for a picker run.
func emitTeamMemberJSON(cmd *cobra.Command, added []teamMemberChange, single bool) error {
	if !jsonRequested(cmd) {
		return nil
	}
	if single {
		if len(added) == 0 {
			return nil
		}
		return printJSON(cmd.OutOrStdout(), added[0])
	}
	return printJSON(cmd.OutOrStdout(), added)
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
			"A terminal asks for confirmation; pass --yes to skip it. Removing someone who is not on the team reports that and succeeds.",
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
				accountID, err := memberAccount(ctx, c, user)
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
					// The team resolved above, so a 404 means the user is not on it.
					if isCoreNotFound(err) {
						fmt.Fprintf(cmd.OutOrStdout(), "%s is not on team %s; nothing to remove\n", label, team.Name)
						return nil
					}
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
