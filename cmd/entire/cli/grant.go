package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
)

// The `grant` subtrees under `entire org`, `entire project` and `entire repo`:
// one builder, three target descriptions. Every leaf gives a user access to a
// target — `add <target> <grantee> --role`, `list <target>`, `remove <target>
// <grantee>` — and only the target kind changes: how it is addressed, which
// roles it has, and which generated client calls it maps to. The builder owns
// the command shape and the shared plumbing; a grantTarget owns the typed calls.
//
// Grantees are addressed by a provider-qualified handle (e.g. github:alice),
// which the CLI resolves to the provider account behind the scenes, and by
// nothing else: ensureGranteeIsHandle refuses an account ULID on all three
// targets, before any lookup, on add and remove alike. A user account is the
// only grantee kind the API grants to today.

// grantTarget describes one resource kind the shared `<noun> grant` subtree
// manages. Row is the wire type of one listing entry.
type grantTarget[Row any] struct {
	noun        string   // "org" | "project" | "repo": in Use, help and messages
	refUsage    string   // how a target is addressed, for Long: "name or ULID"; reads after "addressed by"
	exampleRef  string   // a target ref for the Example lines
	roles       []string // accepted --role values, in help order
	leastRole   string   // the least-privileged of roles; help order runs least-first for access but most-first for org, so it is named rather than indexed
	defaultRole string   // "" means --role is required; else the server default applied when --role is omitted
	columns     []string
	row         func(Row) []string

	// resolve turns the user's target ref into its ULID. project and repo wrap
	// theirs because those resolvers take an interface, not *coreapi.Client.
	resolve func(ctx context.Context, c *coreapi.Client, ref string) (string, error)
	// grant gives the provider account the role on the resolved target and
	// returns the effective role: the server's, when role was left to default.
	grant            func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (granted string, wire any, err error)
	list             func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]Row, coreapi.OptString, error)
	revokeByProvider func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error
	// revokeByID revokes by the grantee ULID a listing row carries, which needs
	// no handle lookup and survives a rename. Only the remove picker reaches
	// it — a typed ULID is refused — so it is nil on org, whose rows are
	// addressed by handle because org membership has no such route.
	revokeByID func(ctx context.Context, c *coreapi.Client, id, granteeID string) error
	// candidates lists who could be granted this target for the interactive
	// picker: members of the owning org with no direct grant on it, plus the
	// counts an empty pool needs to say why. nil where no pool is enumerable
	// (org), which is what leaves `org grant add` exactly as it was.
	candidates func(ctx context.Context, c *coreapi.Client, id string) (memberPool, error)
	// holders lists the grants on this target that revoking would actually
	// remove, for the remove picker, and reports whether the listing stopped at
	// the fetch budget with more left. Set on all three targets: the members to
	// remove from an org ARE an enumerable list, which is what the add side
	// lacks.
	holders func(ctx context.Context, c *coreapi.Client, id string) ([]grantCandidate, listWindow, error)
	// ownerNotOrg phrases an ownerNotOrgError for this target, given the name of
	// the account-owned project. The repo wording has to name the project
	// standing between the repo and the missing org, so one shared sentence
	// cannot serve both.
	ownerNotOrg func(pt grantPickerTarget, project string) string

	// unwritableRef reports a ref that names something this target's `add` and
	// `remove` cannot write — the repo target's GitHub mirrors, whose access is
	// the upstream repository's. Checked before required flags are validated or
	// anything is dialed, so the answer is about the repo the user named rather
	// than a missing --role they would then supply for nothing. nil where every
	// ref a target parses is writable (org, project).
	unwritableRef func(ref string) error

	// listBranch is the second reading `list` gives a target ref, for a target
	// whose refs do not all name the same thing: a repo is an Entire repository
	// or a GitHub mirror, and only the first has grants. nil for org and
	// project, which have one kind of target each.
	listBranch *grantListBranch
}

