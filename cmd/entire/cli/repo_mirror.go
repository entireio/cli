package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// column is a table column with two separable identities: key is the canonical
// name a caller types for --sort (and the value parseSortColumn returns, so the
// sort switches compare against these constants directly); header is the text
// shown in the table. They differ only where the header carries a display hint
// the sort key shouldn't — e.g. NAME's inline "(owner/repo)" — which keeps
// --sort matching a simple equality on key with no header parsing.
type column struct {
	key    string
	header string
}

// Keys are lower-case, single shell tokens (kebab-case for multi-word columns)
// so `--sort clone-url` needs no quoting; headers stay upper-case display text.
var (
	colName     = column{key: "name", header: "NAME (owner/repo)"}
	colCloneURL = column{key: "clone-url", header: "CLONE URL"}
	colPrivate  = column{key: "private", header: "PRIVATE"}
	colAccess   = column{key: "access", header: "ACCESS"}
	colStatus   = column{key: "status", header: "STATUS"}
)

// columnHeaders is the display-header view of a column set, for the table/field
// renderers (runCoreList/runCoreObject) which take plain header strings.
func columnHeaders(cols []column) []string {
	h := make([]string, len(cols))
	for i, c := range cols {
		h[i] = c.header
	}
	return h
}

// mirrorColumns is the human table/field view of a mirror: the scannable
// owner/repo name, the clone URL you'd copy, and whether the upstream is
// private. Owner, provider, and cluster aren't columns of their own — they're
// inferable from the owner/repo pair and the clone URL
// (entire://<cluster>/gh/<owner>/<repo>). `--name` filters on the owner/repo
// name only; owner/provider/cluster stay server-side filters, and the wire
// model's internal ids are dropped. The clone URL is synthesised from the
// mirror's coords (the form `git clone` accepts), since the list API doesn't
// return it.
var mirrorColumns = []column{colName, colCloneURL, colPrivate}

// mirrorPrivate renders the PRIVATE column ("yes"/"no"), shared by the table
// row and the --sort private key so both agree on the cell value.
func mirrorPrivate(m coreapi.Mirror) string {
	if m.IsPrivate.Or(false) {
		return "yes"
	}
	return "no"
}

func mirrorRow(m coreapi.Mirror) []string {
	repo := m.Owner + "/" + m.Repo
	cloneURL := mirrorCloneURL(m.ClusterHost, m.Owner, m.Repo)
	return []string{repo, cloneURL, mirrorPrivate(m)}
}

// parseSortColumn resolves a --sort spec to the column it names and a
// direction. It trims first, then reads the '-' prefix, so leading/trailing
// whitespace is handled identically on every path (the direction and the column
// name never disagree). An empty spec selects the first column. A spec matches a
// column by its key (case-insensitive) — a plain equality, since key holds no
// display hint. An unknown name errors naming the valid keys. Returning the
// matched column lets callers switch on the col* constants directly.
func parseSortColumn(spec string, columns []column) (col column, desc bool, err error) {
	spec = strings.TrimSpace(spec)
	desc = strings.HasPrefix(spec, "-")
	name := strings.TrimSpace(strings.TrimPrefix(spec, "-"))
	if name == "" {
		return columns[0], desc, nil
	}
	for _, c := range columns {
		if strings.EqualFold(c.key, name) {
			return c, desc, nil
		}
	}
	valid := make([]string, len(columns))
	for i, c := range columns {
		valid[i] = c.key
	}
	return column{}, false, fmt.Errorf("unknown sort column %q; valid columns: %s", name, strings.Join(valid, ", "))
}

