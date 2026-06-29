package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trail"

	"charm.land/huh/v2"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

const (
	defaultTrailListLimit  = 10
	trailListAuthorMe      = "me"
	defaultTrailListStatus = string(trail.StatusOpen)
	// trailListStatusAny disables the status filter; user-facing value for --status.
	trailListStatusAny = "any"
	// trailListServerMaxLimit is the most trails the server returns per
	// request (the list endpoint clamps limit to 200).
	trailListServerMaxLimit = 200
	trailFindMaxPages       = 10
)

func newTrailCmd() *cobra.Command {
	var insecureHTTPAuth bool

	cmd := &cobra.Command{
		Use:    "trail",
		Short:  "Manage trails for your branches",
		Hidden: true,
		Args:   cobra.NoArgs,
		Long:   "A trail ties together the context for a branch. Use `entire trail` to view, create, update, or watch it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().BoolVar(&insecureHTTPAuth, "insecure-http-auth", false,
		"Allow API calls over plain HTTP (insecure, for local development only)")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}

	cmd.AddCommand(newTrailShowCmd())
	cmd.AddCommand(newTrailListCmd())
	cmd.AddCommand(newTrailCreateCmd())
	cmd.AddCommand(newTrailUpdateCmd())
	cmd.AddCommand(newTrailCheckoutCmd())
	cmd.AddCommand(newTrailDeleteCmd())
	cmd.AddCommand(newTrailFindingCmd())
	cmd.AddCommand(newTrailWatchCmd())

	return cmd
}

// trailInsecureHTTP reads the persistent --insecure-http-auth flag from the trail root command.
func trailInsecureHTTP(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("insecure-http-auth") //nolint:errcheck // flag is always registered
	return v
}

// trailListOptions are the inputs to runTrailListAll. Keeping them on a
// struct avoids a long positional argument list at the two call sites.
type trailListOptions struct {
	Author       string
	Status       string
	JSON         bool
	Limit        int
	InsecureHTTP bool
}

func defaultTrailListOptions(insecureHTTP bool) trailListOptions {
	return trailListOptions{
		Status:       defaultTrailListStatus,
		Limit:        defaultTrailListLimit,
		InsecureHTTP: insecureHTTP,
	}
}

func newTrailShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [<trail>]",
		Short: "Show a trail",
		Long: `Show a trail.

If <trail> is omitted, shows the trail for the current branch. Otherwise,
<trail> may be a trail number, id, or branch in the current repo.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runTrailShow(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), selector)
		},
	}
	return cmd
}

// runTrailShow shows one trail, defaulting to the current branch's trail.
func runTrailShow(ctx context.Context, w, errW io.Writer, insecureHTTP bool, selector string) error {
	return runAuthenticatedTrailAPI(ctx, errW, insecureHTTP, func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}

		found, err := resolveTrailBySelector(ctx, client, forge, owner, repo, selector)
		if err != nil {
			return err
		}

		// Enrich the list result with the detail endpoint, which carries the
		// rendered description (trail.body_document.text_snapshot) the list
		// omits, and surface a browser URL. The detail fetch is best-effort:
		// the core metadata already came from the list, so a detail failure
		// falls back to the list body with a warning rather than failing.
		m := found.ToMetadata()
		webURL := trailDisplayURL(*found, forge, owner, repo)
		// Seed the description from the list body so a failed (or skipped)
		// detail fetch still shows something; a successful detail fetch
		// supersedes it with the richer body_document text below.
		bodyText := found.Body
		descriptionLoaded := strings.TrimSpace(found.Body) != ""
		if found.Number > 0 {
			if bt, derr := fetchTrailDescription(ctx, client, forge, owner, repo, found.Number); derr == nil {
				// A successful fetch means we authoritatively consulted the
				// description, but it only supersedes the seeded list body when
				// it actually carries text: an older/partial server that omits
				// body_document returns "" here and must not blank out a list
				// body that is present.
				descriptionLoaded = true
				if strings.TrimSpace(bt) != "" {
					bodyText = bt
				}
			} else {
				// Best-effort: warn but still render metadata + URL (and the
				// list body) rather than failing the whole command.
				fmt.Fprintf(errW, "Warning: could not load trail description: %v\n", derr)
			}
		}
		printTrailDetails(w, m, webURL, trailDescriptionForDisplay(bodyText, descriptionLoaded))
		return nil
	})
}

// resolveTrailBySelector resolves a trail by an optional selector (trail
// number, id, or branch). An empty selector falls back to the current branch's
// trail. It returns an actionable error (never a nil trail with a nil error)
// when nothing matches, so callers can rely on a non-nil result.
func resolveTrailBySelector(ctx context.Context, client *api.Client, forge, owner, repo, selector string) (*api.TrailResource, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		branch, err := GetCurrentBranch(ctx)
		if err != nil {
			return nil, fmt.Errorf("no trail selector given and current branch is unknown: %w\nhint: run 'entire trail list --status any' or pass a trail number, id, or branch", err)
		}
		found, err := findTrailByBranch(ctx, client, forge, owner, repo, branch)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, fmt.Errorf("no trail found for current branch %q\nhint: run 'entire trail create' or 'entire trail list --status any'", branch)
		}
		return found, nil
	}
	found, err := findTrailBySelector(ctx, client, forge, owner, repo, selector)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("no trail %q found in %s/%s/%s (run 'entire trail list --status any')", selector, forge, owner, repo)
	}
	return found, nil
}

func printTrailDetails(w io.Writer, m *trail.Metadata, webURL, bodyText string) {
	fmt.Fprintf(w, "Trail: %s\n", m.Title)
	if m.Number > 0 {
		fmt.Fprintf(w, "  Number:  %d\n", m.Number)
	}
	if !m.TrailID.IsEmpty() {
		fmt.Fprintf(w, "  ID:      %s\n", m.TrailID)
	}
	fmt.Fprintf(w, "  Branch:  %s\n", m.Branch)
	fmt.Fprintf(w, "  Base:    %s\n", m.Base)
	fmt.Fprintf(w, "  Status:  %s\n", m.Status)
	fmt.Fprintf(w, "  Author:  %s\n", m.AuthorLogin())
	if strings.TrimSpace(m.Phase) != "" {
		fmt.Fprintf(w, "  Phase:   %s\n", trailPhaseDisplay(m.Phase))
	}
	if webURL != "" {
		fmt.Fprintf(w, "  URL:     %s\n", webURL)
	}
	if len(m.Labels) > 0 {
		fmt.Fprintf(w, "  Labels:  %s\n", strings.Join(m.Labels, ", "))
	}
	if len(m.Assignees) > 0 {
		fmt.Fprintf(w, "  Assignees: %s\n", strings.Join(m.Assignees, ", "))
	}
	fmt.Fprintf(w, "  Created: %s\n", m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(w, "  Updated: %s\n", m.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
	if strings.TrimSpace(bodyText) != "" {
		fmt.Fprintf(w, "\nDescription:\n%s\n", bodyText)
	}
}

// noTrailDescription is shown by `trail show` when a trail's description loaded
// successfully but is empty — distinguishing "no description yet" from a load
// failure (which warns and renders nothing).
const noTrailDescription = "-- no description provided --"

// trailDescriptionForDisplay returns the text `trail show` renders for the
// description: the body when present; the placeholder when it loaded but is
// empty; or "" when it couldn't be loaded (the caller has already warned).
func trailDescriptionForDisplay(bodyText string, loaded bool) string {
	if strings.TrimSpace(bodyText) != "" {
		return bodyText
	}
	if loaded {
		return noTrailDescription
	}
	return ""
}

// printCreatedTrail reports a newly created trail, including its browser URL
// (the same URL `trail show` surfaces) when one is available.
func printCreatedTrail(w io.Writer, t api.TrailResource, forge, owner, repo string) {
	if t.Branch == "" {
		fmt.Fprintf(w, "Created trail %q (ID: %s)\n", t.Title, t.ID)
	} else {
		fmt.Fprintf(w, "Created trail %q for branch %s (ID: %s)\n", t.Title, t.Branch, t.ID)
	}
	if url := trailDisplayURL(t, forge, owner, repo); url != "" {
		fmt.Fprintf(w, "  URL: %s\n", url)
	}
}

// trailDisplayURL returns the trail's browser URL. It prefers the canonical URL
// the server now returns, so the CLI tracks any route change without being
// updated in lockstep, and falls back to a locally constructed URL only for
// older servers that omit the field.
func trailDisplayURL(t api.TrailResource, forge, owner, repo string) string {
	if strings.TrimSpace(t.URL) != "" {
		return t.URL
	}
	if t.Number > 0 {
		return trailWebURL(api.BaseURL(), forge, owner, repo, t.Number)
	}
	return ""
}

// trailWebURL builds a fallback browser URL for a trail used only when the
// server does not supply one (older servers):
// <web-origin>/<forge>/<owner>/<repo>/trails/<number>. In production the web app
// is served from the same origin as the data API, so the API base URL doubles
// as the web origin. A split local-dev setup (API and frontend on different
// ports) would point this at the API port rather than the dev frontend.
func trailWebURL(base, forge, owner, repo string, number int) string {
	return strings.TrimRight(base, "/") + "/" + forge + "/" + owner + "/" + repo + "/trails/" + strconv.Itoa(number)
}

// fetchTrailDescription fetches a trail's rendered description text
// (`trail.body_document.text_snapshot`), which the list endpoint omits, by
// integer number. It returns only the description — the list result already
// supplies the metadata — and decodes only the fields it needs, so it is
// unaffected by the shape of sibling fields like `checkpoints`/`thread`.
func fetchTrailDescription(ctx context.Context, client *api.Client, forge, owner, repo string, number int) (string, error) {
	resp, err := client.Get(ctx, trailNumberPath(forge, owner, repo, number))
	if err != nil {
		return "", fmt.Errorf("failed to fetch trail detail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return "", err
	}
	var detail struct {
		Trail api.TrailResource `json:"trail"`
	}
	if err := api.DecodeJSON(resp, &detail); err != nil {
		return "", fmt.Errorf("failed to decode trail detail: %w", err)
	}
	if detail.Trail.BodyDocument == nil {
		return "", nil
	}
	return strings.TrimSpace(detail.Trail.BodyDocument.TextSnapshot), nil
}

func newTrailListCmd() *cobra.Command {
	var opts trailListOptions

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent trails",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.InsecureHTTP = trailInsecureHTTP(cmd)
			return runTrailListAll(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.Author, "author", "",
		"Filter by author login (case-insensitive); use '"+trailListAuthorMe+"' for yourself (requires gh CLI); omit for any author")
	cmd.Flags().StringVar(&opts.Status, "status", defaultTrailListStatus,
		"Filter by comma-separated status(es): "+formatValidStatuses()+"; use '"+trailListStatusAny+"' for all statuses")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON (respects --author, --status, and --limit)")
	cmd.Flags().IntVarP(&opts.Limit, "limit", "n", defaultTrailListLimit, "Maximum number of trails to show")

	return cmd
}

func runTrailListAll(ctx context.Context, w, errW io.Writer, opts trailListOptions) error {
	statusFilters, err := validateTrailListOptions(opts)
	if err != nil {
		return err
	}
	return runAuthenticatedTrailAPI(ctx, errW, opts.InsecureHTTP, func(ctx context.Context, client *api.Client) error {
		return runTrailListAllWithClient(ctx, w, client, opts, statusFilters)
	})
}

func validateTrailListOptions(opts trailListOptions) ([]trail.Status, error) {
	if opts.Limit <= 0 {
		return nil, errors.New("limit must be greater than 0")
	}
	return parseTrailStatusFilter(opts.Status)
}

func runTrailListAllValidatedWithClient(ctx context.Context, w io.Writer, client *api.Client, opts trailListOptions) error {
	statusFilters, err := validateTrailListOptions(opts)
	if err != nil {
		return err
	}
	return runTrailListAllWithClient(ctx, w, client, opts, statusFilters)
}

func runTrailListAllWithClient(ctx context.Context, w io.Writer, client *api.Client, opts trailListOptions, statusFilters []trail.Status) error {
	authorFilter := opts.Author
	currentUserLogin := ""
	if authorFilter == trailListAuthorMe {
		login, err := fetchCurrentUserLogin(ctx, execRunner{})
		if err != nil {
			return err
		}
		currentUserLogin = login
		authorFilter = login
	}

	forge, owner, repo, err := resolveTrailRemote(ctx)
	if err != nil {
		return err
	}

	// Filtering, sorting (updated_at desc), and truncation all happen
	// server-side; the response carries the total match count so a capped
	// page never reads as the total number of matches.
	resp, err := client.Get(ctx, trailsBasePath(forge, owner, repo)+trailListQuery(statusFilters, authorFilter, opts.Limit))
	if err != nil {
		return fmt.Errorf("failed to list trails: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return err
	}

	var listResp api.TrailListResponse
	if err := api.DecodeJSON(resp, &listResp); err != nil {
		return fmt.Errorf("failed to decode trail list: %w", err)
	}

	// Convert to metadata for display, attaching the browser URL (server-provided
	// when present, locally constructed as a fallback for older servers).
	trails := make([]*trail.Metadata, 0, len(listResp.Trails))
	for i := range listResp.Trails {
		m := listResp.Trails[i].ToMetadata()
		m.URL = trailDisplayURL(listResp.Trails[i], forge, owner, repo)
		trails = append(trails, m)
	}

	totalMatched := listResp.Total
	if totalMatched < len(trails) {
		// Older servers don't report a total; fall back to the page size.
		totalMatched = len(trails)
	}

	if opts.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(trails); err != nil {
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
		return nil
	}

	if len(trails) == 0 {
		printTrailListEmpty(w, authorFilter, statusFilters)
		return nil
	}

	printTrailList(w, trails, trailListDisplayOptions{
		RequestedAuthor: authorFilter,
		CurrentUser:     currentUserLogin,
		StatusFilters:   statusFilters,
		TotalMatched:    totalMatched,
	})

	if opts.Limit > trailListServerMaxLimit && totalMatched > len(trails) {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Note: --limit %d exceeds the server maximum of %d trails per request.\n", opts.Limit, trailListServerMaxLimit)
	}

	return nil
}

// trailListQuery builds the server-side filter query for the trail list
// endpoint. Empty statusFilters (--status any) omits the status param so the
// server returns all statuses; the limit is capped at the server maximum.
func trailListQuery(statusFilters []trail.Status, author string, limit int) string {
	return trailListQueryWithOffset(statusFilters, author, limit, 0)
}

func trailListQueryWithOffset(statusFilters []trail.Status, author string, limit, offset int) string {
	q := url.Values{}
	if len(statusFilters) > 0 {
		parts := make([]string, len(statusFilters))
		for i, status := range statusFilters {
			parts[i] = string(status)
		}
		q.Set("status", strings.Join(parts, ","))
	}
	if author != "" {
		q.Set("author", author)
	}
	if limit > trailListServerMaxLimit {
		limit = trailListServerMaxLimit
	}
	q.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	return "?" + q.Encode()
}

// printTrailListEmpty renders the empty-state message. It names the active
// status filter so a bare `entire trail list` (which defaults to open)
// doesn't read as "this repo has no trails" when trails exist in other
// statuses. statusFilters is empty when the user passed --status any.
func printTrailListEmpty(w io.Writer, authorFilter string, statusFilters []trail.Status) {
	desc := "No trails found"
	if len(statusFilters) > 0 {
		desc = fmt.Sprintf("No %s trails found", trailStatusListDisplay(statusFilters))
	}
	if authorFilter != "" {
		desc += " for " + authorFilter
	}
	fmt.Fprintf(w, "%s.\n", desc)

	if len(statusFilters) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Use --status any to see trails in other statuses.")
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  entire trail create   Create a trail for the current branch")
	fmt.Fprintln(w, "  entire trail list     List recent trails")
	fmt.Fprintln(w, "  entire trail update   Update trail metadata")
}

func parseTrailStatusFilter(filter string) ([]trail.Status, error) {
	if filter == "" || filter == trailListStatusAny {
		return nil, nil
	}

	parts := strings.Split(filter, ",")
	statuses := make([]trail.Status, 0, len(parts))
	seen := make(map[trail.Status]bool, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("invalid status filter %q: empty status", filter)
		}
		status := trail.Status(name)
		if !status.IsValid() {
			return nil, fmt.Errorf("invalid status %q: valid values are %s", name, formatValidStatuses())
		}
		if seen[status] {
			continue
		}
		seen[status] = true
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// fetchCurrentUserLogin resolves --author me to a GitHub login via the local
// gh CLI. The runner is injectable so tests can stub gh without touching the
// process environment.
func fetchCurrentUserLogin(ctx context.Context, runner bootstrapRunner) (string, error) {
	login, err := ghCurrentUser(ctx, runner)
	if err != nil {
		return "", fmt.Errorf("resolve --author %s via gh CLI: %w\nhint: pass --author <login> explicitly if gh is unavailable", trailListAuthorMe, err)
	}
	if login == "" {
		return "", errors.New("resolve --author me: gh returned an empty login")
	}
	return login, nil
}

type trailListDisplayOptions struct {
	RequestedAuthor string
	CurrentUser     string
	StatusFilters   []trail.Status
	// TotalMatched is the number of trails matching the filters server-side,
	// before --limit truncation. Counts render as "shown/total" when they
	// differ so a capped page doesn't read as the total number of matches.
	TotalMatched int
}

func printTrailList(w io.Writer, trails []*trail.Metadata, opts trailListDisplayOptions) {
	showAuthor := opts.RequestedAuthor == ""
	// Show the status column unless exactly one status is filtered — that
	// status is already named in the header.
	showStatus := len(opts.StatusFilters) != 1
	printTrailListHeader(w, opts, len(trails))
	fmt.Fprintln(w)
	printTrailRows(w, trails, showAuthor, showStatus)
}

func printTrailListHeader(w io.Writer, opts trailListDisplayOptions, count int) {
	countStr := trailCountDisplay(count, opts.TotalMatched)
	// The noun refers to the full match set, so pluralize by the total when
	// the page is truncated ("1/2 trails", not "1/2 trail").
	nounCount := count
	if opts.TotalMatched > count {
		nounCount = opts.TotalMatched
	}
	if opts.RequestedAuthor == "" {
		if len(opts.StatusFilters) == 0 {
			fmt.Fprintf(w, "  Recent %s · %s\n", pluralize("trail", nounCount), countStr)
			return
		}
		fmt.Fprintf(w, "  %s · %s %s\n", trailStatusListTitle(opts.StatusFilters), countStr, pluralize("trail", nounCount))
		return
	}

	label := opts.RequestedAuthor
	// When --author me resolves to the same login the server already returned
	// for the trail, render "Your trails (login)" so identity drift between
	// gh and Entire is visible at a glance.
	if opts.CurrentUser != "" && strings.EqualFold(opts.RequestedAuthor, opts.CurrentUser) {
		label = fmt.Sprintf("Your trails (%s)", opts.CurrentUser)
	}
	if len(opts.StatusFilters) == 0 {
		fmt.Fprintf(w, "  %s · %s\n", label, countStr)
		return
	}
	fmt.Fprintf(w, "  %s · %s %s\n", label, countStr, trailStatusListDisplay(opts.StatusFilters))
}

func printTrailRows(w io.Writer, trails []*trail.Metadata, showAuthor, showStatus bool) {
	// tabwriter aligns by display columns instead of bytes, so multi-byte
	// branch names or logins don't throw off the table.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	showPhase := trailListHasPhase(trails)
	showURL := trailListHasURL(trails)
	columns := []string{"NUM", "BRANCH", "TITLE"}
	if showStatus {
		columns = append(columns, "STATUS")
	}
	if showPhase {
		columns = append(columns, "PHASE")
	}
	if showAuthor {
		columns = append(columns, "AUTHOR")
	}
	columns = append(columns, "UPDATED")
	if showURL {
		columns = append(columns, "URL")
	}
	fmt.Fprintln(tw, "  "+strings.Join(columns, "\t"))
	for _, t := range trails {
		number := "-"
		if t.Number > 0 {
			number = strconv.Itoa(t.Number)
		}
		title := truncateOneLine(t.Title, 60)
		if title == "" {
			title = "(untitled)"
		}
		fields := []string{number, t.Branch, title}
		if showStatus {
			fields = append(fields, trailStatusDisplay(t.Status))
		}
		if showPhase {
			fields = append(fields, trailPhaseDisplay(t.Phase))
		}
		if showAuthor {
			fields = append(fields, t.AuthorLogin())
		}
		fields = append(fields, timeAgo(t.UpdatedAt))
		if showURL {
			fields = append(fields, t.URL)
		}
		fmt.Fprintln(tw, "  "+strings.Join(fields, "\t"))
	}
	_ = tw.Flush()
}

func trailListHasPhase(trails []*trail.Metadata) bool {
	for _, t := range trails {
		if t != nil && strings.TrimSpace(t.Phase) != "" {
			return true
		}
	}
	return false
}

func trailListHasURL(trails []*trail.Metadata) bool {
	for _, t := range trails {
		if t != nil && strings.TrimSpace(t.URL) != "" {
			return true
		}
	}
	return false
}

func trailPhaseDisplay(phase string) string {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return "-"
	}
	return strings.ReplaceAll(phase, "_", " ")
}

func trailStatusListDisplay(statuses []trail.Status) string {
	parts := make([]string, len(statuses))
	for i, status := range statuses {
		parts[i] = trailStatusDisplay(status)
	}
	return strings.Join(parts, ", ")
}

func trailStatusListTitle(statuses []trail.Status) string {
	display := trailStatusListDisplay(statuses)
	if display == "" {
		return ""
	}
	return strings.ToUpper(display[:1]) + display[1:]
}

func trailStatusDisplay(status trail.Status) string {
	return strings.ReplaceAll(string(status), "_", " ")
}

// trailCountDisplay renders a count as "shown/total" when --limit truncated
// the list, so a capped page doesn't read as the total number of matches.
func trailCountDisplay(shown, total int) string {
	if total > shown {
		return fmt.Sprintf("%d/%d", shown, total)
	}
	return strconv.Itoa(shown)
}

func pluralize(s string, count int) string {
	if count == 1 {
		return s
	}
	return s + "s"
}

func newTrailCreateCmd() *cobra.Command {
	var title, body, base, branch, status string
	var checkout, noBranch bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a trail for the current, a new, or no branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrailCreate(cmd, title, body, base, branch, status, checkout, noBranch)
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "Trail title")
	cmd.Flags().StringVar(&body, "body", "", "Trail body")
	cmd.Flags().StringVar(&base, "base", "", "Base branch (defaults to detected default branch)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for the trail (defaults to current branch)")
	cmd.Flags().StringVar(&status, "status", "", "Initial status (defaults to open)")
	cmd.Flags().BoolVar(&checkout, "checkout", false, "Check out the branch after creating it")
	cmd.Flags().BoolVar(&noBranch, "no-branch", false, "Create a branchless trail")

	return cmd
}

//nolint:cyclop // sequential steps for creating a trail — splitting would obscure the flow
func runTrailCreate(cmd *cobra.Command, title, body, base, branch, statusStr string, checkout, noBranch bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	if err := validateTrailCreateFlagCombos(cmd, checkout, noBranch); err != nil {
		return err
	}

	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	base = resolveTrailCreateBase(repo, base)
	_, currentBranch, _ := isOnDefaultBranchRepo(repo) //nolint:errcheck // best-effort; reuse the open repo for the current branch name
	title, body, base, branch, statusStr, err = resolveTrailCreateFields(cmd, w, title, body, base, branch, statusStr, currentBranch, noBranch)
	if err != nil {
		return err
	}
	if err := validateTrailCreateFields(ctx, title, branch, statusStr, noBranch); err != nil {
		return err
	}

	client, err := NewAuthenticatedAPIClient(ctx, trailInsecureHTTP(cmd))
	if err != nil {
		return renderDataAPIAuthError(cmd.ErrOrStderr(), err)
	}
	forge, owner, repoName, err := resolveTrailRemote(ctx)
	if err != nil {
		return err
	}

	branchState, err := prepareTrailCreateBranch(w, errW, repo, branch, currentBranch, noBranch)
	if err != nil {
		return err
	}

	createResp, err := postTrailCreate(ctx, client, forge, owner, repoName, title, body, branch, base, statusStr)
	if err != nil {
		cleanupCreatedTrailBranch(repo, branch, branchState.LocalCreated, branchState.RemotePushed, errW)
		return err
	}
	printCreatedTrail(w, createResp.Trail, forge, owner, repoName)

	return maybeCheckoutTrailCreateBranch(ctx, cmd, w, branch, currentBranch, checkout, branchState.NeedsCreation)
}

type trailCreateBranchState struct {
	NeedsCreation bool
	LocalCreated  bool
	RemotePushed  bool
}

func validateTrailCreateFlagCombos(cmd *cobra.Command, checkout, noBranch bool) error {
	if noBranch && cmd.Flags().Changed("branch") {
		return errors.New("cannot combine --no-branch with --branch")
	}
	if noBranch && checkout {
		return errors.New("cannot combine --no-branch with --checkout")
	}
	return nil
}

func resolveTrailCreateBase(repo *git.Repository, base string) string {
	if base != "" {
		return base
	}
	if detected := strategy.GetDefaultBranchName(repo); detected != "" {
		return detected
	}
	return defaultBaseBranch
}

func resolveTrailCreateFields(cmd *cobra.Command, w io.Writer, title, body, base, branch, statusStr, currentBranch string, noBranch bool) (string, string, string, string, string, error) {
	interactive := !cmd.Flags().Changed("title") && !cmd.Flags().Changed("branch")
	switch {
	case interactive:
		// Interactive flow: title → body → branch (unless branchless) → status.
		if err := runTrailCreateInteractive(&title, &body, &branch, &statusStr, noBranch); err != nil {
			return "", "", "", "", "", handleFormCancellation(w, "Trail creation", err)
		}
	case noBranch:
		branch = ""
	default:
		// Non-interactive: derive missing values from provided flags. With
		// --branch omitted, use the checked-out branch (a feature branch); only
		// slug a new branch from the title when the checked-out branch is the base.
		branch = resolveCreateBranch(branch, currentBranch, base, title, cmd.Flags().Changed("title"))
		if title == "" {
			title = trail.HumanizeBranchName(branch)
		}
	}
	statusStr = strings.TrimSpace(statusStr)
	if statusStr == "" {
		statusStr = string(trail.StatusOpen)
	}
	return strings.TrimSpace(title), body, strings.TrimSpace(base), strings.TrimSpace(branch), statusStr, nil
}

func validateTrailCreateFields(ctx context.Context, title, branch, statusStr string, noBranch bool) error {
	if title == "" {
		return errors.New("trail title is required")
	}
	if !noBranch {
		if branch == "" {
			return errors.New("branch name is required")
		}
		if err := ValidateBranchName(ctx, branch); err != nil {
			return err
		}
	}
	if status := trail.Status(statusStr); !status.IsValid() {
		return fmt.Errorf("invalid status %q: valid values are %s", statusStr, formatValidStatuses())
	}
	return nil
}

func prepareTrailCreateBranch(w, errW io.Writer, repo *git.Repository, branch, currentBranch string, noBranch bool) (trailCreateBranchState, error) {
	var state trailCreateBranchState
	if noBranch || branch == "" {
		// Branchless trails have no remote branch to create, fetch, or push.
		return state, nil
	}

	state.NeedsCreation = branchNeedsCreation(repo, branch)
	existedOnOrigin, existErr := branchExistsOnOrigin(branch)
	if existErr != nil {
		fmt.Fprintf(errW, "Warning: could not check whether branch %s already exists on origin: %v\n", branch, existErr)
		existedOnOrigin = true
	}

	if err := ensureTrailCreateBranchExists(w, repo, branch, currentBranch, existedOnOrigin, &state); err != nil {
		return state, err
	}

	// For branch-backed trails, always push the branch first: the trail binds to a
	// remote branch, so deliver it before creating the trail rather than letting
	// the server backfill it at the base tip. Branchless trails skip this entirely.
	if err := pushBranchToOrigin(branch); err != nil {
		cleanupCreatedTrailBranch(repo, branch, state.LocalCreated, false, errW)
		return state, fmt.Errorf("failed to push branch %q to origin: %w\nhint: the trail was not created because its branch could not be delivered to the remote.\n  - if this is an auth error, link your GitHub account and retry\n  - if this is a non-fast-forward, update branch %q from origin and retry", branch, err, branch)
	}
	state.RemotePushed = !existedOnOrigin
	fmt.Fprintf(w, "Pushed branch %s to origin\n", branch)
	return state, nil
}

func ensureTrailCreateBranchExists(w io.Writer, repo *git.Repository, branch, currentBranch string, existedOnOrigin bool, state *trailCreateBranchState) error {
	if !state.NeedsCreation {
		if currentBranch != branch {
			fmt.Fprintf(w, "Note: trail will be created for branch %q (not the current branch)\n", branch)
		}
		return nil
	}
	if existedOnOrigin {
		if err := fetchBranchFromOrigin(branch); err != nil {
			return fmt.Errorf("failed to fetch branch %q from origin: %w", branch, err)
		}
		state.LocalCreated = true
		fmt.Fprintf(w, "Fetched branch %s from origin\n", branch)
		return nil
	}
	if err := createBranch(repo, branch); err != nil {
		return fmt.Errorf("failed to create branch %q: %w", branch, err)
	}
	state.LocalCreated = true
	fmt.Fprintf(w, "Created branch %s\n", branch)
	return nil
}

func postTrailCreate(ctx context.Context, client *api.Client, forge, owner, repoName, title, body, branch, base, statusStr string) (api.TrailCreateResponse, error) {
	createReq := newTrailCreateRequest(title, body, branch, base, statusStr)
	resp, err := client.Post(ctx, trailsBasePath(forge, owner, repoName), createReq)
	if err != nil {
		noteTrailCommandEnablement(ctx, client, err)
		return api.TrailCreateResponse{}, fmt.Errorf("failed to create trail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		noteTrailCommandEnablement(ctx, client, err)
		return api.TrailCreateResponse{}, err
	}
	saveTrailsEnabledForRemoteBestEffort(ctx, forge, owner, repoName, true)

	var createResp api.TrailCreateResponse
	if err := api.DecodeJSON(resp, &createResp); err != nil {
		return api.TrailCreateResponse{}, fmt.Errorf("failed to decode create response: %w", err)
	}
	return createResp, nil
}

func maybeCheckoutTrailCreateBranch(ctx context.Context, cmd *cobra.Command, w io.Writer, branch, currentBranch string, checkout, needsCreation bool) error {
	if !needsCreation || currentBranch == branch {
		return nil
	}
	shouldCheckout := checkout
	if !shouldCheckout && !cmd.Flags().Changed("checkout") {
		// Interactive: ask whether to checkout
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Check out branch %s?", branch)).
					Value(&shouldCheckout),
			),
		)
		if formErr := form.Run(); formErr != nil {
			shouldCheckout = false
		}
	}
	if !shouldCheckout {
		return nil
	}
	if err := CheckoutBranch(ctx, branch); err != nil {
		return fmt.Errorf("failed to checkout branch %q: %w", branch, err)
	}
	fmt.Fprintf(w, "Switched to branch %s\n", branch)
	return nil
}

func newTrailCreateRequest(title, body, branch, base, statusStr string) api.TrailCreateRequest {
	req := api.TrailCreateRequest{
		Title:  title,
		Body:   body,
		Base:   base,
		Status: statusStr,
	}
	if branch != "" {
		req.BranchName = branch
		req.BranchAction = "link"
	}
	return req
}

func newTrailUpdateCmd() *cobra.Command {
	var statusStr, title, body, branch string
	var labelAdd, labelRemove []string

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update trail metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrailUpdate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), trailUpdateInputs{
				Status:        statusStr,
				StatusChanged: cmd.Flags().Changed("status"),
				Title:         title,
				TitleChanged:  cmd.Flags().Changed("title"),
				Body:          body,
				BodyChanged:   cmd.Flags().Changed("body"),
				Branch:        branch,
				LabelAdd:      labelAdd,
				LabelRemove:   labelRemove,
			})
		},
	}

	cmd.Flags().StringVar(&statusStr, "status", "", "Update status")
	cmd.Flags().StringVar(&title, "title", "", "Update title")
	cmd.Flags().StringVar(&body, "body", "", "Update body")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to update trail for (defaults to current)")
	cmd.Flags().StringSliceVar(&labelAdd, "add-label", nil, "Add label(s)")
	cmd.Flags().StringSliceVar(&labelRemove, "remove-label", nil, "Remove label(s)")

	return cmd
}

type trailUpdateInputs struct {
	Status        string
	StatusChanged bool
	Title         string
	TitleChanged  bool
	Body          string
	BodyChanged   bool
	Branch        string
	LabelAdd      []string
	LabelRemove   []string
}

func runTrailUpdate(ctx context.Context, w, errW io.Writer, insecureHTTP bool, inputs trailUpdateInputs) error {
	return runAuthenticatedTrailAPI(ctx, errW, insecureHTTP, func(ctx context.Context, client *api.Client) error {
		forge, owner, repoName, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}

		// Determine branch.
		branch := inputs.Branch
		if branch == "" {
			branch, err = GetCurrentBranch(ctx)
			if err != nil {
				return fmt.Errorf("failed to determine current branch: %w", err)
			}
		}

		// Find the trail by branch.
		found, err := findTrailByBranch(ctx, client, forge, owner, repoName, branch)
		if err != nil {
			return err
		}
		if found == nil {
			return fmt.Errorf("no trail found for branch %q", branch)
		}

		// Interactive mode when no update flags are provided.
		statusStr := inputs.Status
		title := inputs.Title
		body := inputs.Body
		noFlags := !inputs.StatusChanged && !inputs.TitleChanged && !inputs.BodyChanged && inputs.LabelAdd == nil && inputs.LabelRemove == nil
		if noFlags {
			metadata := found.ToMetadata()
			// Build status options with current value as default.
			var statusOptions []huh.Option[string]
			for _, s := range trail.ValidStatuses() {
				if (s == trail.StatusMerged || s == trail.StatusClosed) && s != metadata.Status {
					continue
				}
				label := string(s)
				if s == metadata.Status {
					label += " (current)"
				}
				statusOptions = append(statusOptions, huh.NewOption(label, string(s)))
			}
			statusStr = string(metadata.Status)
			title = metadata.Title
			body = metadata.Body

			form := NewAccessibleForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Status").
						Options(statusOptions...).
						Value(&statusStr),
					huh.NewInput().
						Title("Title").
						Value(&title),
					huh.NewText().
						Title("Body").
						Value(&body),
				),
			)
			if formErr := form.Run(); formErr != nil {
				return handleFormCancellation(w, "Trail update", formErr)
			}
			inputs.StatusChanged = true
			inputs.TitleChanged = true
			inputs.BodyChanged = true
		}

		statusStr = strings.TrimSpace(statusStr)
		title = strings.TrimSpace(title)
		if err := validateTrailUpdateFields(trailUpdateInputs{
			Status:        statusStr,
			StatusChanged: inputs.StatusChanged,
			Title:         title,
			TitleChanged:  inputs.TitleChanged,
		}); err != nil {
			return err
		}

		// Build update request with only changed fields.
		updateReq := buildTrailUpdateRequest(found, trailUpdateInputs{
			Status:        statusStr,
			StatusChanged: inputs.StatusChanged,
			Title:         title,
			TitleChanged:  inputs.TitleChanged,
			Body:          body,
			BodyChanged:   inputs.BodyChanged,
			LabelAdd:      inputs.LabelAdd,
			LabelRemove:   inputs.LabelRemove,
		})

		// The single-trail endpoint is keyed by trail number, not id; the server
		// rejects an id here with "Invalid trail number format".
		if found.Number <= 0 {
			return fmt.Errorf("trail for branch %q has no number yet; cannot update", branch)
		}
		resp, err := client.Patch(ctx, trailNumberPath(forge, owner, repoName, found.Number), updateReq)
		if err != nil {
			return fmt.Errorf("failed to update trail: %w", err)
		}
		defer resp.Body.Close()
		if err := checkTrailResponse(resp); err != nil {
			return err
		}

		var updateResp api.TrailUpdateResponse
		if err := api.DecodeJSON(resp, &updateResp); err != nil {
			return fmt.Errorf("failed to decode update response: %w", err)
		}

		fmt.Fprintf(w, "Updated trail for branch %s\n", branch)
		return nil
	})
}

func validateTrailUpdateFields(inputs trailUpdateInputs) error {
	if inputs.TitleChanged && strings.TrimSpace(inputs.Title) == "" {
		return errors.New("trail title is required")
	}
	if inputs.StatusChanged {
		status := trail.Status(strings.TrimSpace(inputs.Status))
		if !status.IsValid() {
			return fmt.Errorf("invalid status %q: valid values are %s", inputs.Status, formatValidStatuses())
		}
	}
	return nil
}

// buildTrailUpdateRequest constructs a PATCH request body from the current trail and the requested changes.
func buildTrailUpdateRequest(current *api.TrailResource, inputs trailUpdateInputs) api.TrailUpdateRequest {
	var req api.TrailUpdateRequest

	if inputs.StatusChanged {
		req.Status = &inputs.Status
	}
	if inputs.TitleChanged {
		req.Title = &inputs.Title
	}
	if inputs.BodyChanged {
		req.Body = &inputs.Body
	}

	// Handle label changes: merge adds, remove removes.
	if len(inputs.LabelAdd) > 0 || len(inputs.LabelRemove) > 0 {
		labels := make([]string, 0, len(current.Labels)+len(inputs.LabelAdd))
		labels = append(labels, current.Labels...)
		for _, l := range inputs.LabelAdd {
			found := false
			for _, existing := range labels {
				if existing == l {
					found = true
					break
				}
			}
			if !found {
				labels = append(labels, l)
			}
		}
		for _, l := range inputs.LabelRemove {
			for i, existing := range labels {
				if existing == l {
					labels = append(labels[:i], labels[i+1:]...)
					break
				}
			}
		}
		req.Labels = &labels
	}

	return req
}

func newTrailCheckoutCmd() *cobra.Command {
	var trailSelector string
	var force bool

	cmd := &cobra.Command{
		Use:   "checkout [<trail>]",
		Short: "Check out a trail's branch",
		Long: `Check out the branch of a trail.

