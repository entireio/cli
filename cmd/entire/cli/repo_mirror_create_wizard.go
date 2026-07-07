package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// mirrorCreateConcurrency bounds how many (repo, region) mirror creations run
// at once. The slow phase is the per-mirror clone wait, which is I/O-bound on
// the cluster, so a modest fan-out keeps the wizard responsive without
// hammering the control plane or the user's STS.
const mirrorCreateConcurrency = 8

// Per-mirror outcome labels shown in the results table's STATUS column.
const (
	mirrorStatusReady      = "ready"      // clone landed, ready to use
	mirrorStatusRegistered = "registered" // placement created, clone in progress (--no-wait)
	mirrorStatusEmpty      = "empty"      // upstream has no commits, nothing to clone
	mirrorStatusSuspended  = "suspended"  // placement exists but the cluster won't serve it
	mirrorStatusFailed     = "failed"     // initial clone reached the terminal failed status
	mirrorStatusTimedOut   = "timed out"  // clone didn't finish within --wait-timeout
	mirrorStatusError      = "error"      // create or poll failed
)

// regionChoice is one mirrorable region offered by the create wizard's region
// picker, sourced from the control plane's cluster catalog
// (GET /api/v1/clusters via availableRegions).
type regionChoice struct {
	slug         string
	jurisdiction string
	host         string // bare cluster host passed to CreateMirror / validateClusterHost
	isDefault    bool
}

// availableRegions lists the data-plane clusters the user may mirror into,
// fetched from the control plane's cluster catalog. A cluster whose advertised
// public URL can't be safely reduced to a bare host (hostFromPublicURL) is
// skipped rather than failing the whole wizard, so a single malformed entry
// can't block onboarding into the others.
func availableRegions(ctx context.Context, c *coreapi.Client) ([]regionChoice, error) {
	out, err := c.ListClusters(ctx)
	if err != nil {
		return nil, renderCoreError(err)
	}
	return clustersToRegions(out.Clusters), nil
}

// clustersToRegions maps the catalog's clusters to picker choices, dropping any
// whose advertised public URL can't be safely reduced to a bare host.
func clustersToRegions(clusters []coreapi.Cluster) []regionChoice {
	regions := make([]regionChoice, 0, len(clusters))
	for _, cl := range clusters {
		host, herr := hostFromPublicURL(cl.PublicUrl)
		if herr != nil {
			continue
		}
		regions = append(regions, regionChoice{
			slug:         cl.Slug,
			jurisdiction: cl.Jurisdiction,
			host:         host,
			isDefault:    cl.IsDefault,
		})
	}
	return regions
}

// hostFromPublicURL extracts the bare cluster host from a cluster's public_url
// (with or without a scheme) and runs it through validateClusterHost, the same
// anti-token-leak guard the positional <cluster-host> arg uses. Kept separate
// so the ListClusters → regionChoice mapping is unit-testable without a live
// catalog.
func hostFromPublicURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty public_url")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse public_url %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("public_url %q has no host", raw)
	}
	// Reject anything beyond scheme://host[:port]. url.Parse demotes the
	// `host@evil.com` userinfo trick into u.User (leaving u.Host=evil.com) and
	// stashes a trailing path in u.Path, neither of which validateClusterHost
	// would otherwise see. A bare "/" path is tolerated: publicUrl is a trusted
	// catalog field, and a trailing slash (https://host/) is benign — rejecting
	// it would silently drop the cluster and could leave the wizard with no
	// regions. Anything richer (a real path, query, fragment, userinfo) is still
	// refused, since the host flows into clone URLs and the STS audience.
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("public_url %q must be scheme://host[:port] only", raw)
	}
	if err := validateClusterHost(u.Host); err != nil {
		return "", err
	}
	return u.Host, nil
}

