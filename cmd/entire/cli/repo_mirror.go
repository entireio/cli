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

	"github.com/entireio/cli/cmd/entire/cli/interactive"
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
	colCloneURL   = column{key: "clone-url", header: colHeaderCloneURL}
	colClusters   = column{key: "clusters", header: "CLUSTERS"}
	colVisibility = column{key: "visibility", header: "VISIBILITY"}
	colAccess     = column{key: "access", header: "ACCESS"}
	colStatus     = column{key: "status", header: colHeaderStatus}
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
	cloneURL := forgeCloneURL(mirrorCloneForge, m.ClusterHost, m.Owner, m.Repo)
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
// in `repo mirror get /gh/<owner>/<repo>` — a directory this size stays one row per
// repo, not one per placement.
var repoDirColumns = []column{colName, colClusters, colVisibility, colStatus, colAccess}

// repoDirPlacement is one GitHub-mirror placement of a directory row's repo.
// CloneURL is omitted from JSON when the placement's cluster host couldn't be
// resolved (unknown slug, or a publicUrl that failed validation) — the slug
// still names the placement, and no unsafe URL is emitted.
type repoDirPlacement struct {
	Cluster string `json:"cluster"`
	Status  string `json:"status"`
	// Role is the wire vocabulary of POST /repos/resolve: "primary" or
	// "native_mirror". It is set only for Entire-native repos, where the two
	// differ in what you may do to the placement itself: the primary is not
	// removable through the mirror verbs. A GitHub repo's placements are
	// all mirrors of an upstream that is not a placement at all, so they carry
	// no role and the column stays out of that view.
	Role string `json:"role,omitempty"`
	// Stage is the native provisioning progress (pending → provisioned →
	// registered → seeded → announced). It qualifies a processing placement
	// only: readiness is Status and nothing else, so stage never decides
	// anything — it says how far along a wait is.
	Stage string `json:"stage,omitempty"`
	// Removing marks a placement whose teardown is in flight. It stays listed
	// rather than being hidden: creating on that cluster meanwhile is refused
	// until the row is gone, so hiding it would hide the reason.
	Removing bool   `json:"removing,omitempty"`
	CloneURL string `json:"cloneUrl,omitempty"`
}

