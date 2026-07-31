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
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
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
// so a --sort value needs no quoting; headers stay upper-case display text.
var (
	colName       = column{key: "name", header: "NAME (owner/repo)"}
	colCloneURL   = column{key: "clone-url", header: "CLONE URL"}
	colClusters   = column{key: "clusters", header: "CLUSTERS"}
	colVisibility = column{key: "visibility", header: "VISIBILITY"}
	colAccess     = column{key: "access", header: "ACCESS"}
	colStatus     = column{key: "status", header: "STATUS"}
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
var mirrorColumns = []column{colName, colCloneURL, colVisibility}

// mirrorVisibility renders the VISIBILITY column for a `get` mirror, sharing
// visibilityDisplay with the `list` directory row so both agree on the cell
// value.
func mirrorVisibility(m coreapi.Mirror) string {
	return visibilityDisplay(m.IsPrivate.Or(false))
}

func mirrorRow(m coreapi.Mirror) []string {
	repo := m.Owner + "/" + m.Repo
	cloneURL := mirrorCloneURL(m.ClusterHost, m.Owner, m.Repo)
	return []string{repo, cloneURL, mirrorVisibility(m)}
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

// repoDirColumns is the merged `repo mirror list` view: existing mirrors and
// onboardable GitHub candidates in one table, from GET /repos?scope=all. NAME
// and VISIBILITY come from every row; CLUSTERS and the placement STATUS are
// onboarded-only; ACCESS is candidate-only. Sparse cells render as "-".
// Per-placement detail (clone URLs, per-cluster status) lives one step down,
// in `repo mirror get <owner/repo>` — a directory this size stays one row per
// repo, not one per placement.
var repoDirColumns = []column{colName, colClusters, colVisibility, colStatus, colAccess}

// repoDirPlacement is one GitHub-mirror placement of a directory row's repo.
// CloneURL is omitted from JSON when the placement's cluster host couldn't be
// resolved (unknown slug, or a publicUrl that failed validation) — the slug
// still names the placement, and no unsafe URL is emitted.
type repoDirPlacement struct {
	Cluster  string `json:"cluster"`
	Status   string `json:"status"`
	CloneURL string `json:"cloneUrl,omitempty"`
}

// repoDirRow is one directory row: one per onboarded repo (its GitHub-mirror
// placements nested, so a repo mirrored across cells still lists once), or one
// per onboardable candidate. Fields are exported with JSON tags so --json
// emits this grouped, filtered, sorted view directly (the raw wire model stays
// reachable via `entire api --to core /repos`). Status is the placements'
// shared status when they agree, "mixed" when they don't, or the candidate's
// availability. Placements/Access are omitted from JSON when empty so a
// candidate row and a mirror row are distinguishable.
type repoDirRow struct {
	Repo       string             `json:"repo"`
	Private    bool               `json:"private"`
	Status     string             `json:"status"`           // shared placement status, "mixed", or candidate availability
	Access     string             `json:"access,omitempty"` // candidate only
	Placements []repoDirPlacement `json:"placements,omitempty"`
}

// repoDirStatusMixed is the STATUS cell of a repo whose placements disagree;
// `--status <one of them>` still matches the row (see applyRepoDirLocal).
const repoDirStatusMixed = "mixed"

// repoDirClusters renders the CLUSTERS cell: the row's placement cluster
// slugs, comma-joined in placement order; empty for candidates.
func repoDirClusters(r repoDirRow) string {
	slugs := make([]string, len(r.Placements))
	for i, p := range r.Placements {
		slugs[i] = p.Cluster
	}
	return strings.Join(slugs, ", ")
}

func repoDirCells(r repoDirRow) []string {
	return []string{r.Repo, orDash(repoDirClusters(r)), visibilityDisplay(r.Private), r.Status, orDash(r.Access)}
}

// repoDirCellsStyled wraps repoDirCells with trail-list-style cell coloring:
// clusters/access cyan, visibility by audience, status by lifecycle (see
// repoStatusColor); NAME stays the terminal's default foreground as the
// primary identifier. Cells are pre-colored and the table renderer measures
// widths with lipgloss.Width (ANSI-agnostic), so color never shifts columns.
// st must be built against the final output writer, not the pager buffer the
// render goes through — the buffer never looks like a TTY (see the styles
// wiring in newRepoMirrorListCmd).
func repoDirCellsStyled(st statusStyles) func(repoDirRow) []string {
	return func(r repoDirRow) []string {
		cells := repoDirCells(r)
		if !st.colorEnabled {
			return cells
		}
		if cells[1] != "-" {
			cells[1] = st.render(st.cyan, cells[1])
		}
		cells[2] = st.render(visibilityColor(st, r.Private), cells[2])
		if style, ok := repoStatusColor(st, r.Status); ok {
			cells[3] = st.render(style, cells[3])
		}
		if cells[4] != "-" {
			cells[4] = st.render(st.cyan, cells[4])
		}
		return cells
	}
}

// repoStatusColor maps a STATUS cell to its lifecycle color: healthy
// (ready/available) green, in-flight (processing) and part-degraded (mixed)
// yellow, failed red, suspended magenta. owner-only and unknown statuses stay
// uncolored — same palette roles as `trail list`'s status column.
func repoStatusColor(st statusStyles, status string) (lipgloss.Style, bool) {
	switch status {
	case "ready", "available":
		return st.green, true
	case "processing", repoDirStatusMixed:
		return st.yellow, true
	case "failed":
		return st.red, true
	case "suspended":
		return st.magenta, true
	default:
		return lipgloss.Style{}, false
	}
}

// styledHeaders pre-colors table headers (trail list's yellow) for renders
// that go through a pager buffer, where the shared printTable can't detect
// the terminal itself. Plain when color is off, so tests and pipes see the
// bare text.
func styledHeaders(st statusStyles, headers []string) []string {
	if !st.colorEnabled {
		return headers
	}
	out := make([]string, len(headers))
	for i, h := range headers {
		out[i] = st.render(st.yellow, h)
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// visibilityDisplay renders the VISIBILITY cell (and the `get` record's
// Visibility section): the repo's audience in GitHub's terms, not a yes/no.
func visibilityDisplay(private bool) string {
	if private {
		return "Private"
	}
	return "Public"
}

// visibilityColor maps a visibility value to its color: Public green (openly
// reachable), Private magenta (restricted — the accent, distinct from every
// status color that shares a row with it). Shared by the list column and the
// `get` record so the same value always looks the same.
func visibilityColor(st statusStyles, private bool) lipgloss.Style {
	if private {
		return st.magenta
	}
	return st.green
}

// clusterHostBySlug maps each cluster's slug to the validated bare host of its
// public URL, the host `git clone` needs in the entire:// clone URL. /repos
// placements carry only the cluster slug, so the clone URL for a mirror row is
// reconstructed by joining the placement slug against the cluster catalog (GET
// /clusters). A cluster whose publicUrl fails validation is omitted from the
// map, so its mirrors render with a dashed clone URL rather than a spoofable
// one — this guards the host@evil.com catalog-poisoning trick, where a naive
// url.Parse would demote the real host to userinfo and yield host=evil.com
// (see hostFromPublicURL / validateClusterHost).
func clusterHostBySlug(clusters []coreapi.Cluster) map[string]string {
	m := make(map[string]string, len(clusters))
	for _, cl := range clusters {
		host, err := hostFromPublicURL(cl.PublicUrl)
		if err != nil {
			continue // unsafe/malformed publicUrl: omit → dashed clone URL, never a spoofed one
		}
		m[cl.Slug] = host
	}
	return m
}

// buildRepoDir shapes the /repos?scope=all index into displayable rows: a
// candidate entry (has .Candidate) becomes one row with ACCESS + availability
// STATUS; an onboarded entry becomes ONE row with its GitHub-mirror placements
// nested — each placement carrying its cluster slug, clone STATUS
// (processing/ready/failed/suspended), and a clone URL synthesised from the
// placement's cluster host. The row's own STATUS is the placements' shared
// value, or "mixed" when they disagree. Non-mirror (native Entire) placements
// are skipped: `repo mirror list` is the mirror directory, and a native repo
// has no GitHub mirror clone URL to advertise (fabricating an
// entire://.../gh/... URL for one would point nowhere). A repo with only
// native placements therefore doesn't appear here. A placement whose cluster
// host can't be resolved (unknown slug, or a publicUrl that failed validation)
// still lists — its slug shows in CLUSTERS — but with an empty clone URL in
// JSON; no unsafe URL is emitted.
func buildRepoDir(entries []coreapi.RepoIndexEntry, hostBySlug map[string]string) []repoDirRow {
	var rows []repoDirRow
	for _, e := range entries {
		name := e.FullName
		if name == "" {
			name = e.Name
		}
		private := strings.EqualFold(e.Visibility, "private")
		if cand, ok := e.Candidate.Get(); ok {
			status := "owner-only"
			if cand.Onboardable {
				status = "available"
			}
			rows = append(rows, repoDirRow{Repo: name, Private: private, Status: status, Access: string(cand.Access)})
			continue
		}
		owner, repo, _ := strings.Cut(name, "/")
		var placements []repoDirPlacement
		status := ""
		for _, p := range e.Placements {
			if !p.Mirror {
				continue // native Entire repo, not a GitHub mirror
			}
			clone := ""
			if host := hostBySlug[p.ClusterSlug]; host != "" && repo != "" {
				clone = mirrorCloneURL(host, owner, repo)
			}
			placements = append(placements, repoDirPlacement{Cluster: p.ClusterSlug, Status: string(p.Status), CloneURL: clone})
			switch status {
			case "", string(p.Status):
				status = string(p.Status)
			default:
				status = repoDirStatusMixed
			}
		}
		if len(placements) == 0 {
			continue // native-only repo: not part of the mirror directory
		}
		rows = append(rows, repoDirRow{Repo: name, Private: private, Status: status, Placements: placements})
	}
	return rows
}

// sortRepoDir orders directory rows in place by the --sort spec: by the named
// column ascending (case-insensitive), tie-broken by repo name then the
// CLUSTERS cell for a deterministic order. A '-' prefix reverses.
func sortRepoDir(rows []repoDirRow, spec string) error {
	col, desc, err := parseSortColumn(spec, repoDirColumns)
	if err != nil {
		return err
	}
	key := func(r repoDirRow) string {
		switch col {
		case colClusters:
			return strings.ToLower(repoDirClusters(r))
		case colVisibility:
			return strings.ToLower(visibilityDisplay(r.Private))
		case colStatus:
			return strings.ToLower(r.Status)
		case colAccess:
			return strings.ToLower(r.Access)
		default: // name -> tiebreak alone
			return ""
		}
	}
	slices.SortStableFunc(rows, func(a, b repoDirRow) int {
		c := cmp.Compare(key(a), key(b))
		if c == 0 {
			c = cmp.Compare(strings.ToLower(a.Repo), strings.ToLower(b.Repo))
		}
		if c == 0 {
			c = cmp.Compare(strings.ToLower(repoDirClusters(a)), strings.ToLower(repoDirClusters(b)))
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

// repoDirLocalFilters carries the client-side filter/sort flags
// of `repo mirror list`. privateSet distinguishes an unset --private (keep
// all) from an explicit --private/--private=false (tri-state flag).
// mirroredOnly/availableOnly split the two row types the merged table
// interleaves (cobra rejects setting both).
type repoDirLocalFilters struct {
	name, owner, cluster, status, access string
	privateSet, private                  bool
	mirroredOnly, availableOnly          bool
	sortSpec                             string
}

// applyRepoDirLocal runs the client-side filter/sort pipeline
// over rows. The server cannot filter or sort the directory, so this applies
// only to the rows the caller fetched. hostBySlug is needed because --cluster
// accepts a public host while rows carry only the placement slug.
func applyRepoDirLocal(f repoDirLocalFilters, rows []repoDirRow, hostBySlug map[string]string) ([]repoDirRow, error) {
	// A mirror row is one with placements; a candidate row has none (its
	// Access/availability came from the entry's .Candidate). The two type
	// filters are mutually exclusive at the flag layer.
	if f.mirroredOnly {
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool { return len(r.Placements) == 0 })
	}
	if f.availableOnly {
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool { return len(r.Placements) > 0 })
	}
	rows = filterByName(rows, func(r repoDirRow) string { return r.Repo }, f.name)
	if f.owner != "" {
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool {
			o, _, _ := strings.Cut(r.Repo, "/")
			return !strings.EqualFold(o, f.owner)
		})
	}
	if f.cluster != "" {
		// Candidates are cluster-agnostic, so --cluster keeps only onboarded
		// rows with a placement on the named cluster. Placements carry only a
		// slug, but clone URLs identify clusters by public host (e.g.
		// aws-us-east-2.entire.io). Accept either form so a host copied from
		// a clone URL still matches.
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool {
			return !slices.ContainsFunc(r.Placements, func(p repoDirPlacement) bool {
				return strings.EqualFold(p.Cluster, f.cluster) ||
					strings.EqualFold(hostBySlug[p.Cluster], f.cluster)
			})
		})
	}
	if f.status != "" {
		// Case-insensitive exact match on the displayed STATUS cell —
		// mirrors (ready/processing/failed/suspended, or "mixed"), candidates
		// (available/owner-only) — OR on any single placement's status, so
		// `--status failed` still finds a repo whose other placements are
		// fine (its cell reads "mixed").
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool {
			return !strings.EqualFold(r.Status, f.status) &&
				!slices.ContainsFunc(r.Placements, func(p repoDirPlacement) bool {
					return strings.EqualFold(p.Status, f.status)
				})
		})
	}
	if f.access != "" {
		// ACCESS is candidate-only (read/write/admin); mirror rows carry
		// none, so --access naturally narrows to matching candidates.
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool {
			return !strings.EqualFold(r.Access, f.access)
		})
	}
	if f.privateSet {
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool {
			return r.Private != f.private
		})
	}
	if err := sortRepoDir(rows, f.sortSpec); err != nil {
		return nil, err
	}
	return rows, nil
}

// fetchRepoDirCatalog resolves the slug→host catalog the directory needs for
// clone URLs, and prints the identity banner: the directory shows repos
// visible from the active login's federation, so naming the core the client
// actually dials (c.CoreOrigin, which reflects ENTIRE_TOKEN's aud) makes a
// surprising empty result legible. On stderr so it never lands in a piped
// table; skipped for --json to keep output clean.
//
// The catalog round-trip exists ONLY to resolve slug->host for the
// synthesized clone URL (see mirrorCloneURL): if /repos ever returns the
// clone URL (or host) on a placement, drop it and the synthesis. It fails the
// whole command if unavailable rather than degrade: the clone URL is the
// payload of a mirror listing, and --json suppresses the stderr banner, so a
// degraded run would hand a script row-complete data with silently empty
// clone URLs and a zero exit.
func fetchRepoDirCatalog(ctx context.Context, cmd *cobra.Command, c *coreapi.Client) (map[string]string, error) {
	if !jsonRequested(cmd) {
		fmt.Fprintf(cmd.ErrOrStderr(), "Listing repos on %s\n", c.CoreOrigin())
	}
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	return clusterHostBySlug(clusters.Clusters), nil
}

// warnRepoDirTruncated discloses a server-side truncation with no cursor to
// continue from (legacy server, or a hard directory cap): repos exist that no
// further request can reach, so the output must not read as complete. Warns on
// stderr rather than failing, and prints for --json too — a script acting on
// silently truncated data is the worst outcome, and stderr never corrupts the
// stdout JSON.
func warnRepoDirTruncated(cmd *cobra.Command) {
	fmt.Fprintln(cmd.ErrOrStderr(), "Warning: the repo directory was truncated by the server; some repos are not shown.")
}

// repoMirrorListOpts carries `repo mirror list`'s flag values into the run
// functions below, keeping the cobra constructor to flag wiring.
type repoMirrorListOpts struct {
	filters   repoDirLocalFilters
	limit     int
	pageSize  int
	pageToken string
	noPager   bool
	all       bool
}

// runRepoMirrorList owns the shared frame of both list modes: the styled
// headers/cells, the client-side filter pipeline, the detail hint, and the
// pager. Style is decided against the final writer HERE: flushThroughPager is
// about to swap stdout for a buffer, and a buffer never looks like a TTY —
// deciding color inside the render would always disable it. Cells are
// pre-colored (trail-list style), so the shared table renderer just aligns
// and passes them through; `less -R` keeps the ANSI codes alive in the paged
// view.
func runRepoMirrorList(cmd *cobra.Command, o repoMirrorListOpts) error {
	st := newStatusStyles(cmd.OutOrStdout())
	headers := styledHeaders(st, columnHeaders(repoDirColumns))
	cells := repoDirCellsStyled(st)
	// The client-side pipeline is shared by both modes: every filter and the
	// sort run over whatever rows the server round-trip(s) yielded — the
	// fetched window in walk mode, one page in page mode. listedAny records
	// whether any row survived it, so the detail hint below prints only
	// under a real table.
	listedAny := false
	applyLocal := func(rows []repoDirRow, hostBySlug map[string]string) ([]repoDirRow, error) {
		rows, err := applyRepoDirLocal(o.filters, rows, hostBySlug)
		listedAny = listedAny || len(rows) > 0
		return rows, err
	}
	// The NAME cell is the handle into the detail view; the hint on stderr
	// keeps the workflow discoverable without corrupting a piped table, and
	// is skipped for --json (scripts get nested placements in the rows
	// already).
	hintDetail := func(err error) error {
		if err == nil && listedAny && !jsonRequested(cmd) {
			fmt.Fprintln(cmd.ErrOrStderr(), "\nPer-cluster detail and clone URLs: entire repo mirror get <owner/repo>")
		}
		return err
	}
	run := func() error { return runRepoMirrorListWalk(cmd, o, headers, cells, applyLocal) }
	if pageModeRequested(cmd) {
		run = func() error { return runRepoMirrorListPage(cmd, o, headers, cells, applyLocal) }
	}
	// Buffer the rendered table so long TTY output can go through a pager;
	// the row set is fully materialized for the client-side sort anyway, so
	// buffering the render adds nothing.
	return hintDetail(flushThroughPager(cmd, o.noPager, run))
}

// runRepoMirrorListPage is the single-page cursor passthrough: one /repos
// request, cursor reported for resumption. The client-side local pipeline
// applies to just this page; the cursor survives filtering.
func runRepoMirrorListPage(cmd *cobra.Command, o repoMirrorListOpts, headers []string, cells func(repoDirRow) []string, applyLocal func([]repoDirRow, map[string]string) ([]repoDirRow, error)) error {
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		hostBySlug, err := fetchRepoDirCatalog(ctx, cmd, c)
		if err != nil {
			return err
		}
		params := coreapi.ListReposParams{Scope: coreapi.NewOptListReposScope(coreapi.ListReposScopeAll)}
		if o.pageToken != "" {
			params.PageToken = coreapi.NewOptString(o.pageToken)
		}
		if o.pageSize > 0 {
			params.PageSize = coreapi.NewOptInt32(int32(o.pageSize)) //nolint:gosec // G115: validatePageSize bounds it
		}
		out, err := c.ListRepos(ctx, params)
		if err != nil {
			return err
		}
		next := out.NextPageToken.Or("")
		// A truncated page the cursor can resume past needs no warning — the
		// resume hint covers it. Truncated with no cursor means unreachable
		// repos.
		if out.Truncated && next == "" {
			warnRepoDirTruncated(cmd)
		}
		rows, err := applyLocal(buildRepoDir(out.Repos, hostBySlug), hostBySlug)
		if err != nil {
			return err
		}
		return renderCoreListPage(cmd, "No repos found.", headers, cells, rows, next)
	})
}