// sortMirrors orders mirrors in place by the --sort spec: by the named column's
// value ascending (case-insensitive), always breaking ties by owner/repo then
// cluster host so a repo mirrored across clusters (or rows equal on any other
// column) has a stable, deterministic order rather than arbitrary server order.
// A '-' prefix reverses the whole ordering. `name`/default sorts by the
// tiebreak alone.
func sortMirrors(mirrors []coreapi.Mirror, spec string) error {
	col, desc, err := parseSortColumn(spec, mirrorColumns)
	if err != nil {
		return err
	}
	key := func(m coreapi.Mirror) string {
		switch col {
		case colCloneURL:
			return strings.ToLower(mirrorCloneURL(m.ClusterHost, m.Owner, m.Repo))
		case colPrivate:
			return mirrorPrivate(m)
		default: // name -> tiebreak alone
			return ""
		}
	}
	slices.SortStableFunc(mirrors, func(a, b coreapi.Mirror) int {
		c := cmp.Compare(key(a), key(b))
		if c == 0 {
			c = cmp.Compare(strings.ToLower(a.Owner+"/"+a.Repo), strings.ToLower(b.Owner+"/"+b.Repo))
		}
		if c == 0 {
			c = cmp.Compare(strings.ToLower(a.ClusterHost), strings.ToLower(b.ClusterHost))
		}
		if desc {
			return -c
		}
		return c
	})
	return nil
}

// sortAvailable orders available mirrors in place by the --sort spec, matching
// sortMirrors: by the named column ascending (case-insensitive) with an
// owner/repo tiebreak for a deterministic order on equal keys. AvailableMirror
// has no cluster host (the onboardable set is cluster-agnostic), so owner/repo
// is the only secondary key. A '-' prefix reverses the whole ordering.
func sortAvailable(avail []coreapi.AvailableMirror, spec string) error {
	col, desc, err := parseSortColumn(spec, availableMirrorColumns)
	if err != nil {
		return err
	}
	key := func(m coreapi.AvailableMirror) string {
		switch col {
		case colAccess:
			return strings.ToLower(string(m.Access))
		case colStatus:
			return strings.ToLower(string(m.Status))
		default: // name -> tiebreak alone
			return ""
		}
	}
	slices.SortStableFunc(avail, func(a, b coreapi.AvailableMirror) int {
		c := cmp.Compare(key(a), key(b))
		if c == 0 {
			c = cmp.Compare(strings.ToLower(a.Owner+"/"+a.Repo), strings.ToLower(b.Owner+"/"+b.Repo))
		}
		if desc {
			return -c
		}
		return c
	})
	return nil
}

// filterByName keeps items whose owner/repo name contains substr (case-
// insensitive). The control plane already filters by owner/provider/cluster
// server-side but not by name, so `repo mirror list --name` narrows that last
// dimension client-side. nameOf returns the item's displayed identifier — the
// callers pass the owner/repo form shown in the NAME column, so a value copied
// from the table (e.g. acme/web) matches the row it came from. An empty substr
// returns items unchanged.
func filterByName[T any](items []T, nameOf func(T) string, substr string) []T {
	substr = strings.TrimSpace(substr)
	if substr == "" {
		return items
	}
	substr = strings.ToLower(substr)
	out := make([]T, 0, len(items))
	for _, it := range items {
		if strings.Contains(strings.ToLower(nameOf(it)), substr) {
			out = append(out, it)
		}
	}
	return out
}

// availableMirrorColumns is the view of a repo you *could* mirror: the
// scannable repo name, your effective GitHub access, and whether it's
// onboardable. STATUS is "available" (run `entire repo mirror create` to
// onboard), "mirrored" (already done — `entire repo mirror list` shows the
// clone URL), or "owner-only" (a personal repo of another user; only its
// owner may mirror it). No clone URL column: an un-onboarded repo doesn't
// have one yet.
var availableMirrorColumns = []column{colName, colAccess, colStatus}

func availableMirrorRow(m coreapi.AvailableMirror) []string {
	return []string{m.Owner + "/" + m.Repo, string(m.Access), string(m.Status)}
}

// defaultClusterHost is the cluster the positional-arg mirror commands target
// when the caller omits the <cluster-host> argument. The no-arg create wizard
// and the interactive one-shot `create <github-url>` instead enumerate real
// clusters from the catalog (GET /api/v1/clusters, see availableRegions and
// resolveOneShotClusterHost in repo_mirror_create_wizard.go); this stays as
// the fixed fallback for non-interactive invocations, so scripts keep a
// stable, offline-resolvable default.
const defaultClusterHost = "aws-us-east-2.entire.io"

