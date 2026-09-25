package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// Entire-native repositories are mirrored through a different resource than
// GitHub ones. A GitHub mirror is a clone of an upstream, addressed by
// (provider, owner, repo, clusterHost) and created through an asynchronous
// mirror REQUEST. A native mirror is an extra placement of a repo Entire
// already holds, addressed by (repoId, clusterSlug) under
// /repos/{repoId}/native-mirrors.
//
// Three consequences shape everything below:
//
//   - The primary placement is NOT in the native-mirror list. GET returns only
//     the additional ones, so any view of "where does this repo live" joins the
//     repo's own clusterSlug onto that list.
//   - A replica is never promoted: removing one tears down that copy alone,
//     and the primary is not in the list to remove.
//   - v1 places mirrors cross-jurisdiction only, and the primary is fixed to
//     the owning project's region.
//
// The control-plane routes are home-core-scoped: a repo whose cluster is in
// another jurisdiction answers 421, which coreapi's transport follows on its
// own (see internal/coreapi/cross_juris_client.go). So these all run on the
// plain active-context client — no cluster-fronting detour.

// loadNativeRepo reads the repo a native ref names together with the cluster
// catalog, the two things every native mirror verb needs before it can decide
// anything: the repo carries its primary cluster and region, and the catalog
// says which other clusters exist and where they are.
func loadNativeRepo(ctx context.Context, c *coreapi.Client, ref mirrorRepoRef) (*coreapi.Repo, []coreapi.Cluster, error) {
	repo, err := resolveNativeRepo(ctx, c, ref.owner, ref.repo)
	if err != nil {
		return nil, nil, err
	}
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return nil, nil, err
	}
	return repo, clusters.Clusters, nil
}

// checkNativeMirrorTarget refuses, before any write, everything decidable from
// the repo and the cluster catalog — in the order that gives the most useful
// answer first.
//
// These are not duplicated validation: each has a server-side counterpart that
// answers 400 or 409. Doing them here buys a message that names the repo's own
// primary and region instead of a generic refusal, and costs no round trip
// because both inputs were already fetched.
func checkNativeMirrorTarget(repo *coreapi.Repo, clusters []coreapi.Cluster, clusterSlug, ref string) error {
	// Provider is optional on the wire, so an UNSET one is "the server did not
	// say" and is left for the server to judge — the same reasoning the state
	// check below applies, and the same `repo protection` uses. Only a value
	// that definitely names another forge is refused here; failing closed on ""
	// would reject every repo whose provider the response omits, with a message
	// quoting an empty string.
	if provider := repo.Provider.Or(""); provider != "" && provider != repoProviderEntire {
		return fmt.Errorf("repo %s is not an Entire-native repository (provider %q); only native repos have native mirrors", ref, provider)
	}
	// State is an open string on the client, so an UNSET one is "the server
	// did not say" and is left for the server to judge; only a value that is
	// definitely not active is refused here (repoStateActive, repo_readiness.go).
	if state := repo.State.Or(""); state != "" && state != repoStateActive {
		return fmt.Errorf("repo %s is %s, not active; wait for it to finish provisioning before mirroring it", ref, state)
	}
	primary := repo.ClusterSlug.Or("")
	if primary != "" && strings.EqualFold(primary, clusterSlug) {
		return fmt.Errorf("repo %s already lives on %s: that is its primary, not a mirror of it", ref, primary)
	}
	target, ok := clusterBySlug(clusters, clusterSlug)
	if !ok {
		return fmt.Errorf("unknown cluster %q; available: %s", clusterSlug, strings.Join(clusterSlugs(clusters), ", "))
	}
	// v1 places a native mirror in a different jurisdiction from the primary —
	// the point of the feature is reach, not redundancy within one region. The
	// server refuses the same thing; saying it here names both regions.
	if home := repo.Jurisdiction.Or(""); home != "" && strings.EqualFold(home, target.Jurisdiction) {
		return fmt.Errorf("repo %s is already in the %s region, and a native mirror must be in a different one; pick a cluster outside %s (run `entire cluster list`)", ref, target.Jurisdiction, home)
	}
	return nil
}