The trail may be given as the first argument or via --trail, as a number, id, or
branch. Without one, the trail for the current branch is used. The trail's branch
is checked out, fetching it from origin first when it only exists there.

This must be run from within a clone of the repository the trail belongs to; the
trail is looked up against that repository's origin remote.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := trailSelector
			if len(args) == 1 {
				if cmd.Flags().Changed("trail") {
					return errors.New("cannot combine a trail argument with --trail")
				}
				selector = args[0]
			}
			return runTrailCheckout(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), selector, force)
		},
	}

	cmd.Flags().StringVar(&trailSelector, "trail", "", "Trail to check out (number, id, or branch; defaults to the current branch's trail)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip the prompt before fetching a remote-only branch")

	return cmd
}

func runTrailCheckout(ctx context.Context, w, errW io.Writer, insecureHTTP bool, selector string, force bool) error {
	return runAuthenticatedTrailAPI(ctx, errW, insecureHTTP, func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}

		found, err := resolveTrailBySelector(ctx, client, forge, owner, repo, selector)
		if err != nil {
			return err
		}

		branch := strings.TrimSpace(found.Branch)
		if branch == "" {
			return fmt.Errorf("%s has no branch to check out", describeTrailRef(found))
		}

		currentBranch, _ := GetCurrentBranch(ctx) //nolint:errcheck // best-effort; a detached HEAD just means "not already on the branch"
		if currentBranch == branch {
			fmt.Fprintf(w, "Already on branch %s for %s.\n", branch, describeTrailRef(found))
			return nil
		}

		fmt.Fprintf(w, "Checking out %s\n", describeTrailRef(found))
		// switchToBranchForResume handles local vs. remote-only branches, the
		// uncommitted-changes guard, and the fetch prompt; reuse it rather than
		// re-deriving that logic here.
		proceed, err := switchToBranchForResume(ctx, w, errW, branch, force)
		if err != nil {
			return err
		}
		if !proceed {
			// The user declined to fetch a remote-only branch — a clean stop, not
			// an error. Say so explicitly so the preceding "Checking out …" line
			// doesn't read as a successful switch.
			fmt.Fprintf(w, "Checkout of branch %s cancelled.\n", branch)
		}
		return nil
	})
}

