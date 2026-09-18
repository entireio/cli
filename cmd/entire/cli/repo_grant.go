package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// "Who can reach this repository?" is one question, and `repo grant` is where
// it is answered — for both kinds of repository, because a user asking it does
// not necessarily know which backs the repo. The two answers come from
// different places, which is what this file holds:
//
//   - An Entire repository's access IS its grants: `list` reads them and `add`
//     and `remove` write them.
//   - A GitHub mirror's access is the upstream repository's. Entire only
//     materializes it per placement, so `list` reads it and nothing writes it
//     — mirrorGrantsAreUpstream is what `add` and `remove` say instead.
//
// The mirror half is deliberately absent from `add`/`remove`'s grammar rather
// than accepted and always refused: a command that takes a ref it can never act
// on reads as a bug the first time and a lie the second.

// mirrorCollaboratorColumns is the mirror half of `repo grant list`. It is
// grantColumns' leading pair, the two things the mirror endpoint answers for:
// who, and with which role. The columns behind them are the native listing's
// provenance (SOURCE/TYPE), which the mirror endpoint does not report and which
// would be invented if this table filled them in. Like every grant table it
// prints no internal id — the account ULID is in the --json output.
var mirrorCollaboratorColumns = []string{colHeaderGrantee, colHeaderRole}

func mirrorCollaboratorRow(c coreapi.MirrorCollaborator) []string {
	return []string{granteeName(c.Handle, c.AccountId), c.Role}
}

// mirrorCollaboratorView is how the mirror half renders: the table above, and
// a --json object carrying the identity keys the native half uses.
func mirrorCollaboratorView() listView[coreapi.MirrorCollaborator] {
	return listView[coreapi.MirrorCollaborator]{
		table: func([]coreapi.MirrorCollaborator) ([]string, func(coreapi.MirrorCollaborator) []string) {
			return mirrorCollaboratorColumns, mirrorCollaboratorRow
		},
		toJSON: mirrorCollaboratorJSON,
	}
}

// mirrorCollaboratorJSON gives one verb one machine-readable answer. The table
// already reconciles the two sources through granteeName, but --json printed
// each endpoint's own model, and the two named the grantee differently, so a
// script reading `.granteeName` got nulls for a mirror ref rather than an
// error — the failure folding these verbs together was meant to end.
//
// A mirror row is therefore rewritten into the grant vocabulary, each value
// named once: `accountId` becomes `granteeId`, `handle` becomes `granteeName`,
// and `source` is "github" because that is where a mirror's access comes from —
// the same vocabulary the native side uses for "direct" or "project:<name>".
// Keeping the endpoint's own spellings beside these would print one value twice
// under two names, which is no more machine-readable than the split it replaced.
//
// `granteeType` is the one native key left out, and deliberately: this endpoint
// reports an account id and no kind, so "account" would be a guess about a
// principal whose kind we were never told — a GitHub team materialized as one
// would be labelled wrong, silently, in the key a script filters on. Absence is
// not ambiguous here, because the native rows always carry it (the schema makes
// it required), so a row without one is a mirror row whose kind went
// unreported. A sentinel like "unknown" would say the same thing while adding a
// value no server emits, which every consumer switching on the field would then
// have to learn.
func mirrorCollaboratorJSON(collaborators []coreapi.MirrorCollaborator) (any, error) {
	out := make([]map[string]json.RawMessage, 0, len(collaborators))
	for i := range collaborators {
		c := &collaborators[i]
		// Merged rather than marshalled from a struct of our own: the generated
		// types carry arbitrary additional properties, and a fixed shape would
		// swallow whatever the server adds next.
		obj, err := mergeSynthesizedFields(c, map[string]func() string{
			"granteeId":   func() string { return c.AccountId },
			"granteeName": func() string { return c.Handle.Or("") },
			"source":      func() string { return repoProviderGitHub },
		})
		if err != nil {
			return nil, err
		}
		// The two strings the merged keys now carry, under the names the native
		// rows use for them.
		delete(obj, "accountId")
		delete(obj, "handle")
		out = append(out, obj)
	}
	return out, nil
}

// repoGrantListLong explains the one question and its two answers, including
// the two ways a caller can be refused: the native listing is answered by
// Entire from your Entire access, the mirror listing by GitHub from your GitHub
// identity.
const repoGrantListLong = "List who can reach a repository.\n\n" +
	"An Entire repository (/" + nativeCloneForge + "/<project>/<repo>) lists its grants: each grantee, " +
	"their reader/writer/admin role, and whether the grant is held on the repo itself or " +
	"inherited. Entire's control plane answers it from your access to the repo.\n\n" +
	"A GitHub mirror (/" + mirrorCloneForge + "/<owner>/<repo>) lists who can pull the mirror. Mirror " +
	"access follows the upstream GitHub repository and is " +
	"read-only here, so `add` and `remove` do not take a mirror ref. The check is live and " +
	"against your own GitHub identity — you must be a current admin of the upstream " +
	"repository, or the owner of a personal one — so run it as yourself rather than with a " +
	"service-account token.\n\n" +
	"--json answers both the same way: every row carries `granteeId`, `role` and " +
	"`source`, plus `granteeName` when a name resolved, so one script reads either. " +
	"A mirror's rows carry no `granteeType`: that endpoint reports no grantee kind, " +
	"and the missing key is the answer, since an Entire repository's rows always " +
	"have one."