// selectableAvailableRepos narrows the ListAvailableMirrors result to repos the
// wizard should offer: status "available" (not already mirrored or owner-only)
// with write or admin access (read-only can't be onboarded). Sorted by
// owner/repo for a stable picker order.
func selectableAvailableRepos(avail []coreapi.AvailableMirror) []coreapi.AvailableMirror {
	out := make([]coreapi.AvailableMirror, 0, len(avail))
	for _, m := range avail {
		if m.Status != coreapi.AvailableMirrorStatusAvailable {
			continue
		}
		if m.Access != coreapi.AvailableMirrorAccessWrite && m.Access != coreapi.AvailableMirrorAccessAdmin {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

// multiSelectHeight returns an explicit huh multi-select Height that keeps every
// option visible. huh auto-sizes an unset height to (rendered option lines −
// title/description rows), which collapses to ~1 visible row for short lists
// (e.g. 3 regions vs. a long repo list) — the cause of the region picker
// appearing clamped to one option. We set it to the option count plus slack for
// the title + (possibly wrapped) description so the whole list shows; huh still
// scrolls if the list outgrows the terminal.
func multiSelectHeight(n int) int {
	const headerSlack = 3 // title (1) + description (1–2 when wrapped)
	return n + headerSlack
}

// clusterChoices maps regions to multi-select options (value = bare host),
// listing every cluster, and returns the host(s) that should start checked:
// the default cluster for the caller's jurisdiction. is_default is
// per-jurisdiction, so pre-selecting only the caller's avoids defaulting a repo
// into every jurisdiction. With no jurisdiction nothing is pre-checked (the user
// picks). All clusters stay selectable regardless.
//
// The caller's-jurisdiction clusters are listed first so that on a short
// terminal — where huh's option viewport shows only the top rows — the visible,
// pre-checked default is the relevant one, not some other jurisdiction's.
func clusterChoices(regions []regionChoice, jurisdiction string) (opts []huh.Option[string], defaults []string) {
	ordered := make([]regionChoice, len(regions))
	copy(ordered, regions)
	if jurisdiction != "" {
		sort.SliceStable(ordered, func(i, j int) bool {
			return ordered[i].jurisdiction == jurisdiction && ordered[j].jurisdiction != jurisdiction
		})
	}
	opts = make([]huh.Option[string], 0, len(ordered))
	for _, r := range ordered {
		opts = append(opts, huh.NewOption(regionLabel(r), r.host))
		if jurisdiction != "" && r.isDefault && r.jurisdiction == jurisdiction {
			defaults = append(defaults, r.host)
		}
	}
	return opts, defaults
}

// regionLabel is the human label for a region in the picker and the results
// table: "slug (jurisdiction)" when both are known, else whatever identifier we
// have, falling back to the bare host.
func regionLabel(r regionChoice) string {
	switch {
	case r.slug != "" && r.jurisdiction != "":
		return fmt.Sprintf("%s (%s)", r.slug, r.jurisdiction)
	case r.slug != "":
		return r.slug
	default:
		return r.host
	}
}

// resolveOneShotClusterHost picks the cluster `repo mirror create
// <github-url>` targets when [cluster-host] is omitted. Non-interactive
// callers keep the fixed defaultClusterHost so scripts stay stable and
// offline-resolvable; on a terminal the control plane's cluster catalog is
// offered as a single-select (skipped when only one cluster exists),
// pre-selecting the caller's jurisdiction default — the same
// prompt-only-when-there-is-a-choice shape `repo clone` uses for
// multi-cluster placements.
func resolveOneShotClusterHost(cmd *cobra.Command) (string, error) {
	if !interactive.CanPromptInteractively() {
		return defaultClusterHost, nil
	}
	errW := cmd.ErrOrStderr()
	var (
		regions      []regionChoice
		jurisdiction string
	)
	if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		stop := startSpinner(errW, "Fetching clusters")
		var err error
		if regions, err = availableRegions(ctx, c); err != nil {
			stop(false)
			return err
		}
		// The jurisdiction only pre-selects the picker's default; a /me
		// hiccup shouldn't sink the create, so fall back to no pre-selection.
		if me, merr := c.GetMe(ctx); merr == nil {
			jurisdiction, _ = me.Jurisdiction.Get()
		}
		stop(true)
		return nil
	}); err != nil {
		return "", err
	}
	if len(regions) == 0 {
		return "", errors.New("no clusters available to mirror into; pass [cluster-host] explicitly")
	}
	if len(regions) == 1 {
		fmt.Fprintf(errW, "Using cluster %s\n", regions[0].host)
		return regions[0].host, nil
	}
	return pickOneCluster(cmd.Context(), errW, regions, jurisdiction)
}

// pickOneCluster runs the one-shot create's cluster single-select,
// pre-selecting the default cluster for the caller's jurisdiction. A clean
// cancel (Ctrl+C / cancelled ctx) surfaces as a SilentError so the create
// stops instead of falling through to a cluster the user didn't choose.
func pickOneCluster(ctx context.Context, w io.Writer, regions []regionChoice, jurisdiction string) (string, error) {
	opts, defaults := clusterChoices(regions, jurisdiction)
	var selected string
	if len(defaults) > 0 {
		selected = defaults[0]
	}
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select the cluster to mirror into").
				Options(opts...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if cerr := handleFormCancellation(w, "Mirror create", err); cerr != nil {
			return "", cerr
		}
		return "", NewSilentError(errors.New("mirror create cancelled"))
	}
	// Guard the selection against the offered hosts (like repo clone's
	// picker) so a zero-value fall-through can't reach the caller as a
	// misleading "invalid [cluster-host]" error.
	for _, r := range regions {
		if r.host == selected {
			return selected, nil
		}
	}
	return "", NewSilentError(errors.New("mirror create cancelled"))
}

// mirrorTarget is one unit of work: a selected repo to be mirrored into a
// selected region. The wizard creates the cross-product of repos × regions.
type mirrorTarget struct {
	owner  string
	repo   string
	region regionChoice
}

// mirrorTargets expands the selected repos and regions into the full
// cross-product of (repo, region) pairs.
func mirrorTargets(repos []coreapi.AvailableMirror, regions []regionChoice) []mirrorTarget {
	targets := make([]mirrorTarget, 0, len(repos)*len(regions))
	for _, r := range repos {
		for _, reg := range regions {
			targets = append(targets, mirrorTarget{owner: r.Owner, repo: r.Repo, region: reg})
		}
	}
	return targets
}

// mirrorResult is the outcome of creating one (repo, region) mirror.
type mirrorResult struct {
	owner       string
	repo        string
	regionLabel string
	cloneURL    string
	status      string // ready | registered | empty | suspended | timed out | error
	err         error
}

var mirrorCreateResultColumns = []string{"REPO", "REGION", "STATUS", "CLONE URL"}

func mirrorCreateResultRow(r mirrorResult) []string {
	url := r.cloneURL
	if url == "" {
		url = placeholderDash
	}
	return []string{r.owner + "/" + r.repo, r.regionLabel, r.status, url}
}

// runMirrorCreateWizard is the zero-argument `entire repo mirror create` flow:
// verify auth, pick repos, pick regions, then create the cross-product of
// mirrors in parallel and report the clone URLs. noWait/waitTimeout carry the
// same meaning as the positional-arg create path.
func runMirrorCreateWizard(cmd *cobra.Command, noWait bool, waitTimeout time.Duration) error {
	cmd.SilenceUsage = true
	ctx := cmd.Context()
	outW := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	// The wizard drives interactive huh pickers, so it needs a real terminal.
	// Without one (CI, pipes), fail fast with a clear pointer at the
	// non-interactive form rather than letting huh error obscurely.
	if !interactive.CanPromptInteractively() {
		fmt.Fprintln(errW, "The mirror create wizard needs an interactive terminal.")
		fmt.Fprintln(errW, "Run 'entire repo mirror create <github-url> [cluster-host]' to create one non-interactively.")
		return NewSilentError(errors.New("not an interactive terminal"))
	}

	insecure := insecureHTTPRequested(cmd)
	if insecure {
		auth.EnableInsecureHTTP()
	}

	jurisdiction, err := ensureMirrorWizardAuth(ctx, errW, insecure)
	if err != nil {
		return err
	}

	client, err := coreapi.New()
	if err != nil {
		return fmt.Errorf("connect to Entire control plane: %w", err)
	}

	// --- pick repos ---------------------------------------------------------
	stopRepos := startSpinner(errW, "Fetching available repos")
	avail, err := client.ListAvailableMirrors(ctx, coreapi.ListAvailableMirrorsParams{})
	if err != nil {
		stopRepos(false)
		return renderCoreError(err)
	}
	stopRepos(true)
	repos := selectableAvailableRepos(avail.Available)
	if len(repos) == 0 {
		fmt.Fprintln(errW, "No GitHub repos available to mirror (you need write access to a repo that isn't mirrored yet).")
		fmt.Fprintln(errW, "Run 'entire repo mirror list --show-available' to see what's onboardable.")
		return nil
	}
	selectedRepos, err := pickRepos(ctx, outW, repos)
	if err != nil || len(selectedRepos) == 0 {
		return err
	}

	// --- pick regions -------------------------------------------------------
	stopRegions := startSpinner(errW, "Fetching regions")
	regions, err := availableRegions(ctx, client)
	if err != nil {
		stopRegions(false)
		return fmt.Errorf("list regions: %w", err)
	}
	stopRegions(true)
	if len(regions) == 0 {
		return errors.New("no regions available to mirror into")
	}
	selectedRegions, err := pickRegions(ctx, outW, regions, jurisdiction)
	if err != nil || len(selectedRegions) == 0 {
		return err
	}

	// --- create + poll ------------------------------------------------------
	targets := mirrorTargets(selectedRepos, selectedRegions)
	results := createMirrors(ctx, errW, targets, noWait, waitTimeout)

	// A cancelled run (Ctrl+C) leaves in-flight mirrors looking like errors;
	// exit quietly instead of reporting them as "N mirror(s) failed".
	if ctx.Err() != nil {
		return NewSilentError(ctx.Err())
	}
	return reportMirrorResults(outW, errW, results)
}

// ensureMirrorWizardAuth mirrors `entire auth status`: resolve the active
// target (honouring ENTIRE_TOKEN), enforce TLS on the core we'll dial, and
// validate the token with a /me probe so the wizard fails fast with a re-login
// hint rather than deep inside the first API call. Returns the caller's home
// jurisdiction (from /me, may be "") so the region picker can pre-select that
// jurisdiction's default cluster.
func ensureMirrorWizardAuth(ctx context.Context, errW io.Writer, insecure bool) (string, error) {
	target, err := resolveAuthStatusTarget(ctx, auth.Contexts, auth.RefreshedLoginToken)
	if err != nil {
		return "", err
	}
	if target.token == "" {
		fmt.Fprintln(errW, "Not logged in. Run 'entire login' to authenticate.")
		return "", NewSilentError(errors.New("not logged in"))
	}
	if !insecure && target.coreURL != "" {
		if err := api.RequireSecureURL(target.coreURL); err != nil {
			return "", fmt.Errorf("login server URL check: %w", err)
		}
	}
	profile, err := defaultFetchProfile(ctx, target.coreURL, target.token)
	if err != nil {
		if isKeychainTokenRejected(err) {
			fmt.Fprintf(errW, "Login for %s is no longer valid. Run 'entire login' to re-authenticate.\n", target.coreURL)
			return "", NewSilentError(errors.New("login no longer valid"))
		}
		return "", fmt.Errorf("validate auth: %w", err)
	}
	if profile.Jurisdiction != "" {
		fmt.Fprintf(errW, "Signed in as %s (%s) via %s\n", profile.Handle, profile.Jurisdiction, target.coreURL)
	} else {
		fmt.Fprintf(errW, "Signed in as %s via %s\n", profile.Handle, target.coreURL)
	}
	return profile.Jurisdiction, nil
}

// pickRepos runs the repo multi-select and returns the chosen available
// mirrors. A clean cancel (Ctrl+C / cancelled ctx) returns (nil, nil).
func pickRepos(ctx context.Context, w io.Writer, repos []coreapi.AvailableMirror) ([]coreapi.AvailableMirror, error) {
	repoByKey := make(map[string]coreapi.AvailableMirror, len(repos))
	options := make([]huh.Option[string], len(repos))
	for i, m := range repos {
		key := m.Owner + "/" + m.Repo
		repoByKey[key] = m
		options[i] = huh.NewOption(key, key)
	}

	var selected []string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select repos to mirror").
				Description("Space to select, enter to confirm.").
				Options(options...).
				Height(multiSelectHeight(len(options))).
				Validate(func(s []string) error {
					if len(s) == 0 {
						return errors.New("select at least one repo")
					}
					return nil
				}).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return nil, handleFormCancellation(w, "Mirror create", err)
	}

	chosen := make([]coreapi.AvailableMirror, 0, len(selected))
	for _, key := range selected {
		if m, ok := repoByKey[key]; ok {
			chosen = append(chosen, m)
		}
	}
	return chosen, nil
}