// describeTrailRef renders a short human reference to a trail for status
// messages, e.g. "trail #575 (Add foo)" or, when the trail has no number yet,
// "trail \"Add foo\"".
func describeTrailRef(t *api.TrailResource) string {
	title := strings.TrimSpace(t.Title)
	if t.Number > 0 {
		if title == "" {
			return fmt.Sprintf("trail #%d", t.Number)
		}
		return fmt.Sprintf("trail #%d (%s)", t.Number, title)
	}
	if title == "" {
		return "trail"
	}
	return fmt.Sprintf("trail %q", title)
}

// parseTrailNumberArg parses an optional positional trail-number argument.
// It returns 0 when no argument is supplied; a supplied value must be a
// positive integer (the server keys single-trail endpoints by number).
func parseTrailNumberArg(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid trail number %q: expected a positive integer (see 'entire trail list')", args[0])
	}
	return n, nil
}

func newTrailDeleteCmd() *cobra.Command {
	var branch string
	var force bool

	cmd := &cobra.Command{
		Use:   "delete [<number>]",
		Short: "Delete a trail",
		Long: `Delete a trail by number, or the trail for a branch.

If <number> is omitted, the trail for --branch (or the current branch) is used.
Deletion is permanent; you are prompted to confirm unless --force is passed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseTrailNumberArg(args)
			if err != nil {
				return err
			}
			if number > 0 && cmd.Flags().Changed("branch") {
				return errors.New("cannot combine a trail <number> with --branch")
			}
			return runTrailDelete(cmd, number, branch, force)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose trail to delete (defaults to current)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip the confirmation prompt")

	return cmd
}

func runTrailDelete(cmd *cobra.Command, number int, branch string, force bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	return runAuthenticatedTrailAPI(ctx, cmd.ErrOrStderr(), trailInsecureHTTP(cmd), func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}

		// Resolve the target trail. An explicit number is authoritative (a
		// lookup is best-effort, only to label the confirmation); otherwise the
		// branch's trail supplies the number.
		title := ""
		if number == 0 {
			if branch == "" {
				branch, err = GetCurrentBranch(ctx)
				if err != nil {
					return fmt.Errorf("failed to determine current branch: %w", err)
				}
			}
			found, ferr := findTrailByBranch(ctx, client, forge, owner, repo, branch)
			if ferr != nil {
				return ferr
			}
			if found == nil {
				return fmt.Errorf("no trail found for branch %q", branch)
			}
			if found.Number <= 0 {
				return fmt.Errorf("trail for branch %q has no number yet; cannot delete", branch)
			}
			number = found.Number
			title = found.Title
		} else if found, ferr := findTrailByNumber(ctx, client, forge, owner, repo, number); ferr == nil && found != nil {
			title = found.Title
		}

		proceed, err := confirmTrailDeletion(ctx, w, number, title, force, interactive.CanPromptInteractively())
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}

		if err := deleteTrailByNumber(ctx, client, forge, owner, repo, number); err != nil {
			return err
		}

		fmt.Fprintf(w, "Deleted trail #%d\n", number)
		return nil
	})
}

// deleteTrailByNumber deletes the trail with the given integer number and
// verifies the server's {ok:true} signal. CheckResponse accepts any 2xx and the
// body is otherwise unread, so a destructive delete must confirm ok before
// reporting success (decoding also drains the body for connection reuse).
func deleteTrailByNumber(ctx context.Context, client *api.Client, forge, owner, repo string, number int) error {
	resp, err := client.Delete(ctx, trailNumberPath(forge, owner, repo, number))
	if err != nil {
		return fmt.Errorf("failed to delete trail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return err
	}
	var delResp api.TrailDeleteResponse
	if err := api.DecodeJSON(resp, &delResp); err != nil {
		return fmt.Errorf("failed to decode delete response: %w", err)
	}
	if !delResp.OK {
		return fmt.Errorf("trail API did not confirm deletion of trail #%d", number)
	}
	return nil
}

// confirmTrailDeletion decides whether a trail delete should proceed. With
// force it proceeds silently. Otherwise it requires an interactive terminal:
// when none is available it refuses (returns an error) rather than deleting
// unprompted; when one is, it shows a confirmation form. canPrompt is passed in
// (rather than queried) so the decision is unit-testable without a TTY.
func confirmTrailDeletion(ctx context.Context, w io.Writer, number int, title string, force, canPrompt bool) (bool, error) {
	if force {
		return true, nil
	}
	if !canPrompt {
		return false, fmt.Errorf("refusing to delete trail #%d without confirmation; pass --force", number)
	}
	// huh opens the TTY during form startup regardless of context state, so
	// guard explicitly to honor an already-cancelled command context.
	if ctx.Err() != nil {
		return false, nil //nolint:nilerr // cancelled context is a clean skip, not an error
	}
	prompt := fmt.Sprintf("Delete trail #%d?", number)
	if title != "" {
		prompt = fmt.Sprintf("Delete trail #%d (%s)?", number, title)
	}
	confirmed := false
	form := NewAccessibleForm(
		huh.NewGroup(huh.NewConfirm().Title(prompt).Value(&confirmed)),
	)
	if err := form.RunWithContext(ctx); err != nil {
		// A user abort (Esc) or context cancel (Ctrl+C) is a clean cancel, not
		// an error — mirror confirmDoctorFix / uiform.PromptYN.
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, fmt.Errorf("trail deletion prompt: %w", err)
	}
	if !confirmed {
		fmt.Fprintln(w, "Trail deletion cancelled.")
		return false, nil
	}
	return true, nil
}

// defaultBaseBranch is the fallback base branch name when it cannot be determined.
const defaultBaseBranch = "main"

// masterBaseBranch is the secondary fallback for repos still using "master"
// (pre-git-2.28 defaults, forks of older projects, etc.). Extracted as a
// constant so goconst stays quiet across the several call sites in the cli
// package.
const masterBaseBranch = "master"

func formatValidStatuses() string {
	statuses := trail.ValidStatuses()
	names := make([]string, len(statuses))
	for i, s := range statuses {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

var runTrailCreateForm = func(form *huh.Form) error { return form.Run() }

// runTrailCreateInteractive runs the interactive form for trail creation.
// Prompts for title, body, branch (derived from title, unless branchless), and status.
func runTrailCreateInteractive(title, body, branch, statusStr *string, noBranch bool) error {
	// Step 1: Title and body
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Trail title").
				Placeholder("What are you working on?").
				Value(title),
			huh.NewText().
				Title("Body (optional)").
				Value(body),
		),
	)
	if err := runTrailCreateForm(form); err != nil {
		return fmt.Errorf("form cancelled: %w", err)
	}
	*title = strings.TrimSpace(*title)
	if *title == "" {
		return errors.New("trail title is required")
	}

	// Build status options, excluding done/closed
	var statusOptions []huh.Option[string]
	for _, s := range trail.ValidStatuses() {
		if s == trail.StatusMerged || s == trail.StatusClosed {
			continue
		}
		statusOptions = append(statusOptions, huh.NewOption(string(s), string(s)))
	}
	if *statusStr == "" {
		*statusStr = string(trail.StatusOpen)
	}

	if noBranch {
		*branch = ""
		form = NewAccessibleForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Status").
					Options(statusOptions...).
					Value(statusStr),
			),
		)
		if err := runTrailCreateForm(form); err != nil {
			return fmt.Errorf("form cancelled: %w", err)
		}
		return nil
	}

	// Step 2: Branch (derived from title) and status
	suggested := slugifyTitle(*title)
	*branch = suggested

	form = NewAccessibleForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Branch name").
				Placeholder(suggested).
				Value(branch),
			huh.NewSelect[string]().
				Title("Status").
				Options(statusOptions...).
				Value(statusStr),
		),
	)
	if err := runTrailCreateForm(form); err != nil {
		return fmt.Errorf("form cancelled: %w", err)
	}
	*branch = strings.TrimSpace(*branch)
	if *branch == "" {
		*branch = suggested
	}
	return nil
}

// findTrailByBranch looks up a trail by branch name via the list API.
func findTrailBySelector(ctx context.Context, client *api.Client, forge, owner, repo, selector string) (*api.TrailResource, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil //nolint:nilnil // empty selector means not found for this helper
	}
	if n, ok := parseTrailNumberSelector(selector); ok {
		found, err := findTrailByNumber(ctx, client, forge, owner, repo, n)
		if err != nil || found != nil {
			return found, err
		}
	}
	return findTrail(ctx, client, forge, owner, repo, func(t api.TrailResource) bool {
		return t.ID == selector || t.Branch == selector
	})
}

func parseTrailNumberSelector(selector string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(selector))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func findTrailByBranch(ctx context.Context, client *api.Client, forge, owner, repo, branch string) (*api.TrailResource, error) {
	return findTrail(ctx, client, forge, owner, repo, func(t api.TrailResource) bool {
		return t.Branch == branch
	})
}

// findTrailByNumber looks up a trail by numeric identifier via the list API.
func findTrailByNumber(ctx context.Context, client *api.Client, forge, owner, repo string, number int) (*api.TrailResource, error) {
	return findTrail(ctx, client, forge, owner, repo, func(t api.TrailResource) bool {
		return t.Number == number
	})
}

func findTrail(ctx context.Context, client *api.Client, forge, owner, repo string, match func(api.TrailResource) bool) (*api.TrailResource, error) {
	// The list endpoint paginates; walk bounded pages so branch/number/id lookups do
	// not silently miss older trails beyond the first server-max page.
	offset := 0
	previousPageSignature := ""
	for range trailFindMaxPages {
		resp, err := client.Get(ctx, trailsBasePath(forge, owner, repo)+trailListQueryWithOffset(nil, "", trailListServerMaxLimit, offset))
		if err != nil {
			return nil, fmt.Errorf("list trails: %w", err)
		}

		var listResp api.TrailListResponse
		decodeErr := func() error {
			defer resp.Body.Close()
			if err := checkTrailResponse(resp); err != nil {
				return err
			}
			if err := api.DecodeJSON(resp, &listResp); err != nil {
				return fmt.Errorf("decode trail list: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return nil, decodeErr
		}

		for i := range listResp.Trails {
			if match(listResp.Trails[i]) {
				return &listResp.Trails[i], nil
			}
		}

		pageLen := len(listResp.Trails)
		if pageLen == 0 || pageLen < trailListServerMaxLimit {
			break
		}
		if listResp.Total == 0 {
			pageSignature := trailListPageSignature(listResp.Trails)
			if pageSignature != "" && pageSignature == previousPageSignature {
				break
			}
			previousPageSignature = pageSignature
		}
		offset += pageLen
		if listResp.Total > 0 && offset >= listResp.Total {
			break
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means "not found" — callers check both
}

func trailListPageSignature(trails []api.TrailResource) string {
	if len(trails) == 0 {
		return ""
	}
	first := trails[0]
	last := trails[len(trails)-1]
	return fmt.Sprintf("%s/%d/%s:%s/%d/%s", first.ID, first.Number, first.Branch, last.ID, last.Number, last.Branch)
}

// trailsBasePath returns the API path prefix for trails endpoints
// (e.g., "/api/v1/trails/gh/org/repo").
func trailsBasePath(forge, owner, repo string) string {
	return fmt.Sprintf("/api/v1/trails/%s/%s/%s", forge, owner, repo)
}

// trailNumberPath returns the single-trail API path keyed by integer trail
// number (e.g. "/api/v1/trails/gh/acme/repo/575"). The server validates an
// integer here and rejects the trail UUID, so callers must pass Number, not ID.
func trailNumberPath(forge, owner, repo string, number int) string {
	return trailsBasePath(forge, owner, repo) + "/" + strconv.Itoa(number)
}

// resolveTrailRemote resolves the origin remote and ensures the forge is
// known to the trails API. Without this guard, an unmapped host (e.g.
// gitlab.com, or a misconfigured entire:// URL with no forge prefix)
// produces a malformed `/api/v1/trails//owner/repo` path that the server
// rejects with an opaque error instead of a clear "unsupported forge" one.
func resolveTrailRemote(ctx context.Context) (forge, owner, repo string, err error) {
	forge, owner, repo, err = gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return "", "", "", fmt.Errorf("failed to resolve repository: %w", err)
	}
	if forge == "" {
		return "", "", "", errors.New("origin remote is not on a forge supported by Entire trails (supported: github.com)")
	}
	return forge, owner, repo, nil
}

// checkTrailResponse checks the API response and returns user-friendly errors.
// For auth failures, it appends a hint to re-authenticate while preserving the server's error message.
func checkTrailResponse(resp *http.Response) error {
	if err := api.CheckResponse(resp); err != nil {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w — run 'entire login' to re-authenticate", err)
		}
		return fmt.Errorf("trail API: %w", err)
	}
	return nil
}