func clusterBySlug(clusters []coreapi.Cluster, slug string) (coreapi.Cluster, bool) {
	for _, cl := range clusters {
		if strings.EqualFold(cl.Slug, slug) {
			return cl, true
		}
	}
	return coreapi.Cluster{}, false
}

func clusterSlugs(clusters []coreapi.Cluster) []string {
	out := make([]string, 0, len(clusters))
	for _, cl := range clusters {
		out = append(out, cl.Slug)
	}
	slices.Sort(out)
	return out
}

// nativeRefOf renders a parsed native ref back into the /et/<project>/<repo>
// spelling, so every message names the repo the way the user must type it.
func nativeRefOf(ref mirrorRepoRef) string {
	return "/" + nativeCloneForge + "/" + ref.owner + "/" + ref.repo
}

// errNativeMirrorSuspended and errNativeMirrorFailed are the terminal
// placement outcomes. Suspended means an operator parked the placement; it will
// not move on its own and resuming is not a CLI operation.
var (
	errNativeMirrorFailed    = errors.New("native mirror placement failed")
	errNativeMirrorSuspended = errors.New("native mirror placement is suspended")
)

// findNativeMirror returns the placement for clusterSlug among a repo's native
// mirrors. The list holds only the additional placements, so a miss means this
// cluster carries no mirror of the repo (or the primary, which is never here).
func findNativeMirror(placements []coreapi.NativeMirrorPlacement, clusterSlug string) (coreapi.NativeMirrorPlacement, bool) {
	for _, p := range placements {
		if strings.EqualFold(p.ClusterSlug, clusterSlug) {
			return p, true
		}
	}
	return coreapi.NativeMirrorPlacement{}, false
}

func listNativeMirrors(ctx context.Context, c *coreapi.Client, repoID string) ([]coreapi.NativeMirrorPlacement, error) {
	out, err := c.ListNativeMirrors(ctx, coreapi.ListNativeMirrorsParams{RepoId: repoID})
	if err != nil {
		return nil, err
	}
	return out.NativeMirrors, nil
}

// nativeMirrorIsFresh reports whether a create response describes an intent
// that was just made rather than one that already existed. A fresh intent is
// always (pending, processing, 0 attempts) and the endpoint answers identically
// for a repeat call, so this triple is the only way to tell the two apart — and
// telling them apart is what keeps `add` from claiming it created something it
// found.
func nativeMirrorIsFresh(p coreapi.NativeMirrorPlacement) bool {
	return p.Stage == coreapi.NativeMirrorPlacementStagePending &&
		p.Status == coreapi.NativeMirrorPlacementStatusProcessing &&
		p.Attempts == 0
}