// pickRegions runs the region multi-select, pre-selecting the default cluster
// for the caller's jurisdiction. A clean cancel returns (nil, nil).
func pickRegions(ctx context.Context, w io.Writer, regions []regionChoice, jurisdiction string) ([]regionChoice, error) {
	opts, defaults := clusterChoices(regions, jurisdiction)
	regionByHost := make(map[string]regionChoice, len(regions))
	for _, r := range regions {
		regionByHost[r.host] = r
	}

	// Pre-fill with the default hosts so they start checked.
	selected := append([]string(nil), defaults...)
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select regions to mirror into").
				Description("Each repo is mirrored into every selected region.").
				Options(opts...).
				Height(multiSelectHeight(len(opts))).
				Validate(func(s []string) error {
					if len(s) == 0 {
						return errors.New("select at least one region")
					}
					return nil
				}).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return nil, handleFormCancellation(w, "Mirror create", err)
	}

	chosen := make([]regionChoice, 0, len(selected))
	for _, host := range selected {
		if r, ok := regionByHost[host]; ok {
			chosen = append(chosen, r)
		}
	}
	return chosen, nil
}

// createMirrors fans out CreateMirror (and the clone-readiness poll) across all
// targets in parallel, returning one result per target in input order. One
// cluster client is built per region and shared across that region's repos; a
// region the active login can't reach fails every pair in that region rather
// than aborting the whole run.
func createMirrors(ctx context.Context, errW io.Writer, targets []mirrorTarget, noWait bool, waitTimeout time.Duration) []mirrorResult {
	// One client per distinct region, built once.
	clientByHost := make(map[string]*coreapi.Client)
	clientErrByHost := make(map[string]error)
	for _, t := range targets {
		if _, seen := clientByHost[t.region.host]; seen {
			continue
		}
		if _, seen := clientErrByHost[t.region.host]; seen {
			continue
		}
		c, err := coreapi.NewForCluster(ctx, t.region.host)
		if err != nil {
			clientErrByHost[t.region.host] = err
		} else {
			clientByHost[t.region.host] = c
		}
	}

	// Docker-pull-style live progress: one line per (repo, region), each
	// updating independently as its CreateMirror + clone poll advance.
	labels := make([]string, len(targets))
	for i, t := range targets {
		labels[i] = t.owner + "/" + t.repo + " @ " + t.region.host
	}
	prog := newMirrorProgress(errW, labels)
	prog.start()

	results := make([]mirrorResult, len(targets))
	g := new(errgroup.Group)
	g.SetLimit(mirrorCreateConcurrency)
	for i, t := range targets {
		g.Go(func() error {
			results[i] = createOneMirror(ctx, t, clientByHost[t.region.host], clientErrByHost[t.region.host], noWait, waitTimeout,
				func(status string, final, ok bool) { prog.set(i, status, final, ok) })
			return nil
		})
	}
	// createOneMirror folds every failure into results and never returns an
	// error, so Wait is structurally always nil; check it anyway to satisfy
	// errcheck and stay correct if that invariant ever changes.
	if err := g.Wait(); err != nil {
		fmt.Fprintf(errW, "mirror creation: %v\n", err)
	}
	prog.stop()
	return results
}