// clusterArg returns the cluster host from the optional second positional
// (after <github-url>), or defaultClusterHost when it was omitted.
func clusterArg(args []string) string {
	return clusterArgAt(args, 1)
}

// clusterArgAt returns the cluster host from the optional positional at idx,
// or defaultClusterHost when it was omitted. Commands with leading positionals
// (e.g. collaborators list <github-url> [cluster-host]) pass the trailing index.
func clusterArgAt(args []string, idx int) string {
	if len(args) > idx {
		return args[idx]
	}
	return defaultClusterHost
}

// clusterHostLabelRe matches one DNS label: alphanumeric, internal hyphens
// allowed, no leading/trailing hyphen.
var clusterHostLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// validateClusterHost rejects a cluster host that is anything other than a
// bare DNS name or IP with an optional :port. The host is concatenated as
// "https://"+host into the clone URL and the STS audience
// (entireclient/repocreds), so a value carrying URL metacharacters can redirect
// the request — and the repo-scoped basic-auth token it carries — somewhere
// other than the intended cluster. Classic case:
// `aws-us-east-2.entire.io@evil.com`, which Go's URL parser reads as
// host=evil.com with the real cluster demoted to userinfo, leaking the token
// to evil.com. We parse the host the same way the rest of the code does and
// require it to round-trip to a bare host with no userinfo, path, query, or
// fragment, then confirm the hostname is a valid IP or DNS name. This is
// cheap client-side defense-in-depth and doesn't depend on the server's STS
// invalid_target canonicalization catching the trick.
func validateClusterHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return errors.New("cluster host is empty")
	}
	u, err := url.Parse("https://" + host)
	if err != nil {
		return fmt.Errorf("%q is not a valid host", host)
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host != host {
		return fmt.Errorf("%q must be a bare host[:port] (no scheme, userinfo, path, query, or fragment)", host)
	}
	hostname := u.Hostname()
	if net.ParseIP(hostname) != nil {
		return nil
	}
	for _, label := range strings.Split(hostname, ".") {
		if !clusterHostLabelRe.MatchString(label) {
			return fmt.Errorf("%q is not a valid DNS name or IP", host)
		}
	}
	return nil
}

// newRepoMirrorCmd is the `entire repo mirror` subtree: manage EntireDB
// GitHub-mirror placements on a cluster. Mirrors the standalone entiredb
// CLI's `entire repo mirror` surface for the server-side half (create /
// list / get / remove). The local-clone rewrite (`mirror use`) is not
// ported — it's a git-config + git-remote-entire concern outside the
// control-plane API.
func newRepoMirrorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Manage GitHub-mirror placements on EntireDB clusters",
	}
	cmd.AddCommand(newRepoMirrorCreateCmd())
	cmd.AddCommand(newRepoMirrorListCmd())
	cmd.AddCommand(newRepoMirrorGetCmd())
	cmd.AddCommand(newRepoMirrorRemoveCmd())
	cmd.AddCommand(newRepoMirrorCollaboratorsCmd())
	return cmd
}