// awaitNativeMirrorReady polls until the placement on clusterSlug is readable,
// or a terminal condition says it never will be.
//
// Readiness is status == ready and only that. Stage (pending → provisioned →
// registered → seeded → announced) reports provisioning progress and is shown
// while waiting, but a placement is not readable until its status says so:
// reads are gated on status server-side, so stage "announced" with status
// "processing" still serves nothing.
//
// Creation retries server-side, so attempts, nextRetryAt and creationFailedAt
// are bookkeeping, not verdicts — the loop keeps waiting through them and stops
// only on a terminal status. A vanished row means something else removed the
// intent; a row turned to desiredState "deleted" means a teardown overtook us.
// Neither will ever become ready, so both stop the wait rather than spin.
func awaitNativeMirrorReady(ctx context.Context, c *coreapi.Client, repoID, clusterSlug string) (coreapi.NativeMirrorPlacement, error) {
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	var last coreapi.NativeMirrorPlacement
	var consecutiveErrs int
	// The caller has just been handed this placement by the create, so the FIRST
	// listing that does not carry it is the two endpoints disagreeing for a
	// moment, not a removal. Exactly one tick of grace: after that, absence is
	// terminal — a row that never appears must fail rather than spin, which is
	// what waiting on "seen at least once" would do forever.
	polls := 0
	for {
		placements, err := listNativeMirrors(ctx, c, repoID)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return last, classifyWaitContextErr(ctx.Err(), "waiting for the native mirror")
			}
			// Tolerate transient glitches: the placement may still be
			// progressing, so retry on the next tick.
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return last, fmt.Errorf("poll native mirror: %w", err)
			}
		default:
			consecutiveErrs = 0
			polls++
			p, ok := findNativeMirror(placements, clusterSlug)
			if !ok {
				if polls == 1 {
					break // one tick for the listing to catch up with the create
				}
				return last, fmt.Errorf("native mirror on %s is no longer listed; something else removed it", clusterSlug)
			}
			last = p
			if p.DesiredState == coreapi.NativeMirrorPlacementDesiredStateDeleted {
				return p, fmt.Errorf("native mirror on %s is being torn down; wait for it to disappear before creating it again", clusterSlug)
			}
			switch p.Status {
			case coreapi.NativeMirrorPlacementStatusReady:
				return p, nil
			case coreapi.NativeMirrorPlacementStatusFailed:
				return p, errNativeMirrorFailed
			case coreapi.NativeMirrorPlacementStatusSuspended:
				return p, errNativeMirrorSuspended
			case coreapi.NativeMirrorPlacementStatusProcessing:
				// keep waiting; the server retries on its own
			}
		}
		select {
		case <-ctx.Done():
			return last, classifyWaitContextErr(ctx.Err(), "waiting for the native mirror")
		case <-ticker.C:
		}
	}
}

// awaitNativeMirrorRemoved polls until the placement on clusterSlug is gone.
// Teardown is asynchronous: DELETE only records the intent. A row that stops
// being desiredState "deleted" was re-created by someone else, which this wait
// can never satisfy, so it stops rather than looping forever.
func awaitNativeMirrorRemoved(ctx context.Context, c *coreapi.Client, repoID, clusterSlug string) error {
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	var consecutiveErrs int
	// The same read-after-write grace awaitNativeMirrorReady gives the create:
	// the FIRST listing after a delete may not carry the intent yet, and a row
	// that still reads active then is lag, not a re-creation. Exactly one tick,
	// so a row that genuinely stops being deleted still ends the wait.
	polls := 0
	for {
		placements, err := listNativeMirrors(ctx, c, repoID)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return classifyWaitContextErr(ctx.Err(), "waiting for the native mirror to be removed")
			}
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return fmt.Errorf("poll native mirror: %w", err)
			}
		default:
			consecutiveErrs = 0
			polls++
			p, ok := findNativeMirror(placements, clusterSlug)
			if !ok {
				return nil
			}
			if p.DesiredState != coreapi.NativeMirrorPlacementDesiredStateDeleted && polls > 1 {
				return fmt.Errorf("native mirror on %s is no longer being removed; something re-created it", clusterSlug)
			}
		}
		select {
		case <-ctx.Done():
			return classifyWaitContextErr(ctx.Err(), "waiting for the native mirror to be removed")
		case <-ticker.C:
		}
	}
}

