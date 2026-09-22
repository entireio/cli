package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
)

// mirrorPlacement is one cluster a repo is currently mirrored on, in the terms
// `mirror remove` acts in. Both forges resolve to this so the picker, the
// parallel removal and the result table are written once.
type mirrorPlacement struct {
	region regionChoice
	status string
	// primary marks a native repo's home cluster. It is listed so the picker
	// shows where the repo actually lives, but it is never removable here:
	// dropping it is `entire repo delete`.
	primary bool
}

// listMirrorPlacements reports where a repo is mirrored today, and (for a
// native ref) the repo it resolved, so the caller does not resolve it twice.
//
// A GitHub repo's placements come from the pull-gated resolver (anything you
// could clone), a native repo's from its own primary plus its native-mirror
// list — the primary is not in that list, so it is joined on.
//
// Each placement names its own cluster: the catalog only ENRICHES it with the
// slug and region. A placement whose cluster the catalog does not list (an
// unusable publicUrl, or one dropped from the registry) is still listed rather
// than filtered away — the same choice selectPlacement makes for the clone
// picker.
//
// A placement the catalog does not list keeps its own host, which is what
// --cluster takes, so it stays nameable on the command line as well as in the
// picker. Only a NATIVE placement missing from the catalog is picker-only: it
// carries a slug and no host, and there is no host to type for it.
func listMirrorPlacements(ctx context.Context, c *coreapi.Client, ref mirrorRepoRef, regions []regionChoice) ([]mirrorPlacement, *coreapi.Repo, error) {
	if ref.forge == nativeCloneForge {
		repo, err := resolveNativeRepo(ctx, c, ref.owner, ref.repo)
		if err != nil {
			return nil, nil, err
		}
		mirrors, err := listNativeMirrors(ctx, c, repo.ID)
		if err != nil {
			return nil, nil, err
		}
		out := make([]mirrorPlacement, 0, len(mirrors)+1)
		if primary := repo.ClusterSlug.Or(""); primary != "" {
			out = append(out, mirrorPlacement{region: regionForSlug(regions, primary), status: repo.State.Or("-"), primary: true})
		}
		for _, m := range mirrors {
			out = append(out, mirrorPlacement{region: regionForSlug(regions, m.ClusterSlug), status: string(m.Status)})
		}
		return out, repo, nil
	}

	placements, err := resolvePullablePlacements(ctx, c, ref.owner, ref.repo)
	if err != nil {
		return nil, nil, err
	}
	out := make([]mirrorPlacement, 0, len(placements))
	for _, p := range placements {
		out = append(out, mirrorPlacement{region: regionForHost(regions, p.ClusterHost)})
	}
	return out, nil, nil
}

// regionForSlug names a cluster the catalog knows, or falls back to the slug
// alone. The fallback carries no host, which is all a native removal needs
// (those are addressed by repo ULID and slug) — but it is also why such a
// placement can only be chosen from the picker: --cluster takes a host.
func regionForSlug(regions []regionChoice, slug string) regionChoice {
	if r, ok := regionBySlug(regions, slug); ok {
		return r
	}
	return regionChoice{slug: slug}
}

// regionForHost is the same for a GitHub placement, which names its cluster by
// host. The fallback keeps the host — both what the delete is addressed at and
// what --cluster takes — and uses it as the slug so the placement still has a
// label.
func regionForHost(regions []regionChoice, host string) regionChoice {
	if r, ok := regionByHost(regions, host); ok {
		return r
	}
	return regionChoice{slug: host, host: host}
}

// removableMirrorPlacements drops the ones `mirror remove` must not act on.
func removableMirrorPlacements(placements []mirrorPlacement) []mirrorPlacement {
	out := make([]mirrorPlacement, 0, len(placements))
	for _, p := range placements {
		if p.primary {
			continue
		}
		out = append(out, p)
	}
	return out
}

func mirrorPlacementRegions(placements []mirrorPlacement) []regionChoice {
	out := make([]regionChoice, 0, len(placements))
	for _, p := range placements {
		out = append(out, p.region)
	}
	return out
}