// Placement roles, in the control plane's own vocabulary (POST /repos/resolve).
const (
	placementRolePrimary      = "primary"
	placementRoleNativeMirror = "native_mirror"
)

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
// value, or "mixed" when they disagree. A placement whose cluster host can't be
// resolved (unknown slug, or a publicUrl that failed validation) still lists —
// its slug shows in CLUSTERS — but with an empty clone URL in JSON; no unsafe
// URL is emitted.
//
// forge selects which repos are in the directory at all. It is the entry's
// `provider` that decides, not the placements' `mirror` flag: cell_fanout.go
// documents that real rows mark every placement Mirror:true, so keying on it
// would be reading a field that does not answer this question. An entry whose
// provider matches neither known value falls into no forge's view rather than
// silently into both.
func buildRepoDir(entries []coreapi.RepoIndexEntry, hostBySlug map[string]string, forge string) []repoDirRow {
	var rows []repoDirRow
	for _, e := range entries {
		if !entryServesForge(e, forge) {
			continue
		}
		name := e.FullName
		if name == "" {
			name = e.Name
		}
		entryForge, forgeFromProvider := forgeOfEntry(e)
		private := strings.EqualFold(e.Visibility, "private")
		if cand, ok := e.Candidate.Get(); ok {
			status := "owner-only"
			if cand.Onboardable {
				status = "available"
			}
			rows = append(rows, repoDirRow{Repo: qualifyRepoRef(entryForge, name), Private: private, Status: status, Access: string(cand.Access)})
			continue
		}
		owner, repo, _ := strings.Cut(name, "/")
		var placements []repoDirPlacement
		status := ""
		for _, p := range e.Placements {
			if !forgeFromProvider && !placementServesForge(p, entryForge) {
				continue
			}
			clone := ""
			if host := hostBySlug[p.ClusterSlug]; host != "" && repo != "" {
				clone = forgeCloneURL(entryForge, host, owner, repo)
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
			continue // an onboarded repo with nowhere to clone from is not a row
		}
		rows = append(rows, repoDirRow{Repo: qualifyRepoRef(entryForge, name), Private: private, Status: status, Placements: placements})
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

// qualifyRepoRef qualifies a bare <a>/<b> from the repos index with the forge
// it belongs to, so a directory row prints the same shape every mirror verb
// accepts. A value copied from the NAME column, or read out of --json, is then
// a reference rather than something to prepend a forge to by hand.
func qualifyRepoRef(forge, ownerRepo string) string {
	return "/" + forge + "/" + ownerRepo
}

// forgeOfEntry reads which forge backs a directory entry. `provider` is the
// field that answers it ("github" | "entire"), but it is optional and open on
// the client (normalize.go strips its enum), so an entry that omits it falls
// back to its placements' `mirror` flag — which agrees with provider on every
// row of the live index, GitHub repos marking every placement true and native
// repos every placement false.
//
// The flag is the FALLBACK and not the primary signal on purpose:
// cell_fanout.go documents that `mirror` must never decide which placement
// routes a repo, and keying the common path on it would invite exactly that
// confusion. An entry with neither signal yields "" — a row in no forge's view,
// rather than one bucketed into whichever is tested first.
// It also reports whether `provider` is what answered. That matters to the
// caller: the placement filter below may only be applied when it did NOT, since
// the flag would then be both the classifier and the filter.
func forgeOfEntry(e coreapi.RepoIndexEntry) (forge string, fromProvider bool) {
	// Get, not Or(""): an ABSENT provider is "the server did not say" and falls
	// through to the flag below, while a provider this build does not know is a
	// definite answer of "neither of ours". Collapsing the two let a future
	// forge's repo be classified by its placement flag and rendered with a
	// fabricated /gh/ clone URL pointing nowhere.
	if provider, ok := e.Provider.Get(); ok {
		switch provider {
		case repoProviderGitHub:
			return mirrorCloneForge, true
		case repoProviderEntire:
			return nativeCloneForge, true
		default:
			return "", true
		}
	}
	// A candidate is a GitHub repo that could be onboarded. It has no provider
	// and no placements — there is nothing placed yet — so it is recognised by
	// being a candidate at all.
	if _, ok := e.Candidate.Get(); ok {
		return mirrorCloneForge, true
	}
	if len(e.Placements) == 0 {
		return "", false
	}
	if slices.ContainsFunc(e.Placements, func(p coreapi.RepoPlacement) bool { return p.Mirror }) {
		return mirrorCloneForge, false
	}
	return nativeCloneForge, false
}

// placementServesForge keeps a row's placements to the forge the row belongs
// to, for an entry that carries both kinds. It is applied ONLY when the
// `mirror` flag is also what classified the row: once `provider` has answered,
// re-deriving the forge per placement can only disagree with it, and a
// disagreement empties the row and drops the repo from the directory entirely.
func placementServesForge(p coreapi.RepoPlacement, forge string) bool {
	return p.Mirror == (forge == mirrorCloneForge)
}

// forgeFilterAll is the --forge value that merges both directories.
const forgeFilterAll = "all"

// entryServesForge reports whether an entry belongs in a directory filtered to
// forge. A row whose forge cannot be named is in NO view, "all" included: it
// has no reference to print and no clone URL that would resolve, so listing it
// would only invite someone to copy a name nothing accepts.
func entryServesForge(e coreapi.RepoIndexEntry, forge string) bool {
	entryForge, _ := forgeOfEntry(e)
	if entryForge == "" {
		return false
	}
	return forge == forgeFilterAll || entryForge == forge
}

// mirrorRefOwner returns the owner segment of a forge-qualified directory name
// — the GitHub owner, or the Entire project. Whichever forge token leads is
// dropped: stripping only `gh/` read "/et/acme/web" as owner "et", so
// `--forge et --owner acme` matched nothing while `--owner et` matched every
// native row. A value carrying no forge token is left alone rather than losing
// its first segment.
func mirrorRefOwner(ref string) string {
	trimmed := trimRefPrefix(ref)
	for _, forge := range []string{mirrorCloneForge, nativeCloneForge} {
		if rest, ok := strings.CutPrefix(trimmed, forge+"/"); ok {
			trimmed = rest
			break
		}
	}
	owner, _, _ := strings.Cut(trimmed, "/")
	return owner
}

// filterByName keeps items whose owner/repo name contains substr (case-
// insensitive). The control plane already filters by owner/provider/cluster
// server-side but not by name, so `repo mirror list --name` narrows that last
// dimension client-side. nameOf returns the item's displayed identifier — the
// callers pass the form shown in the NAME column, so a value copied from the
// table (e.g. /gh/acme/web) matches the row it came from, and so does the bare
// acme/web it contains. An empty substr
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

// defaultClusterHost is the cluster a mirror command targets when --cluster is
// omitted and there is no terminal to offer a picker on: `mirror add` falls
// back to it for a non-interactive run, and mirrorReadCluster reads it when no
// placement of a mirror chose a cluster, so scripts keep a stable,
// offline-resolvable default.
//
// `mirror remove` deliberately has no default. Which clusters a repo is on is a
// property of the repo rather than of the catalog, and removing is destructive,
// so guessing would tear down a copy the caller never named (see
// chooseMirrorRemoveRegions).
//
// Interactive runs never reach this: they enumerate the real catalog (GET
// /api/v1/clusters via availableRegions) and pick from it — chooseMirrorAddRegions
// → pickRegions for add, chooseMirrorRemoveRegions → pickRemoveRegions for remove.
const defaultClusterHost = "aws-us-east-2.entire.io"

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
// GitHub-mirror placements on a cluster (add / list / get / remove). The
// local-clone rewrite lives at `repo remote add` (repo_remote.go) and the
// collaborator view at `repo grant list` (repo_grant.go).
func newRepoMirrorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Manage where a repository is mirrored across Entire clusters",
	}
	cmd.AddCommand(newRepoMirrorAddCmd())
	cmd.AddCommand(newRepoMirrorListCmd())
	cmd.AddCommand(newRepoMirrorGetCmd())
	cmd.AddCommand(newRepoMirrorRemoveCmd())
	return requireSubcommand(cmd)
}

func newRepoMirrorAddCmd() *cobra.Command {
	var (
		opts     mirrorAddOptions
		clusters []string
	)
	cmd := &cobra.Command{
		Use:   "add [repo]",
		Short: "Mirror a repository onto one or more clusters",
		Long: "With no arguments, launches an interactive wizard: pick repos to " +
			"mirror, pick one or more regions, then creates every (repo, region) " +
			"mirror in parallel and prints the clone URLs. --cluster requires a <repo>.\n\n" +
			"With a <repo>, places it on every cluster named by --cluster — repeat " +
			"the flag or comma-separate the hosts — in parallel, then waits for " +
			"each to become usable. Pass --no-wait to return as soon as the " +
			"placements are registered. Idempotent on (repo, cluster), so naming a " +
			"cluster the repo is already on reports it rather than failing.\n\n" +
			"When --cluster is omitted, an interactive terminal offers the " +
			"available clusters as a multi-select; non-interactive runs default to " +
			defaultClusterHost + ".\n\n" +
			"With an /et/ ref, places replicas of an Entire-native repo and waits " +
			"for each to be seeded. A native mirror goes in a region other than the " +
			"repo's own, so only those clusters are offered and --cluster has no " +
			"default there: non-interactive runs must name one.\n\n" +
			"Every cluster is attempted: one that fails does not stop the others, " +
			"and the command exits non-zero naming the ones that did.\n\n" + mirrorRepoRefHelp,
		Example: "  entire repo mirror add\n" +
			"  entire repo mirror add /gh/octocat/hello-world\n" +
			"  entire repo mirror add /gh/octocat/hello-world --cluster aws-us-east-2.entire.io\n" +
			"  entire repo mirror add /gh/octocat/hello-world --cluster aws-us-east-2.entire.io,aws-eu-central-1.entire.io\n" +
			"  entire repo mirror add /et/acme/web --cluster aws-eu-central-1.entire.io",
		Args: cobra.MaximumNArgs(1),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			// Preserve zero as an unbounded wait for existing callers.
			if opts.timeout < 0 {
				return errors.New("--timeout must be zero or positive")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if cmd.Flags().Changed("cluster") {
					cmd.SilenceUsage = true
					return errors.New("--cluster requires <repo>; for example: entire repo mirror add /gh/owner/repo --cluster aws-us-east-2.entire.io")
				}
				return runMirrorAddWizard(cmd, opts)
			}
			return runMirrorAdd(cmd, args[0], clusters, opts)
		},
	}
	cmd.Flags().StringSliceVar(&clusters, "cluster", nil, "Cluster host(s) to mirror onto; repeat or comma-separate for several (a terminal offers a multi-select when omitted; other runs use "+defaultClusterHost+")")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "Return once the placement is registered, without waiting for the initial clone")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 30*time.Minute, "How long to wait for mirror request submission, placement, and clone readiness (0 waits indefinitely)")
	return cmd
}