// renderNativeMirrorCreateError renders a create refusal, recognising the one
// whose fix is not derivable from its own words: creating on a cluster whose
// previous placement is still tearing down. The detail says the mirror is being
// deleted; it does not say that waiting for the row to disappear is the whole
// remedy, so a caller would retry immediately and see it again.
//
// That is deliberately the ONLY message matched on. Every other refusal
// (hosting capability, registry state, trust publication, feature flags) is
// rendered from the server's own problem+json detail by renderCoreError, which
// keeps the CLI from encoding server prose it would then have to chase.
//
// It renders for itself rather than taking an already-rendered error because
// the match needs the structured *coreapi.ErrorModelStatusCode, and
// renderCoreError flattens that into a plain errors.New — so a caller that
// rendered first would switch the hint off with nothing to show for it.
func renderNativeMirrorCreateError(err error, ref, clusterSlug string) error {
	detail := strings.ToLower(coreapi.APIError(err))
	rendered := renderCoreError(err)
	if !strings.Contains(detail, "being deleted") {
		return rendered
	}
	return fmt.Errorf("%w; a previous mirror of %s on %s is still being torn down \u2014 wait for it to disappear from `entire repo view %s`, then add it again",
		rendered, ref, clusterSlug, ref)
}

// createOneNativeMirror is the native half of the parallel engine
// (createOneMirror dispatches here): place one replica and, unless --no-wait,
// wait for it to be readable. Like its GitHub sibling it never returns an
// error — every outcome folds into the mirrorResult so one failure cannot sink
// the batch.
//
// The caller has already refused everything decidable without a write (see
// checkNativeMirrorTarget), so what is left here is the create and the wait.
func createOneNativeMirror(ctx context.Context, t mirrorTarget, c *coreapi.Client, clientErr error, opts mirrorAddOptions, report func(status string, final, ok bool)) mirrorResult {
	res := mirrorResult{forge: t.forge, owner: t.owner, repo: t.repo, regionLabel: regionLabel(t.region)}
	if clientErr != nil {
		res.status, res.err = mirrorStatusError, clientErr
		report(mirrorStatusError, true, false)
		return res
	}
	// Zero preserves the caller context without adding a timeout.
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	report(string(mirrorAddPhasePlacing), false, false)
	created, err := c.CreateNativeMirror(ctx,
		&coreapi.CreateNativeMirrorInputBody{ClusterSlug: t.region.slug},
		coreapi.CreateNativeMirrorParams{RepoId: t.nativeRepo.ID})
	if err != nil {
		res.status = mirrorStatusError
		res.err = renderNativeMirrorCreateError(err, t.ref(), t.region.slug)
		report(mirrorStatusError, true, false)
		return res
	}
	res.cloneURL = nativeRepoURLAt(t.nativeRepo, t.region.host)

	// The endpoint is idempotent per (repo, cluster) and answers identically
	// either way, so the row's own state is the only thing that can say whether
	// this call made the placement or found it.
	//
	// That distinction only reaches the STATUS column with --no-wait, where
	// nothing was verified and "was it already here" is all there is to report.
	// A waiting run must wait either way: re-running the command after a Ctrl+C
	// is the normal way to resume a seed, and returning "exists" there would
	// hand back a clone URL for a replica still being written.
	existed := !nativeMirrorIsFresh(*created)
	if opts.noWait {
		res.status = mirrorStatusRegistered
		if existed {
			res.status = mirrorStatusExists
		}
		report(res.status, true, true)
		return res
	}

	report(string(mirrorAddPhaseCloning), false, false)
	final, waitErr := awaitNativeMirrorReady(ctx, c, t.nativeRepo.ID, t.region.slug)
	switch {
	case waitErr == nil:
		res.status = mirrorStatusReady
	case errors.Is(waitErr, errNativeMirrorFailed):
		res.status, res.err = mirrorStatusFailed, nativeMirrorDetail(final, "the seed failed")
	case errors.Is(waitErr, errNativeMirrorSuspended):
		res.status, res.err = mirrorStatusSuspended, nativeMirrorDetail(final, "the placement is suspended; an operator has to resume it")
	case errors.Is(waitErr, context.DeadlineExceeded):
		res.status, res.err = mirrorStatusTimedOut, waitErr
	default:
		res.status, res.err = mirrorStatusError, renderCoreError(waitErr)
	}
	report(res.status, true, res.err == nil)
	return res
}