func newRepoMirrorCreateCmd() *cobra.Command {
	var (
		noWait      bool
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create [github-url] [cluster-host]",
		Short: "Register a GitHub mirror on a cluster",
		Long: "With no arguments, launches an interactive wizard: pick repos to " +
			"mirror, pick one or more regions, then creates every (repo, region) " +
			"mirror in parallel and prints the clone URLs.\n\n" +
			"With a <github-url>, registers a mirror placement for that repo on " +
			"the target cluster, then waits for the initial GitHub→EntireDB clone " +
			"to finish so `git clone` works on return. Pass --no-wait to return " +
			"as soon as the placement is registered. Idempotent on " +
			"(upstream, cluster). When the cluster-host is omitted, an " +
			"interactive terminal offers the available clusters as a picker; " +
			"non-interactive runs default to " + defaultClusterHost + ".",
		Example: "  entire repo mirror create\n" +
			"  entire repo mirror create github.com/octocat/hello-world\n" +
			"  entire repo mirror create github.com/octocat/hello-world aws-us-east-2.entire.io",
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runMirrorCreateWizard(cmd, noWait, waitTimeout)
			}
			owner, repo, err := parseGitHubURL(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid <github-url>: %w", err)
			}
			// [cluster-host] omitted: on an interactive terminal, offer the
			// catalog's clusters as a picker (the same prompt-only-when-there-
			// is-a-choice shape as `repo clone`); non-interactive invocations
			// keep the fixed defaultClusterHost so scripts get stable behavior.
			var clusterHost string
			if len(args) > 1 {
				clusterHost = args[1]
			} else {
				var rerr error
				if clusterHost, rerr = resolveOneShotClusterHost(cmd); rerr != nil {
					return rerr
				}
			}
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid [cluster-host]: %w", err)
			}
			return runCoreForCluster(cmd, clusterHost, func(ctx context.Context, c *coreapi.Client) error {
				errW := cmd.ErrOrStderr()
				// Two-phase progress: a "Placing" spinner covers the fast
				// CreateMirror call (placement, <15s), then a separate "Cloning"
				// spinner covers the clone-readiness poll. An already-ready mirror
				// completes the first poll faster than the spinner's initial delay,
				// so the Cloning line never paints and we go straight to the clone
				// instructions.
				placing := startSpinner(errW, fmt.Sprintf("Placing mirror %s/%s into %s", owner, repo, clusterHost))
				placed := false
				var cloning func(success bool)
				// nil onStatus: the one-shot's spinners show liveness; the
				// per-mirror progress lines are the wizard's concern.
				outcome, err := createAndAwaitMirror(ctx, c, owner, repo, clusterHost, noWait, waitTimeout,
					func(created *coreapi.CreatedMirror) {
						placing(true)
						placed = true
						// Only start a Cloning spinner when there's a clone to await —
						// not for an empty upstream, and not for an admin-suspended
						// placement (which never becomes ready).
						if !noWait && !created.Empty && !created.Suspended { //nolint:staticcheck // CreatedMirror.Empty deprecated by /repos spec bump; create-flow cleanup tracked separately
							cloning = startSpinner(errW, fmt.Sprintf("Cloning %s/%s into %s", owner, repo, clusterHost))
						}
					}, nil)
				if !placed {
					// CreateMirror failed before onCreated fired — erase the line.
					placing(false)
				}
				if cloning != nil {
					// Only a confirmed-ready clone earns the ✓; everything else
					// (suspended, failed, timeout) erases the line and lets
					// reportOneShotMirror print the specific outcome.
					cloning(err == nil && outcome.polled && outcome.status == coreapi.MirrorStatusReady)
				}
				return reportOneShotMirror(cmd.OutOrStdout(), errW, outcome, err)
			})
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return once the placement is registered, without waiting for the initial clone")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "How long to wait for the initial clone to finish")
	return cmd
}

// mirrorCreateOutcome bundles the create response with the clone status
// observed while waiting. polled is false for --no-wait and for empty upstreams,
// where there is nothing to await; in those cases status is unset.
type mirrorCreateOutcome struct {
	created *coreapi.CreatedMirror
	status  coreapi.MirrorStatus
	polled  bool
}