// runMirrorAdd is the one-shot `repo mirror add <repo>` body: resolve the repo
// and the clusters it should land on, refuse everything decidable before a
// write, then place them all through the same parallel engine the no-argument
// wizard uses — so one repo across three clusters gets the same live progress
// and summary table as three repos across three clusters.
//
// --cluster names cluster HOSTS, the coordinate runCoreForCluster and the clone
// URL already work in. The catalog maps each to the slug the native-mirror API
// is keyed by, so the two forges take one spelling from the user.
func runMirrorAdd(cmd *cobra.Command, repoRef string, clusterHosts []string, opts mirrorAddOptions) error {
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

	// One active-context round trip resolves both things the choice depends on:
	// the catalog, and (for a native ref) the repo whose region decides which
	// clusters are even eligible.
	var (
		regions    []regionChoice
		clusters   []coreapi.Cluster
		nativeRepo *coreapi.Repo
	)
	if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		out, lerr := c.ListClusters(ctx)
		if lerr != nil {
			return lerr
		}
		clusters, regions = out.Clusters, clustersToRegions(out.Clusters)
		if ref.forge == nativeCloneForge {
			nativeRepo, lerr = resolveNativeRepo(ctx, c, ref.owner, ref.repo)
			if lerr != nil {
				return lerr
			}
		}
		return nil
	}); err != nil {
		return err
	}

	chosen, err := chooseMirrorAddRegions(cmd, ref, nativeRepo, clusters, regions, clusterHosts)
	if err != nil {
		return err
	}
	if len(chosen) == 0 {
		// A cancelled picker reports itself and returns no selection; running an
		// empty batch would print a progress block and a table for nothing.
		return nil
	}
	results := createMirrors(cmd.Context(), cmd.ErrOrStderr(), oneRepoTargets(ref, nativeRepo, chosen), opts)
	return reportMirrorResults(cmd.OutOrStdout(), cmd.ErrOrStderr(), results)
}