// nativeMirrorDetail prefers the server's own reason for an unhealthy
// placement, falling back to what the status alone can say.
func nativeMirrorDetail(p coreapi.NativeMirrorPlacement, fallback string) error {
	if detail := strings.TrimSpace(p.LastError.Or("")); detail != "" {
		return errors.New(detail)
	}
	return errors.New(fallback)
}

func regionSlugs(regions []regionChoice) []string {
	out := make([]string, 0, len(regions))
	for _, r := range regions {
		out = append(out, r.slug)
	}
	slices.Sort(out)
	return out
}

// regionHosts is the same list in the spelling --cluster takes, for the
// "available: ..." half of a refusal — a reader must be able to paste one back.
func regionHosts(regions []regionChoice) []string {
	out := make([]string, 0, len(regions))
	for _, r := range regions {
		if r.host != "" {
			out = append(out, r.host)
		}
	}
	slices.Sort(out)
	return out
}

// runNativeRepoView is `repo view` for an Entire-native repo: its identity,
// then every cluster holding a copy of it.
//
// The view joins two reads because neither is complete on its own: the repo
// carries its primary placement, and /native-mirrors lists only the ADDITIONAL
// ones. It deliberately does not go through the /repos directory that the
// GitHub path uses — only this endpoint carries stage and lastError, the two
// fields that say anything useful about a placement that is stuck.
//
// The ref is resolved by the shared repo resolver, so every spelling `repo
// view` has always taken reaches this view: the /et/<project>/<repo> path, a
// bare name with --project, and a repo ULID.
//
// clusterHost is empty for every one of those, which name no cluster and so
// resolve on the active context's core. An entire:// clone URL names one, and
// is resolved there instead (coreRunnerFor).
func runNativeRepoView(cmd *cobra.Command, ref, project, clusterHost string, authoritative bool) error {
	return coreRunnerFor(clusterHost)(cmd, func(ctx context.Context, c *coreapi.Client) error {
		resolved, err := resolveRepoRefResolved(ctx, c, ref, project)
		if err != nil {
			return err
		}
		repoID := resolved.ID
		repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
		if err != nil {
			return err
		}
		cat, err := c.ListClusters(ctx)
		if err != nil {
			return err
		}
		clusters := cat.Clusters
		mirrors, err := listNativeMirrors(ctx, c, repo.ID)
		if err != nil {
			return err
		}
		// The name is the server's, never a ref rebuilt from what the user
		// typed: `repo view` also takes a ULID and a bare name, neither of which
		// spells the /et/<project>/<repo> form the other verbs want back, and
		// echoing the typed path beside whatever ULID resolved reads as success
		// even when the two disagree (COR-1892).
		//
		// Two server answers, most specific first. The repo's own path is
		// absent in the seconds after create, before the coordinates the create
		// response already carried reach the registry; the resolution that
		// found this repo carries the server's full name for it and covers that
		// window. A ULID ref looked nothing up, so neither exists and the bare
		// name is all there is.
		name := strings.TrimSpace(repo.Path.Or(""))
		if name == "" {
			name = resolved.Name
		}
		if name == "" {
			name = repo.Name
		}
		// A plain repo read leaves `state` unset, which would dash the one cell
		// in this table that says whether the primary is usable — and a dashed
		// primary next to a "ready" mirror reads as broken. The authoritative
		// read is the one that answers it (the same flag `repo view` exposes).
		//
		// Only the lifecycle pair is taken from it, never the whole repo: an
		// authoritative lifecycle response can omit clusterSlug and path (see
		// the fixture in repo_readiness_test.go), and swapping the object
		// wholesale would drop the primary placement and every clone URL this
		// view exists to show. Best-effort for the same reason it is narrow — a
		// registry-only fallback cannot answer the readiness question, and a
		// dash is a better trade than losing the table.
		auth, aerr := c.GetRepo(ctx, coreapi.GetRepoParams{
			RepoId:        repo.ID,
			Authoritative: coreapi.NewOptBool(true),
		})
		switch {
		case aerr == nil:
			// State and reason are one answer: the reason is why the state is
			// what it is, and it is all the STATUS cell has to explain a primary
			// that failed. Taking the state fresh and the reason from the plain
			// read would pair them across two moments — so they move together,
			// including when the fresh answer carries no reason at all
			// (retainRepoCreation replaces the field for the same reason).
			if state, ok := auth.State.Get(); ok {
				repo.State = coreapi.NewOptString(state)
				repo.ProvisionReason = auth.ProvisionReason
			}
		case errors.Is(aerr, context.Canceled), errors.Is(aerr, context.DeadlineExceeded):
			// An interrupted read is not a readiness answer, so it is never
			// swallowed the way a server that cannot answer is: without this the
			// default (flagless) path printed a table built on the plain read's
			// stale state and exited 0, reporting success for a command the user
			// stopped.
			return aerr
		case authoritative && readinessCheckUnavailable(aerr):
			// --authoritative is the caller saying the readiness answer is the
			// point of the command, so a registry-only fallback that cannot give
			// one is an error rather than a dashed cell. Print here so
			// renderCoreError cannot strip the recovery hint with the API error
			// wrapper; the plain read is the default, so the hint names no flag.
			fmt.Fprintf(cmd.ErrOrStderr(), "%v\nUse entire repo view %s to inspect repository details without a readiness check.\n", renderRepoReadError(aerr), repoID)
			return NewSilentError(aerr)
		case authoritative:
			// Any other failure of the authoritative read is a real error under
			// the flag, and reads better as itself than as a readiness hint.
			return aerr
		default:
			// Without the flag the table still renders — losing it costs more
			// than a dashed cell — but the failure is disclosed. Silence here
			// let a core outage downgrade every `repo view` to a table that
			// asserts nothing is wrong, at exit 0; fetchRepoDirCatalog refuses
			// exactly that trade for the same reason.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not confirm provisioning state: %v\nThe primary's STATUS is unknown; pass --authoritative to fail instead.\n", renderRepoReadError(aerr))
		}
		row := nativeRepoDetailRow(name, repo, mirrors, clusters)
		if jsonRequested(cmd) {
			return printJSON(cmd.OutOrStdout(), row)
		}
		renderRepoDetail(cmd.OutOrStdout(), row)
		reportNativeMirrorNotes(cmd.ErrOrStderr(), repo, mirrors, clusterHostBySlug(clusters))
		return nil
	})
}

