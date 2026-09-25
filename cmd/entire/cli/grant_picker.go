package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// The pool is the owning org's membership minus whoever holds a DIRECT grant on
// the target. Org membership does not itself grant project or repo access — the
// server's authz schema gives an org member `view` but neither `read` nor
// `write` — so every org member is a real candidate until they hold one.
//
// The `project:<name>` rows `ListRepoGrants` returns alongside the direct ones
// are deliberately NOT subtracted. Project access does reach the project's
// repos, but a member holding a repo only that way has no grant on the repo
// itself, so granting one is a real action: it pins the role here rather than
// following the project's. Subtracting them emptied the pool on any repo whose
// project already covered the org — see orgMemberCandidates, which is where
// this rule is argued out in full.
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
	// role is what the grantee holds on the target today, for the remove pools:
	// revoking is destructive and the row is the last thing the user reads
	// before confirming, so it says what is being taken away and not only from
	// whom. The add pools leave it empty — nobody in that pool holds anything
	// on the target yet — and nothing ever acts on it.
	role string
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

// option is the row as a picker shows it and as a prompt names it. label is the
// identity and is what every message about the grant says; the role is
// parenthesised after it where one is known, so a destructive choice is made
// and confirmed with the access in view.
//
// Parenthesised rather than spaced into a column because the labels are
// handles of every length, so no separator aligns them — and the single-grantee
// confirmation puts this inside a sentence ("Revoke github:alice (admin) from
// project widgets?"), where a column gap reads as a typo.
//
// Source is deliberately absent: the remove pools offer direct grants only, so
// it would be the same word on every row.
func (c grantCandidate) option() string {
	if c.role == "" {
		return c.label
	}
	return c.label + " (" + c.role + ")"
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
	items, _, err := boundedList(ctx, 0, fetch)
	return items, err
}