// chooseMirrorAddRegions turns --cluster (or the absence of it) into the
// clusters a one-shot add will place on, refusing anything the CLI can rule out
// before a write.
//
// Named clusters are checked individually so a batch cannot half-apply on a
// mistake: all of them must be placeable, or none is attempted.
//
// A host is resolved against the catalog rather than passed through, because
// the native API is keyed by slug — a cluster the catalog does not list has no
// slug to place on, so it cannot be a target for either forge.
func chooseMirrorAddRegions(cmd *cobra.Command, ref mirrorRepoRef, nativeRepo *coreapi.Repo, clusters []coreapi.Cluster, regions []regionChoice, hosts []string) ([]regionChoice, error) {
	eligible := regions
	if ref.forge == nativeCloneForge {
		eligible = nativeEligibleRegions(regions, nativeRepo)
	}
	if len(hosts) > 0 {
		chosen := make([]regionChoice, 0, len(hosts))
		seen := map[string]bool{}
		for _, host := range hosts {
			region, ok := regionByHost(regions, host)
			if !ok {
				return nil, fmt.Errorf("invalid --cluster: unknown cluster %q; available: %s", host, strings.Join(regionHosts(regions), ", "))
			}
			if ref.forge == nativeCloneForge {
				if err := checkNativeMirrorTarget(nativeRepo, clusters, region.slug, nativeRefOf(ref)); err != nil {
					return nil, err
				}
			}
			if seen[region.slug] {
				continue // naming a cluster twice asks for one placement, not two
			}
			seen[region.slug] = true
			chosen = append(chosen, region)
		}
		return chosen, nil
	}

	if len(eligible) == 0 {
		return nil, fmt.Errorf("no cluster is available to mirror %s into", ref.qualified())
	}
	if !interactive.CanPromptInteractively() {
		// A native repo's eligible clusters depend on the repo, so there is no
		// safe fixed default; GitHub mirrors keep one so scripts are stable.
		if ref.forge == nativeCloneForge {
			return nil, fmt.Errorf("pass --cluster to say where to mirror %s: a native mirror goes in a region other than the repo's own, so there is no safe default (available: %s)",
				ref.qualified(), strings.Join(regionHosts(eligible), ", "))
		}
		region, ok := regionByHost(regions, defaultClusterHost)
		if !ok {
			return nil, fmt.Errorf("default cluster %s is not in the control plane's catalog; pass --cluster explicitly (available: %s)", defaultClusterHost, strings.Join(regionHosts(regions), ", "))
		}
		return []regionChoice{region}, nil
	}
	return pickRegions(cmd.Context(), cmd.ErrOrStderr(), eligible, callerJurisdiction(cmd))
}