// resolveCreateBranch picks the branch a non-interactive `trail create` targets.
// An explicit --branch always wins. Otherwise, on a feature branch it uses the
// checked-out branch; when the checked-out branch IS the base (the trail's
// target/default branch) — or HEAD is detached — it derives a new branch from
// the title when one was given (starting fresh work), else falls back to current.
// Comparing against the already-resolved base keeps this consistent with how
// `base` itself was detected (avoids a second, divergent default-branch lookup).
func resolveCreateBranch(branchFlag, currentBranch, base, title string, titleProvided bool) string {
	if branchFlag != "" {
		return branchFlag
	}
	if titleProvided && (currentBranch == base || currentBranch == "") {
		return slugifyTitle(title)
	}
	return currentBranch
}

// slugifyTitle converts a title string into a branch-friendly slug.
// Example: "Add user authentication" -> "add-user-authentication"
func slugifyTitle(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	// Replace spaces and underscores with hyphens
	s = strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	// Remove anything that's not alphanumeric, hyphen, or slash
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '/' {
			b.WriteRune(r)
			prevHyphen = false
		} else if r == '-' && !prevHyphen {
			b.WriteRune('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// branchNeedsCreation checks if a branch exists locally.
func branchNeedsCreation(repo *git.Repository, branchName string) bool {
	_, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	return err != nil
}

// createBranch creates a new local branch pointing at HEAD without checking it out.
func createBranch(repo *git.Repository, branchName string) error {
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), head.Hash())
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to create branch ref: %w", err)
	}
	return nil
}