// boundedList is pagedList with a fetch budget, reporting whether the walk
// stopped with entries left behind.
//
// A listing that IS a pool is fetched this way: it is read before a single row
// can be shown, so on a large org an unbounded walk costs one round trip per
// page before the picker appears, and a pool longer than the budget is past
// being choosable from anyway. A listing used as a FILTER — the grants
// subtracted from the add pool — stays unbounded, because a partial filter
// would offer someone the target they already hold.
func boundedList[T any](ctx context.Context, budget int, fetch func(ctx context.Context, pageToken coreapi.OptString) (items []T, next coreapi.OptString, err error)) ([]T, bool, error) {
	return fetchPagesBounded(ctx, budget, func(ctx context.Context, cursor string) ([]T, string, error) {
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

// listWindow is what a bounded walk knows about its own completeness: how many
// listing rows it READ, and whether more were left behind.
//
// scanned counts rows read, never rows kept. Every use of it is a statement
// about how much of a listing something covers, and the pools drop rows — an
// org member who already holds the target, a grant the owner row carries — so
// "the first 800 members" would name a number that was never the first
// anything. It is also what makes an empty pool sayable: with more rows
// unread, "every member already has a grant" is a claim about an org this
// never looked at.
type listWindow struct {
	scanned int
	partial bool
}

// partialPoolNote is what a truncated pool has to say for itself. A picker is a
// convenience over a listing the control plane cannot filter or sort, so a
// truncated one is still useful — what it must not do is look complete.
//
// It is text for the picker to SHOW, not a line to print before it. The form
// may be rendering on the controlling terminal precisely because stderr is
// redirected (see runPromptForm), so a caveat written to a stream is one the
// person reading the list never sees — and it would be missing in exactly the
// case the routing exists for. Carried to the screen on grantPickerTarget.
func partialPoolNote(w listWindow, what string) string {
	return fmt.Sprintf("Only the first %d %s were read — cancel and name the grantee if the one you want is not listed.", w.scanned, what)
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
	window      listWindow // how much of the org's membership this read
	addressable int        // of the members read, the ones that can be granted at all
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
	members, partial, err := boundedList(ctx, coreListFetchBudget, listOrgMembers(c, orgID))
	if err != nil {
		return memberPool{}, err
	}
	pool := memberPool{window: listWindow{scanned: len(members), partial: partial}}
	for _, m := range members {
		handle, ok := grantableMember(m)
		if !ok {
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

// listOrgMembers is the one org-membership page call, shared by the pool that
// offers members for adding and the pool that offers them for removing.
func listOrgMembers(c *coreapi.Client, orgID string) func(context.Context, coreapi.OptString) ([]coreapi.Membership, coreapi.OptString, error) {
	return func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.Membership, coreapi.OptString, error) {
		out, err := c.ListOrgMembers(ctx, coreapi.ListOrgMembersParams{OrgId: orgID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	}
}

// grantableMember reports whether a membership row is worth offering in a
// picker. Both pools apply it, so the two agree on who is addressable — and on
// the remove side that matters twice over, because revoking walks the chosen
// set and returns on the first error, so one unaddressable row would strand
// every row after it.
//
// The handle is the load-bearing half: every org route goes through the
// (provider, providerUserId) pair resolveGranteeProvider derives from it, so a
// row without one cannot be acted on at all.
//
// The status half is a deliberately conservative guess, NOT an established
// server behaviour. Membership.status is an unconstrained string in
// core.openapi.json — no enum — and every membership observable from here (47
// rows across four orgs) is "active" WITH a handle, so nothing proves that a
// non-active row would fail to resolve, or even that one can carry a handle.
// What makes the guess safe to keep is that it only hides a row from a
// PICKER: `<noun> grant remove <ref> provider:handle` still addresses anyone
// the server will resolve. Verify against the control plane's real status
// vocabulary before relying on this as a rule, or before extending it to a
// path where being filtered out is the end of the road.
func grantableMember(m coreapi.Membership) (handle string, ok bool) {
	handle = strings.TrimSpace(m.Handle.Or(""))
	return handle, handle != "" && m.Status == orgMembershipActive
}

// listProjectGrants and listRepoGrants are the one page call per target,
// shared by the pool that subtracts these rows and the pool that offers them.
func listProjectGrants(c *coreapi.Client, projectID string) func(context.Context, coreapi.OptString) ([]coreapi.ProjectGrant, coreapi.OptString, error) {
	return func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.ProjectGrant, coreapi.OptString, error) {
		out, err := c.ListProjectMembers(ctx, coreapi.ListProjectMembersParams{ProjectId: projectID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	}
}

func listRepoGrants(c *coreapi.Client, repoID string) func(context.Context, coreapi.OptString) ([]coreapi.RepoGrant, coreapi.OptString, error) {
	return func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.RepoGrant, coreapi.OptString, error) {
		out, err := c.ListRepoGrants(ctx, coreapi.ListRepoGrantsParams{RepoId: repoID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Grants, out.NextPageToken, nil
	}
}

func projectGrantCandidates(ctx context.Context, c *coreapi.Client, projectID string) (memberPool, error) {
	orgID, err := owningOrgOf(ctx, c, projectID)
	if err != nil {
		return memberPool{}, err
	}
	// Unbounded: these rows are the filter, not the pool. A partial one would
	// offer a member the project access they already hold.
	grants, err := pagedList(ctx, listProjectGrants(c, projectID))
	if err != nil {
		return memberPool{}, err
	}
	return orgMemberCandidates(ctx, c, orgID, directHolders(mapRows(grants, projectGrantRowOf)))
}

// grantRow is one project or repo listing row reduced to what the pools read.
// ProjectGrant and RepoGrant carry the same five fields under two types that
// share no interface, so each is mapped once here rather than threaded through
// both pools as a handful of accessors apiece.
type grantRow struct {
	granteeID   string
	granteeType string
	source      string
	name        string
	role        string
}

func projectGrantRowOf(g coreapi.ProjectGrant) grantRow {
	return grantRow{granteeID: g.GranteeId, granteeType: g.GranteeType, source: g.Source, name: g.GranteeName.Or(""), role: g.Role}
}

func repoGrantRowOf(g coreapi.RepoGrant) grantRow {
	return grantRow{granteeID: g.GranteeId, granteeType: g.GranteeType, source: g.Source, name: g.GranteeName.Or(""), role: g.Role}
}

// directHolders is the set of accounts holding a grant written on the resource
// itself. A row inherited from the project is deliberately not in it: that is
// access to the project, not a grant on this target, so it neither blocks a
// grant here nor could be revoked here.
func directHolders(rows []grantRow) map[string]bool {
	held := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.source == grantSourceDirect {
			held[r.granteeID] = true
		}
	}
	return held
}

// mapRows converts a fetched listing page set into grantRows.
func mapRows[Row any](rows []Row, to func(Row) grantRow) []grantRow {
	out := make([]grantRow, len(rows))
	for i, r := range rows {
		out[i] = to(r)
	}
	return out
}

// repoGrantCandidates offers the members of the org owning the repo's project
// who hold no DIRECT grant on the repo. ListRepoGrants also returns the rows
// the repo inherits from its project; those are not subtracted, because
// granting on the repo itself is a real action for someone who only holds it
// through the project.
func repoGrantCandidates(ctx context.Context, c *coreapi.Client, repoID string) (memberPool, error) {
	repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
	if err != nil {
		return memberPool{}, err
	}
	orgID, err := owningOrgOf(ctx, c, repo.OwningProjectId)
	if err != nil {
		return memberPool{}, err
	}
	// Unbounded, for the reason projectGrantCandidates gives.
	grants, err := pagedList(ctx, listRepoGrants(c, repoID))
	if err != nil {
		return memberPool{}, err
	}
	// A repo lists its own grants and its project's; only the former counts.
	return orgMemberCandidates(ctx, c, orgID, directHolders(mapRows(grants, repoGrantRowOf)))
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
		picked, err := pickGrantees(cmd, t, grantAction, "Select grantees for "+t.describe(), candidates)
		if err != nil {
			return nil, err
		}
		chosen = picked
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
// granting: the noun and ref for its titles, the roles to offer, and which of
// them a row starts on.
type grantPickerTarget struct {
	noun  string
	ref   string
	roles []string
	// least is the least-privileged role, which is what an unanswered row
	// should grant and what an example in a refusal should suggest. It is named
	// rather than taken as roles[0], because help order runs in opposite
	// directions: reader/writer/admin is least first, owner/admin/member last.
	least string
	// poolNote qualifies the rows on offer — today, that the listing behind
	// them was truncated. Empty when the pool is everything there is. It rides
	// here rather than being printed by the caller so that it reaches whatever
	// writer the form does; see partialPoolNote.
	poolNote string
}

func (t grantPickerTarget) describe() string { return t.noun + " " + t.ref }

// runPickerScreen runs one of the picker's screens and reports a cancellation
// as the command's own error.
//
// The screen must not go to stdout: huh writes to stderr in its TUI mode but to
// STDOUT in accessible mode, and these commands can be asked for --json, so
// under ACCESSIBLE the prompts would land in the middle of the JSON a caller is
// parsing. Nor can it simply be pinned to stderr, which is redirected often
// enough (`grant add /et/acme/web 2>log`) that doing so renders an invisible
// prompt on an apparently hung command. runPromptForm resolves both, and hands
// back the writer it used so the cancellation lands where the user was looking.
func runPickerScreen(cmd *cobra.Command, action string, groups ...*huh.Group) error {
	render, err := runPromptForm(cmd, NewAccessibleForm(groups...))
	if err != nil {
		return cancelledPicker(render, action, err)
	}
	return nil
}

// The two things a picker screen can be backed out of. The grantee multi-select
// serves both flows, so what a cancelled one is called has to come from the
// caller; revokeAction matches the wording revokeConfirmed prints one screen
// later, so cancelling either half of a revoke reads the same.
const (
	grantAction  = "Grant"
	revokeAction = "Revocation"
)

// pickGrantees runs the multi-select over the offered candidates and returns
// the chosen rows.
//
// Rows rather than the refs huh binds to: every caller wants the whole
// candidate back — to show what each grantee holds, or to name it in the
// confirmation — so returning refs meant both callers rebuilding the same map
// afterwards, and an unchecked lookup on each. Recovering the row here makes
// that one lookup, and it is the same one that checks the answer was on offer.
func pickGrantees(cmd *cobra.Command, t grantPickerTarget, action, title string, candidates []grantCandidate) ([]grantCandidate, error) {
	offered := make(map[string]grantCandidate, len(candidates))
	options := make([]huh.Option[string], len(candidates))
	for i, c := range candidates {
		offered[c.ref] = c
		options[i] = huh.NewOption(c.option(), c.ref)
	}
	var selected []string
	// Filterable because the pool is a whole org's membership: at a few hundred
	// people an unfiltered list is an arrow-key scroll with no way to search.
	// The caveat goes in the TITLE, not in Description: huh's accessible mode
	// renders a field's title and its options and nothing else, so a
	// Description would be dropped for exactly the readers who cannot see the
	// styled form. Its own line, so neither mode runs the two together.
	if t.poolNote != "" {
		title += "\n" + t.poolNote
	}
	if err := runPickerScreen(cmd, action, huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title(title).
			Options(options...).
			Filterable(true).
			Height(uiform.SingleLineMultiSelectHeight(len(options))).
			Value(&selected),
	)); err != nil {
		return nil, err
	}
	// Confirming an empty selection is a decision not to grant anything, not a
	// failure — and it exited 1 with no message at all, which is the worst of
	// both. Nothing chosen, nothing done, exit 0.
	if len(selected) == 0 {
		return nil, nil
	}
	picked := make([]grantCandidate, 0, len(selected))
	for _, ref := range selected {
		// The form returned a value that was not on offer. Nothing has been
		// printed, so this must not be silent, or the command would exit
		// non-zero with no message — and taking the zero-valued row instead
		// would revoke against an empty ref and report it against an empty
		// label.
		c, ok := offered[ref]
		if !ok {
			return nil, fmt.Errorf("grantee %q was not among the %d offered", ref, len(candidates))
		}
		picked = append(picked, c)
	}
	return picked, nil
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
		if err := runPickerScreen(cmd, grantAction,
			huh.NewGroup(
				huh.NewNote().
					Title(fmt.Sprintf("Role for each grantee (set by --role %s)", fixedRole)).
					Description(b.String()),
			),
		); err != nil {
			return nil, err
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
		roles[i] = t.least
		fields[i] = huh.NewSelect[string]().
			Title(g.label).
			Options(options...).
			Inline(true).
			Value(&roles[i])
	}
	if err := runPickerScreen(cmd, grantAction, huh.NewGroup(fields...).Title("Role for each grantee")); err != nil {
		return nil, err
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
//
// render is the writer the form was shown on, not the command's stderr: when
// the prompt fell back to the controlling terminal, stderr is by definition not
// visible, and explaining an outcome into a stream the user is not reading is
// the same bug as prompting into one.
func cancelledPicker(render io.Writer, action string, err error) error {
	if cerr := handleFormCancellation(render, action, err); cerr != nil {
		return cerr
	}
	return NewSilentError(errors.New(strings.ToLower(action) + " cancelled"))
}

// pickerUnavailable explains why no picker can run, always naming the explicit
// grantee form so the command stays usable. A grantee never has to be an org
// member — the pool is a convenience, not the set of legal grantees.
func pickerUnavailable(t grantPickerTarget, reason string) error {
	example := fmt.Sprintf("entire %s grant add %s github:alice --role %s", t.noun, t.ref, t.least)
	return fmt.Errorf("%s; pass a grantee as provider:handle, e.g. %s", reason, example)
}

// revokeUnavailable is pickerUnavailable's remove-worded half: whatever stopped
// the picker, naming the grantee is the way through.
func revokeUnavailable(t grantPickerTarget, reason string) error {
	example := fmt.Sprintf("entire %s grant remove %s github:alice", t.noun, t.ref)
	return fmt.Errorf("%s; pass the grantee as provider:handle, e.g. %s", reason, example)
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
// name the server resolved and by the role the grant carries.
//
// A row the server could not name falls back to its grantee ULID rather than
// being dropped: that ULID is then the only identity the grant has, and a row
// left out of the only pool there is would be a grant this command cannot
// revoke at all.
func grantHolders(rows []grantRow) []grantCandidate {
	holders := make([]grantCandidate, 0, len(rows))
	for _, r := range rows {
		if r.granteeType != granteeTypeAccount || r.source != grantSourceDirect || r.granteeID == "" {
			continue
		}
		holders = append(holders, grantCandidate{
			ref:   r.granteeID,
			label: granteeName(coreapi.NewOptString(r.name), r.granteeID),
			role:  r.role,
			byID:  true,
		})
	}
	return holders
}

// fetchGrantHolders is the whole project/repo remove pool: walk the target's
// grant listing within the fetch budget, map the rows, keep the revocable ones.
// The two targets differ only in the call and the row type.
func fetchGrantHolders[Row any](
	ctx context.Context,
	fetch func(context.Context, coreapi.OptString) ([]Row, coreapi.OptString, error),
	to func(Row) grantRow,
) ([]grantCandidate, listWindow, error) {
	rows, partial, err := boundedList(ctx, coreListFetchBudget, fetch)
	if err != nil {
		return nil, listWindow{}, err
	}
	return grantHolders(mapRows(rows, to)), listWindow{scanned: len(rows), partial: partial}, nil
}

// grantSourceDirect is the source of a grant written on the resource itself,
// as opposed to one inherited from its project or implied by its owner.
const grantSourceDirect = "direct"

func projectGrantHolders(ctx context.Context, c *coreapi.Client, projectID string) ([]grantCandidate, listWindow, error) {
	return fetchGrantHolders(ctx, listProjectGrants(c, projectID), projectGrantRowOf)
}

func repoGrantHolders(ctx context.Context, c *coreapi.Client, repoID string) ([]grantCandidate, listWindow, error) {
	return fetchGrantHolders(ctx, listRepoGrants(c, repoID), repoGrantRowOf)
}

// orgMemberHolders lists the org's members for removal. They are addressed by
// handle, not ULID: org membership has no typed-id revoke route, so removal
// goes through the same provider identity a grant does — which is why this pool
// applies grantableMember, exactly as the add pool does. A member the routes
// cannot address is left out rather than offered and then refused, and here
// that matters twice over: revoking walks the chosen set and returns on the
// first error, so one unrevocable row would strand every selection after it.
//
// The role comes along for the row, so removing an owner does not look like
// removing anyone else.
func orgMemberHolders(ctx context.Context, c *coreapi.Client, orgID string) ([]grantCandidate, listWindow, error) {
	members, partial, err := boundedList(ctx, coreListFetchBudget, listOrgMembers(c, orgID))
	if err != nil {
		return nil, listWindow{}, err
	}
	holders := make([]grantCandidate, 0, len(members))
	for _, m := range members {
		handle, ok := grantableMember(m)
		if !ok {
			continue
		}
		h := handleCandidate(handle)
		h.role = m.Role
		holders = append(holders, h)
	}
	return holders, listWindow{scanned: len(members), partial: partial}, nil
}

// removePicker is the seam the remove flow's form sits behind, matching
// grantPicker's role for add.
var removePicker = func(cmd *cobra.Command, t grantPickerTarget, candidates []grantCandidate) ([]grantCandidate, error) {
	return pickGrantees(cmd, t, revokeAction, "Select grants to revoke on "+t.describe(), candidates)
}