// chooseMirrorRemoveRegions turns --cluster (or the absence of it) into the
// placements a remove will drop.
//
// Unlike `add`, there is no default: removing is destructive, and the set of
// clusters is a property of the repo rather than of the catalog, so guessing
// one would delete a copy the caller never named. A terminal gets a
// multi-select over what the repo actually has — which is also the
// confirmation, since nothing is removed that was not ticked.
func chooseMirrorRemoveRegions(cmd *cobra.Command, ref mirrorRepoRef, placements []mirrorPlacement, regions []regionChoice, hosts []string) ([]regionChoice, error) {
	byPrimary := map[string]bool{}
	for _, p := range placements {
		if p.primary && p.region.host != "" {
			byPrimary[strings.ToLower(p.region.host)] = true
		}
	}
	removable := removableMirrorPlacements(placements)

	if len(hosts) > 0 {
		chosen := make([]regionChoice, 0, len(hosts))
		seen := map[string]bool{}
		for _, host := range hosts {
			if byPrimary[strings.ToLower(host)] {
				return nil, fmt.Errorf("%s lives on %s: that is its primary, not a mirror, and removing it is `entire repo delete %s`", ref.qualified(), host, ref.qualified())
			}
			region, ok := regionByHost(mirrorPlacementRegions(removable), host)
			if !ok {
				// The listing is not the authority on what exists, only on what
				// is worth OFFERING. A GitHub placement comes from the
				// pull-gated resolver, so a failed or suspended one — exactly
				// the kind worth tearing down — can be missing from it. A
				// cluster the user named explicitly is therefore attempted
				// against the server, which answers 404 if it really is not
				// there. Only the picker is limited to what was listed.
				if catalogued, known := regionByHost(regions, host); known {
					region = catalogued
				} else {
					return nil, fmt.Errorf("%s: unknown cluster %q; it is mirrored on: %s", ref.qualified(), host, strings.Join(regionHosts(mirrorPlacementRegions(removable)), ", "))
				}
			}
			if seen[region.slug] {
				continue
			}
			seen[region.slug] = true
			chosen = append(chosen, region)
		}
		return chosen, nil
	}

	if len(removable) == 0 {
		return nil, fmt.Errorf("%s has no mirrors to remove", ref.qualified())
	}
	if !interactive.CanPromptInteractively() {
		return nil, fmt.Errorf("pass --cluster to say which mirror of %s to remove; it is on: %s",
			ref.qualified(), strings.Join(regionHosts(mirrorPlacementRegions(removable)), ", "))
	}
	return pickRemoveRegions(cmd.Context(), cmd.ErrOrStderr(), removable)
}

// runMirrorRemove is the `repo mirror remove <repo>` body: find where the repo
// is mirrored, choose which of those to drop, then remove them in parallel.
func runMirrorRemove(cmd *cobra.Command, repoRef string, clusterHosts []string, timeout time.Duration) error {
	cmd.SilenceUsage = true
	ref, err := parseMirrorRepoRef(repoRef)
	if err != nil {
		return err
	}
	for _, host := range clusterHosts {
		if err := validateClusterHost(host); err != nil {
			return fmt.Errorf("invalid --cluster: %w", err)
		}
	}

	var (
		regions    []regionChoice
		placements []mirrorPlacement
		nativeRepo *coreapi.Repo
	)
	if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		out, lerr := c.ListClusters(ctx)
		if lerr != nil {
			return lerr
		}
		regions = clustersToRegions(out.Clusters)
		placements, nativeRepo, lerr = listMirrorPlacements(ctx, c, ref, regions)
		return lerr
	}); err != nil {
		return err
	}

	chosen, err := chooseMirrorRemoveRegions(cmd, ref, placements, regions, clusterHosts)
	if err != nil {
		return err
	}
	if len(chosen) == 0 {
		// A cancelled picker reports itself and returns no selection (see
		// handleFormCancellation). Stop here rather than running an empty batch,
		// which would print a progress block and a headerless table for nothing.
		return nil
	}
	results := removeMirrors(cmd.Context(), cmd.ErrOrStderr(), oneRepoTargets(ref, nativeRepo, chosen), timeout)
	return reportMirrorRemoveResults(cmd.OutOrStdout(), cmd.ErrOrStderr(), results)
}

// pickRemoveRegions is the remove verb's multi-select. Nothing starts checked —
// unlike `add`, where a default region is a convenience, a pre-ticked box here
// would be a copy deleted by pressing enter. The status of each placement is
// shown because it is often the reason one is being removed.
func pickRemoveRegions(ctx context.Context, w io.Writer, placements []mirrorPlacement) ([]regionChoice, error) {
	opts := make([]huh.Option[string], 0, len(placements))
	bySlug := make(map[string]regionChoice, len(placements))
	for _, p := range placements {
		label := regionLabel(p.region)
		if p.status != "" {
			label += " — " + p.status
		}
		opts = append(opts, huh.NewOption(label, p.region.slug))
		bySlug[p.region.slug] = p.region
	}

	var selected []string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select the mirrors to remove").
				Description("Only the placements you tick are removed; the repo itself is untouched.").
				Options(opts...).
				Height(uiform.SingleLineMultiSelectHeight(len(opts))).
				Validate(func(s []string) error {
					if len(s) == 0 {
						return errors.New("select at least one mirror")
					}
					return nil
				}).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return nil, handleFormCancellation(w, "Mirror remove", err)
	}
	chosen := make([]regionChoice, 0, len(selected))
	for _, slug := range selected {
		if r, ok := bySlug[slug]; ok {
			chosen = append(chosen, r)
		}
	}
	return chosen, nil
}