// createOneMirror registers a single (repo, region) mirror and, unless noWait
// or the upstream is empty, waits for its initial clone. It never returns an
// error: every outcome is folded into the mirrorResult so a single failure
// can't sink the batch. report (may be nil) is called as the mirror moves
// through its phases so the caller can render live progress; the final call has
// final=true and ok set to whether it succeeded.
func createOneMirror(ctx context.Context, t mirrorTarget, c *coreapi.Client, clientErr error, noWait bool, waitTimeout time.Duration, report func(status string, final, ok bool)) mirrorResult {
	if report == nil {
		report = func(string, bool, bool) {}
	}
	res := mirrorResult{owner: t.owner, repo: t.repo, regionLabel: regionLabel(t.region)}
	if clientErr != nil {
		res.status, res.err = mirrorStatusError, clientErr
		report(mirrorStatusError, true, false)
		return res
	}
	report("creating", false, false)
	// Same create-then-wait path as the one-shot `repo mirror create <url>`
	// (createAndAwaitMirror), so both report identical lifecycle states. The
	// per-poll status drives this mirror's progress line.
	outcome, err := createAndAwaitMirror(ctx, c, t.owner, t.repo, t.region.host, noWait, waitTimeout, nil,
		func(s coreapi.MirrorStatus) { report(string(s), false, false) })
	if outcome.created == nil {
		res.status, res.err = mirrorStatusError, renderCoreError(err)
		report(mirrorStatusError, true, false)
		return res
	}
	res.cloneURL = outcome.created.MirrorUrl

	if outcome.created.Suspended {
		// An admin suspended this existing placement, so it won't be served.
		// Surface it as a distinct status and set an error so the batch exits
		// non-zero, matching the one-shot: a suspended mirror isn't a success.
		res.status, res.err = mirrorStatusSuspended, errors.New("suspended by an admin; won't be usable")
		report(mirrorStatusSuspended, true, false)
		return res
	}

	if !outcome.polled {
		if outcome.created.Empty {
			res.status = mirrorStatusEmpty
		} else {
			res.status = mirrorStatusRegistered
		}
		report(res.status, true, true)
		return res
	}

	// nonTerminal classifies a still-processing/unknown result: the poll ended
	// without a terminal status, so the wait timed out or a poll call errored.
	nonTerminal := func() {
		if errors.Is(err, context.DeadlineExceeded) {
			res.status, res.err = mirrorStatusTimedOut, err
		} else {
			res.status, res.err = mirrorStatusError, err
		}
	}
	switch outcome.status {
	case coreapi.MirrorStatusReady:
		res.status = mirrorStatusReady
	case coreapi.MirrorStatusSuspended:
		res.status, res.err = mirrorStatusSuspended, errors.New("the mirror is suspended; contact support")
	case coreapi.MirrorStatusFailed:
		res.status, res.err = mirrorStatusFailed, errors.New("the initial clone failed; contact support")
	case coreapi.MirrorStatusProcessing:
		nonTerminal()
	default:
		nonTerminal()
	}
	report(res.status, true, res.err == nil)
	return res
}

