package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
)

// The interactive half of `<noun> grant add`: when the grantee argument is
// omitted, offer the people who could be granted the target and collect a role
// for each.
//
// The pool is the owning org's membership minus whoever already holds the
// target. Org membership does not itself grant project or repo access — the
// server's authz schema gives an org member `view` but neither `read` nor
// `write` — so every org member is a real candidate until they hold a grant.
// Project access DOES reach the project's repos, which is why the repo pool
// subtracts the `project:<name>` rows `ListRepoGrants` already returns
// alongside the direct ones: offering someone access they hold through the
// project would be offering a no-op.
//
// Org membership is also the only pool whose entries can be granted directly.
// Project and repo grant rows carry a grantee ULID and no provider identity,
// and there is no reverse lookup from one to the other, whereas a Membership
// carries the provider-qualified handle that resolveGranteeProvider already
// takes. So a selection is a handle string and rejoins the typed path every
// other grantee takes.

// grantCandidate is one row a picker offers. ref is how the command addresses
// that grantee — the same spelling a user could have typed — so a selection
// rejoins the typed path instead of needing one of its own. label is what the
// picker shows.
//
// The two differ only when removing: a project or repo grant is revoked by
// account ULID through a typed route, which needs no handle lookup and cannot
// be defeated by a handle that has since been renamed, while the row still
// shows the friendly name the server resolved. When adding, and for org
// membership either way, ref is the provider-qualified handle and the two are
// the same string.
type grantCandidate struct {
	ref   string
	label string
	// byID routes the revoke through the typed-id route rather than resolving
	// the ref as a handle. Set only by the remove pools, which read the grantee
	// ULID off a listing row; it is never shown and never typed. Sniffing the
	// ref's shape instead would both mistake a non-ULID grantee id for a handle
	// and let a user reach the typed-id route by pasting one, which the CLI no
	// longer accepts — a grantee is a provider-qualified handle and nothing else.
	byID bool
}

// handleCandidate is a candidate addressed and shown by its handle.
func handleCandidate(handle string) grantCandidate {
	return grantCandidate{ref: handle, label: handle}
}

// grantSelection pairs a chosen grantee with the role to grant them. Roles are
// per grantee rather than one role for the whole selection, so a single run can
// add a reader and an admin together.
type grantSelection struct {
	handle string
	role   string
}

// pagedList walks a control-plane listing to the end. Every one of these
// listings takes an OptString cursor and returns an OptString next token, so
// the conversion at both ends is the same three times over; only the call in
// the middle differs.
func pagedList[T any](ctx context.Context, fetch func(ctx context.Context, pageToken coreapi.OptString) (items []T, next coreapi.OptString, err error)) ([]T, error) {
	return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]T, string, error) {
		var pageToken coreapi.OptString
		if cursor != "" {
			pageToken = coreapi.NewOptString(cursor)
		}
		items, next, err := fetch(ctx, pageToken)
		if err != nil {
			return nil, "", err
		}
		return items, next.Or(""), nil
	})
}

// ownerNotOrgError reports that a project is owned by an account rather than an
// org, so no membership list exists to draw candidates from. It carries the
// project's name because the target phrases the refusal itself: a repo's message
// has to name the project standing between it and the missing org.
type ownerNotOrgError struct{ project string }

func (e *ownerNotOrgError) Error() string {
	return fmt.Sprintf("project %s is owned by an account, not an org", e.project)
}