// primaryPlacementStatus renders a repo's own lifecycle state in the vocabulary
// the rest of the STATUS column speaks. A repo says "active" where a placement
// says "ready", and "provisioning" where one says "processing": the same two
// facts under two names, which in a single column reads as a difference that is
// not there.
//
// An unrecognised value passes through unchanged. State is an open string on
// the wire, so translating one the server added later would be a guess printed
// as fact — and the mapping lives on the FIELD, not the rendering, so --json
// and `mirror list --status ready` agree with the table rather than needing
// the repo vocabulary nobody else uses.
func primaryPlacementStatus(state string) string {
	switch state {
	case repoStateActive:
		return string(coreapi.NativeMirrorPlacementStatusReady)
	case repoStateProvisioning:
		return string(coreapi.NativeMirrorPlacementStatusProcessing)
	default:
		return state
	}
}

// nativeRepoDetailRow shapes a native repo and its mirrors into the same row
// the GitHub detail view renders, so both forges produce one table and one
// --json shape. The primary comes first; the mirrors follow in slug order.
func nativeRepoDetailRow(name string, repo *coreapi.Repo, mirrors []coreapi.NativeMirrorPlacement, clusters []coreapi.Cluster) repoDirRow {
	hostBySlug := clusterHostBySlug(clusters)
	// The catalog is the source of truth for a cluster's host, because the
	// record and the catalog drift. But falling to "" on a catalog miss made
	// `repo view` report no clone URL for a repo `repo clone` and `repo create
	// --json` both resolve from repo.ClusterHost — one repo, two answers. The
	// record is the fallback, and only through validateClusterHost: an
	// unvalidated host in a pasted clone URL is the spoofing risk this view
	// refuses to hand out.
	cloneURL := func(slug string) string {
		path := strings.TrimSpace(repo.Path.Or(""))
		if path == "" {
			return ""
		}
		host := hostBySlug[slug]
		if host == "" && slug == repo.ClusterSlug.Or("") {
			if recorded := strings.TrimSpace(repo.ClusterHost.Or("")); validateClusterHost(recorded) == nil {
				host = recorded
			}
		}
		if host == "" {
			return ""
		}
		return entireCloneURLScheme + host + "/" + strings.TrimPrefix(path, "/")
	}

	jurisdictionOf := func(slug string) string {
		if cl, ok := clusterBySlug(clusters, slug); ok {
			return cl.Jurisdiction
		}
		return ""
	}

	placements := make([]repoDirPlacement, 0, len(mirrors)+1)
	if primary := repo.ClusterSlug.Or(""); primary != "" {
		// The primary has no placement record of its own here, so its status is
		// the repo's provisioning state — the same question, answered by the
		// only field that answers it, in the column's own vocabulary.
		placements = append(placements, repoDirPlacement{
			Cluster:      placementCluster(hostBySlug, primary),
			ClusterSlug:  primary,
			Jurisdiction: jurisdictionOf(primary),
			Status:       primaryPlacementStatus(repo.State.Or("")),
			Role:         placementRolePrimary,
			CloneURL:     cloneURL(primary),
		})
	}
	// Sorted by the CLUSTER cell itself, which is what a reader follows — the
	// GitHub half of this shared table sorts on the same value. Ordering by the
	// slug put the rows in a sequence unrelated to the column displayed; so
	// does ordering by the raw host, because placementCluster falls back to the
	// slug when the catalog has no host, and then the key and the cell are
	// different strings. The slug breaks ties for determinism.
	sorted := slices.Clone(mirrors)
	slices.SortFunc(sorted, func(a, b coreapi.NativeMirrorPlacement) int {
		if c := strings.Compare(placementCluster(hostBySlug, a.ClusterSlug), placementCluster(hostBySlug, b.ClusterSlug)); c != 0 {
			return c
		}
		return strings.Compare(a.ClusterSlug, b.ClusterSlug)
	})
	for _, m := range sorted {
		p := repoDirPlacement{
			Cluster:      placementCluster(hostBySlug, m.ClusterSlug),
			ClusterSlug:  m.ClusterSlug,
			Jurisdiction: jurisdictionOf(m.ClusterSlug),
			Status:       string(m.Status),
			Role:         placementRoleMirror,
			CloneURL:     cloneURL(m.ClusterSlug),
		}
		if m.Status == coreapi.NativeMirrorPlacementStatusProcessing {
			p.Stage = string(m.Stage)
		}
		p.Removing = m.DesiredState == coreapi.NativeMirrorPlacementDesiredStateDeleted
		placements = append(placements, p)
	}
	// The project's NAME comes out of the repo's own path, which already spells
	// it — the repo record carries only the owning project's ULID, and resolving
	// that to a name would cost a round trip to print something the path in the
	// row above already shows.
	project, _, perr := parseNativeCloneRef(name)
	if perr != nil {
		project = ""
	}
	return repoDirRow{
		Repo:    name,
		Private: visibilityOf(repo.Visibility.Or("")),
		// The same fold the GitHub path applies. Leaving it unset emitted
		// `"status": ""` for a repo whose placements plainly agreed, which is
		// half of a row shape the two forges are supposed to share.
		Status: sharedPlacementStatus(placements),
		ID:     repo.ID,
		// The repo's own lifecycle word, unmapped. A repo with no placement
		// yet has one of these and no status at all, so this is what tells a
		// failed repo from a read that landed too early.
		State:           strings.TrimSpace(repo.State.Or("")),
		Project:         project,
		ProvisionReason: strings.TrimSpace(repo.ProvisionReason.Or("")),
		Placements:      placements,
	}
}