const repoGrantListExample = "  entire repo grant list /" + nativeCloneForge + "/acme/web\n" +
	"  entire repo grant list /" + mirrorCloneForge + "/acme/widget"

// mirrorGrantListing is the mirror reading of a `repo grant list` ref. It
// claims every ref that does not declare the native forge, which is what keeps
// the ref errors honest: this verb takes both forges, so a ref naming neither
// must be offered both readings rather than the native path alone.
var mirrorGrantListing = &grantListBranch{
	long:    repoGrantListLong,
	example: repoGrantListExample,
	claims:  func(ref string) bool { return !declaresForge(ref, nativeCloneForge) },
	list: func(cmd *cobra.Command, ref string) error {
		// Every failure from here is about the ref, never the command's shape.
		cmd.SilenceUsage = true
		if !declaresForge(ref, mirrorCloneForge) {
			return forgeQualifiedRefError(ref)
		}
		_, owner, repo, err := parseMirrorCloneRef(ref)
		if err != nil {
			return fmt.Errorf("invalid <repo> %q: %w", ref, err)
		}
		return listMirrorCollaborators(cmd, owner, repo)
	},
}

// listMirrorCollaborators prints who can pull owner/repo's mirror.
//
// The collaborator endpoint is served by the core fronting ONE cluster, so a
// cluster has to be named. Which one is a hint, never a gate: the two endpoints
// answer to different authorities — /mirrors/placements is pull-gated, while
// /mirrors/collaborators runs a live GitHub-admin check — so a GitHub admin who
// holds no Entire grant resolves no placements and must still get their answer.
// Nothing the hint does can fail the command: not an empty answer, not a failed
// lookup, and not a control plane the active login cannot even dial, since the
// read below authenticates through whichever saved login the cluster trusts.
// The fallback is the default cluster, which is where this read went before
// placements were consulted at all; what the hint buys is the repo mirrored
// only outside that default.
//
// A fallback is a guess, though, and a guess that misses looks like a missing
// mirror — so when the read fails on a cluster nothing pointed at, the error
// says the cluster was ours to pick and names the verb that lists the real ones.
//
// The caller is never asked which region they mean: every placement
// materializes the same upstream GitHub collaborators, so any one answers.
func listMirrorCollaborators(cmd *cobra.Command, owner, repo string) error {
	clusterHost, guess := mirrorReadTarget(cmd, owner, repo)
	empty := "No collaborators on this mirror; its access follows the upstream GitHub repository."
	if guess != nil {
		// The affirmative sentence is only honest about a cluster something
		// pointed at. On a guess, an empty answer may just be the wrong cell.
		empty = fmt.Sprintf("No collaborators reported by %s.", clusterHost)
	}
	collaborators := 0
	err := runCoreListShapedForCluster(cmd, clusterHost, empty, mirrorCollaboratorView(), func(ctx context.Context, c *coreapi.Client) ([]coreapi.MirrorCollaborator, error) {
		out, err := c.ListMirrorCollaborators(ctx, coreapi.ListMirrorCollaboratorsParams{
			Provider:    coreapi.ListMirrorCollaboratorsProviderGithub,
			Owner:       owner,
			Repo:        repo,
			ClusterHost: clusterHost,
		})
		if err != nil {
			return nil, err
		}
		collaborators = len(out.Collaborators)
		return out.Collaborators, nil
	})
	switch {
	case guess == nil:
		return err
	// Every failure on a guessed cluster owns the guess, not just a not-found:
	// a cluster nothing pointed at is also one the caller may hold no login
	// for, and "not trusted here" reads as a dead end until you know we picked
	// the cluster ourselves.
	case err != nil:
		return fmt.Errorf("%w (asked %s: %s, so that cluster was a guess — %s)", err, clusterHost, guess.reason, guess.next)
	case collaborators == 0:
		// An empty answer from a guessed cluster is the one success that can
		// mislead, and --json is where it misleads silently: an empty array
		// exits 0 and reads exactly like a mirror with no collaborators. The
		// caveat goes to stderr so it reaches both renderings without putting
		// anything but rows on stdout (see repo list's partial-fetch note).
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: %s reported no collaborators, but %s, so that cluster was a guess. %s\n", clusterHost, guess.reason, guess.next)
	}
	return nil
}

// clusterGuess says why the default cluster is standing in for a placement, and
// what the reader can do about it. The two travel together because the answer
// to "what now" depends on the reason: only one of them leaves a `repo mirror
// get` that can answer.
type clusterGuess struct{ reason, next string }