// grantListBranch is the second answer a `list` leaf can give: claims reports
// which refs it answers for, leaving every other ref to the target's own
// resolver, and list answers one of them. long and example become the leaf's
// help, because a leaf taking two kinds of ref is the only one with more to say
// than its Short.
type grantListBranch struct {
	long    string
	example string
	claims  func(ref string) bool
	list    func(cmd *cobra.Command, ref string) error
}

func newOrgGrantCmd() *cobra.Command     { return newGrantSubtreeCmd(orgGrantTarget) }
func newProjectGrantCmd() *cobra.Command { return newGrantSubtreeCmd(projectGrantTarget) }
func newRepoGrantCmd() *cobra.Command    { return newGrantSubtreeCmd(repoGrantTarget) }

func newGrantSubtreeCmd[Row any](t grantTarget[Row]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdGrant,
		Short: "Manage " + t.noun + " access",
	}
	cmd.AddCommand(newGrantAddCmd(t), newGrantListCmd(t), newGrantRemoveCmd(t))
	return requireSubcommand(cmd)
}

func newGrantAddCmd[Row any](t grantTarget[Row]) *cobra.Command {
	var role string
	required := t.defaultRole == ""
	example := fmt.Sprintf("  entire %s grant add %s github:alice", t.noun, t.exampleRef)
	roleHelp := "Role: one of " + strings.Join(t.roles, ", ")
	if required {
		example += " --role " + t.roles[0]
		roleHelp += " (required; asked for if omitted on a terminal)"
	} else {
		roleHelp += " (default " + t.defaultRole + ")"
	}
	// A target with no candidate pool (org) keeps the two-argument shape, so
	// cobra reports a missing grantee exactly as it always has.
	use := fmt.Sprintf("add <%s> <grantee>", t.noun)
	long := fmt.Sprintf("Grant a user (addressed as provider:handle, e.g. github:alice) %s access. The %s is addressed by %s.", t.noun, t.noun, t.refUsage)
	args := cobra.ExactArgs(2)
	if t.candidates != nil {
		use = fmt.Sprintf("add <%s> [grantee]", t.noun)
		long += fmt.Sprintf(" Omit the grantee on a terminal to choose from the members of the owning org who do not have %s access yet, and set a role for each.", t.noun)
		args = cobra.RangeArgs(1, 2)
	}
	cmd := &cobra.Command{
		Use:     use,
		Short:   fmt.Sprintf("Grant a user %s access", t.noun),
		Long:    long,
		Example: example,
		Args:    args,
		PreRunE: refuseUnwritableRef(t),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			// A role the user typed is always checked, an explicit `--role=`
			// included: an empty value is not the same as leaving the flag out.
			// An omitted --role means the server default where the target has
			// one, and otherwise is resolved per grantee below.
			if cmd.Flags().Changed("role") {
				if err := validateRole(role, t.roles); err != nil {
					return err
				}
			}
			pt := grantPickerTarget{noun: t.noun, ref: args[0], roles: t.roles, least: t.leastRole}
			grantee := ""
			if len(args) == 2 {
				grantee = args[1]
				if err := ensureGranteeIsHandle(grantee); err != nil {
					return err
				}
			}
			// Both refusals a non-interactive run can hit are decided from the
			// command line alone, so they are settled before any request: an
			// unanswerable prompt must not cost a lookup, and an omitted --role
			// must not reach the API — the property cobra's required-flag check
			// used to provide.
			if !interactive.CanPromptInteractively() {
				if grantee == "" {
					return pickerUnavailable(pt, "no grantee given")
				}
				if role == "" && required {
					return missingRoleErr(t.roles)
				}
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return err
				}
				picked, err := resolveGrantSelections(ctx, cmd, c, t, pt, grantee, id, role, required)
				if err != nil {
					return err
				}
				return grantEach(ctx, cmd, c, t, pt, id, picked, grantee != "")
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", roleHelp)
	addJSONFlag(cmd)
	return cmd
}

// resolveGrantSelections turns the command line into the grantee/role pairs to
// grant. A grantee argument is one pair, taking --role or the prompt; an omitted
// grantee opens the picker.
//
// --role is deliberately NOT cobra-required, even where the target has no server
// default: cobra enforces required flags before RunE, which would make a role
// impossible to prompt for. The property that used to guarantee — an omitted
// role never reaching validation, a lookup, or the API — is kept here instead,
// by resolving one before anything is granted.
func resolveGrantSelections[Row any](ctx context.Context, cmd *cobra.Command, c *coreapi.Client, t grantTarget[Row], pt grantPickerTarget, grantee, id, role string, roleRequired bool) ([]grantSelection, error) {
	if grantee != "" {
		// A grantee with no --role where the target has no server default still
		// needs one; prompting for it is the one-row version of the picker's
		// second screen. The non-interactive case was refused before any request.
		if role == "" && roleRequired {
			return grantPicker(cmd, pt, nil, []string{grantee}, "")
		}
		return []grantSelection{{handle: grantee, role: role}}, nil
	}
	pool, err := t.candidates(ctx, c, id)
	if err != nil {
		var notOrg *ownerNotOrgError
		if errors.As(err, &notOrg) {
			return nil, pickerUnavailable(pt, t.ownerNotOrg(pt, notOrg.project))
		}
		return nil, err
	}
	if len(pool.candidates) == 0 {
		// Every sentence below is a statement about the WHOLE org, and a
		// truncated walk read the first N of it — so on a partial pool none of
		// them can be said, and this is the one empty pool that is not a clean
		// success: the state the user wanted may not hold at all, it simply was
		// not looked for past the budget. Name the way through instead.
		if pool.window.partial {
			return nil, pickerUnavailable(pt, fmt.Sprintf("none of the first %d members of the org owning %s can be added here, and it has more", pool.window.scanned, pt.describe()))
		}
		// An empty pool is otherwise not a failure. Nothing went wrong, nothing
		// is left for the user to fix, and in the common case the state they
		// wanted already holds — the same reasoning that makes revoking an
		// already-revoked grant a success rather than a 404. So this reports
		// and stops, and the command exits 0.
		//
		// Three different answers to "who can I add?", so three messages. The
		// last is not "already has access": a member holding the target only
		// through its project is still offered, so reaching it means every one
		// of them holds a grant on this target itself.
		var reason string
		switch {
		case pool.window.scanned == 0:
			reason = pt.describe() + " has no org members to choose from"
		case pool.addressable == 0:
			reason = fmt.Sprintf("no member of the org owning %s can be granted access here", pt.describe())
		default:
			reason = fmt.Sprintf("every member of the org owning %s already has a grant on it", pt.describe())
		}
		reportNothingToAdd(cmd, reason)
		return nil, nil
	}
	if pool.window.partial {
		pt.poolNote = partialPoolNote(pool.window, "members of the org owning "+pt.describe())
	}
	return grantPicker(cmd, pt, pool.candidates, nil, role)
}

// reportNothingToAdd says why a pool came back empty. The reason is the human
// output; with --json the stdout shape has to stay parseable, so it moves to
// stderr and grantEach puts the empty array that "no grants were made" means
// on stdout instead.
func reportNothingToAdd(cmd *cobra.Command, reason string) {
	w := cmd.OutOrStdout()
	if jsonRequested(cmd) {
		w = cmd.ErrOrStderr()
	}
	fmt.Fprintln(w, reason)
}

// grantEach grants every pair in turn. On a failure it stops and returns,
// having reported the grants that already landed: those are real, and the CLI
// cannot undo them, so the user needs to know which ones to skip on a retry.
func grantEach[Row any](ctx context.Context, cmd *cobra.Command, c *coreapi.Client, t grantTarget[Row], pt grantPickerTarget, id string, picked []grantSelection, single bool) error {
	wires := make([]any, 0, len(picked))
	for _, p := range picked {
		provider, providerUserID, err := resolveGranteeProvider(ctx, c, p.handle)
		if err != nil {
			return errors.Join(err, emitGrantJSON(cmd, wires, single))
		}
		granted, wire, err := t.grant(ctx, c, id, provider, providerUserID, p.role)
		if err != nil {
			return errors.Join(err, emitGrantJSON(cmd, wires, single))
		}
		wires = append(wires, wire)
		if !jsonRequested(cmd) {
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Granted %s %s access to %s\n", p.handle, granted, pt.describe())
		}
	}
	return emitGrantJSON(cmd, wires, single)
}

// emitGrantJSON writes the wire objects for --json; text mode has already
// printed a line per grant as it went.
//
// The shape follows the INVOCATION, not the outcome. A grantee named on the
// command line is one mutation and emits the bare wire object, which is what
// every other mutation's --json emits (`org create`, `project create`, and what
// `grant add` itself emitted through runCoreMutation before the picker existed)
// — so the scripted form neither breaks nor makes this the one command in the
// CLI answering a mutation with an array. The picker grants a set and emits an
// array, one entry per grant that landed, including none, so a caller reading
// it never has to branch.
//
// Deciding on the OUTCOME instead — an array only once more than one landed —
// is the version to avoid: it makes a picker run that granted one person
// indistinguishable from a typed one, so the shape depends on what the user
// happened to click.
func emitGrantJSON(cmd *cobra.Command, wires []any, single bool) error {
	if !jsonRequested(cmd) {
		return nil
	}
	if single {
		// A failed single grant emits nothing, as runCoreMutation always did.
		if len(wires) == 0 {
			return nil
		}
		return printJSON(cmd.OutOrStdout(), wires[0])
	}
	return printJSON(cmd.OutOrStdout(), wires)
}

// missingRoleErr replaces cobra's required-flag message for --role, which no
// longer marks it required (see resolveGrantSelections).
func missingRoleErr(roles []string) error {
	return fmt.Errorf("--role is required: one of %s", strings.Join(roles, ", "))
}

func newGrantListCmd[Row any](t grantTarget[Row]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("list <%s>", t.noun),
		Short: fmt.Sprintf("List who has %s access", t.noun),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if t.listBranch != nil && t.listBranch.claims(args[0]) {
				return t.listBranch.list(cmd, args[0])
			}
			return runCoreList(cmd, "No grants found.", t.columns, t.row, func(ctx context.Context, c *coreapi.Client) ([]Row, error) {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]Row, coreapi.OptString, error) {
					return t.list(ctx, c, id, pageToken)
				})
			})
		},
	}
	addJSONFlag(cmd)
	if t.listBranch != nil {
		cmd.Long, cmd.Example = t.listBranch.long, t.listBranch.example
	}
	return cmd
}