// runRepoMirrorListWalk is the default bounded cursor walk over the whole
// directory (budget-capped, lifted by --all), with partial/truncated
// disclosure on stderr.
func runRepoMirrorListWalk(cmd *cobra.Command, o repoMirrorListOpts, headers []string, cells func(repoDirRow) []string, applyLocal func([]repoDirRow, map[string]string) ([]repoDirRow, error)) error {
	return runCoreList(cmd, "No repos found.", headers, cells, func(ctx context.Context, c *coreapi.Client) ([]repoDirRow, error) {
		hostBySlug, err := fetchRepoDirCatalog(ctx, cmd, c)
		if err != nil {
			return nil, err
		}
		// The server cannot filter or sort this directory, so the whole
		// pipeline below is local. Bound what one call fetches: the budget
		// caps the cursor walk (raised to --limit when larger, lifted
		// entirely by --all) so a huge org pays for a few pages, not
		// thousands — at the disclosed price of filters and sort seeing only
		// the fetched window.
		budget := max(coreListFetchBudget, o.limit)
		if o.all {
			budget = 0 // unbounded
		}
		truncated := false
		repos, partial, err := fetchPagesBounded(ctx, budget, func(ctx context.Context, cursor string) ([]coreapi.RepoIndexEntry, string, error) {
			params := coreapi.ListReposParams{Scope: coreapi.NewOptListReposScope(coreapi.ListReposScopeAll)}
			if cursor != "" {
				params.PageToken = coreapi.NewOptString(cursor)
			}
			out, lerr := c.ListRepos(ctx, params)
			if lerr != nil {
				return nil, "", lerr
			}
			next := out.NextPageToken.Or("")
			// A capped page mid-chain is fine — the cursor walks past it.
			// Only a capped page with no cursor to continue from (legacy
			// server, or a hard directory cap) leaves repos unseen, and a
			// short directory must not read as "this is everything".
			truncated = truncated || (out.Truncated && next == "")
			return out.Repos, next, nil
		})
		if err != nil {
			return nil, err
		}
		if partial {
			// Deliberate client behavior with an escape hatch, and a script
			// acting on silently partial data is the worst outcome — so it
			// prints for --json too (stderr never corrupts the stdout JSON).
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Note: the repo directory has more entries; results were computed from the first %d fetched.\n"+
					"All filters and --sort are local to that window — pass --all to fetch the complete directory.\n",
				len(repos))
		}
		if truncated {
			warnRepoDirTruncated(cmd)
		}
		rows, err := applyLocal(buildRepoDir(repos, hostBySlug), hostBySlug)
		if err != nil {
			return nil, err
		}
		// Cap last, after filters and the sort, so --limit N always means
		// "the first N rows of the table you would have seen".
		if o.limit > 0 && len(rows) > o.limit {
			rows = rows[:o.limit]
		}
		return rows, nil
	})
}