// mirrorReadTarget resolves which cluster answers for owner/repo's mirror. The
// second result is nil when a placement chose it.
func mirrorReadTarget(cmd *cobra.Command, owner, repo string) (clusterHost string, guess *clusterGuess) {
	// Losing the lookup says nothing about where the repo is mirrored, so it
	// leaves the reader where an invisible placement does: acting as a login
	// that can see it.
	unavailable := &clusterGuess{reason: "the placement lookup was unavailable", next: mirrorLoginHint()}
	// Advisory, so every failure here is logged and dropped — including
	// runCore's own, which is the active login failing to dial its control
	// plane rather than anything about this repo.
	if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		// The pull-gated placement lookup, the same authority `repo clone`,
		// `remote use` and `remote url` resolve through, so a public mirror
		// resolves too.
		placements, err := resolvePullablePlacements(ctx, c, owner, repo)
		if err != nil {
			logging.Debug(ctx, "mirror collaborators: placement hint lookup failed", "error", err)
			guess = &clusterGuess{reason: "the placement lookup failed", next: unavailable.next}
			return nil
		}
		switch clusterHost = mirrorReadCluster(placements); {
		case clusterHost != "":
		case len(placements) == 0:
			// `repo mirror get` cannot answer here: it resolves through the
			// affiliation-scoped repo directory, which is narrower than the
			// pull-gated lookup that just came back empty, so it would fail
			// for the same reason one step later. What is left is the login —
			// a placement invisible to this one may be visible to another.
			guess = &clusterGuess{
				reason: fmt.Sprintf("no placement of %s/%s is visible to this login", owner, repo),
				next:   mirrorLoginHint(),
			}
		default:
			// Placements DID resolve, so `repo mirror get` will list them.
			guess = &clusterGuess{
				reason: "no placement named a cluster host this command can dial",
				next:   mirrorPlacementsHint(owner, repo),
			}
		}
		return nil
	}); err != nil {
		logging.Debug(cmd.Context(), "mirror collaborators: placement hint unavailable", "error", err)
		guess = unavailable
	}
	if clusterHost == "" {
		clusterHost = defaultClusterHost
	}
	return clusterHost, guess
}

// mirrorLoginHint names the way to a placement the active login cannot see: the
// lookup is pull-gated, so another login may resolve what this one does not.
func mirrorLoginHint() string {
	return "`entire auth contexts` lists your logins; --context acts as one."
}

// mirrorPlacementsHint names the verb that lists a mirror's real placements. It
// answers only where placements DID resolve: `repo mirror get` reads the
// affiliation-scoped repo directory, so it cannot see what the broader
// pull-gated lookup could not.
func mirrorPlacementsHint(owner, repo string) string {
	return fmt.Sprintf("`entire repo mirror get /%s/%s/%s` lists its placements.", mirrorCloneForge, owner, repo)
}

// mirrorReadCluster picks which placement answers for the mirror. Any of them
// gives the same upstream collaborators, so this is only about being
// deterministic: the default cluster when the repo is mirrored there, else the
// first host in sorted order. A host that is not a bare host[:port] is skipped
// rather than dialed — it names the core this command authenticates to, so the
// same guard repoRemoteURL applies to a server-provided host applies here.
// Returns "" when the placements name no usable host, leaving the caller on the
// default cluster.
func mirrorReadCluster(placements []coreapi.ResolvedPlacement) string {
	hosts := make([]string, 0, len(placements))
	for _, p := range placements {
		host := strings.TrimSpace(p.ClusterHost)
		if validateClusterHost(host) != nil {
			continue
		}
		if strings.EqualFold(host, defaultClusterHost) {
			return defaultClusterHost
		}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return ""
	}
	sort.Strings(hosts)
	return hosts[0]
}

// mirrorGrantsAreUpstream is what `repo grant add` and `repo grant remove`
// answer a mirror ref: the grammar they accept is the native path, and a mirror
// has no grant to write. It names the read path that does answer for a mirror,
// so the refusal ends somewhere rather than at "unsupported".
//
// A github.com URL is claimed too, though it is not the accepted spelling: it
// says plainly which repository the user means, and answering it with the
// native path's grammar — the one shape that repository can never have — would
// send them to rewrite the ref rather than to the verb that reads it. The hint
// names the `/gh/` form, since that is what `grant list` takes.
//
// Every other ref passes, to be judged by the resolver that parses it.
func mirrorGrantsAreUpstream(ref string) error {
	owner, repo, urlErr := parseHostedGitHubURL(ref)
	readable := ref
	switch {
	case declaresForge(ref, mirrorCloneForge):
	case urlErr == nil:
		// A URL is not a spelling these verbs take, so the hint names the ref
		// `grant list` does.
		readable = fmt.Sprintf("/%s/%s/%s", mirrorCloneForge, owner, repo)
	default:
		return nil // not a GitHub repository; its own resolver judges the ref
	}
	return fmt.Errorf("repo %q is a GitHub mirror: its access comes from the upstream GitHub repository, so it cannot be granted or revoked here — manage collaborators on GitHub; `entire repo grant list %s` shows who has access to the repo", ref, readable)
}