// mirrorProgress renders a Docker-pull-style live list: one line per mirror,
// each showing its label and a status that updates independently — a spinner
// while in flight, ✓/✗ once terminal. On a non-terminal writer (pipes, tests)
// it degrades to one printed line per mirror as each reaches a terminal state.
type mirrorProgress struct {
	w       io.Writer
	tty     bool
	labelW  int
	mu      sync.Mutex
	lines   []mirrorProgressLine
	frame   int
	painted bool
	done    chan struct{}
	stopped chan struct{}
}

type mirrorProgressLine struct {
	label   string
	status  string
	final   bool
	ok      bool
	printed bool // non-tty: terminal line already emitted
}

func newMirrorProgress(w io.Writer, labels []string) *mirrorProgress {
	lines := make([]mirrorProgressLine, len(labels))
	labelW := 0
	for i, l := range labels {
		lines[i] = mirrorProgressLine{label: l, status: "queued"}
		if n := len(l); n > labelW {
			labelW = n
		}
	}
	return &mirrorProgress{w: w, tty: interactive.IsTerminalWriter(w), labelW: labelW, lines: lines}
}

// start paints the initial block and, on a TTY, begins animating the spinner.
func (p *mirrorProgress) start() {
	if !p.tty {
		return
	}
	p.done = make(chan struct{})
	p.stopped = make(chan struct{})
	p.mu.Lock()
	p.renderLocked()
	p.mu.Unlock()
	go func() {
		defer close(p.stopped)
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.frame++
				p.renderLocked()
				p.mu.Unlock()
			}
		}
	}()
}