func newRepoMirrorListCmd() *cobra.Command {
	var cluster, owner, name, status, access string
	var private bool
	var mirrored, available bool
	var sortSpec string
	var limit, pageSize int
	var pageToken string
	var noPager, all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List repos you can see: existing mirrors and GitHub repos you could onboard",
		Long: "List repos visible from your login in one table: existing mirrors " +
			"(one row per repo, with the clusters it is mirrored on and the clone " +
			"status) and GitHub repos you could onboard (access, availability). " +
			"Sparse cells show '-'. Per-cluster detail and clone URLs: " +
			"`entire repo mirror get <owner/repo>`.\n\n" +
			"The first " + strconv.Itoa(coreListFetchBudget) + " entries are fetched by default, with a note on stderr " +
			"when more exist. Filters and --sort apply to those fetched rows — add " +
			"--all to work over the complete list, or --limit N for just the first N.\n\n" +
			"For manual paging, --page-size/--page-token fetch one page at a time; " +
			"with --json the rows come wrapped in an {items, nextPageToken} envelope.",
		Args: cobra.NoArgs,
		// Validate --sort before RunE so a bad column fails fast, without the
		// network round-trip RunE would otherwise do first.
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return fmt.Errorf("--limit must be zero or positive, got %d", limit)
			}
			if err := validatePageSize(cmd, pageSize); err != nil {
				return err
			}
			_, _, err := parseSortColumn(sortSpec, repoDirColumns)
			return err
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRepoMirrorList(cmd, repoMirrorListOpts{
				filters: repoDirLocalFilters{
					name: name, owner: owner, cluster: cluster,
					status: status, access: access,
					privateSet: cmd.Flags().Changed("private"), private: private,
					mirroredOnly: mirrored, availableOnly: available,
					sortSpec: sortSpec,
				},
				limit: limit, pageSize: pageSize, pageToken: pageToken,
				noPager: noPager, all: all,
			})
		},
	}
	// Every flag in the Filtering & Sorting group runs on the client today
	// (/repos offers the server no filter or sort params), so the shared
	// client-side caveat renders once, as the group's note (see the
	// useGroupedFlagHelp call below), not on each flag. A flag that gains a
	// server-side implementation must leave the group.
	cmd.Flags().StringVar(&cluster, "cluster", "", "Keep only repos mirrored on this cluster, by slug or public host (drops onboardable candidates)")
	cmd.Flags().StringVar(&owner, "owner", "", "Filter by upstream owner login")
	cmd.Flags().StringVar(&name, "name", "", "Filter by owner/repo substring, matching the NAME column (case-insensitive)")
	cmd.Flags().StringVar(&status, "status", "", "Filter by exact STATUS (mirrors: ready/processing/failed/suspended, matching any of a repo's placements; candidates: available/owner-only)")
	cmd.Flags().StringVar(&access, "access", "", "Filter by exact ACCESS (candidates only: read/write/admin)")
	cmd.Flags().BoolVar(&private, "private", false, "Filter by visibility: --private for private only, --private=false for public only (omit for all)")
	cmd.Flags().BoolVar(&mirrored, "mirrored", false, "Keep only repos already mirrored (drops onboardable candidates)")
	cmd.Flags().BoolVar(&available, "available", false, "Keep only GitHub repos you could onboard as mirrors (drops existing mirrors)")
	cmd.Flags().StringVar(&sortSpec, "sort", "", "Sort by column key (e.g. name, clusters; prefix '-' for descending). Default: name ascending")
	cmd.Flags().IntVar(&limit, "limit", 0, "Show at most N rows, applied after the local filters and sort (0 shows all fetched)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch the complete directory instead of the first "+strconv.Itoa(coreListFetchBudget)+" entries (slower on large orgs)")
	cmd.Flags().BoolVar(&noPager, "no-pager", false, "Print directly to stdout instead of a pager for long output")
	cmd.MarkFlagsMutuallyExclusive("mirrored", "available")
	pageModeFlags(cmd, &pageSize, &pageToken)
	addJSONFlag(cmd)
	setFlagGroup(cmd, flagGroupNavigation, "all", "limit", "page-size", "page-token")
	setFlagGroup(cmd, flagGroupFiltering, "name", "owner", "cluster", "status", "access", "private", "mirrored", "available", "sort")
	setFlagGroup(cmd, flagGroupFormatting, "json", "no-pager")
	useGroupedFlagHelp(cmd,
		flagGroup{name: flagGroupNavigation},
		flagGroup{name: flagGroupFiltering, note: "Applied only to the fetched rows; combine with --all to filter/sort the complete mirror list."},
		flagGroup{name: flagGroupFormatting},
	)
	return cmd
}

func newRepoMirrorGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <mirror>",
		Short: "Show a repo's mirrors by owner/repo, or one mirror by ULID or clone URL",
		Long: "Show a mirror, or every mirror of a repo. <mirror> is one of:\n\n" +
			"  - <owner>/<repo>, as shown in the `mirror list` NAME column — shows the\n" +
			"    repo (visibility, access) and its mirror on every cluster, with\n" +
			"    per-cluster clone URL and status\n" +
			"  - a mirror ULID\n" +
			"  - an entire:// clone URL (entire://<cluster>/gh/<owner>/<repo>) — the form\n" +
			"    `git clone` accepts; a trailing .git, as pasted from `git remote -v`, is\n" +
			"    accepted too\n\n" +
			"A clone URL is looked up on the login server fronting its cluster, so it\n" +
			"resolves even when that cluster belongs to a federation other than the active\n" +
			"auth context; an owner/repo or ULID is looked up on the active context's\n" +
			"login server.",
		Example: "  entire repo mirror get octocat/hello-world\n" +
			"  entire repo mirror get 01KS6KFJR2XS6PZ188MVYE07AN\n" +
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
			// The owner/repo form is the drill-down from the grouped `mirror
			// list` NAME column: a record view of that repo — visibility,
			// access, then its mirror on every cluster with the per-placement
			// detail the list aggregates away (clone URL, per-cluster
			// status). Like a ULID it carries no cluster coordinate, so it
			// resolves on the active context's core.
			if isOwnerRepoRef(ref) {
				return runRepoMirrorGetByName(cmd, ref)
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

// runRepoMirrorGetByName renders the record view behind `get <owner/repo>`:
// the repo's identity fields (visibility, access), then its mirror placements
// as a cluster/clone-URL/status table. One exact-match /repos?filter= lookup
// (the endpoint returns that repo's zero-or-one entries; no directory walk)
// plus the cluster catalog for clone-URL synthesis. A candidate entry renders
// its access and availability instead of a placements table — though today's
// control plane only matches onboarded repos in the filter, so that path
// waits on the server (the not-found error points at `list --available`).
// --json emits the same repoDirRow shape `list --json` uses, placements
// nested.
func runRepoMirrorGetByName(cmd *cobra.Command, ref string) error {
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		out, err := c.ListRepos(ctx, coreapi.ListReposParams{Filter: coreapi.NewOptString(ref)})
		if err != nil {
			return err
		}
		if len(out.Repos) == 0 {
			// The filter only matches onboarded repos on today's control
			// plane, so a not-yet-mirrored GitHub repo lands here too —
			// point at the list mode that shows those.
			return fmt.Errorf("no repo matching %q visible from your login (GitHub repos you could onboard: `entire repo mirror list --available`)", ref)
		}
		clusters, err := c.ListClusters(ctx)
		if err != nil {
			return err
		}
		row := mirrorRepoDetailRow(out.Repos[0], clusterHostBySlug(clusters.Clusters))
		if jsonRequested(cmd) {
			return printJSON(cmd.OutOrStdout(), row)
		}
		renderRepoDetail(cmd.OutOrStdout(), row)
		return nil
	})
}