// createAndAwaitMirror is the single create-then-wait path shared by the
// `repo mirror create <github-url>` one-shot and the onboarding wizard, so both
// report identical lifecycle states. It registers the GitHub mirror on
// clusterHost (idempotent on (upstream, cluster)) and, unless noWait or the
// upstream is empty, polls the control plane until the clone reaches a terminal
// status. The returned error is the create error (when outcome.created is nil)
// or the wait error — a status sentinel (errMirrorCloneFailed /
// errMirrorSuspended) or a timeout; callers read outcome.status for the state.
//
// onCreated (may be nil) fires once the placement is registered, before any
// clone polling — it separates the fast "placing" phase from the slow "cloning"
// wait so callers can render them as distinct steps.
func createAndAwaitMirror(ctx context.Context, c *coreapi.Client, owner, repo, clusterHost string, noWait bool, timeout time.Duration, onCreated func(*coreapi.CreatedMirror), onStatus func(coreapi.MirrorStatus)) (mirrorCreateOutcome, error) {
	created, err := c.CreateMirror(ctx, &coreapi.CreateMirrorInputBody{
		Provider:    coreapi.CreateMirrorInputBodyProviderGithub,
		Owner:       owner,
		Repo:        repo,
		ClusterHost: clusterHost,
	})
	if err != nil {
		return mirrorCreateOutcome{}, err
	}
	if onCreated != nil {
		onCreated(created)
	}
	// Heal the onboarding probe cache on every successful placement — the
	// setup checklist's own hint is `entire repo mirror create <slug>`, and a
	// cached "not mirrored" must not survive the prescribed remediation. A
	// suspended placement never serves, so it is deliberately not cached.
	if !created.Suspended {
		slugOwner, slugRepo := githubSlug(owner, repo)
		defaultMirrorProbeCache().put(slugOwner+"/"+slugRepo, true, time.Now())
	}
	outcome := mirrorCreateOutcome{created: created}
	if created.Suspended {
		// The placement already existed and an admin has suspended it, so it
		// will never serve — skip the clone poll. The caller warns after echoing
		// the placement; a suspended re-create is still a (non-fatal) success,
		// so return no error.
		return outcome, nil
	}
	if created.Empty { //nolint:staticcheck // CreatedMirror.Empty deprecated by /repos spec bump; create-flow cleanup tracked separately
		// An empty upstream has nothing to clone, so don't poll for "ready" — it
		// never would. But an *existing* placement can be suspended even when
		// empty, and one status read surfaces that (a fresh create can't be
		// suspended — suspension follows upstream access loss). Mirrors the old
		// finishMirrorCreate behavior; the read is best-effort, so a transient
		// GetMirror error just falls through to the benign "nothing to clone".
		if !created.Created {
			if m, gerr := c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: created.MirrorId}); gerr == nil {
				if s, ok := m.Status.Get(); ok && s == coreapi.MirrorStatusSuspended {
					outcome.status = s
					outcome.polled = true
					return outcome, errMirrorSuspended
				}
			}
		}
		return outcome, nil
	}
	if noWait {
		return outcome, nil
	}
	status, werr := awaitMirrorReady(ctx, c, created.MirrorId, timeout, onStatus)
	outcome.status = status
	outcome.polled = true
	return outcome, werr
}

// reportOneShotMirror renders the human output for `repo mirror create
// <github-url>` from the shared createAndAwaitMirror result. A nil
// outcome.created means CreateMirror itself failed — surface that error (nothing
// was printed yet). Otherwise echo the placement, then the lifecycle outcome.
func reportOneShotMirror(out, errW io.Writer, outcome mirrorCreateOutcome, err error) error {
	created := outcome.created
	if created == nil {
		return err
	}
	if created.Created {
		fmt.Fprintf(out, "\n✓ Registered mirror %s\n", created.MirrorId)
	} else {
		fmt.Fprintf(out, "\nMirror exists (%s)\n", created.MirrorId)
	}
	fmt.Fprintf(out, "  %s\n", created.MirrorUrl)

	if created.Suspended {
		// Echo the placement (above), warn, and exit non-zero: the mirror can't
		// be used, so a script chaining a clone shouldn't treat this as success.
		// SilentError keeps main.go from reprinting — the warning is the message.
		fmt.Fprintln(errW, "\nWARNING: this mirror has been suspended by an admin and won't be usable.")
		return NewSilentError(errMirrorSuspended)
	}

	if !outcome.polled {
		if created.Empty { //nolint:staticcheck // CreatedMirror.Empty deprecated by /repos spec bump; create-flow cleanup tracked separately
			fmt.Fprintln(out, "Upstream has no commits yet — nothing to clone. The mirror will pick up refs once the upstream is pushed to.")
		} else {
			fmt.Fprintf(out, "Initial clone may still be in progress; `git clone %s` will work once it completes.\n", created.MirrorUrl)
		}
		return nil
	}

	switch outcome.status {
	case coreapi.MirrorStatusReady:
		fmt.Fprintf(out, "\nClone it:\n  git clone %s\n", created.MirrorUrl)
		return nil
	case coreapi.MirrorStatusSuspended:
		explainSuspendedMirror(errW, created.MirrorId)
		return NewSilentError(errMirrorSuspended)
	case coreapi.MirrorStatusFailed:
		return fmt.Errorf("initial clone of mirror %s failed", created.MirrorId)
	case coreapi.MirrorStatusProcessing:
		// Still processing when the poll returned: the wait timed out (or a
		// poll call errored). awaitMirrorReady's err carries which. Route it
		// through renderCoreError so an API error (e.g. a 404 problem+json)
		// renders as the server's Detail rather than ogen's raw decoded struct;
		// a timeout error passes through unchanged.
		return renderCoreError(err)
	default:
		return renderCoreError(err)
	}
}