// set updates one mirror's line. On a TTY it repaints immediately; otherwise it
// prints a single line the first time the mirror reaches a terminal state.
func (p *mirrorProgress) set(i int, status string, final, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines[i].status = status
	p.lines[i].final = final
	p.lines[i].ok = ok
	switch {
	case p.tty:
		p.renderLocked()
	case final && !p.lines[i].printed:
		p.lines[i].printed = true
		fmt.Fprintf(p.w, "%s %s %s\n", terminalIcon(ok), p.lines[i].label, status)
	}
}

// stop ends the animation and leaves the final state painted.
func (p *mirrorProgress) stop() {
	if !p.tty {
		return
	}
	close(p.done)
	<-p.stopped
	p.mu.Lock()
	p.renderLocked()
	p.mu.Unlock()
}

// renderLocked repaints the whole block in place. Caller holds p.mu.
func (p *mirrorProgress) renderLocked() {
	if p.painted {
		fmt.Fprintf(p.w, "\033[%dA", len(p.lines)) // move up to the block's top
	}
	p.painted = true
	for _, ln := range p.lines {
		var icon string
		if ln.final {
			icon = terminalIcon(ln.ok)
		} else {
			icon = spinnerFrames[p.frame%len(spinnerFrames)]
		}
		fmt.Fprintf(p.w, "\r\033[K%-*s  %s %s\n", p.labelW, ln.label, icon, ln.status)
	}
}