// removeMirrors tears down every target in parallel, one result per target in
// input order — the same shape createMirrors uses, so a batch remove reports
// like a batch add.
func removeMirrors(ctx context.Context, errW io.Writer, targets []mirrorTarget, timeout time.Duration) []mirrorResult {
	clientByHost := make(map[string]*coreapi.Client)
	clientErrByHost := make(map[string]error)
	for _, t := range targets {
		host := t.clientHost()
		if _, seen := clientByHost[host]; seen {
			continue
		}
		if _, seen := clientErrByHost[host]; seen {
			continue
		}
		c, err := mirrorTargetClient(ctx, host)
		if err != nil {
			clientErrByHost[host] = err
		} else {
			clientByHost[host] = c
		}
	}

	labels := make([]string, len(targets))
	for i, t := range targets {
		labels[i] = t.ref() + " @ " + t.region.slug
	}
	prog := newMirrorProgress(errW, labels)
	prog.start()

	// Bounded like createMirrors: the two are a matched pair, and an unbounded
	// fan-out would differ only by accident.
	results := make([]mirrorResult, len(targets))
	g := new(errgroup.Group)
	g.SetLimit(mirrorCreateConcurrency)
	for i, t := range targets {
		g.Go(func() error {
			results[i] = removeOneMirror(ctx, t, clientByHost[t.clientHost()], clientErrByHost[t.clientHost()], timeout,
				func(status string, final, ok bool) { prog.set(i, status, final, ok) })
			return nil
		})
	}
	//nolint:errcheck // every goroutine returns nil: removeOneMirror folds each
	// outcome into its result so one failure cannot cancel the others.
	_ = g.Wait()
	prog.stop()
	return results
}

// removeOneMirror tears down a single placement. Like its create counterpart it
// never returns an error: every outcome folds into the result so one failure
// cannot sink the batch.
func removeOneMirror(ctx context.Context, t mirrorTarget, c *coreapi.Client, clientErr error, timeout time.Duration, report func(status string, final, ok bool)) mirrorResult {
	// report may be nil, as in createOneMirror: a caller that wants the outcome
	// but not the live progress should not have to supply a no-op.
	if report == nil {
		report = func(string, bool, bool) {}
	}
	res := mirrorResult{forge: t.forge, owner: t.owner, repo: t.repo, regionLabel: regionLabel(t.region)}
	if clientErr != nil {
		res.status, res.err = mirrorStatusError, clientErr
		report(mirrorStatusError, true, false)
		return res
	}
	// Native teardown is asynchronous and waited on, so it needs the same bound
	// `add` puts on its wait: without one a placement the server never reaps
	// hangs the command behind a spinner forever. Zero preserves the caller
	// context, keeping "wait indefinitely" available.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	report(mirrorStatusRemoving, false, false)

	if t.forge == nativeCloneForge {
		if _, err := c.DeleteNativeMirror(ctx, coreapi.DeleteNativeMirrorParams{
			RepoId: t.nativeRepo.ID, ClusterSlug: t.region.slug,
		}); err != nil {
			return failedRemoval(&res, err, report)
		}
		// Teardown is asynchronous: the delete records the intent and the
		// placement disappears later, so the wait is what makes "removed" true.
		if err := awaitNativeMirrorRemoved(ctx, c, t.nativeRepo.ID, t.region.slug); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				res.status, res.err = mirrorStatusTimedOut, err
				report(res.status, true, false)
				return res
			}
			return failedRemoval(&res, err, report)
		}
		res.status = mirrorStatusRemoved
		report(res.status, true, true)
		return res
	}

	if err := c.DeleteMirror(ctx, coreapi.DeleteMirrorParams{
		Provider:    coreapi.DeleteMirrorProviderGithub,
		Owner:       t.owner,
		Repo:        t.repo,
		ClusterHost: t.region.host,
	}); err != nil {
		return failedRemoval(&res, err, report)
	}
	res.status = mirrorStatusRemoved
	report(res.status, true, true)
	return res
}

func failedRemoval(res *mirrorResult, err error, report func(status string, final, ok bool)) mirrorResult {
	res.status, res.err = mirrorStatusError, renderCoreError(err)
	report(mirrorStatusError, true, false)
	return *res
}

var mirrorRemoveResultColumns = []string{colHeaderRepo, colHeaderRegion, colHeaderStatus}

func mirrorRemoveResultRow(r mirrorResult) []string {
	return []string{r.ref(), r.regionLabel, r.status}
}

// reportMirrorRemoveResults prints the summary table and fails the command when
// any placement survived, naming each one.
func reportMirrorRemoveResults(outW, errW io.Writer, results []mirrorResult) error {
	if len(results) == 0 {
		return nil
	}
	fmt.Fprintln(outW)
	if err := printTable(outW, mirrorRemoveResultColumns, results, mirrorRemoveResultRow); err != nil {
		return err
	}
	failures := 0
	for _, r := range results {
		if r.err != nil {
			failures++
			fmt.Fprintf(errW, "%s @ %s: %v\n", r.ref(), r.regionLabel, r.err)
		}
	}
	if failures > 0 {
		return NewSilentError(fmt.Errorf("%d mirror(s) could not be removed", failures))
	}
	return nil
}