// owningOrgOf returns the ULID of the org owning projectID, or errOwnerNotOrg
// when an account owns it.
func owningOrgOf(ctx context.Context, c *coreapi.Client, projectID string) (string, error) {
	p, err := c.GetProject(ctx, coreapi.GetProjectParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	if p.OwnerType != coreapi.ProjectOwnerTypeOrg || p.OwnerId == "" {
		return "", &ownerNotOrgError{project: p.Name}
	}
	return p.OwnerId, nil
}

// memberPool is the add picker's candidate set plus the counts needed to say
// why it is empty: an org with no members, one whose members cannot be
// addressed, and one where everyone already holds a grant are three different
// answers to "who can I add?".
type memberPool struct {
	candidates  []grantCandidate
	total       int // members the org has
	addressable int // of those, the ones that can be granted at all
}

// orgMemberCandidates lists the owning org's members who can be granted the
// target.
//
// **A member with a DIRECT grant on the target is left out; one who only holds
// it through the project is offered.** Those are different situations, and
// conflating them is what made two earlier versions of this pool wrong in
// opposite directions. Someone with a direct grant has nothing to add here —
// `grant add <target> <handle> --role` changes their role, which is the typed
// form's job. Someone whose access comes from the project has no grant on THIS
// target, so granting one is a real action: it pins the role on this repo
// rather than following the project's. Subtracting them too emptied the pool on
// any repo whose project already covered the org.
//
// Every candidate is shown as its plain handle. An earlier version annotated
// the inherited ones with the role and project they came from, which is true
// but is detail about a grant the user is not editing, in a list whose question
// is only "who". `grant list` is where the current state is read.
//
// Members that are not active, or that carry no handle, are dropped whatever
// they hold: neither can be resolved to the (provider, providerUserId) pair the
// grant routes need, so offering one would produce a selection that fails at
// the grant.
func orgMemberCandidates(ctx context.Context, c *coreapi.Client, orgID string, directHolders map[string]bool) (memberPool, error) {
	members, err := pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.Membership, coreapi.OptString, error) {
		out, err := c.ListOrgMembers(ctx, coreapi.ListOrgMembersParams{OrgId: orgID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	})
	if err != nil {
		return memberPool{}, err
	}
	pool := memberPool{total: len(members)}
	for _, m := range members {
		handle := strings.TrimSpace(m.Handle.Or(""))
		if handle == "" || m.Status != orgMembershipActive {
			continue
		}
		pool.addressable++
		if directHolders[m.AccountId] {
			continue
		}
		pool.candidates = append(pool.candidates, handleCandidate(handle))
	}
	return pool, nil
}

// orgMembershipActive is the Membership.Status of a member who has joined. Any
// other status (invited, pending) may have no resolvable provider identity yet.
const orgMembershipActive = "active"

func projectGrantCandidates(ctx context.Context, c *coreapi.Client, projectID string) (memberPool, error) {
	orgID, err := owningOrgOf(ctx, c, projectID)
	if err != nil {
		return memberPool{}, err
	}
	grants, err := pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.ProjectGrant, coreapi.OptString, error) {
		out, err := c.ListProjectMembers(ctx, coreapi.ListProjectMembersParams{ProjectId: projectID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	})
	if err != nil {
		return memberPool{}, err
	}
	return orgMemberCandidates(ctx, c, orgID, directHolders(grants, func(g coreapi.ProjectGrant) (string, string) {
		return g.GranteeId, g.Source
	}))
}

// directHolders is the set of accounts holding a grant written on the resource
// itself. A row inherited from the project is deliberately not in it: that is
// access to the project, not a grant on this target, so it neither blocks a
// grant here nor could be revoked here.
func directHolders[Row any](rows []Row, key func(Row) (granteeID, source string)) map[string]bool {
	held := make(map[string]bool, len(rows))
	for _, r := range rows {
		if id, source := key(r); source == grantSourceDirect {
			held[id] = true
		}
	}
	return held
}

// repoGrantCandidates offers the members of the org owning the repo's project
// who hold no grant on the repo — neither a direct one nor one inherited from
// the project, both of which ListRepoGrants returns.
func repoGrantCandidates(ctx context.Context, c *coreapi.Client, repoID string) (memberPool, error) {
	repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
	if err != nil {
		return memberPool{}, err
	}
	orgID, err := owningOrgOf(ctx, c, repo.OwningProjectId)
	if err != nil {
		return memberPool{}, err
	}
	grants, err := pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.RepoGrant, coreapi.OptString, error) {
		out, err := c.ListRepoGrants(ctx, coreapi.ListRepoGrantsParams{RepoId: repoID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Grants, out.NextPageToken, nil
	})
	if err != nil {
		return memberPool{}, err
	}
	// A repo lists its own grants and its project's; only the former counts.
	return orgMemberCandidates(ctx, c, orgID, directHolders(grants, func(g coreapi.RepoGrant) (string, string) {
		return g.GranteeId, g.Source
	}))
}

// grantPicker is the single seam the picker's forms sit behind. Production
// wiring is runGrantPicker; command-level tests swap it, because the forms are
// unreachable under `go test` (CanPromptInteractively is false there). One seam
// rather than one per screen, so a test states the whole outcome in one place.
var grantPicker = runGrantPicker

// runGrantPicker collects the grantee/role pairs. known non-empty means the
// grantee came from the command line and only roles are wanted, which is the
// one-row version of the same second screen. fixedRole non-empty means --role
// was given: the role rows are then displayed rather than offered, so the user
// still sees what each grantee will receive but cannot change it.
func runGrantPicker(cmd *cobra.Command, t grantPickerTarget, candidates []grantCandidate, known []string, fixedRole string) ([]grantSelection, error) {
	chosen := make([]grantCandidate, 0, len(known))
	for _, h := range known {
		chosen = append(chosen, handleCandidate(h))
	}
	if len(chosen) == 0 {
		refs, err := pickGrantees(cmd, "Select grantees for "+t.describe(), candidates)
		if err != nil {
			return nil, err
		}
		// Back to whole rows, so the role screen can show what each grantee
		// already holds and default their row to it.
		byRef := make(map[string]grantCandidate, len(candidates))
		for _, c := range candidates {
			byRef[c.ref] = c
		}
		for _, ref := range refs {
			chosen = append(chosen, byRef[ref])
		}
		// Nobody chosen is a decision not to grant anything, so stop here.
		// Falling through asked for a role per grantee over an empty list,
		// which with --role is a note about nothing and without it hands huh a
		// group with no fields — and huh indexes its first field unguarded, so
		// that panics rather than merely looking odd.
		if len(chosen) == 0 {
			return nil, nil
		}
	}
	return pickRoles(cmd, t, chosen, fixedRole)
}

// grantPickerTarget is what the picker needs to know about the target it is
// granting: the noun and ref for its titles, and the roles to offer.
type grantPickerTarget struct {
	noun  string
	ref   string
	roles []string
}

func (t grantPickerTarget) describe() string { return t.noun + " " + t.ref }

// promptForm is NewAccessibleForm with the form's own output pinned to stderr.
// huh writes to STDERR in its TUI mode but to STDOUT in accessible mode, and
// these commands can be asked for --json, so under ACCESSIBLE the prompts would
// land in the middle of the JSON a caller is parsing. Stderr is where a prompt
// belongs whenever stdout is carrying data; for the TUI mode this restates the
// default rather than changing it.
func promptForm(cmd *cobra.Command, groups ...*huh.Group) *huh.Form {
	return NewAccessibleForm(groups...).WithOutput(cmd.ErrOrStderr())
}

// pickGrantees runs the multi-select over the offered candidates, returning the
// refs of the chosen rows.
func pickGrantees(cmd *cobra.Command, title string, candidates []grantCandidate) ([]string, error) {
	offered := make(map[string]bool, len(candidates))
	options := make([]huh.Option[string], len(candidates))
	for i, c := range candidates {
		offered[c.ref] = true
		options[i] = huh.NewOption(c.label, c.ref)
	}
	var selected []string
	form := promptForm(cmd,
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title(title).
				Options(options...).
				Height(uiform.SingleLineMultiSelectHeight(len(options))).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(cmd.Context()); err != nil {
		return nil, cancelledPicker(cmd, err)
	}
	// Confirming an empty selection is a decision not to grant anything, not a
	// failure — and it exited 1 with no message at all, which is the worst of
	// both. Nothing chosen, nothing done, exit 0.
	if len(selected) == 0 {
		return nil, nil
	}
	for _, h := range selected {
		// The form returned a value that was not on offer. Nothing has been
		// printed, so this must not be silent, or the command would exit
		// non-zero with no message.
		if !offered[h] {
			return nil, fmt.Errorf("grantee %q was not among the %d offered", h, len(candidates))
		}
	}
	return selected, nil
}

// pickRoles collects a role per grantee. With fixedRole set the rows are a note
// instead of selects: huh notes are non-focusable, so the roles are shown as
// already decided and cannot be edited.
func pickRoles(cmd *cobra.Command, t grantPickerTarget, chosen []grantCandidate, fixedRole string) ([]grantSelection, error) {
	if fixedRole != "" {
		var b strings.Builder
		for _, g := range chosen {
			fmt.Fprintf(&b, "%s%s  %s\n", uiform.SelectOptionIndent, g.label, fixedRole)
		}
		form := promptForm(cmd,
			huh.NewGroup(
				huh.NewNote().
					Title(fmt.Sprintf("Role for each grantee (set by --role %s)", fixedRole)).
					Description(b.String()),
			),
		)
		if err := form.RunWithContext(cmd.Context()); err != nil {
			return nil, cancelledPicker(cmd, err)
		}
		out := make([]grantSelection, len(chosen))
		for i, g := range chosen {
			out[i] = grantSelection{handle: g.ref, role: fixedRole}
		}
		return out, nil
	}

	options := make([]huh.Option[string], len(t.roles))
	for i, r := range t.roles {
		options[i] = huh.NewOption(r, r)
	}
	// Each row binds its own element, so the selections stay independent.
	roles := make([]string, len(chosen))
	fields := make([]huh.Field, len(chosen))
	for i, g := range chosen {
		roles[i] = t.roles[0] // least-privileged default, per the target's help order
		fields[i] = huh.NewSelect[string]().
			Title(g.label).
			Options(options...).
			Inline(true).
			Value(&roles[i])
	}
	form := promptForm(cmd, huh.NewGroup(fields...).Title("Role for each grantee"))
	if err := form.RunWithContext(cmd.Context()); err != nil {
		return nil, cancelledPicker(cmd, err)
	}
	out := make([]grantSelection, len(chosen))
	for i, g := range chosen {
		if err := validateRole(roles[i], t.roles); err != nil {
			return nil, err
		}
		out[i] = grantSelection{handle: g.ref, role: roles[i]}
	}
	return out, nil
}

// cancelledPicker converts a form error into the command's. A Ctrl+C becomes a
// SilentError so the caller stops without main.go reprinting the "cancelled."
// line handleFormCancellation already wrote; a real form failure propagates.
func cancelledPicker(cmd *cobra.Command, err error) error {
	if cerr := handleFormCancellation(cmd.ErrOrStderr(), "Grant", err); cerr != nil {
		return cerr
	}
	return NewSilentError(errors.New("grant cancelled"))
}

// pickerUnavailable explains why no picker can run, always naming the explicit
// grantee form so the command stays usable. A grantee never has to be an org
// member — the pool is a convenience, not the set of legal grantees.
func pickerUnavailable(t grantPickerTarget, reason string) error {
	example := fmt.Sprintf("entire %s grant add %s github:alice --role %s", t.noun, t.ref, t.roles[0])
	return fmt.Errorf("%s; pass a grantee as provider:handle, e.g. %s", reason, example)
}

// The remove half. Its pool is the inverse of add's — who holds the target
// now — and unlike add it exists for all three targets: org membership cannot
// be offered for adding, because everyone eligible is by definition absent from
// the only list there is, but the members to REMOVE are exactly that list.
//
// A row is offered only when revoking it would actually do something. Two
// filters, each for a condition observed on a real listing:
//
//   - granteeType must be an account. The `owner` row is the owning org itself
//     and holds the target through the authz schema's owner relation rather
//     than a grant, so there is nothing to revoke; the typed revoke route sends
//     granteeType=account and could not address it anyway.
//   - source must be direct. A repo listing also carries the project's grants
//     as `project:<name>` rows, and those are held through the project, not the
//     repo: revoking one at the repo level answers "no such grant; nothing to
//     revoke", so offering it would be offering a no-op. Removing that access
//     means removing the project grant, which `project grant remove` does.

// grantHolders lists the account grants that can be revoked on a project or
// repo, addressed by ULID so no handle has to resolve, labelled by the friendly
// name the server resolved.
func grantHolders[Row any](rows []Row, granteeID, granteeType, source, name func(Row) string) []grantCandidate {
	holders := make([]grantCandidate, 0, len(rows))
	for _, r := range rows {
		if granteeType(r) != granteeTypeAccount || source(r) != grantSourceDirect {
			continue
		}
		id := granteeID(r)
		if id == "" {
			continue
		}
		holders = append(holders, grantCandidate{ref: id, label: granteeName(coreapi.NewOptString(name(r)), id), byID: true})
	}
	return holders
}

// grantSourceDirect is the source of a grant written on the resource itself,
// as opposed to one inherited from its project or implied by its owner.
const grantSourceDirect = "direct"

func projectGrantHolders(ctx context.Context, c *coreapi.Client, projectID string) ([]grantCandidate, error) {
	rows, err := pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.ProjectGrant, coreapi.OptString, error) {
		out, err := c.ListProjectMembers(ctx, coreapi.ListProjectMembersParams{ProjectId: projectID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	})
	if err != nil {
		return nil, err
	}
	return grantHolders(rows,
		func(g coreapi.ProjectGrant) string { return g.GranteeId },
		func(g coreapi.ProjectGrant) string { return g.GranteeType },
		func(g coreapi.ProjectGrant) string { return g.Source },
		func(g coreapi.ProjectGrant) string { return g.GranteeName.Or("") },
	), nil
}

func repoGrantHolders(ctx context.Context, c *coreapi.Client, repoID string) ([]grantCandidate, error) {
	rows, err := pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.RepoGrant, coreapi.OptString, error) {
		out, err := c.ListRepoGrants(ctx, coreapi.ListRepoGrantsParams{RepoId: repoID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Grants, out.NextPageToken, nil
	})
	if err != nil {
		return nil, err
	}
	return grantHolders(rows,
		func(g coreapi.RepoGrant) string { return g.GranteeId },
		func(g coreapi.RepoGrant) string { return g.GranteeType },
		func(g coreapi.RepoGrant) string { return g.Source },
		func(g coreapi.RepoGrant) string { return g.GranteeName.Or("") },
	), nil
}

// orgMemberHolders lists the org's members for removal. They are addressed by
// handle, not ULID: org membership has no typed-id revoke route, so a member
// whose handle the server did not resolve cannot be removed by this command at
// all and is left out rather than offered and then refused.
func orgMemberHolders(ctx context.Context, c *coreapi.Client, orgID string) ([]grantCandidate, error) {
	members, err := pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.Membership, coreapi.OptString, error) {
		out, err := c.ListOrgMembers(ctx, coreapi.ListOrgMembersParams{OrgId: orgID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	})
	if err != nil {
		return nil, err
	}
	holders := make([]grantCandidate, 0, len(members))
	for _, m := range members {
		if handle := strings.TrimSpace(m.Handle.Or("")); handle != "" {
			holders = append(holders, handleCandidate(handle))
		}
	}
	return holders, nil
}

// removePicker is the seam the remove flow's form sits behind, matching
// grantPicker's role for add.
var removePicker = func(cmd *cobra.Command, t grantPickerTarget, candidates []grantCandidate) ([]string, error) {
	return pickGrantees(cmd, "Select grants to revoke on "+t.describe(), candidates)
}