func cleanupCreatedTrailBranch(repo *git.Repository, branchName string, localCreated, remotePushed bool, errW io.Writer) {
	localRemoved := !localCreated
	if localCreated {
		branchRef := plumbing.NewBranchReferenceName(branchName)
		if head, err := repo.Head(); err == nil && head.Name() == branchRef {
			fmt.Fprintf(errW, "Warning: not deleting local branch %s after trail creation failed because it is checked out; switch branches and run 'git branch -D %s' if you do not need it\n", branchName, branchName)
		} else if err := repo.Storer.RemoveReference(branchRef); err != nil {
			fmt.Fprintf(errW, "Warning: failed to delete local branch %s after trail creation failed: %v; run 'git branch -D %s' if you do not need it\n", branchName, err, branchName)
		} else {
			localRemoved = true
		}
	}
	if remotePushed {
		if !localRemoved {
			fmt.Fprintf(errW, "Warning: not deleting remote branch %s after trail creation failed because local cleanup did not complete; run 'git push origin --delete %s' if you do not need it\n", branchName, branchName)
			return
		}
		if err := deleteBranchFromOrigin(branchName); err != nil {
			fmt.Fprintf(errW, "Warning: failed to delete remote branch %s after trail creation failed: %v\n", branchName, err)
		}
	}
}

func fetchBranchFromOrigin(branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ValidateBranchName(ctx, branchName); err != nil {
		return err
	}
	refspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branchName, branchName)
	cmd := exec.CommandContext(ctx, "git", "fetch", "--no-tags", "origin", refspec)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// pushBranchToOrigin pushes a branch to the origin remote.
func pushBranchToOrigin(branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "push", "--no-verify", "-u", "origin", branchName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// branchExistsOnOrigin reports whether origin already has a branch with the
// given name, so callers can avoid treating a pre-existing remote branch as one
// they created.
func branchExistsOnOrigin(branchName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "origin", branchName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return strings.TrimSpace(string(output)) != "", nil
}

func deleteBranchFromOrigin(branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "push", "--no-verify", "origin", "--delete", branchName)
	if output, err := cmd.CombinedOutput(); err != nil {
		outputText := strings.TrimSpace(string(output))
		if strings.Contains(outputText, "remote ref does not exist") {
			return nil
		}
		return fmt.Errorf("%s: %w", outputText, err)
	}
	return nil
}