func terminalIcon(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// reportMirrorResults renders the results table, a copy-pasteable git-clone
// block for the ready mirrors, and per-failure detail. It returns a
// SilentError (so the table isn't reprinted) when any mirror failed, giving the
// command a non-zero exit while still showing what succeeded.
func reportMirrorResults(outW, errW io.Writer, results []mirrorResult) error {
	if len(results) == 0 {
		return nil
	}
	// Headroom between the live progress block and the summary table.
	fmt.Fprintln(outW)
	if err := printTable(outW, mirrorCreateResultColumns, results, mirrorCreateResultRow); err != nil {
		return err
	}

	var readyURLs []string
	var failures int
	for _, r := range results {
		if r.status == mirrorStatusReady && r.cloneURL != "" {
			readyURLs = append(readyURLs, r.cloneURL)
		}
		if r.err != nil {
			failures++
		}
	}
	if len(readyURLs) > 0 {
		fmt.Fprintln(outW, "\nClone them:")
		for _, u := range readyURLs {
			fmt.Fprintf(outW, "  git clone %s\n", u)
		}
	}
	if failures > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(errW, "%s/%s @ %s: %v\n", r.owner, r.repo, r.regionLabel, r.err)
			}
		}
		return NewSilentError(fmt.Errorf("%d mirror(s) failed", failures))
	}
	return nil
}