// mirrorRepoDetailRow shapes one directory entry for the record view, reusing the
// list's row builder so both views agree on placement/candidate semantics.
// buildRepoDir drops a repo with no GitHub-mirror placements (a native
// `entire repo create` repo); the detail view was asked about that repo by
// name, so it falls back to a bare identity row instead of vanishing.
// Placements are ordered by cluster slug for a deterministic table.
func mirrorRepoDetailRow(e coreapi.RepoIndexEntry, hostBySlug map[string]string) repoDirRow {
	rows := buildRepoDir([]coreapi.RepoIndexEntry{e}, hostBySlug)
	if len(rows) == 0 {
		name := e.FullName
		if name == "" {
			name = e.Name
		}
		return repoDirRow{Repo: name, Private: strings.EqualFold(e.Visibility, "private")}
	}
	row := rows[0]
	slices.SortFunc(row.Placements, func(a, b repoDirPlacement) int {
		return cmp.Compare(a.Cluster, b.Cluster)
	})
	return row
}

// renderRepoDetail prints the `get <owner/repo>` record as labeled sections —
// the label line in the same yellow as the table headers below it, the value
// indented beneath — then the placements table: cluster cyan, clone URL the
// default foreground (it is the payload of this view), status by lifecycle.
// Visibility carries its audience color (Public green, Private magenta),
// matching the list's VISIBILITY column. A candidate (no placements,
// availability in Status) states its availability instead of an empty table;
// a native-only repo states it has no GitHub mirrors.
func renderRepoDetail(w io.Writer, row repoDirRow) {
	st := newStatusStyles(w)
	section := func(label, value string) {
		fmt.Fprintln(w, st.render(st.yellow, label+":"))
		fmt.Fprintf(w, "  %s\n", value)
	}
	section("Name", st.render(st.bold, row.Repo))
	section("Visibility", st.render(visibilityColor(st, row.Private), visibilityDisplay(row.Private)))
	if row.Access != "" {
		section("Access", row.Access)
	}
	fmt.Fprintln(w)

	if len(row.Placements) == 0 {
		if row.Status != "" {
			fmt.Fprintf(w, "Not mirrored on any cluster (%s).\n", row.Status)
			return
		}
		fmt.Fprintln(w, "No GitHub mirror placements.")
		return
	}

	headers := styledHeaders(st, []string{"CLUSTER", "CLONE URL", "STATUS"})
	rows := make([][]string, len(row.Placements))
	for i, p := range row.Placements {
		cluster, status := p.Cluster, p.Status
		if st.colorEnabled {
			cluster = st.render(st.cyan, cluster)
			if style, ok := repoStatusColor(st, status); ok {
				status = st.render(style, status)
			}
		}
		rows[i] = []string{cluster, orDash(p.CloneURL), status}
	}
	widths := columnWidths(headers, rows)
	var b strings.Builder
	plain := func(int) lipgloss.Style { return lipgloss.Style{} }
	writeTableRow(&b, headers, widths, plain, tableStyles{})
	for _, r := range rows {
		writeTableRow(&b, r, widths, plain, tableStyles{})
	}
	fmt.Fprint(w, b.String())
}

// isOwnerRepoRef reports whether ref is a bare <owner>/<repo> mirror
// reference — the NAME cell of `mirror list`, passed verbatim to the /repos
// exact-match filter. Anything carrying a scheme, extra path segments, or an
// empty side is not this form (it falls through to clone-URL parsing, whose
// error names the expected shapes).
func isOwnerRepoRef(ref string) bool {
	if strings.Contains(ref, "://") {
		return false
	}
	owner, repo, found := strings.Cut(ref, "/")
	return found && owner != "" && repo != "" && !strings.Contains(repo, "/")
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
	return fmt.Errorf("%w; pass <owner>/<repo>, a mirror ULID, or a clone URL (entire://<cluster>/gh/<owner>/<repo>)", err)
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