// mirrorAddOutcome bundles the create response with the clone status
// observed while waiting. polled is false for --no-wait, where status is unset.
type mirrorAddOutcome struct {
	created *coreapi.MirrorRequestResult
	status  coreapi.MirrorStatus
	polled  bool
}

type mirrorAddPhase string

const (
	mirrorAddPhaseQueued  mirrorAddPhase = "queued"
	mirrorAddPhasePlacing mirrorAddPhase = "placing"
	mirrorAddPhaseCloning mirrorAddPhase = "cloning"
)

type mirrorAddOptions struct {
	noWait  bool
	timeout time.Duration
	onPhase func(mirrorAddPhase)
}

func addAndAwaitMirror(ctx context.Context, c *coreapi.Client, owner, repo, clusterHost string, opts mirrorAddOptions) (mirrorAddOutcome, error) {
	var currentPhase mirrorAddPhase
	reportPhase := func(phase mirrorAddPhase) {
		if opts.onPhase == nil || phase == currentPhase {
			return
		}
		currentPhase = phase
		opts.onPhase(phase)
	}

	waitCtx := ctx
	// Zero preserves the caller context without adding a timeout.
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	reportPhase(mirrorAddPhaseQueued)
	accepted, err := c.CreateMirrorRequest(waitCtx, &coreapi.CreateMirrorRequestInputBody{
		Provider:    coreapi.CreateMirrorRequestInputBodyProviderGithub,
		Owner:       owner,
		Repo:        repo,
		ClusterHost: clusterHost,
	})
	if err != nil {
		if waitErr := waitCtx.Err(); waitErr != nil {
			return mirrorAddOutcome{}, classifyWaitContextErr(waitErr, "submitting mirror request")
		}
		return mirrorAddOutcome{}, err
	}
	location, _ := accepted.Location.Get()
	created, err := awaitMirrorPlacement(waitCtx, c, accepted.Response, location, func(status coreapi.MirrorRequestStatus) {
		switch status {
		case coreapi.MirrorRequestStatusPending:
			reportPhase(mirrorAddPhaseQueued)
		case coreapi.MirrorRequestStatusProcessing:
			reportPhase(mirrorAddPhasePlacing)
		case coreapi.MirrorRequestStatusSucceeded, coreapi.MirrorRequestStatusFailed:
		}
	})
	if err != nil {
		return mirrorAddOutcome{}, err
	}
	outcome := mirrorAddOutcome{created: created}
	if opts.noWait {
		return outcome, nil
	}
	reportPhase(mirrorAddPhaseCloning)
	status, werr := awaitMirrorReady(waitCtx, c, created.MirrorId, 0)
	outcome.status = status
	outcome.polled = true
	return outcome, werr
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
// only to the rows the caller fetched.
func applyRepoDirLocal(f repoDirLocalFilters, rows []repoDirRow) ([]repoDirRow, error) {
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
			return !strings.EqualFold(mirrorRefOwner(r.Repo), f.owner)
		})
	}
	if f.cluster != "" {
		// Candidates are cluster-agnostic, so --cluster keeps only onboarded
		// rows with a placement on the named cluster. The value is the catalog
		// slug — what placements carry, what the CLUSTERS column prints, and
		// the same spelling every other --cluster takes. The public host inside
		// a clone URL is not a second accepted form: PreRunE refuses it rather
		// than let it silently match nothing.
		rows = slices.DeleteFunc(rows, func(r repoDirRow) bool {
			return !slices.ContainsFunc(r.Placements, func(p repoDirPlacement) bool {
				return strings.EqualFold(p.Cluster, f.cluster)
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
	forge     string
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
	applyLocal := func(rows []repoDirRow) ([]repoDirRow, error) {
		rows, err := applyRepoDirLocal(o.filters, rows)
		listedAny = listedAny || len(rows) > 0
		return rows, err
	}
	// The NAME cell is the handle into the detail view; the hint on stderr
	// keeps the workflow discoverable without corrupting a piped table, and
	// is skipped for --json (scripts get nested placements in the rows
	// already).
	// The hint names the shape of the refs in the table it follows, so copying
	// a NAME cell into it actually works. Under --forge all both shapes are
	// present, so it names the column instead of picking one.
	detailRef := "/" + o.forge + "/<owner>/<repo>"
	switch o.forge {
	case nativeCloneForge:
		detailRef = "/" + nativeCloneForge + "/<project>/<repo>"
	case forgeFilterAll:
		detailRef = "<name from the NAME column>"
	}
	hintDetail := func(err error) error {
		if err == nil && listedAny && !jsonRequested(cmd) {
			fmt.Fprintln(cmd.ErrOrStderr(), "\nPer-cluster detail and clone URLs: entire repo mirror get "+detailRef)
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
func runRepoMirrorListPage(cmd *cobra.Command, o repoMirrorListOpts, headers []string, cells func(repoDirRow) []string, applyLocal func([]repoDirRow) ([]repoDirRow, error)) error {
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
		rows, err := applyLocal(buildRepoDir(out.Repos, hostBySlug, o.forge))
		if err != nil {
			return err
		}
		return renderCoreListPage(cmd, "No repos found.", headers, cells, rows, next)
	})
}

// runRepoMirrorListWalk is the default bounded cursor walk over the whole
// directory (budget-capped, lifted by --all), with partial/truncated
// disclosure on stderr.
func runRepoMirrorListWalk(cmd *cobra.Command, o repoMirrorListOpts, headers []string, cells func(repoDirRow) []string, applyLocal func([]repoDirRow) ([]repoDirRow, error)) error {
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
		rows, err := applyLocal(buildRepoDir(repos, hostBySlug, o.forge))
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
	var cluster, owner, name, status, access, forge string
	var private bool
	var mirrored, available bool
	var sortSpec string
	var limit, pageSize int
	var pageToken string
	var noPager, all bool
	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List repos you can see: existing mirrors and GitHub repos you could onboard",
		Long: "List repos visible from your login in one table: existing mirrors " +
			"(one row per repo, with the clusters it is mirrored on and the clone " +
			"status) and GitHub repos you could onboard (access, availability). " +
			"Sparse cells show '-'. Per-cluster detail and clone URLs: " +
			"`entire repo mirror get /gh/<owner>/<repo>`.\n\n" +
			"GitHub is the default view. Pass --forge " + nativeCloneForge + " for " +
			"Entire-native repos and where each is placed, or --forge " + forgeFilterAll +
			" for both in one table; the NAME column always prints the " +
			"forge-qualified reference the other verbs take.\n\n" +
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
			if !slices.Contains([]string{mirrorCloneForge, nativeCloneForge, forgeFilterAll}, forge) {
				return fmt.Errorf("invalid --forge %q: must be %s, %s, or %s", forge, mirrorCloneForge, nativeCloneForge, forgeFilterAll)
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
				forge: forge,
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
	cmd.Flags().StringVar(&name, "name", "", "Filter by substring of the NAME column, e.g. acme/web or /gh/acme (case-insensitive)")
	cmd.Flags().StringVar(&status, "status", "", "Filter by exact STATUS (mirrors: ready/processing/failed/suspended, matching any of a repo's placements; candidates: available/owner-only)")
	cmd.Flags().StringVar(&access, "access", "", "Filter by exact ACCESS (candidates only: read/write/admin)")
	cmd.Flags().BoolVar(&private, "private", false, "Filter by visibility: --private for private only, --private=false for public only (omit for all)")
	cmd.Flags().BoolVar(&mirrored, "mirrored", false, "Keep only repos already mirrored (drops onboardable candidates)")
	cmd.Flags().BoolVar(&available, "available", false, "Keep only GitHub repos you could onboard as mirrors (drops existing mirrors)")
	cmd.Flags().StringVar(&forge, "forge", mirrorCloneForge, "Which repositories to list: "+mirrorCloneForge+" for GitHub mirrors and onboardable GitHub repos, "+nativeCloneForge+" for Entire-native repos, or "+forgeFilterAll+" for both")
	cmd.Flags().StringVar(&sortSpec, "sort", "", "Sort by column key (e.g. name, clusters; prefix '-' for descending). Default: name ascending")
	cmd.Flags().IntVar(&limit, "limit", 0, "Show at most N rows, applied after the local filters and sort (0 shows all fetched)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch the complete directory instead of the first "+strconv.Itoa(coreListFetchBudget)+" entries (slower on large orgs)")
	cmd.Flags().BoolVar(&noPager, "no-pager", false, "Print directly to stdout instead of a pager for long output")
	cmd.MarkFlagsMutuallyExclusive("mirrored", "available")
	pageModeFlags(cmd, &pageSize, &pageToken)
	addJSONFlag(cmd)
	setFlagGroup(cmd, flagGroupNavigation, "all", "limit", "page-size", "page-token")
	setFlagGroup(cmd, flagGroupFiltering, "forge", "name", "owner", "cluster", "status", "access", "private", "mirrored", "available", "sort")
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
		Short: "Show where a repo is mirrored, or one mirror by ULID or clone URL",
		Long: "Show a mirror, or every placement of a repo. <mirror> is one of:\n\n" +
			"  - /gh/<owner>/<repo>, as shown in the `mirror list` NAME column — shows\n" +
			"    the repo (visibility, access) and its mirror on every cluster, with\n" +
			"    per-cluster clone URL and status\n" +
			"  - /et/<project>/<repo> — shows an Entire-native repo's primary cluster\n" +
			"    and each mirror of it, with per-cluster clone URL, status\n" +
			"    and, while one is being seeded, how far it has got\n" +
			"  - a mirror ULID\n" +
			"  - an entire:// clone URL (entire://<cluster>/gh/<owner>/<repo>) — the form\n" +
			"    `git clone` accepts; a trailing .git, as pasted from `git remote -v`, is\n" +
			"    accepted too\n\n" +
			"A clone URL is looked up on the login server fronting its cluster, so it\n" +
			"resolves even when that cluster belongs to a federation other than the active\n" +
			"auth context; a repository reference or ULID is looked up on the active\n" +
			"context's login server.",
		Example: "  entire repo mirror get /gh/octocat/hello-world\n" +
			"  entire repo mirror get /et/acme/web\n" +
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
			// A clone URL names its own cluster, so it is looked up on the
			// core fronting that cluster. Recognised by its scheme so a
			// malformed one keeps the clone-URL parser's reason instead of
			// being reported as a bad repository reference.
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref)), "entire:") {
				clusterHost, _, _, _, err := parseMirrorCloneURL(ref)
				if err != nil {
					cmd.SilenceUsage = true
					return badMirrorRefErr(err)
				}
				return runCoreObjectForCluster(cmd, clusterHost, columnHeaders(mirrorColumns), mirrorRow, show)
			}
			// Everything else is a repository reference, in the one grammar
			// the whole mirror subtree takes.
			target, err := parseMirrorRepoRef(ref)
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			if target.forge == nativeCloneForge {
				return runNativeMirrorGet(cmd, target)
			}
			return runRepoMirrorGetByName(cmd, target.owner+"/"+target.repo)
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
	rows := buildRepoDir([]coreapi.RepoIndexEntry{e}, hostBySlug, mirrorCloneForge)
	if len(rows) == 0 {
		name := e.FullName
		if name == "" {
			name = e.Name
		}
		return repoDirRow{Repo: qualifyRepoRef(mirrorCloneForge, name), Private: strings.EqualFold(e.Visibility, "private")}
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
//
// A ROLE column appears only when some placement carries one, which is the
// native view: there a repo's primary and its mirrors sit in the same table,
// and only the primary is outside the mirror verbs' reach. GitHub rows carry
// no role — every placement there mirrors an upstream that is not itself a
// placement — so that view keeps three columns.
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
		fmt.Fprintln(w, "Not mirrored on any cluster.")
		return
	}

	withRole := slices.ContainsFunc(row.Placements, func(p repoDirPlacement) bool { return p.Role != "" })
	cols := []string{colHeaderCluster, colHeaderCloneURL, colHeaderStatus}
	if withRole {
		cols = []string{colHeaderCluster, "ROLE", colHeaderCloneURL, colHeaderStatus}
	}
	headers := styledHeaders(st, cols)
	rows := make([][]string, len(row.Placements))
	for i, p := range row.Placements {
		cluster, status, role := p.Cluster, p.Status, p.Role
		// Stage and the teardown marker qualify the status cell rather than
		// taking columns of their own: each is set only in one transient state,
		// and an always-empty column costs every reader something one reader
		// wants. The Status FIELD stays the server's own value, so --json is
		// machine-readable and only the rendering is prose.
		if p.Stage != "" {
			status += " (" + p.Stage + ")"
		}
		if p.Removing {
			status += " (removing)"
		}
		if st.colorEnabled {
			cluster = st.render(st.cyan, cluster)
			if style, ok := repoStatusColor(st, p.Status); ok {
				status = st.render(style, status)
			}
			role = st.render(st.cyan, role)
		}
		if withRole {
			rows[i] = []string{cluster, orDash(role), orDash(p.CloneURL), status}
			continue
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
	repo = strings.ToLower(strings.TrimSuffix(parts[2], mirrorGitDirSuffix))
	return u.Host, string(coreapi.CreateMirrorRequestInputBodyProviderGithub), strings.ToLower(parts[1]), repo, nil
}

func noMirrorErr(ref string) error {
	return fmt.Errorf("no mirror matching %q (run `entire repo mirror list` to see clone URLs, or pass a ULID)", ref)
}

// badMirrorRefErr wraps a clone-URL parse failure with the accepted <mirror>
// forms. Shared by the pre-dial parse in `mirror get` and resolveMirrorRef so
// both boundaries report identically.
func badMirrorRefErr(err error) error {
	return fmt.Errorf("%w; pass /gh/<owner>/<repo>, a mirror ULID, or a clone URL (entire://<cluster>/gh/<owner>/<repo>)", err)
}

func newRepoMirrorRemoveCmd() *cobra.Command {
	var clusters []string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "remove <repo>",
		Short: "Remove a repository's mirrors from one or more clusters",
		Long: "Removes mirror placements from the clusters named by --cluster, in " +
			"parallel. Other clusters' placements of the same repository, and the " +
			"repository itself, are unaffected.\n\n" +
			"With --cluster omitted, a terminal offers the clusters the repo is " +
			"actually mirrored on as a multi-select, with each placement's status — " +
			"nothing starts ticked, so the selection is also the confirmation. " +
			"There is no default: which clusters a repo is on is a property of the " +
			"repo, so guessing one would delete a copy you never named. " +
			"Non-interactive runs must name them.\n\n" +
			"An /et/ repo's own primary cluster is listed but never removable: " +
			"dropping that is `entire repo delete`. Native teardown is " +
			"asynchronous, so the command waits for each placement to disappear.\n\n" + mirrorRepoRefHelp,
		Example: "  entire repo mirror remove /gh/octocat/hello-world\n" +
			"  entire repo mirror remove /gh/octocat/hello-world --cluster aws-eu-central-1.entire.io\n" +
			"  entire repo mirror remove /et/acme/web --cluster aws-eu-central-1.entire.io,aws-ap-south-1.entire.io",
		Args: cobra.ExactArgs(1),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			// Zero is an unbounded wait, matching `add`.
			if timeout < 0 {
				return errors.New("--timeout must be zero or positive")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMirrorRemove(cmd, args[0], clusters, timeout)
		},
	}
	cmd.Flags().StringSliceVar(&clusters, "cluster", nil, "Cluster host(s) to remove the mirror from; repeat or comma-separate for several (a terminal offers a multi-select when omitted)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "How long to wait for each placement to be torn down (0 waits indefinitely)")
	return cmd
}