func newRepoMirrorListCmd() *cobra.Command {
	var cluster, provider, owner, name string
	var sortSpec string
	var showAvailable bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List mirrors you can see (or, with --show-available, repos you could mirror)",
		Args:  cobra.NoArgs,
		// Validate --sort before RunE so a bad column fails fast, without the
		// network round-trip RunE would otherwise do first. The valid column
		// set depends on --show-available (different table shape).
		PreRunE: func(_ *cobra.Command, _ []string) error {
			cols := mirrorColumns
			if showAvailable {
				cols = availableMirrorColumns
			}
			_, _, err := parseSortColumn(sortSpec, cols)
			return err
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showAvailable {
				return runCoreList(cmd, "No repos available to mirror.", columnHeaders(availableMirrorColumns), availableMirrorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.AvailableMirror, error) {
					// Computed live from GitHub using your own login, so name the
					// core being dialled (same rationale as the existing-mirror
					// banner). --cluster/--provider don't apply here: the
					// onboardable set is cluster-agnostic and GitHub-only.
					if !jsonRequested(cmd) {
						fmt.Fprintf(cmd.ErrOrStderr(), "Listing repos you could mirror, via %s\n", c.CoreOrigin())
					}
					var params coreapi.ListAvailableMirrorsParams
					if owner != "" {
						params.Owner = coreapi.NewOptString(owner)
					}
					out, err := c.ListAvailableMirrors(ctx, params)
					if err != nil {
						return nil, err
					}
					avail := filterByName(out.Available, func(m coreapi.AvailableMirror) string { return m.Owner + "/" + m.Repo }, name)
					if err := sortAvailable(avail, sortSpec); err != nil {
						return nil, err
					}
					return avail, nil
				})
			}
			return runCoreList(cmd, "No mirrors found.", columnHeaders(mirrorColumns), mirrorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Mirror, error) {
				// mirror list is identity-scoped: it shows the mirrors visible
				// from the active login's federation, so naming that login server
				// makes a surprising empty result legible — e.g. mirrors in a
				// different deployment than the active context (--cluster is a
				// filter, not a router). Name the core the client actually dials
				// (c.CoreOrigin) so the banner can never diverge from where the
				// request goes — in particular it reflects ENTIRE_TOKEN's aud,
				// which a separately-resolved ResolveControlPlaneTarget would miss.
				// On stderr so it never lands in a piped table; skipped for --json
				// to keep machine output clean.
				if !jsonRequested(cmd) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Listing mirrors on %s\n", c.CoreOrigin())
				}
				mirrors, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Mirror, string, error) {
					params := coreapi.ListMirrorsParams{}
					if cluster != "" {
						params.Cluster = coreapi.NewOptString(cluster)
					}
					if provider != "" {
						params.Provider = coreapi.NewOptString(provider)
					}
					if owner != "" {
						params.Owner = coreapi.NewOptString(owner)
					}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListMirrors(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Mirrors, out.NextPageToken.Or(""), nil
				})
				if err != nil {
					return nil, err
				}
				mirrors = filterByName(mirrors, func(m coreapi.Mirror) string { return m.Owner + "/" + m.Repo }, name)
				if err := sortMirrors(mirrors, sortSpec); err != nil {
					return nil, err
				}
				return mirrors, nil
			})
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "Filter by cluster public host")
	cmd.Flags().StringVar(&provider, "provider", "", "Filter by upstream provider (e.g. github)")
	cmd.Flags().StringVar(&owner, "owner", "", "Filter by upstream owner login")
	cmd.Flags().StringVar(&name, "name", "", "Filter by owner/repo substring, matching the NAME column (case-insensitive)")
	cmd.Flags().StringVar(&sortSpec, "sort", "", "Sort by column key (e.g. name, clone-url; prefix '-' for descending). Default: name ascending")
	cmd.Flags().BoolVar(&showAvailable, "show-available", false, "Instead of existing mirrors, list GitHub repos you could onboard as mirrors (ignores --cluster/--provider)")
	addJSONFlag(cmd)
	return cmd
}

func newRepoMirrorGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <mirror>",
		Short: "Show a mirror by ULID or clone URL",
		Long: "Show a mirror. <mirror> is either a mirror ULID or an entire:// clone URL\n" +
			"(entire://<cluster>/gh/<owner>/<repo>) — the form `mirror list` prints and\n" +
			"`git clone` accepts; a trailing .git, as pasted from `git remote -v`, is\n" +
			"accepted too. A clone URL is looked up on the login server fronting its\n" +
			"cluster, so it resolves even when that cluster belongs to a federation other\n" +
			"than the active auth context; a ULID is looked up on the active context's\n" +
			"login server.",
		Example: "  entire repo mirror get 01KS6KFJR2XS6PZ188MVYE07AN\n" +
			"  entire repo mirror get entire://aws-us-east-2.entire.io/gh/octocat/hello-world",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			show := func(ctx context.Context, c *coreapi.Client) (*coreapi.Mirror, error) {
				mirrorID, err := resolveMirrorRef(ctx, c, ref)
				if err != nil {
					return nil, err
				}
				return c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: mirrorID})
			}
			// A ULID carries no cluster coordinate, so it can only be looked up
			// on the active context's core. A clone URL names its cluster — dial
			// the core fronting that cluster (discovered from its well-known and
			// authenticated with the matching local context, the same path
			// create/remove use), so the lookup works when the mirror lives in a
			// federation other than the active login instead of failing with
			// "no mirror matching".
			if looksLikeULID(ref) {
				return runCoreObject(cmd, columnHeaders(mirrorColumns), mirrorRow, show)
			}
			clusterHost, _, _, _, err := parseMirrorCloneURL(ref)
			if err != nil {
				cmd.SilenceUsage = true
				return badMirrorRefErr(err)
			}
			return runCoreObjectForCluster(cmd, clusterHost, columnHeaders(mirrorColumns), mirrorRow, show)
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// resolveMirrorRef turns a mirror reference into its ULID. A ULID passes
// through unchanged. Otherwise the ref is parsed as an entire:// clone URL and
// resolved by listing the caller-visible mirrors for that (cluster, provider,
// owner) and matching the repo — there is no get-by-coords endpoint, only
// GetMirror(ULID). The clone URL carries the cluster, so the match is
// unambiguous even when the same upstream is mirrored on several clusters.
func resolveMirrorRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	clusterHost, provider, owner, repo, err := parseMirrorCloneURL(ref)
	if err != nil {
		return "", badMirrorRefErr(err)
	}
	mirrors, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Mirror, string, error) {
		params := coreapi.ListMirrorsParams{
			Cluster:  coreapi.NewOptString(clusterHost),
			Provider: coreapi.NewOptString(provider),
			Owner:    coreapi.NewOptString(owner),
		}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, lerr := c.ListMirrors(ctx, params)
		if lerr != nil {
			return nil, "", lerr
		}
		return out.Mirrors, out.NextPageToken.Or(""), nil
	})
	if err != nil {
		return "", err
	}
	// ListMirrors has no repo filter, so the owner-scoped page is matched on
	// repo client-side. Owner/repo are stored lowercase; EqualFold guards
	// against a differently-cased clone URL.
	for _, m := range mirrors {
		if strings.EqualFold(m.Repo, repo) {
			return m.MirrorId, nil
		}
	}
	return "", noMirrorErr(ref)
}