// reportNativeMirrorNotes writes what the table has no column for: the server's
// own reason for a placement that is not healthy. It goes to stderr so a piped
// table or --json stays clean, and names the cluster so a multi-placement repo
// stays legible.
func reportNativeMirrorNotes(w io.Writer, repo *coreapi.Repo, mirrors []coreapi.NativeMirrorPlacement, hostBySlug map[string]string) {
	// The primary's equivalent of a mirror's lastError: the STATUS cell says a
	// repo failed to provision, and this is the only place that says why.
	if reason := strings.TrimSpace(repo.ProvisionReason.Or("")); reason != "" {
		// A repo that never got placed has no cluster to name, and prefixing
		// the one line carrying the failure reason with ": " loses its subject
		// without gaining one.
		if cluster := placementCluster(hostBySlug, repo.ClusterSlug.Or("")); cluster != "" {
			fmt.Fprintf(w, "%s: %s\n", cluster, reason)
		} else {
			fmt.Fprintln(w, reason)
		}
	}
	for _, m := range mirrors {
		if detail := strings.TrimSpace(m.LastError.Or("")); detail != "" {
			fmt.Fprintf(w, "%s: %s\n", placementCluster(hostBySlug, m.ClusterSlug), detail)
		}
	}
}

// nativeUsePlacements lists the clusters `repo remote add` may point a git
// remote at: the repo's primary, plus every mirror that is actually readable.
//
// A placement that is still seeding, failed or suspended serves nothing, so
// offering it would hand the user a remote that cannot fetch. The primary is
// always offered: it is where the repo lives, and it is ready by definition
// once the repo is. Pushes are not a reason — a ready mirror serves those too,
// which is why a remote pointed at one needs no separate push target.
//
// The result is coreapi.ResolvedPlacement, the shape the GitHub path resolves
// from the server, so both forges feed one picker (selectPlacement) instead of
// growing a second. Cell carries the slug, which is what the picker labels and
// what --cluster names.
func nativeUsePlacements(repo *coreapi.Repo, mirrors []coreapi.NativeMirrorPlacement, clusters []coreapi.Cluster) []coreapi.ResolvedPlacement {
	hostBySlug := clusterHostBySlug(clusters)
	out := make([]coreapi.ResolvedPlacement, 0, len(mirrors)+1)
	add := func(slug string) {
		host := hostBySlug[slug]
		if host == "" {
			return // unresolvable or unsafe public URL: never offer a spoofable remote
		}
		p := coreapi.ResolvedPlacement{ClusterHost: host, Cell: coreapi.NewOptString(slug)}
		if cl, ok := clusterBySlug(clusters, slug); ok && cl.Jurisdiction != "" {
			p.Jurisdiction = coreapi.NewOptString(cl.Jurisdiction)
		}
		out = append(out, p)
	}
	if primary := repo.ClusterSlug.Or(""); primary != "" {
		add(primary)
	}
	for _, m := range mirrors {
		if m.Status != coreapi.NativeMirrorPlacementStatusReady ||
			m.DesiredState == coreapi.NativeMirrorPlacementDesiredStateDeleted {
			continue
		}
		add(m.ClusterSlug)
	}
	return out
}

// nativeRepoURLAt builds the entire:// URL for this repo on host, from the
// server's own `path`. Empty when the repo has no path yet (still provisioning)
// or the host could not be validated.
func nativeRepoURLAt(repo *coreapi.Repo, host string) string {
	path := strings.TrimSpace(repo.Path.Or(""))
	if host == "" || path == "" {
		return ""
	}
	return entireCloneURLScheme + host + "/" + strings.TrimPrefix(path, "/")
}