func newGrantRemoveCmd[Row any](t grantTarget[Row]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("remove <%s> [grantee]", t.noun),
		Short: fmt.Sprintf("Revoke a user's %s access", t.noun),
		Long: fmt.Sprintf("Revoke a grantee's %s access. The %s is addressed by %s; the grantee is a "+
			"provider-qualified handle (e.g. github:alice). Omit the grantee on a terminal to choose "+
			"from who holds %s access now.", t.noun, t.noun, t.refUsage, t.noun),
		Example: fmt.Sprintf("  entire %s grant remove %s github:alice", t.noun, t.exampleRef),
		Args:    cobra.RangeArgs(1, 2),
		PreRunE: refuseUnwritableRef(t),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			pt := grantPickerTarget{noun: t.noun, ref: args[0], roles: t.roles, least: t.leastRole}
			if len(args) == 2 {
				if err := ensureGranteeIsHandle(args[1]); err != nil {
					return err
				}
			}
			// Decided before any request, like the add side: without a prompt
			// the list of who holds the target has no use here.
			if len(args) == 1 && !interactive.CanPromptInteractively() {
				return granteeRequiredErr(pt)
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return err
				}
				// Either way the set is grantCandidates: a typed grantee is
				// its own ref and label, while a picked row is reported by the
				// name the picker showed rather than the id it acts on.
				typed := len(args) == 2
				picked := []grantCandidate{}
				if typed {
					picked = append(picked, handleCandidate(args[1]))
				} else if picked, err = pickGrantsToRevoke(ctx, cmd, c, t, pt, id); err != nil {
					return err
				}
				// Nobody chosen: nothing to confirm and nothing to revoke.
				// Confirming anyway asked "Revoke 0 grants on …?".
				if len(picked) == 0 {
					return nil
				}
				// Only the picker confirms, once for the whole set it chose.
				//
				// A typed grantee is already an explicit instruction naming who
				// to revoke, and it is the form a script uses: `entire <noun>
				// grant remove <ref> github:alice` revoked without asking before
				// this command grew a picker, and still does — whether or not a
				// terminal happens to be attached, so a redirected stdin cannot
				// turn it into a prompt nobody answers. What a confirmation is
				// for is a set clicked off a list, where the user chose rows
				// rather than names. Gating only that path is also what leaves
				// nothing to bypass: no caller who wanted --force ever meets it.
				if !typed {
					proceed, err := revokeConfirmed(cmd, pt, picked)
					if err != nil {
						return err
					}
					if !proceed {
						// Declined. revokeConfirmed said so where the prompt
						// was and nothing has been revoked, so this is a clean
						// stop — kept a branch of its own, because folding it
						// into the one above returns a known-nil err to mean
						// success and hides which outcome this is.
						return nil
					}
				}
				for _, p := range picked {
					if err := revokeOne(ctx, cmd, c, t, pt, id, p); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	return cmd
}

// revokeConfirmed is the seam the confirmation sits behind, matching
// removePicker's role for the picker: the form needs a terminal, which `go
// test` does not have, so a test answers it here instead.
//
// It is the picker's own prompt rather than confirmControlPlaneDeletion's.
// That gate exists to refuse without a terminal and offers --force to get past
// the refusal; this one has neither, because it only ever runs behind the
// picker. And it must be read where the picker was shown: runPromptForm puts
// the question on a writer the user can see, where huh's accessible mode would
// otherwise print it to stdout, in among the `✓ Revoked` lines.
var revokeConfirmed = func(cmd *cobra.Command, pt grantPickerTarget, picked []grantCandidate) (bool, error) {
	if err := revocationInterrupted(cmd); err != nil {
		return false, err
	}
	label, detail := revokeConfirmation(pt, picked)
	confirmed := false
	prompt := huh.NewConfirm().Title("Revoke " + label + "?").Value(&confirmed)
	if detail != "" {
		prompt = prompt.Description(detail)
	}
	render, err := runPromptForm(cmd, NewAccessibleForm(huh.NewGroup(prompt)))
	// Before the form error is looked at, because handleFormCancellation treats
	// context.Canceled as a clean abort and would report a signal as an answer.
	if ierr := revocationInterrupted(cmd); ierr != nil {
		return false, ierr
	}
	if err != nil {
		// An abort at the prompt IS an answer: Esc or Ctrl+C inside the form is
		// the user saying no, which is a decision rather than a failure.
		if cerr := handleFormCancellation(render, "Revocation", err); cerr != nil {
			return false, cerr
		}
		return false, nil
	}
	if !confirmed {
		fmt.Fprintln(render, "Revocation cancelled.")
		return false, nil
	}
	return true, nil
}

// revocationInterrupted reports a command context that has been cancelled out
// from under the confirmation, which is an interruption and not an answer.
//
// (false, nil) means the user declined, and nothing else may borrow it: the
// caller exits 0 on it. Wrapping ctx.Err() instead is what lets main.go match
// the signal it recorded and exit the way every other Ctrl+C in this CLI does —
// quietly, 130, breaking an enclosing shell loop. plugin_confirm.go is the
// shape this follows, checking either side of its form for the same reason;
// confirmControlPlaneDeletion's nilerr skip is the outlier, and carries the
// same bug for `delete`.
//
// Checked before the form as well as after, because huh opens the TTY during
// startup regardless of context state.
func revocationInterrupted(cmd *cobra.Command) error {
	if err := cmd.Context().Err(); err != nil {
		return fmt.Errorf("revocation cancelled: %w", err)
	}
	return nil
}

// revokeConfirmation describes what is about to be revoked. A single grantee
// reads as one sentence; several are counted in the title and listed under it,
// so the prompt never hides who is in the set behind a number.
func revokeConfirmation(pt grantPickerTarget, picked []grantCandidate) (label, detail string) {
	if len(picked) == 1 {
		return picked[0].option() + " from " + pt.describe(), ""
	}
	var b strings.Builder
	for _, p := range picked {
		fmt.Fprintf(&b, "%s%s\n", uiform.SelectOptionIndent, p.option())
	}
	return fmt.Sprintf("%d grants on %s", len(picked), pt.describe()), b.String()
}

// pickGrantsToRevoke offers who holds the target now. An empty pool is an
// error rather than a silent success: the user asked to revoke something and
// nothing was revoked.
func pickGrantsToRevoke[Row any](ctx context.Context, cmd *cobra.Command, c *coreapi.Client, t grantTarget[Row], pt grantPickerTarget, id string) ([]grantCandidate, error) {
	holders, window, err := t.holders(ctx, c, id)
	if err != nil {
		return nil, err
	}
	if len(holders) == 0 {
		// "has no grants" is a statement about the target; a truncated walk
		// only read the first N rows on it, which is a different claim and a
		// different remedy.
		if window.partial {
			return nil, revokeUnavailable(pt, fmt.Sprintf("none of the first %d grants on %s can be revoked here, and it has more", window.scanned, pt.describe()))
		}
		return nil, fmt.Errorf("%s has no grants that can be revoked here", pt.describe())
	}
	if window.partial {
		pt.poolNote = partialPoolNote(window, "grants on "+pt.describe())
	}
	// Whole rows, so the confirmation can name what was shown rather than the
	// id it acts on.
	return removePicker(cmd, pt, holders)
}

// revokeOne revokes a single grantee. It routes on g.byID — set by the pool
// that built the row, never sniffed from the ref's shape (see
// grantCandidate.byID) — so a picked project or repo row goes by ULID, while a
// typed grantee, which is always a handle, resolves its provider identity first.
func revokeOne[Row any](ctx context.Context, cmd *cobra.Command, c *coreapi.Client, t grantTarget[Row], pt grantPickerTarget, id string, g grantCandidate) error {
	if g.byID {
		return revokeGrant(cmd, g.label+" from "+pt.describe(), func() error {
			return t.revokeByID(ctx, c, id, g.ref)
		})
	}
	provider, providerUserID, err := resolveGranteeProvider(ctx, c, g.ref)
	if err != nil {
		return err
	}
	return revokeGrant(cmd, g.label+" from "+pt.describe(), func() error {
		return t.revokeByProvider(ctx, c, id, provider, providerUserID)
	})
}

// granteeRequiredErr is the no-terminal case of revokeUnavailable: nothing to
// choose on, so the grantee has to be named. One message serves all three
// targets, because all three accept the same single form.
func granteeRequiredErr(pt grantPickerTarget) error {
	return revokeUnavailable(pt, "no grantee given")
}

// refuseUnwritableRef is the write verbs' first question: does this ref name
// something the target can write at all? Cobra runs PreRunE before it validates
// required flags, which is the point — `repo grant add /gh/acme/widget alice`
// is answered with what is wrong (a mirror's access lives on GitHub) rather
// than sending the user to add a --role that changes nothing. nil for a target
// with no such ref, which leaves the hook off the command entirely.
func refuseUnwritableRef[Row any](t grantTarget[Row]) func(*cobra.Command, []string) error {
	if t.unwritableRef == nil {
		return nil
	}
	return func(cmd *cobra.Command, args []string) error {
		if err := t.unwritableRef(args[0]); err != nil {
			cmd.SilenceUsage = true
			return err
		}
		return nil
	}
}

// validateRole rejects a --role outside the target's set at the CLI boundary
// so the user gets a clear message instead of a server 422. The generated
// bodies use a distinct enum type per target that shares these values, so the
// targets cast the validated string to whichever type they need.
func validateRole(role string, allowed []string) error {
	if slices.Contains(allowed, role) {
		return nil
	}
	return fmt.Errorf("invalid --role %q: must be one of %s", role, strings.Join(allowed, ", "))
}

// revokeGrant runs a grant-removal API call idempotently. A 404 means the
// grantee already has no such grant — the desired end state — so it's reported
// as a no-op rather than surfaced as a raw error, matching runControlPlaneDelete.
// subject describes the grant, e.g. "github:alice from repo /et/acme/web".
func revokeGrant(cmd *cobra.Command, subject string, revoke func() error) error {
	if err := revoke(); err != nil {
		if isCoreNotFound(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: no such grant; nothing to revoke\n", subject)
			return nil
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Revoked %s\n", subject)
	return nil
}

// orgMemberColumns / grantColumns are the human table views of the
// membership/grant listings. Both lead with GRANTEE, the friendly name the
// server resolved (a provider:handle, or an org name), falling back to the
// ULID. Org membership is flat, so it carries the membership STATUS instead
// of provenance; project and repo grants include owner and inherited rows,
// so they add SOURCE and TYPE. No table prints an internal id: the grantee
// ULID is in the --json output for anyone who needs it.
var (
	orgMemberColumns = []string{colHeaderGrantee, colHeaderRole, colHeaderStatus}
	grantColumns     = []string{colHeaderGrantee, colHeaderRole, "SOURCE", "TYPE"}
)

func orgMemberRow(m coreapi.Membership) []string {
	return []string{granteeName(m.Handle, m.AccountId), m.Role, m.Status}
}

func projectGrantRow(g coreapi.ProjectGrant) []string {
	return []string{granteeName(g.GranteeName, g.GranteeId), g.Role, g.Source, g.GranteeType}
}

// repoGrantRow mirrors projectGrantRow; RepoGrant and ProjectGrant share the
// grantee/role/source shape, so both reuse grantColumns.
func repoGrantRow(g coreapi.RepoGrant) []string {
	return []string{granteeName(g.GranteeName, g.GranteeId), g.Role, g.Source, g.GranteeType}
}

// granteeName returns the friendly name when the server resolved one, falling
// back to the ULID for grantees it couldn't label (e.g. teams).
func granteeName(name coreapi.OptString, granteeID string) string {
	if n := name.Or(""); n != "" {
		return n
	}
	return granteeID
}

// granteeTypeAccount is the only grantee kind the revoke-by-id calls take; the
// provider-qualified variant has its own endpoint.
const granteeTypeAccount = "account"

// accessRoles are the project and repo grant roles; the two targets share the
// set because the server's SpiceDB relations are the same for both. They are
// listed least-privileged first, which leastAccessRole names so nothing has to
// rely on that order holding.
var accessRoles = []string{"reader", "writer", "admin"}

const leastAccessRole = "reader"

// orgRoleMember is the org's least-privileged role and the server's default.
const orgRoleMember = "member"

// orgGrantTarget is org membership: roles owner/admin/member with member as
// the server default, a target addressed by name or ULID, and no typed-id
// revoke route — members are removed by their provider identity.
var orgGrantTarget = grantTarget[coreapi.Membership]{
	noun:        cmdOrg,
	refUsage:    "name or ULID",
	exampleRef:  "acme",
	roles:       []string{"owner", "admin", orgRoleMember},
	leastRole:   orgRoleMember,
	defaultRole: orgRoleMember,
	columns:     orgMemberColumns,
	row:         orgMemberRow,
	resolve:     resolveOrgRef,
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		body := &coreapi.AddOrgMemberInputBody{Provider: provider, ProviderUserId: providerUserID}
		if role != "" {
			body.Role = coreapi.NewOptAddOrgMemberInputBodyRole(coreapi.AddOrgMemberInputBodyRole(role))
		}
		m, err := c.AddOrgMember(ctx, body, coreapi.AddOrgMemberParams{OrgId: id})
		if err != nil {
			return "", nil, err
		}
		return m.Role, m, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.Membership, coreapi.OptString, error) {
		out, err := c.ListOrgMembers(ctx, coreapi.ListOrgMembersParams{OrgId: id, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RemoveOrgMember(ctx, coreapi.RemoveOrgMemberParams{OrgId: id, Provider: provider, ProviderUserId: providerUserID})
	},
	holders: orgMemberHolders,
}

// projectGrantTarget is project access: roles reader/writer/admin, required,
// on a project addressed by name or ULID.
var projectGrantTarget = grantTarget[coreapi.ProjectGrant]{
	noun:       "project",
	refUsage:   "name or ULID",
	exampleRef: "widgets",
	roles:      accessRoles,
	leastRole:  leastAccessRole,
	columns:    grantColumns,
	row:        projectGrantRow,
	resolve: func(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
		return resolveProjectRef(ctx, c, ref)
	},
	candidates: projectGrantCandidates,
	holders:    projectGrantHolders,
	ownerNotOrg: func(pt grantPickerTarget, _ string) string {
		// The project the user named IS the account-owned one, so its name is
		// already in pt.ref and the error's copy would just repeat it.
		return pt.describe() + " is owned by an account, so it has no member list to choose from"
	},
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		out, err := c.GrantProjectAccess(ctx, &coreapi.GrantProjectAccessInputBody{
			Provider:       provider,
			ProviderUserId: providerUserID,
			Role:           coreapi.GrantProjectAccessInputBodyRole(role),
		}, coreapi.GrantProjectAccessParams{ProjectId: id})
		if err != nil {
			return "", nil, err
		}
		return role, out, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.ProjectGrant, coreapi.OptString, error) {
		out, err := c.ListProjectMembers(ctx, coreapi.ListProjectMembersParams{ProjectId: id, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RevokeProjectAccessByProvider(ctx, coreapi.RevokeProjectAccessByProviderParams{ProjectId: id, Provider: provider, ProviderUserId: providerUserID})
	},
	revokeByID: func(ctx context.Context, c *coreapi.Client, id, granteeID string) error {
		return c.RevokeProjectAccess(ctx, coreapi.RevokeProjectAccessParams{ProjectId: id, GranteeType: granteeTypeAccount, GranteeId: granteeID})
	},
}

// repoGrantTarget is repo access: roles reader/writer/admin, required, on a
// repo addressed by its /et/<project>/<repo> path and nothing else. `list`
// alone also answers a GitHub mirror ref, from the upstream collaborators the
// placement materializes — see mirrorGrantListing.
var repoGrantTarget = grantTarget[coreapi.RepoGrant]{
	noun:          cmdRepo,
	refUsage:      "its /" + nativeCloneForge + "/<project>/<repo> path",
	exampleRef:    "/" + nativeCloneForge + "/acme/web",
	roles:         accessRoles,
	leastRole:     leastAccessRole,
	columns:       grantColumns,
	row:           repoGrantRow,
	listBranch:    mirrorGrantListing,
	unwritableRef: mirrorGrantsAreUpstream,
	resolve: func(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
		return resolveRepoPath(ctx, c, ref)
	},
	candidates: repoGrantCandidates,
	holders:    repoGrantHolders,
	ownerNotOrg: func(pt grantPickerTarget, project string) string {
		// A repo's pool comes from its project's org, so the refusal names the
		// project in between rather than leaving the user to find it.
		return fmt.Sprintf("%s is in project %s, which is owned by an account, so it has no member list to choose from", pt.describe(), project)
	},
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		out, err := c.GrantRepoAccess(ctx, &coreapi.GrantRepoAccessInputBody{
			Provider:       provider,
			ProviderUserId: providerUserID,
			Role:           coreapi.GrantRepoAccessInputBodyRole(role),
		}, coreapi.GrantRepoAccessParams{RepoId: id})
		if err != nil {
			return "", nil, err
		}
		return role, out, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.RepoGrant, coreapi.OptString, error) {
		out, err := c.ListRepoGrants(ctx, coreapi.ListRepoGrantsParams{RepoId: id, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Grants, out.NextPageToken, nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RevokeRepoAccessByProvider(ctx, coreapi.RevokeRepoAccessByProviderParams{RepoId: id, Provider: provider, ProviderUserId: providerUserID})
	},
	revokeByID: func(ctx context.Context, c *coreapi.Client, id, granteeID string) error {
		return c.RevokeRepoAccess(ctx, coreapi.RevokeRepoAccessParams{RepoId: id, GranteeType: granteeTypeAccount, GranteeId: granteeID})
	},
}