// parseMirrorCloneURL decomposes an entire:// mirror clone URL into its
// coordinates:
//
//	entire://<clusterHost>/gh/<owner>/<repo>
//
// Only the github ("gh") provider path is recognized — the only provider
// mirrors support today. The cluster host is validated the same way the
// create/remove verbs validate it, so a host carrying URL metacharacters is
// rejected at the boundary rather than flowing into the list filter.
func parseMirrorCloneURL(raw string) (clusterHost, provider, owner, repo string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil || u.Scheme != "entire" {
		return "", "", "", "", fmt.Errorf("%q is not an entire:// clone URL", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "gh" {
		return "", "", "", "", fmt.Errorf("%q must be entire://<cluster>/gh/<owner>/<repo>", raw)
	}
	if verr := validateClusterHost(u.Host); verr != nil {
		return "", "", "", "", verr
	}
	// Trim a trailing .git so a URL pasted from `git remote -v` resolves the
	// same as the bare clone URL (matching gitremote.ParseURL). GitHub repo
	// names can contain dots, so only the suffix is trimmed, not all dots.
	repo = strings.ToLower(strings.TrimSuffix(parts[2], ".git"))
	return u.Host, string(coreapi.CreateMirrorInputBodyProviderGithub), strings.ToLower(parts[1]), repo, nil
}

func noMirrorErr(ref string) error {
	return fmt.Errorf("no mirror matching %q (run `entire repo mirror list` to see clone URLs, or pass a ULID)", ref)
}

// badMirrorRefErr wraps a clone-URL parse failure with the accepted <mirror>
// forms. Shared by the pre-dial parse in `mirror get` and resolveMirrorRef so
// both boundaries report identically.
func badMirrorRefErr(err error) error {
	return fmt.Errorf("%w; pass a mirror ULID or a clone URL (entire://<cluster>/gh/<owner>/<repo>)", err)
}

func newRepoMirrorRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <github-url> [cluster-host]",
		Short: "Un-register a GitHub mirror from a cluster",
		Long: "Removes a mirror placement for a GitHub repo from the target " +
			"cluster. Other clusters' placements of the same upstream are " +
			"unaffected. The cluster-host defaults to " + defaultClusterHost +
			" when omitted.",
		Example: "  entire repo mirror remove github.com/octocat/hello-world",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseGitHubURL(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid <github-url>: %w", err)
			}
			clusterHost := clusterArg(args)
			if err := validateClusterHost(clusterHost); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid [cluster-host]: %w", err)
			}
			return runCoreForCluster(cmd, clusterHost, func(ctx context.Context, c *coreapi.Client) error {
				return removeMirror(ctx, cmd.OutOrStdout(), c, owner, repo, clusterHost)
			})
		},
	}
}

// removeMirror deletes the (owner, repo) placement on clusterHost via c and
// reports the outcome on w. A decoded 404 is a real error here (the server
// only answers 204 when it actually removed a placement); it is rewritten
// into a targeted message with the server's own detail appended so no
// information is lost.
func removeMirror(ctx context.Context, w io.Writer, c *coreapi.Client, owner, repo, clusterHost string) error {
	if err := c.DeleteMirror(ctx, coreapi.DeleteMirrorParams{
		Provider:    coreapi.DeleteMirrorProviderGithub,
		Owner:       owner,
		Repo:        repo,
		ClusterHost: clusterHost,
	}); err != nil {
		if isCoreNotFound(err) {
			// Deliberately not %w-wrapped: renderCoreError would extract the
			// server's problem detail and replace this targeted message. The
			// detail is appended as plain text instead, so nothing is lost.
			msg := fmt.Sprintf("no mirror of github.com/%s/%s on %s — it may be on a different cluster (run `entire repo mirror list` to see placements)", owner, repo, clusterHost)
			if detail := coreapi.APIError(err); detail != "" {
				msg += " (server: " + detail + ")"
			}
			return errors.New(msg)
		}
		return err
	}
	fmt.Fprintf(w, "✓ Removed mirror github.com/%s/%s from %s\n", owner, repo, clusterHost)
	return nil
}
