package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	change "github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

const (
	defaultChangeListLimit  = 10
	changeListAuthorMe      = "me"
	defaultChangeListStatus = string(change.StatusOpen)
	// changeListStatusAny disables the status filter; user-facing value for --status.
	changeListStatusAny = "any"
	// changeListServerMaxLimit is entire-api's maximum pageSize.
	changeListServerMaxLimit = 100
	// changeFindMaxPages bounds a branch/ID lookup at a 2,000-change search budget.
	changeFindMaxPages = 20
)

func changeContextBlurb() string {
	return "A change ties together the context for a branch. Use `entire change` to view, create, update, or watch it; use `entire change finding` to manage agent findings."
}

func newChangeCmd() *cobra.Command {
	var insecureHTTPAuth bool
	var repoOverride string

	cmd := &cobra.Command{
		Use:    "change",
		Short:  "Manage changes for your branches",
		Hidden: true,
		// Hidden from root help while the surface matures, but advertised to
		// coding agents through `entire agent-help` — only when changes are
		// enabled for the repo, so we never point agents at changes they can't use.
		Annotations: map[string]string{
			agentHelpAnnotation:               agentHelpAnnotationEnabled,
			agentHelpRequiresTrailsAnnotation: agentHelpAnnotationEnabled,
		},
		Args: cobra.NoArgs,
		Long: changeContextBlurb(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().BoolVar(&insecureHTTPAuth, "insecure-http-auth", false,
		"Allow API calls over plain HTTP (insecure, for local development only)")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}

	// Target an explicit repository instead of the origin remote, so the change
	// commands can drive a repo the caller is not checked out in (e.g. a GUI
	// backend). Commands that mutate the local clone (create, checkout, finding
	// apply) reject it via ensureNoChangeRepoOverride.
	cmd.PersistentFlags().StringVar(&repoOverride, "repo", "",
		"Target repository as forge/owner/repo (e.g. gh/acme/app) or a clone URL; defaults to the origin remote")

	cmd.AddCommand(newChangeShowCmd())
	cmd.AddCommand(newChangeListCmd())
	cmd.AddCommand(newChangeCreateCmd())
	cmd.AddCommand(newChangeUpdateCmd())
	cmd.AddCommand(newChangeCheckoutCmd())
	cmd.AddCommand(newChangeResumeCmd())
	cmd.AddCommand(newChangeDeleteCmd())
	cmd.AddCommand(newChangeFindingCmd())
	cmd.AddCommand(newChangeWatchCmd())
	cmd.AddCommand(newChangeApproveCmd())
	cmd.AddCommand(newChangeRequestChangesCmd())
	cmd.AddCommand(newChangeApprovalsCmd())
	cmd.AddCommand(newChangeCommentCmd())

	return cmd
}

// changeInsecureHTTP reads the persistent --insecure-http-auth flag from the change root command.
func changeInsecureHTTP(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("insecure-http-auth") //nolint:errcheck // flag is always registered
	return v
}

// changeRepoFlag reads the persistent --repo flag from the change command tree.
// It is always registered on the change root, so a missing flag (empty string)
// just means "derive from origin".
func changeRepoFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("repo") //nolint:errcheck // flag is always registered on the change root
	return strings.TrimSpace(v)
}

// changeBranchFlag reads an optional --branch flag. Only some subcommands
// register it; on the rest GetString errors and we treat it as unset.
func changeBranchFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("branch") //nolint:errcheck // absent on commands that don't register --branch
	return strings.TrimSpace(v)
}

// ensureNoChangeRepoOverride rejects --repo for commands that operate on the
// local clone and so cannot target an arbitrary repository.
func ensureNoChangeRepoOverride(cmd *cobra.Command, op string) error {
	if changeRepoFlag(cmd) != "" {
		return fmt.Errorf("--repo is not supported for %q because it operates on the local clone", op)
	}
	return nil
}

// ensureChangeRepoHasTarget requires an explicit branch or selector when --repo
// targets another repo; otherwise the command would resolve the local branch
// against the wrong repo. hint names the acceptable targets.
func ensureChangeRepoHasTarget(cmd *cobra.Command, hasTarget bool, hint string) error {
	if changeRepoFlag(cmd) != "" && !hasTarget {
		return fmt.Errorf("--repo requires an explicit target: %s", hint)
	}
	return nil
}

// changeListOptions are the inputs to runChangeListAll. Keeping them on a
// struct avoids a long positional argument list at the two call sites.
type changeListOptions struct {
	Author       string
	Status       string
	JSON         bool
	Limit        int
	InsecureHTTP bool
	// Repo is an optional --repo override (forge/owner/repo or a clone URL);
	// empty means derive the repo from the origin remote.
	Repo string
}

// changeShowOptions are the inputs to runChangeShow. Keeping them on a struct
// avoids a long positional argument list, matching changeListOptions.
type changeShowOptions struct {
	// Selector picks the change by number, id, or branch. Empty means "the change
	// for Branch", and an empty Branch in turn means the current branch.
	Selector string
	// Branch shows the change for this branch instead of the current branch.
	Branch string
	// Repo is an optional --repo override (forge/owner/repo or a clone URL);
	// empty means derive the repo from the origin remote.
	Repo         string
	JSON         bool
	InsecureHTTP bool
}

func newChangeShowCmd() *cobra.Command {
	var opts changeShowOptions
	// branchFlag is bound only so cobra has somewhere to write --branch; RunE
	// reads it back through changeBranchFlag (which trims) along with every other
	// field, so a value reaches opts exactly one way.
	var branchFlag string
	cmd := &cobra.Command{
		Use:   "show [<change>]",
		Short: "Show a change",
		Long: `Show a change.

If <change> is omitted, shows the change for the current branch (or --branch).
Otherwise, <change> may be a change number, id, or branch in the target repo.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			if selector != "" && changeBranchFlag(cmd) != "" {
				return errors.New("pass a change selector or --branch, not both")
			}
			if err := ensureChangeRepoHasTarget(cmd, selector != "" || changeBranchFlag(cmd) != "", "pass a change selector or --branch"); err != nil {
				return err
			}
			opts.Selector = selector
			opts.Branch = changeBranchFlag(cmd)
			opts.Repo = changeRepoFlag(cmd)
			opts.InsecureHTTP = changeInsecureHTTP(cmd)
			return runChangeShow(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Show the change for this branch instead of the current branch; cannot be combined with a change selector")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output the change as JSON (same object shape as one entry of 'change list --json')")
	return cmd
}

// runChangeShow shows one change, defaulting to the current branch's change.
func runChangeShow(ctx context.Context, w, errW io.Writer, opts changeShowOptions) error {
	return runAuthenticatedChangeAPI(ctx, errW, opts.InsecureHTTP, opts.Repo, func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveChangeRepoOrRemote(ctx, opts.Repo)
		if err != nil {
			return err
		}
		return runChangeShowWithClient(ctx, w, errW, client, forge, owner, repo, opts)
	})
}

// runChangeShowWithClient renders one change once the repo and API client are
// resolved. Warnings go to errW, so --json keeps stdout parseable even when the
// best-effort description fetch fails.
func runChangeShowWithClient(ctx context.Context, w, errW io.Writer, client *api.Client, forge, owner, repo string, opts changeShowOptions) error {
	found, err := resolveChangeBySelector(ctx, client, forge, owner, repo, opts.Selector, opts.Branch)
	if err != nil {
		return err
	}

	// Enrich the list result with the detail endpoint, which carries the
	// rendered description (change.body_document.text_snapshot) the list
	// omits, and surface a browser URL. The detail fetch is best-effort:
	// the core metadata already came from the list, so a detail failure
	// falls back to the list body with a warning rather than failing.
	m := found.ToMetadata()
	m.URL = changeDisplayURL(*found, forge, owner, repo)
	// Seed the description from the list body so a failed (or skipped)
	// detail fetch still shows something; a successful detail fetch
	// supersedes it with the richer body_document text below.
	bodyText := found.Body
	descriptionLoaded := strings.TrimSpace(found.Body) != ""
	switch {
	case found.BodyDocument != nil:
		// A numeric selector already resolved through the detail route, so the
		// description is in hand — re-requesting the same URL would double the
		// round trips for every `change show <number>`. Same precedence as the
		// fetch below: authoritative, but only supersedes a non-empty snapshot.
		descriptionLoaded = true
		if snapshot := strings.TrimSpace(found.BodyDocument.TextSnapshot); snapshot != "" {
			bodyText = snapshot
		}
	case found.Number > 0:
		if bt, _, derr := fetchChangeDescription(ctx, client, forge, owner, repo, found.Number); derr == nil {
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
			fmt.Fprintf(errW, "Warning: could not load change description: %v\n", derr)
		}
	}
	// The list body is the weaker source; carry the resolved description on the
	// metadata so JSON callers read the same text the human view renders.
	m.Body = bodyText

	if opts.JSON {
		// Emit the raw body — never the "no description" placeholder, which is
		// display text, not data. A single object mirrors one entry of
		// `change list --json` so both can feed the same parser.
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(m); err != nil {
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
		return nil
	}

	printChangeDetails(w, m, m.URL, changeDescriptionForDisplay(bodyText, descriptionLoaded))
	return nil
}

// resolveChangeBySelector resolves a change by an optional selector (change
// number, id, or branch). An empty selector falls back to the current branch's
// change. It returns an actionable error (never a nil change with a nil error)
// when nothing matches, so callers can rely on a non-nil result.
func resolveChangeBySelector(ctx context.Context, client *api.Client, forge, owner, repo, selector, branchOverride string) (*api.ChangeResource, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		branch, err := resolveChangeBranch(ctx, branchOverride)
		if err != nil {
			return nil, fmt.Errorf("no change selector given and current branch is unknown: %w\nhint: run 'entire change list --status any' or pass a change number, id, or branch", err)
		}
		found, err := findChangeByBranch(ctx, client, forge, owner, repo, branch)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, fmt.Errorf("no change found for current branch %q\nhint: run 'entire change create' or 'entire change list --status any'", branch)
		}
		return found, nil
	}
	found, err := findChangeBySelector(ctx, client, forge, owner, repo, selector)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("no change %q found in %s/%s/%s (run 'entire change list --status any')", selector, forge, owner, repo)
	}
	return found, nil
}

func printChangeDetails(w io.Writer, m *change.Metadata, webURL, bodyText string) {
	// Color the same fields as the list view (STATUS/PHASE/AUTHOR); everything
	// else stays plain. Values are pre-colored, so alignment is unaffected.
	styles := newStatusStyles(w)
	status := string(m.Status)
	if style, ok := changeStatusColor(styles, m.Status); ok {
		status = styles.render(style, status)
	}
	author := m.AuthorLogin()
	if styles.colorEnabled && author != "" {
		author = styles.render(styles.cyan, author)
	}

	// Field labels and the title line render yellow (matching the list header).
	label := func(s string) string { return styles.render(styles.yellow, s) }

	fmt.Fprintf(w, "%s\n", styles.render(styles.yellow, "Change: "+m.Title))
	if m.Number > 0 {
		fmt.Fprintf(w, "  %s%d\n", label("Number:  "), m.Number)
	}
	if !m.TrailID.IsEmpty() {
		fmt.Fprintf(w, "  %s%s\n", label("ID:      "), m.TrailID)
	}
	fmt.Fprintf(w, "  %s%s\n", label("Branch:  "), m.Branch)
	fmt.Fprintf(w, "  %s%s\n", label("Base:    "), m.Base)
	fmt.Fprintf(w, "  %s%s\n", label("Status:  "), status)
	fmt.Fprintf(w, "  %s%s\n", label("Author:  "), author)
	if strings.TrimSpace(m.Phase) != "" {
		phase := changePhaseDisplay(m.Phase)
		if styles.colorEnabled {
			phase = styles.render(styles.yellow, phase)
		}
		fmt.Fprintf(w, "  %s%s\n", label("Phase:   "), phase)
	}
	if webURL != "" {
		fmt.Fprintf(w, "  %s%s\n", label("URL:     "), webURL)
	}
	if len(m.Labels) > 0 {
		fmt.Fprintf(w, "  %s%s\n", label("Labels:  "), strings.Join(m.Labels, ", "))
	}
	if len(m.Assignees) > 0 {
		fmt.Fprintf(w, "  %s%s\n", label("Assignees: "), strings.Join(m.Assignees, ", "))
	}
	if strings.TrimSpace(string(m.Type)) != "" {
		fmt.Fprintf(w, "  %s%s\n", label("Type:      "), m.Type)
	}
	if p := strings.TrimSpace(string(m.Priority)); p != "" && p != string(change.PriorityNone) {
		fmt.Fprintf(w, "  %s%s\n", label("Priority:  "), m.Priority)
	}
	if len(m.Reviewers) > 0 {
		parts := make([]string, 0, len(m.Reviewers))
		for _, r := range m.Reviewers {
			parts = append(parts, fmt.Sprintf("%s (%s)", r.Login, r.Status))
		}
		fmt.Fprintf(w, "  %s%s\n", label("Reviewers: "), strings.Join(parts, ", "))
	}
	fmt.Fprintf(w, "  %s%s\n", label("Created: "), m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(w, "  %s%s\n", label("Updated: "), m.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
	if strings.TrimSpace(bodyText) != "" {
		fmt.Fprintf(w, "\n%s\n%s\n", label("Description:"), bodyText)
	}
}

// noChangeDescription is shown by `change show` when a change's description loaded
// successfully but is empty — distinguishing "no description yet" from a load
// failure (which warns and renders nothing).
const noChangeDescription = "-- no description provided --"

// changeDescriptionForDisplay returns the text `change show` renders for the
// description: the body when present; the placeholder when it loaded but is
// empty; or "" when it couldn't be loaded (the caller has already warned).
func changeDescriptionForDisplay(bodyText string, loaded bool) string {
	if strings.TrimSpace(bodyText) != "" {
		return bodyText
	}
	if loaded {
		return noChangeDescription
	}
	return ""
}

// printCreatedChange reports a newly created change, including its browser URL
// (the same URL `change show` surfaces) when one is available.
func printCreatedChange(w io.Writer, t api.ChangeResource, forge, owner, repo string) {
	if t.Branch == "" {
		fmt.Fprintf(w, "Created change %q (ID: %s)\n", t.Title, t.ID)
	} else {
		fmt.Fprintf(w, "Created change %q for branch %s (ID: %s)\n", t.Title, t.Branch, t.ID)
	}
	if url := changeDisplayURL(t, forge, owner, repo); url != "" {
		fmt.Fprintf(w, "  URL: %s\n", url)
	}
}

// changeDisplayURL returns the change's browser URL. It prefers the canonical URL
// the server now returns, so the CLI tracks any route change without being
// updated in lockstep, and falls back to a locally constructed URL only for
// older servers that omit the field.
func changeDisplayURL(t api.ChangeResource, forge, owner, repo string) string {
	if strings.TrimSpace(t.URL) != "" {
		return t.URL
	}
	if t.Number > 0 {
		return changeWebURL(api.BaseURL(), forge, owner, repo, t.Number)
	}
	return ""
}

// changeWebURL builds a fallback browser URL for a change used only when the
// server does not supply one (older servers):
// <web-origin>/<forge>/<owner>/<repo>/changes/<number>. In production the web app
// is served from the same origin as the data API, so the API base URL doubles
// as the web origin. A split local-dev setup (API and frontend on different
// ports) would point this at the API port rather than the dev frontend.
func changeWebURL(base, forge, owner, repo string, number int) string {
	return strings.TrimRight(base, "/") + "/" + forge + "/" + owner + "/" + repo + "/changes/" + strconv.Itoa(number)
}

// fetchChangeDescription fetches a change's rendered description text
// (`change.body_document.text_snapshot`) and its etag, which the list endpoint
// omits, by integer number. It returns only the description and etag — the
// list result already supplies the metadata — and decodes only the fields it
// needs, so it is unaffected by the shape of sibling fields like
// `checkpoints`/`thread`.
func fetchChangeDescription(ctx context.Context, client *api.Client, forge, owner, repo string, number int) (string, string, error) {
	resp, err := client.Get(ctx, changeNumberPath(forge, owner, repo, number))
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch change detail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		return "", "", err
	}
	detail, err := decodeChangeResource(resp)
	if err != nil {
		return "", "", fmt.Errorf("failed to decode change detail: %w", err)
	}
	if detail.BodyDocument == nil {
		return "", "", nil
	}
	return strings.TrimSpace(detail.BodyDocument.TextSnapshot), detail.BodyDocument.ETag, nil
}

// decodeChangeResource decodes entire-api's direct detail resource.
func decodeChangeResource(resp *http.Response) (api.ChangeResource, error) {
	var resource api.ChangeResource
	if err := api.DecodeJSON(resp, &resource); err != nil {
		return api.ChangeResource{}, fmt.Errorf("decode change resource: %w", err)
	}
	return resource, nil
}

func newChangeListCmd() *cobra.Command {
	var opts changeListOptions

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent changes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.InsecureHTTP = changeInsecureHTTP(cmd)
			opts.Repo = changeRepoFlag(cmd)
			return runChangeListAll(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.Author, "author", "",
		"Filter by author login (case-insensitive); use '"+changeListAuthorMe+"' for yourself (requires gh CLI); omit for any author")
	cmd.Flags().StringVar(&opts.Status, "status", defaultChangeListStatus,
		"Filter by comma-separated status(es): "+formatValidStatuses()+"; use '"+changeListStatusAny+"' for all statuses")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON (respects --author, --status, and --limit)")
	cmd.Flags().IntVarP(&opts.Limit, "limit", "n", defaultChangeListLimit, "Maximum number of changes to show")

	return cmd
}

func runChangeListAll(ctx context.Context, w, errW io.Writer, opts changeListOptions) error {
	statusFilters, err := validateChangeListOptions(opts)
	if err != nil {
		return err
	}
	return runAuthenticatedChangeAPI(ctx, errW, opts.InsecureHTTP, opts.Repo, func(ctx context.Context, client *api.Client) error {
		return runChangeListAllWithClient(ctx, w, client, opts, statusFilters)
	})
}

func validateChangeListOptions(opts changeListOptions) ([]change.Status, error) {
	if opts.Limit <= 0 {
		return nil, errors.New("limit must be greater than 0")
	}
	return parseChangeStatusFilter(opts.Status)
}

func runChangeListAllWithClient(ctx context.Context, w io.Writer, client *api.Client, opts changeListOptions, statusFilters []change.Status) error {
	authorFilter := opts.Author
	currentUserLogin := ""
	if authorFilter == changeListAuthorMe {
		login, err := fetchCurrentUserLogin(ctx, execRunner{})
		if err != nil {
			return err
		}
		currentUserLogin = login
		authorFilter = login
	}

	forge, owner, repo, err := resolveChangeRepoOrRemote(ctx, opts.Repo)
	if err != nil {
		return err
	}

	// entire-api uses opaque cursor pagination and a 100-row page cap. Walk as
	// many pages as needed for --limit. Author login is filtered client-side:
	// the new backend's author query is account-ID based, while the CLI's public
	// flag has always accepted GitHub logins.
	resources, totalMatched, err := listChangeResources(ctx, client, forge, owner, repo, statusFilters, authorFilter, opts.Limit)
	if err != nil {
		return err
	}

	// Convert to metadata for display, attaching the browser URL (entire-api does
	// not compose the web-app URL on this lower-level surface).
	changes := make([]*change.Metadata, 0, len(resources))
	for i := range resources {
		m := resources[i].ToMetadata()
		m.URL = changeDisplayURL(resources[i], forge, owner, repo)
		changes = append(changes, m)
	}

	if opts.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(changes); err != nil {
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
		return nil
	}

	if len(changes) == 0 {
		printChangeListEmpty(w, authorFilter, statusFilters)
		return nil
	}

	printChangeList(w, changes, changeListDisplayOptions{
		RequestedAuthor: authorFilter,
		CurrentUser:     currentUserLogin,
		StatusFilters:   statusFilters,
		TotalMatched:    totalMatched,
	})

	return nil
}

func listChangeResources(ctx context.Context, client *api.Client, forge, owner, repo string, statuses []change.Status, author string, limit int) ([]api.ChangeResource, int, error) {
	if limit <= 0 {
		return nil, 0, errors.New("limit must be greater than 0")
	}
	items := make([]api.ChangeResource, 0, min(limit, changeListServerMaxLimit))
	pageToken := ""
	seenTokens := map[string]bool{}
	totalMatched := 0
	for {
		pageSize := changeListServerMaxLimit
		if author == "" && limit-len(items) < pageSize {
			pageSize = limit - len(items)
		}
		resp, err := client.Get(ctx, changesBasePath(forge, owner, repo)+changeListPageQuery(statuses, pageSize, pageToken))
		if err != nil {
			return nil, 0, fmt.Errorf("failed to list changes: %w", err)
		}
		var page api.ChangeListResponse
		decodeErr := func() error {
			defer resp.Body.Close()
			if err := checkChangeResponse(resp); err != nil {
				return err
			}
			if err := api.DecodeJSON(resp, &page); err != nil {
				return fmt.Errorf("failed to decode change list: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return nil, 0, decodeErr
		}

		for _, resource := range page.Changes {
			if author != "" {
				login := ""
				if resource.Author != nil && resource.Author.Login != nil {
					login = *resource.Author.Login
				}
				if !strings.EqualFold(login, author) {
					continue
				}
			}
			totalMatched++
			if len(items) < limit {
				items = append(items, resource)
			}
			if author != "" && len(items) >= limit {
				break
			}
		}
		// entire-api's author filter accepts account IDs while the CLI accepts
		// logins, so this path filters locally. Stop as soon as --limit is
		// satisfied rather than walking the repository's complete change history.
		if author != "" && len(items) >= limit {
			break
		}
		if author == "" {
			totalMatched = page.Total
			if totalMatched < len(items) {
				totalMatched = len(items)
			}
			if len(items) >= limit {
				break
			}
		}
		if page.NextPageToken == nil || strings.TrimSpace(*page.NextPageToken) == "" {
			break
		}
		pageToken = strings.TrimSpace(*page.NextPageToken)
		if seenTokens[pageToken] {
			return nil, 0, fmt.Errorf("change list pagination repeated page token %q", pageToken)
		}
		seenTokens[pageToken] = true
	}
	return items, totalMatched, nil
}

// changeListPageQuery builds entire-api's cursor-paginated list query. Empty
// statuses (--status any) omit the filter. Author is intentionally absent: the
// CLI accepts a login while this API's author filter accepts account ULIDs.
func changeListPageQuery(statusFilters []change.Status, pageSize int, pageToken string) string {
	q := url.Values{}
	if len(statusFilters) > 0 {
		parts := make([]string, len(statusFilters))
		for i, status := range statusFilters {
			parts[i] = string(status)
		}
		q.Set("status", strings.Join(parts, ","))
	}
	if pageSize <= 0 || pageSize > changeListServerMaxLimit {
		pageSize = changeListServerMaxLimit
	}
	q.Set("pageSize", strconv.Itoa(pageSize))
	if strings.TrimSpace(pageToken) != "" {
		q.Set("pageToken", strings.TrimSpace(pageToken))
	}
	return "?" + q.Encode()
}

// printChangeListEmpty renders the empty-state message. It names the active
// status filter so a bare `entire change list` (which defaults to open)
// doesn't read as "this repo has no changes" when changes exist in other
// statuses. statusFilters is empty when the user passed --status any.
func printChangeListEmpty(w io.Writer, authorFilter string, statusFilters []change.Status) {
	desc := "No changes found"
	if len(statusFilters) > 0 {
		desc = fmt.Sprintf("No %s changes found", changeStatusListDisplay(statusFilters))
	}
	if authorFilter != "" {
		desc += " for " + authorFilter
	}
	fmt.Fprintf(w, "%s.\n", desc)

	if len(statusFilters) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Use --status any to see changes in other statuses.")
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  entire change create   Create a change for the current branch")
	fmt.Fprintln(w, "  entire change list     List recent changes")
	fmt.Fprintln(w, "  entire change update   Update change metadata")
}

func parseChangeStatusFilter(filter string) ([]change.Status, error) {
	if filter == "" || filter == changeListStatusAny {
		return nil, nil
	}

	parts := strings.Split(filter, ",")
	statuses := make([]change.Status, 0, len(parts))
	seen := make(map[change.Status]bool, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("invalid status filter %q: empty status", filter)
		}
		status := change.Status(name)
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
		return "", fmt.Errorf("resolve --author %s via gh CLI: %w\nhint: pass --author <login> explicitly if gh is unavailable", changeListAuthorMe, err)
	}
	if login == "" {
		return "", errors.New("resolve --author me: gh returned an empty login")
	}
	return login, nil
}

type changeListDisplayOptions struct {
	RequestedAuthor string
	CurrentUser     string
	StatusFilters   []change.Status
	// TotalMatched is the best available match count. It is exact when the
	// backend returns a filtered total; login-filtered entire-api lists stop at
	// --limit and therefore report only the displayed count.
	TotalMatched int
}

func printChangeList(w io.Writer, changes []*change.Metadata, opts changeListDisplayOptions) {
	showAuthor := opts.RequestedAuthor == ""
	// Show the status column unless exactly one status is filtered — that
	// status is already named in the header.
	showStatus := len(opts.StatusFilters) != 1
	printChangeListHeader(w, opts, len(changes))
	fmt.Fprintln(w)
	printChangeRows(w, changes, showAuthor, showStatus)
}

func printChangeListHeader(w io.Writer, opts changeListDisplayOptions, count int) {
	countStr := changeCountDisplay(count, opts.TotalMatched)
	// The noun refers to the full match set, so pluralize by the total when
	// the page is truncated ("1/2 changes", not "1/2 change").
	nounCount := count
	if opts.TotalMatched > count {
		nounCount = opts.TotalMatched
	}
	if opts.RequestedAuthor == "" {
		if len(opts.StatusFilters) == 0 {
			fmt.Fprintf(w, "  Recent %s · %s\n", pluralize("change", nounCount), countStr)
			return
		}
		fmt.Fprintf(w, "  %s · %s %s\n", changeStatusListTitle(opts.StatusFilters), countStr, pluralize("change", nounCount))
		return
	}

	label := opts.RequestedAuthor
	// When --author me resolves to the same login the server already returned
	// for the change, render "Your changes (login)" so identity drift between
	// gh and Entire is visible at a glance.
	if opts.CurrentUser != "" && strings.EqualFold(opts.RequestedAuthor, opts.CurrentUser) {
		label = fmt.Sprintf("Your changes (%s)", opts.CurrentUser)
	}
	if len(opts.StatusFilters) == 0 {
		fmt.Fprintf(w, "  %s · %s\n", label, countStr)
		return
	}
	fmt.Fprintf(w, "  %s · %s %s\n", label, countStr, changeStatusListDisplay(opts.StatusFilters))
}

func printChangeRows(w io.Writer, changes []*change.Metadata, showAuthor, showStatus bool) {
	styles := newStatusStyles(w)
	showPhase := changeListHasPhase(changes)
	showURL := changeListHasURL(changes)

	// The leading two-space indent is folded into the first column so the shared
	// table renderer (columnWidths/writeTableRow) reproduces the list's layout.
	headers := []string{"  NUM", "BRANCH", "TITLE"}
	if showStatus {
		headers = append(headers, "STATUS")
	}
	if showPhase {
		headers = append(headers, "PHASE")
	}
	if showAuthor {
		headers = append(headers, "AUTHOR")
	}
	headers = append(headers, "UPDATED")
	if showURL {
		headers = append(headers, "URL")
	}

	rows := make([][]string, len(changes))
	for i, t := range changes {
		number := "-"
		if t.Number > 0 {
			number = strconv.Itoa(t.Number)
		}
		title := truncateOneLine(t.Title, 60)
		if title == "" {
			title = "(untitled)"
		}
		fields := []string{"  " + number, t.Branch, title}
		// Cells are pre-colored here; columnWidths/writeTableRow measure width
		// with lipgloss.Width (ANSI-agnostic), so color never shifts columns.
		if showStatus {
			status := changeStatusDisplay(t.Status)
			if style, ok := changeStatusColor(styles, t.Status); ok {
				status = styles.render(style, status)
			}
			fields = append(fields, status)
		}
		if showPhase {
			phase := changePhaseDisplay(t.Phase)
			if styles.colorEnabled && phase != "-" {
				phase = styles.render(styles.yellow, phase)
			}
			fields = append(fields, phase)
		}
		if showAuthor {
			author := t.AuthorLogin()
			if styles.colorEnabled && author != "" {
				author = styles.render(styles.cyan, author)
			}
			fields = append(fields, author)
		}
		fields = append(fields, timeAgo(t.UpdatedAt))
		if showURL {
			fields = append(fields, t.URL)
		}
		rows[i] = fields
	}

	widths := columnWidths(headers, rows)
	var b strings.Builder
	// Header row is yellow; data cells are already pre-colored, so they pass
	// through a disabled style. tblSt only supplies the color-enabled gate for
	// the header style.
	tblSt := newTableStyles(w)
	headerStyle := func(int) lipgloss.Style { return styles.yellow }
	plain := func(int) lipgloss.Style { return lipgloss.Style{} }
	writeTableRow(&b, headers, widths, headerStyle, tblSt)
	for _, r := range rows {
		writeTableRow(&b, r, widths, plain, tableStyles{})
	}
	fmt.Fprint(w, b.String())
}

func changeListHasPhase(changes []*change.Metadata) bool {
	for _, t := range changes {
		if t != nil && strings.TrimSpace(t.Phase) != "" {
			return true
		}
	}
	return false
}

func changeListHasURL(changes []*change.Metadata) bool {
	for _, t := range changes {
		if t != nil && strings.TrimSpace(t.URL) != "" {
			return true
		}
	}
	return false
}

func changePhaseDisplay(phase string) string {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return "-"
	}
	return strings.ReplaceAll(phase, "_", " ")
}

func changeStatusListDisplay(statuses []change.Status) string {
	parts := make([]string, len(statuses))
	for i, status := range statuses {
		parts[i] = changeStatusDisplay(status)
	}
	return strings.Join(parts, ", ")
}

func changeStatusListTitle(statuses []change.Status) string {
	display := changeStatusListDisplay(statuses)
	if display == "" {
		return ""
	}
	return strings.ToUpper(display[:1]) + display[1:]
}

func changeStatusDisplay(status change.Status) string {
	return strings.ReplaceAll(string(status), "_", " ")
}

// changeStatusColor returns the style for a change status: open green, merged
// magenta, closed red. draft (the in-progress/building state) and any unknown
// status stay uncolored. The colors avoid AUTHOR's cyan and PHASE's yellow so
// the columns stay distinguishable.
func changeStatusColor(styles statusStyles, status change.Status) (lipgloss.Style, bool) {
	switch status {
	case change.StatusOpen:
		return styles.green, true
	case change.StatusMerged:
		return styles.magenta, true
	case change.StatusClosed:
		return styles.red, true
	case change.StatusDraft:
		return lipgloss.Style{}, false
	default:
		return lipgloss.Style{}, false
	}
}

// changeCountDisplay renders a count as "shown/total" when --limit truncated
// the list, so a capped page doesn't read as the total number of matches.
func changeCountDisplay(shown, total int) string {
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

func newChangeCreateCmd() *cobra.Command {
	var title, body, base, branch, status, typeStr, priorityStr string
	var assignees []string
	var checkout, noBranch bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a change for the current, a new, or no branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := ensureNoChangeRepoOverride(cmd, "change create"); err != nil {
				return err
			}
			return runChangeCreate(cmd, title, body, base, branch, status, typeStr, priorityStr, assignees, checkout, noBranch)
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "Change title")
	cmd.Flags().StringVar(&body, "body", "", "Change body")
	cmd.Flags().StringVar(&base, "base", "", "Base branch (defaults to detected default branch)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for the change (defaults to current branch)")
	cmd.Flags().StringVar(&status, "status", "", "Initial status (defaults to open)")
	cmd.Flags().StringVar(&typeStr, "type", "", fmt.Sprintf("Type (%s)", formatValidTypes()))
	cmd.Flags().StringVar(&priorityStr, "priority", "", fmt.Sprintf("Priority (%s)", formatValidPriorities()))
	cmd.Flags().StringSliceVar(&assignees, "add-assignee", nil, "Assign user(s) by login")
	cmd.Flags().BoolVar(&checkout, "checkout", false, "Check out the branch after creating it")
	cmd.Flags().BoolVar(&noBranch, "no-branch", false, "Create a branchless change")

	return cmd
}

//nolint:cyclop // sequential steps for creating a change — splitting would obscure the flow
func runChangeCreate(cmd *cobra.Command, title, body, base, branch, statusStr, typeStr, priorityStr string, assignees []string, checkout, noBranch bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	if err := validateChangeCreateFlagCombos(cmd, checkout, noBranch); err != nil {
		return err
	}
	if cmd.Flags().Changed("type") {
		if !change.Type(strings.TrimSpace(typeStr)).IsValid() {
			return fmt.Errorf("invalid type %q: valid values are %s", typeStr, formatValidTypes())
		}
	}
	if cmd.Flags().Changed("priority") {
		if !change.Priority(strings.TrimSpace(priorityStr)).IsValid() {
			return fmt.Errorf("invalid priority %q: valid values are %s", priorityStr, formatValidPriorities())
		}
	}

	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	base = resolveChangeCreateBase(repo, base)
	_, currentBranch, _ := isOnDefaultBranchRepo(repo) //nolint:errcheck // best-effort; reuse the open repo for the current branch name
	title, body, base, branch, statusStr, err = resolveChangeCreateFields(cmd, w, title, body, base, branch, statusStr, currentBranch, noBranch)
	if err != nil {
		return err
	}
	if err := validateChangeCreateFields(ctx, title, branch, statusStr, noBranch); err != nil {
		return err
	}

	forge, owner, repoName, err := resolveChangeRemote(ctx)
	if err != nil {
		return err
	}
	client, err := newChangeAPIClient(ctx, changeInsecureHTTP(cmd), owner+"/"+repoName)
	if err != nil {
		return renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repoName, err)
	}

	pushRemote, err := resolveChangePushRemote(ctx, branch)
	if err != nil {
		return err
	}

	branchState, err := prepareChangeCreateBranch(ctx, w, errW, repo, pushRemote, branch, currentBranch, noBranch)
	if err != nil {
		return err
	}

	createResp, err := postChangeCreate(ctx, client, forge, owner, repoName, title, body, branch, base, statusStr, strings.TrimSpace(typeStr), strings.TrimSpace(priorityStr), assignees)
	if err != nil {
		cleanupCreatedChangeBranch(ctx, repo, pushRemote, branch, branchState.LocalCreated, branchState.RemotePushed, errW)
		return err
	}
	printCreatedChange(w, createResp.Change, forge, owner, repoName)

	return maybeCheckoutChangeCreateBranch(ctx, cmd, w, branch, currentBranch, checkout, branchState.NeedsCreation)
}

type changeCreateBranchState struct {
	NeedsCreation bool
	LocalCreated  bool
	RemotePushed  bool
}

func validateChangeCreateFlagCombos(cmd *cobra.Command, checkout, noBranch bool) error {
	if noBranch && cmd.Flags().Changed("branch") {
		return errors.New("cannot combine --no-branch with --branch")
	}
	if noBranch && checkout {
		return errors.New("cannot combine --no-branch with --checkout")
	}
	return nil
}

func resolveChangeCreateBase(repo *git.Repository, base string) string {
	if base != "" {
		return base
	}
	if detected := strategy.GetDefaultBranchName(repo); detected != "" {
		return detected
	}
	return defaultBaseBranch
}

func resolveChangeCreateFields(cmd *cobra.Command, w io.Writer, title, body, base, branch, statusStr, currentBranch string, noBranch bool) (string, string, string, string, string, error) {
	interactive := !cmd.Flags().Changed("title") && !cmd.Flags().Changed("branch")
	switch {
	case interactive:
		// Interactive flow: title → body → branch (unless branchless) → status.
		if err := runChangeCreateInteractive(&title, &body, &branch, &statusStr, noBranch); err != nil {
			return "", "", "", "", "", handleFormCancellation(w, "Change creation", err)
		}
	case noBranch:
		branch = ""
	default:
		// Non-interactive: derive missing values from provided flags. With
		// --branch omitted, use the checked-out branch (a feature branch); only
		// slug a new branch from the title when the checked-out branch is the base.
		branch = resolveCreateBranch(branch, currentBranch, base, title, cmd.Flags().Changed("title"))
		if title == "" {
			title = change.HumanizeBranchName(branch)
		}
	}
	statusStr = strings.TrimSpace(statusStr)
	if statusStr == "" {
		statusStr = string(change.StatusOpen)
	}
	return strings.TrimSpace(title), body, strings.TrimSpace(base), strings.TrimSpace(branch), statusStr, nil
}

func validateChangeCreateFields(ctx context.Context, title, branch, statusStr string, noBranch bool) error {
	if title == "" {
		return errors.New("change title is required")
	}
	if !noBranch {
		if branch == "" {
			return errors.New("branch name is required")
		}
		if err := ValidateBranchName(ctx, branch); err != nil {
			return err
		}
	}
	if status := change.Status(statusStr); !status.IsValid() {
		return fmt.Errorf("invalid status %q: valid values are %s", statusStr, formatValidStatuses())
	}
	return nil
}

func prepareChangeCreateBranch(ctx context.Context, w, errW io.Writer, repo *git.Repository, remote, branch, currentBranch string, noBranch bool) (changeCreateBranchState, error) {
	var state changeCreateBranchState
	if noBranch || branch == "" {
		// Branchless changes have no remote branch to create, fetch, or push.
		return state, nil
	}

	state.NeedsCreation = branchNeedsCreation(repo, branch)
	existedOnRemote, existErr := remoteHasBranch(ctx, remote, branch)
	if existErr != nil {
		fmt.Fprintf(errW, "Warning: could not check whether branch %s already exists on %s: %v\n", branch, remote, existErr)
		existedOnRemote = true
	}

	if err := ensureChangeCreateBranchExists(ctx, w, repo, remote, branch, currentBranch, existedOnRemote, &state); err != nil {
		return state, err
	}

	// For branch-backed changes, always push the branch first: the change binds to a
	// remote branch, so deliver it before creating the change rather than letting
	// the server backfill it at the base tip. Branchless changes skip this entirely.
	if err := pushBranchToRemote(ctx, remote, branch); err != nil {
		cleanupCreatedChangeBranch(ctx, repo, remote, branch, state.LocalCreated, false, errW)
		return state, fmt.Errorf("failed to push branch %q to %q: %w\nhint: the change was not created because its branch could not be delivered to the remote.\n  - if this is an auth error, link your GitHub account and retry\n  - if this is a non-fast-forward, update branch %q from %q and retry", branch, remote, err, branch, remote)
	}
	state.RemotePushed = !existedOnRemote
	fmt.Fprintf(w, "Pushed branch %s to %s\n", branch, remote)
	return state, nil
}

func ensureChangeCreateBranchExists(ctx context.Context, w io.Writer, repo *git.Repository, remote, branch, currentBranch string, existedOnRemote bool, state *changeCreateBranchState) error {
	if !state.NeedsCreation {
		if currentBranch != branch {
			fmt.Fprintf(w, "Note: change will be created for branch %q (not the current branch)\n", branch)
		}
		return nil
	}
	if existedOnRemote {
		if err := fetchBranchFromRemote(ctx, remote, branch); err != nil {
			return fmt.Errorf("failed to fetch branch %q from %q: %w", branch, remote, err)
		}
		state.LocalCreated = true
		fmt.Fprintf(w, "Fetched branch %s from %s\n", branch, remote)
		return nil
	}
	if err := createBranch(repo, branch); err != nil {
		return fmt.Errorf("failed to create branch %q: %w", branch, err)
	}
	state.LocalCreated = true
	fmt.Fprintf(w, "Created branch %s\n", branch)
	return nil
}

func postChangeCreate(ctx context.Context, client *api.Client, forge, owner, repoName, title, body, branch, base, statusStr, typeStr, priorityStr string, assignees []string) (api.ChangeCreateResponse, error) {
	createReq := newChangeCreateRequest(title, body, branch, base, statusStr, typeStr, priorityStr, assignees)
	resp, err := client.Post(ctx, changesBasePath(forge, owner, repoName), createReq)
	if err != nil {
		noteChangeCommandEnablement(ctx, client, err)
		return api.ChangeCreateResponse{}, fmt.Errorf("failed to create change: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		noteChangeCommandEnablement(ctx, client, err)
		return api.ChangeCreateResponse{}, err
	}
	saveTrailsEnabledForRemoteBestEffort(ctx, forge, owner, repoName, true)

	var createResp api.ChangeCreateResponse
	if err := api.DecodeJSON(resp, &createResp); err != nil {
		return api.ChangeCreateResponse{}, fmt.Errorf("failed to decode create response: %w", err)
	}
	return createResp, nil
}

func maybeCheckoutChangeCreateBranch(ctx context.Context, cmd *cobra.Command, w io.Writer, branch, currentBranch string, checkout, needsCreation bool) error {
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

func newChangeCreateRequest(title, body, branch, base, statusStr, typeStr, priorityStr string, assignees []string) api.ChangeCreateRequest {
	req := api.ChangeCreateRequest{
		Title:     title,
		Body:      body,
		Base:      base,
		Status:    statusStr,
		Type:      typeStr,
		Priority:  priorityStr,
		Assignees: assignees,
	}
	if branch != "" {
		req.BranchName = branch
		req.BranchAction = "link"
	}
	return req
}

// resolveChangeUpdateBody returns the body text to seed the interactive update
// form with, plus the etag of the description as read (empty when unavailable,
// e.g. an older server or a fallback to the list body — see sendChangeBody for
// how a missing etag is handled). The list resource omits the description (it
// lives in body_document, served only by the detail endpoint), so update must
// fetch the detail body — otherwise the form prefills from the empty list body
// and a user edit against that blank baseline can overwrite a description they
// never saw. Best-effort: on a failed detail fetch it returns the list body
// (and no etag) plus the error so the caller can warn (mirroring
// runChangeShow); an empty detail body (older/partial server) falls back to the
// list body with no error.
func resolveChangeUpdateBody(ctx context.Context, client *api.Client, forge, owner, repo string, found *api.ChangeResource) (body, etag string, err error) {
	body = found.Body
	if found.Number > 0 {
		bt, et, ferr := fetchChangeDescription(ctx, client, forge, owner, repo, found.Number)
		if ferr != nil {
			return body, "", ferr
		}
		// fetchChangeDescription already trims; a non-empty result supersedes
		// the list body, an empty one (older/partial server) leaves it intact.
		// The etag is kept either way — it describes the document read, not
		// the text, so it's valid even when the description is legitimately
		// empty (avoids a redundant refetch below).
		etag = et
		if bt != "" {
			body = bt
		}
	}
	return body, etag, nil
}

func newChangeUpdateCmd() *cobra.Command {
	var statusStr, title, body, branch, typeStr, priorityStr string
	var assigneeAdd, assigneeRemove, reviewerAdd, reviewerRemove []string
	var overwrite bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update change metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := ensureChangeRepoHasTarget(cmd, strings.TrimSpace(branch) != "", "pass --branch"); err != nil {
				return err
			}
			return runChangeUpdate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), changeInsecureHTTP(cmd), changeUpdateInputs{
				Status:          statusStr,
				StatusChanged:   cmd.Flags().Changed("status"),
				Title:           title,
				TitleChanged:    cmd.Flags().Changed("title"),
				Body:            body,
				BodyChanged:     cmd.Flags().Changed("body"),
				Overwrite:       overwrite,
				Branch:          branch,
				Repo:            changeRepoFlag(cmd),
				AssigneeAdd:     assigneeAdd,
				AssigneeRemove:  assigneeRemove,
				ReviewerAdd:     reviewerAdd,
				ReviewerRemove:  reviewerRemove,
				Type:            typeStr,
				TypeChanged:     cmd.Flags().Changed("type"),
				Priority:        priorityStr,
				PriorityChanged: cmd.Flags().Changed("priority"),
			})
		},
	}

	cmd.Flags().StringVar(&statusStr, "status", "", "Update status")
	cmd.Flags().StringVar(&title, "title", "", "Update title")
	cmd.Flags().StringVar(&body, "body", "", "Replace the description (--body= clears it)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Replace the description unconditionally, even if it changed since being read (only applies when --body is also given)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to update change for (defaults to current)")
	cmd.Flags().StringSliceVar(&assigneeAdd, "add-assignee", nil, "Add assignee(s) by login")
	cmd.Flags().StringSliceVar(&assigneeRemove, "remove-assignee", nil, "Remove assignee(s) by login")
	cmd.Flags().StringSliceVar(&reviewerAdd, "add-reviewer", nil, "Request reviewer(s) by login")
	cmd.Flags().StringSliceVar(&reviewerRemove, "remove-reviewer", nil, "Remove requested reviewer(s) by login")
	cmd.Flags().StringVar(&typeStr, "type", "", fmt.Sprintf("Set type (%s)", formatValidTypes()))
	cmd.Flags().StringVar(&priorityStr, "priority", "", fmt.Sprintf("Set priority (%s)", formatValidPriorities()))

	return cmd
}

type changeUpdateInputs struct {
	Status          string
	StatusChanged   bool
	Title           string
	TitleChanged    bool
	Body            string
	BodyChanged     bool
	Overwrite       bool
	Branch          string
	Repo            string
	AssigneeAdd     []string
	AssigneeRemove  []string
	ReviewerAdd     []string
	ReviewerRemove  []string
	Type            string
	TypeChanged     bool
	Priority        string
	PriorityChanged bool
}

func runChangeUpdate(ctx context.Context, w, errW io.Writer, insecureHTTP bool, inputs changeUpdateInputs) error {
	return runAuthenticatedChangeAPI(ctx, errW, insecureHTTP, inputs.Repo, func(ctx context.Context, client *api.Client) error {
		forge, owner, repoName, err := resolveChangeRepoOrRemote(ctx, inputs.Repo)
		if err != nil {
			return err
		}
		return runChangeUpdateWithClient(ctx, w, errW, client, forge, owner, repoName, inputs)
	})
}

// runChangeUpdateWithClient applies a change update once the repo and API client
// are resolved.
func runChangeUpdateWithClient(ctx context.Context, w, errW io.Writer, client *api.Client, forge, owner, repoName string, inputs changeUpdateInputs) error {
	// Determine branch.
	branch := inputs.Branch
	if branch == "" {
		current, err := GetCurrentBranch(ctx)
		if err != nil {
			return fmt.Errorf("failed to determine current branch: %w", err)
		}
		branch = current
	}

	// Find the change by branch.
	found, err := findChangeByBranch(ctx, client, forge, owner, repoName, branch)
	if err != nil {
		return err
	}
	if found == nil {
		return fmt.Errorf("no change found for branch %q", branch)
	}

	// Interactive mode when no update flags are provided.
	statusStr := inputs.Status
	title := inputs.Title
	body := inputs.Body
	var bodyETag string
	noFlags := !inputs.StatusChanged && !inputs.TitleChanged && !inputs.BodyChanged &&
		inputs.AssigneeAdd == nil && inputs.AssigneeRemove == nil &&
		inputs.ReviewerAdd == nil && inputs.ReviewerRemove == nil &&
		!inputs.TypeChanged && !inputs.PriorityChanged
	if noFlags {
		metadata := found.ToMetadata()
		// Build status options with current value as default.
		var statusOptions []huh.Option[string]
		for _, s := range change.ValidStatuses() {
			if (s == change.StatusMerged || s == change.StatusClosed) && s != metadata.Status {
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
		// The list resource omits the description; fetch the detail body so
		// the form prefills with the current text and change detection below
		// compares against the real server value. Warn on a fetch failure so
		// a blank baseline doesn't silently overwrite an unseen description.
		seedBody, seedETag, bodyErr := resolveChangeUpdateBody(ctx, client, forge, owner, repoName, found)
		if bodyErr != nil {
			fmt.Fprintf(errW, "Warning: could not load current change body: %v\n", bodyErr)
		}
		body = seedBody
		bodyETag = seedETag
		origStatus, origTitle, origBody := statusStr, title, body

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
			return handleFormCancellation(w, "Change update", formErr)
		}
		// Only mark a field changed when the user actually edited it, so an
		// untouched body/title/status isn't needlessly PATCHed (a no-op body
		// PATCH would otherwise rewrite the description on every update).
		inputs.StatusChanged = statusStr != origStatus
		inputs.TitleChanged = title != origTitle
		inputs.BodyChanged = body != origBody
	}

	statusStr = strings.TrimSpace(statusStr)
	title = strings.TrimSpace(title)
	if err := validateChangeUpdateFields(changeUpdateInputs{
		Status:          statusStr,
		StatusChanged:   inputs.StatusChanged,
		Title:           title,
		TitleChanged:    inputs.TitleChanged,
		Type:            inputs.Type,
		TypeChanged:     inputs.TypeChanged,
		Priority:        inputs.Priority,
		PriorityChanged: inputs.PriorityChanged,
	}); err != nil {
		return err
	}

	// Build the metadata request with only changed fields. The description is
	// not part of it — it is written separately, from the `body` local below.
	updateReq := buildChangeUpdateRequest(found, changeUpdateInputs{
		Status:          statusStr,
		StatusChanged:   inputs.StatusChanged,
		Title:           title,
		TitleChanged:    inputs.TitleChanged,
		AssigneeAdd:     inputs.AssigneeAdd,
		AssigneeRemove:  inputs.AssigneeRemove,
		ReviewerAdd:     inputs.ReviewerAdd,
		ReviewerRemove:  inputs.ReviewerRemove,
		Type:            inputs.Type,
		TypeChanged:     inputs.TypeChanged,
		Priority:        inputs.Priority,
		PriorityChanged: inputs.PriorityChanged,
	})

	// The single-change endpoint is keyed by change number, not id; the server
	// rejects an id here with "Invalid change number format".
	if found.Number <= 0 {
		return fmt.Errorf("change for branch %q has no number yet; cannot update", branch)
	}
	path := changeNumberPath(forge, owner, repoName, found.Number)

	// Metadata and description live on different routes, so an update touching
	// both sends two requests. They are not atomic: if the metadata PATCH lands
	// and the body PUT then fails, the metadata change persists. Report that
	// partial state explicitly so the caller knows the metadata already applied
	// and only the body needs a retry, rather than assuming nothing changed.
	hasMeta := changeUpdateRequestHasFields(updateReq)
	if !hasMeta && !inputs.BodyChanged {
		// Nothing changed — most often the interactive form opened and closed
		// untouched. Say so instead of printing the success line: no request went
		// out, and agents read that line as confirmation the write landed.
		fmt.Fprintf(w, "No changes to apply to the change for branch %s\n", branch)
		return nil
	}
	if hasMeta {
		if err := sendChangePatch(ctx, client, path, updateReq); err != nil {
			return err
		}
	}
	if !noFlags && inputs.BodyChanged && bodyETag == "" && !inputs.Overwrite {
		// Non-interactive --body path only (noFlags is the interactive
		// branch, which already read the body and its etag above to seed the
		// editor — gating on that branch rather than on bodyETag=="" matters:
		// an interactive session can legitimately end with no etag too (e.g.
		// the seed read failed and the user was already warned), and redoing
		// the read here would silently pair an etag for content the user
		// never saw with their edit). A --body caller supplied the
		// replacement text directly and skipped that read, so do it here,
		// best-effort, purely for the etag. A failure just leaves bodyETag
		// empty and falls through to sendChangeBody's graceful Overwrite
		// fallback rather than failing the whole command — but say so, since
		// this is the unattended path most likely to run without anyone
		// watching for a silently downgraded conflict check.
		if _, etag, ferr := fetchChangeDescription(ctx, client, forge, owner, repoName, found.Number); ferr != nil {
			fmt.Fprintf(errW, "Warning: could not verify change body is unchanged (%v); writing without conflict detection\n", ferr)
		} else {
			bodyETag = etag
		}
	}
	if inputs.BodyChanged {
		if err := sendChangeBody(ctx, client, changeBodyPath(forge, owner, repoName, found.Number), body, bodyETag, inputs.Overwrite); err != nil {
			if hasMeta {
				return fmt.Errorf("change metadata was updated, but the body update failed (the metadata change already applied; retry only the --body change): %w", err)
			}
			return err
		}
	}

	fmt.Fprintf(w, "Updated change for branch %s\n", branch)
	return nil
}

func validateChangeUpdateFields(inputs changeUpdateInputs) error {
	if inputs.TitleChanged && strings.TrimSpace(inputs.Title) == "" {
		return errors.New("change title is required")
	}
	if inputs.StatusChanged {
		status := change.Status(strings.TrimSpace(inputs.Status))
		if !status.IsValid() {
			return fmt.Errorf("invalid status %q: valid values are %s", inputs.Status, formatValidStatuses())
		}
	}
	if inputs.TypeChanged {
		if typ := change.Type(strings.TrimSpace(inputs.Type)); !typ.IsValid() {
			return fmt.Errorf("invalid type %q: valid values are %s", inputs.Type, formatValidTypes())
		}
	}
	if inputs.PriorityChanged {
		if p := change.Priority(strings.TrimSpace(inputs.Priority)); !p.IsValid() {
			return fmt.Errorf("invalid priority %q: valid values are %s", inputs.Priority, formatValidPriorities())
		}
	}
	return nil
}

// formatValidTypes lists the valid change types for error messages.
func formatValidTypes() string {
	parts := make([]string, 0, len(change.ValidTypes()))
	for _, t := range change.ValidTypes() {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ", ")
}

// formatValidPriorities lists the valid change priorities for error messages.
func formatValidPriorities() string {
	parts := make([]string, 0, len(change.ValidPriorities()))
	for _, p := range change.ValidPriorities() {
		parts = append(parts, string(p))
	}
	return strings.Join(parts, ", ")
}

// mergeStringSet returns current with add appended (dedup, order-preserving)
// and remove entries deleted. Used for replace-set fields (labels, assignees,
// requested reviewers) whose PATCH endpoint takes the full new list.
func mergeStringSet(current, add, remove []string) []string {
	out := make([]string, 0, len(current)+len(add))
	out = append(out, current...)
	for _, a := range add {
		found := false
		for _, e := range out {
			if e == a {
				found = true
				break
			}
		}
		if !found {
			out = append(out, a)
		}
	}
	for _, r := range remove {
		for i, e := range out {
			if e == r {
				out = append(out[:i], out[i+1:]...)
				break
			}
		}
	}
	return out
}

// buildChangeUpdateRequest constructs the metadata PATCH body from the current
// change and the requested changes. It deliberately ignores inputs.Body: the
// description is not part of this request, it is written by sendChangeBody
// against its own route, and api.ChangeUpdateRequest has no field to put it in.
func buildChangeUpdateRequest(current *api.ChangeResource, inputs changeUpdateInputs) api.ChangeUpdateRequest {
	var req api.ChangeUpdateRequest

	if inputs.StatusChanged {
		req.Status = &inputs.Status
	}
	if inputs.TitleChanged {
		req.Title = &inputs.Title
	}
	if inputs.TypeChanged {
		typ := strings.TrimSpace(inputs.Type)
		req.Type = &typ
	}
	if inputs.PriorityChanged {
		priority := strings.TrimSpace(inputs.Priority)
		req.Priority = &priority
	}
	// Replace-set fields: compute the full new list from the current change.
	if len(inputs.AssigneeAdd) > 0 || len(inputs.AssigneeRemove) > 0 {
		assignees := mergeStringSet(current.Assignees, inputs.AssigneeAdd, inputs.AssigneeRemove)
		req.Assignees = &assignees
	}
	if len(inputs.ReviewerAdd) > 0 || len(inputs.ReviewerRemove) > 0 {
		reviewers := mergeStringSet(current.RequestedReviewers, inputs.ReviewerAdd, inputs.ReviewerRemove)
		req.RequestedReviewers = &reviewers
	}

	return req
}

// changeUpdateRequestHasFields reports whether req sets any field at all. It
// walks the struct reflectively rather than naming each field, so a field added
// to api.ChangeUpdateRequest later — a branch rename, say — counts as a metadata
// change without anyone having to remember to extend this check. Forgetting to
// would drop that change silently: the PATCH is never sent, yet the command
// still reports success.
//
// The reflective default is only right for fields the metadata route actually
// serves. A field that has to travel with the description instead — a
// hypothetical body_format or body_version — belongs in api.ChangeBodyRequest
// and on the body PUT; put it here and it goes out on a route that does not
// know it, which is the same wrong-route mistake the body itself used to make.
func changeUpdateRequestHasFields(req api.ChangeUpdateRequest) bool {
	v := reflect.ValueOf(req)
	for i := range v.NumField() {
		// Every field is a pointer today (nil means "not provided"), but IsZero
		// also covers a plain string or slice field added later.
		if !v.Field(i).IsZero() {
			return true
		}
	}
	return false
}

// sendChangePatch issues a single change metadata PATCH and validates the
// response. The description is not sent here — see sendChangeBody.
func sendChangePatch(ctx context.Context, client *api.Client, path string, req api.ChangeUpdateRequest) error {
	resp, err := client.Patch(ctx, path, req)
	if err != nil {
		return fmt.Errorf("failed to update change: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		return err
	}
	var updateResp api.ChangeUpdateResponse
	if err := api.DecodeJSON(resp, &updateResp); err != nil {
		return fmt.Errorf("failed to decode update response: %w", err)
	}
	return nil
}

// sendChangeBody writes a change's description through the body route, the only
// one that serves body writes (api.ChangeUpdateRequest documents why the metadata
// PATCH does not). path must already be that route — see changeBodyPath.
//
// Three dispatch modes, in order:
//
//   - overwrite: sends Overwrite: true and no If-Match. This is the explicit
//     --overwrite flag — the caller wants the replacement to win regardless of
//     what's there.
//   - ifMatch non-empty (and !overwrite): sends If-Match instead of Overwrite,
//     so the write is rejected with 412 if the description changed since
//     ifMatch was read (resolveChangeUpdateBody / the non-interactive etag
//     fetch in runChangeUpdateWithClient).
//   - neither: falls back to Overwrite: true. This is deliberate graceful
//     degradation, not a shortcut to remove — it's what runs against a server
//     that predates the etag field, a change with no body document yet, or
//     after a failed best-effort etag read. Refusing to write in that case
//     would make `change update --body` unusable against anything older or
//     partial than the newest server.
func sendChangeBody(ctx context.Context, client *api.Client, path, body, ifMatch string, overwrite bool) error {
	req := api.ChangeBodyRequest{Markdown: body}
	var headers http.Header
	if ifMatch != "" && !overwrite {
		headers = http.Header{"If-Match": []string{ifMatch}}
	} else {
		req.Overwrite = true
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal change body request: %w", err)
	}
	resp, err := client.Request(ctx, http.MethodPut, path, headers, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to update change body: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		switch {
		case api.IsHTTPErrorStatus(err, http.StatusPreconditionFailed):
			return fmt.Errorf("%w — change body changed since it was read; run 'entire change show' to see the current text and merge it in, then re-run — or pass --overwrite to discard it", err)
		case api.IsHTTPErrorStatus(err, http.StatusConflict):
			return fmt.Errorf("%w — change body is not empty; pass --overwrite to replace it", err)
		default:
			return err
		}
	}
	// Nothing reads the document back; decoding it only confirms the success
	// really was this route's JSON response, and drains the body — the same
	// check sendChangePatch makes on the metadata route.
	var doc api.ChangeBodyDocument
	if err := api.DecodeJSON(resp, &doc); err != nil {
		return fmt.Errorf("failed to decode change body response: %w", err)
	}
	return nil
}

func newChangeCheckoutCmd() *cobra.Command {
	var changeSelector string
	var force bool
	var worktree bool

	cmd := &cobra.Command{
		Use:   "checkout [<change>]",
		Short: "Check out a change's branch",
		Long: `Check out the branch of a change.

The change may be given as the first argument or via --change, as a number, id, or
branch. Without one, the change for the current branch is used. The change's branch
is checked out, fetching it from origin first when it only exists there.

With --worktree, the branch is checked out into a git worktree under
.entire/worktrees at the repo root instead of switching this checkout, and the
command prints a cd command for the new worktree. Gitignored files matching
.worktreeinclude patterns are copied into the worktree. When stdout is not a
terminal, only the worktree path is printed, so scripts can use
cd "$(entire change checkout <change> --worktree)".

This must be run from within a clone of the repository the change belongs to; the
change is looked up against that repository's origin remote.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := changeSelector
			if len(args) == 1 {
				if cmd.Flags().Changed("change") {
					return errors.New("cannot combine a change argument with --change")
				}
				selector = args[0]
			}
			if err := ensureNoChangeRepoOverride(cmd, "change checkout"); err != nil {
				return err
			}
			return runChangeCheckout(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), changeInsecureHTTP(cmd), selector, changeCheckoutOptions{Force: force, Worktree: worktree})
		},
	}

	cmd.Flags().StringVar(&changeSelector, "change", "", "Change to check out (number, id, or branch; defaults to the current branch's change)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip the prompt before fetching a remote-only branch")
	cmd.Flags().BoolVar(&worktree, "worktree", false, "Check out the change branch in a worktree under .entire/worktrees instead of switching this checkout")

	return cmd
}

type changeCheckoutOptions struct {
	Force    bool
	Worktree bool
}

func runChangeCheckout(ctx context.Context, w, errW io.Writer, insecureHTTP bool, selector string, opts changeCheckoutOptions) error {
	// checkout rejects --repo (it operates on the local clone), so the enablement
	// cache always tracks the local origin here.
	return runAuthenticatedChangeAPI(ctx, errW, insecureHTTP, "", func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveChangeRemote(ctx)
		if err != nil {
			return err
		}

		found, err := resolveChangeBySelector(ctx, client, forge, owner, repo, selector, "")
		if err != nil {
			return err
		}

		branch := strings.TrimSpace(found.Branch)
		if branch == "" {
			return fmt.Errorf("%s has no branch to check out", describeChangeRef(found))
		}

		if opts.Worktree {
			fmt.Fprintf(errW, "Checking out %s in a worktree\n", describeChangeRef(found))
			return checkoutTrailWorktree(ctx, w, errW, branch, opts.Force, found.Number)
		}

		currentBranch, _ := GetCurrentBranch(ctx) //nolint:errcheck // best-effort; a detached HEAD just means "not already on the branch"
		if currentBranch == branch {
			fmt.Fprintf(w, "Already on branch %s for %s.\n", branch, describeChangeRef(found))
			return nil
		}

		fmt.Fprintf(w, "Checking out %s\n", describeChangeRef(found))
		// switchToBranchForResume handles local vs. remote-only branches, the
		// uncommitted-changes guard, and the fetch prompt; reuse it rather than
		// re-deriving that logic here.
		proceed, err := switchToBranchForResume(ctx, w, errW, branch, opts.Force)
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

// describeChangeRef renders a short human reference to a change for status
// messages, e.g. "change #575 (Add foo)" or, when the change has no number yet,
// "change \"Add foo\"".
func describeChangeRef(t *api.ChangeResource) string {
	title := strings.TrimSpace(t.Title)
	if t.Number > 0 {
		if title == "" {
			return fmt.Sprintf("change #%d", t.Number)
		}
		return fmt.Sprintf("change #%d (%s)", t.Number, title)
	}
	if title == "" {
		return "change"
	}
	return fmt.Sprintf("change %q", title)
}

// parseChangeNumberArg parses an optional positional change-number argument.
// It returns 0 when no argument is supplied; a supplied value must be a
// positive integer (the server keys single-change endpoints by number).
func parseChangeNumberArg(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid change number %q: expected a positive integer (see 'entire change list')", args[0])
	}
	return n, nil
}

func newChangeDeleteCmd() *cobra.Command {
	var branch string
	var force bool

	cmd := &cobra.Command{
		Use:   "delete [<number>]",
		Short: "Delete a change",
		Long: `Delete a change by number, or the change for a branch.

If <number> is omitted, the change for --branch (or the current branch) is used.
Deletion is permanent; you are prompted to confirm unless --force is passed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseChangeNumberArg(args)
			if err != nil {
				return err
			}
			if number > 0 && cmd.Flags().Changed("branch") {
				return errors.New("cannot combine a change <number> with --branch")
			}
			if err := ensureChangeRepoHasTarget(cmd, number > 0 || strings.TrimSpace(branch) != "", "pass a change number or --branch"); err != nil {
				return err
			}
			return runChangeDelete(cmd, number, branch, force)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose change to delete (defaults to current)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip the confirmation prompt")

	return cmd
}

func runChangeDelete(cmd *cobra.Command, number int, branch string, force bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	return runAuthenticatedChangeAPI(ctx, cmd.ErrOrStderr(), changeInsecureHTTP(cmd), changeRepoFlag(cmd), func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveChangeRepoOrRemote(ctx, changeRepoFlag(cmd))
		if err != nil {
			return err
		}

		// Resolve the target change. An explicit number is authoritative (a
		// lookup is best-effort, only to label the confirmation); otherwise the
		// branch's change supplies the number.
		title := ""
		if number == 0 {
			if branch == "" {
				branch, err = GetCurrentBranch(ctx)
				if err != nil {
					return fmt.Errorf("failed to determine current branch: %w", err)
				}
			}
			found, ferr := findChangeByBranch(ctx, client, forge, owner, repo, branch)
			if ferr != nil {
				return ferr
			}
			if found == nil {
				return fmt.Errorf("no change found for branch %q", branch)
			}
			if found.Number <= 0 {
				return fmt.Errorf("change for branch %q has no number yet; cannot delete", branch)
			}
			number = found.Number
			title = found.Title
		} else if found, ferr := findChangeByNumber(ctx, client, forge, owner, repo, number); ferr == nil && found != nil {
			title = found.Title
		}

		proceed, err := confirmChangeDeletion(ctx, w, number, title, force, interactive.CanPromptInteractively())
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}

		if err := deleteChangeByNumber(ctx, client, forge, owner, repo, number); err != nil {
			return err
		}

		fmt.Fprintf(w, "Deleted change #%d\n", number)
		return nil
	})
}

// deleteChangeByNumber deletes a change; entire-api answers 204 No Content, so any
// 2xx is a successful delete and the body is not read.
func deleteChangeByNumber(ctx context.Context, client *api.Client, forge, owner, repo string, number int) error {
	resp, err := client.Delete(ctx, changeNumberPath(forge, owner, repo, number))
	if err != nil {
		return fmt.Errorf("failed to delete change: %w", err)
	}
	defer resp.Body.Close()
	return checkChangeResponse(resp)
}

// confirmChangeDeletion decides whether a change delete should proceed. With
// force it proceeds silently. Otherwise it requires an interactive terminal:
// when none is available it refuses (returns an error) rather than deleting
// unprompted; when one is, it shows a confirmation form. canPrompt is passed in
// (rather than queried) so the decision is unit-testable without a TTY.
func confirmChangeDeletion(ctx context.Context, w io.Writer, number int, title string, force, canPrompt bool) (bool, error) {
	if force {
		return true, nil
	}
	if !canPrompt {
		return false, fmt.Errorf("refusing to delete change #%d without confirmation; pass --force", number)
	}
	// huh opens the TTY during form startup regardless of context state, so
	// guard explicitly to honor an already-cancelled command context.
	if ctx.Err() != nil {
		return false, nil //nolint:nilerr // cancelled context is a clean skip, not an error
	}
	prompt := fmt.Sprintf("Delete change #%d?", number)
	if title != "" {
		prompt = fmt.Sprintf("Delete change #%d (%s)?", number, title)
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
		return false, fmt.Errorf("change deletion prompt: %w", err)
	}
	if !confirmed {
		fmt.Fprintln(w, "Change deletion cancelled.")
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
	statuses := change.ValidStatuses()
	names := make([]string, len(statuses))
	for i, s := range statuses {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

var runChangeCreateForm = func(form *huh.Form) error { return form.Run() }

// runChangeCreateInteractive runs the interactive form for change creation.
// Prompts for title, body, branch (derived from title, unless branchless), and status.
func runChangeCreateInteractive(title, body, branch, statusStr *string, noBranch bool) error {
	// Step 1: Title and body
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Change title").
				Placeholder("What are you working on?").
				Value(title),
			huh.NewText().
				Title("Body (optional)").
				Value(body),
		),
	)
	if err := runChangeCreateForm(form); err != nil {
		return fmt.Errorf("form cancelled: %w", err)
	}
	*title = strings.TrimSpace(*title)
	if *title == "" {
		return errors.New("change title is required")
	}

	// Build status options, excluding done/closed
	var statusOptions []huh.Option[string]
	for _, s := range change.ValidStatuses() {
		if s == change.StatusMerged || s == change.StatusClosed {
			continue
		}
		statusOptions = append(statusOptions, huh.NewOption(string(s), string(s)))
	}
	if *statusStr == "" {
		*statusStr = string(change.StatusOpen)
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
		if err := runChangeCreateForm(form); err != nil {
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
	if err := runChangeCreateForm(form); err != nil {
		return fmt.Errorf("form cancelled: %w", err)
	}
	*branch = strings.TrimSpace(*branch)
	if *branch == "" {
		*branch = suggested
	}
	return nil
}

// findChangeByBranch looks up a change by branch name via the list API.
func findChangeBySelector(ctx context.Context, client *api.Client, forge, owner, repo, selector string) (*api.ChangeResource, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil //nolint:nilnil // empty selector means not found for this helper
	}
	if n, ok := parseChangeNumberSelector(selector); ok {
		found, err := findChangeByNumber(ctx, client, forge, owner, repo, n)
		if err != nil || found != nil {
			return found, err
		}
	}
	return findChange(ctx, client, forge, owner, repo, func(t api.ChangeResource) bool {
		return t.ID == selector || t.Branch == selector
	})
}

func parseChangeNumberSelector(selector string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(selector))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func findChangeByBranch(ctx context.Context, client *api.Client, forge, owner, repo, branch string) (*api.ChangeResource, error) {
	return findChange(ctx, client, forge, owner, repo, func(t api.ChangeResource) bool {
		return t.Branch == branch
	})
}

// findChangeByNumber looks up a change by numeric identifier through entire-api's
// direct number route, so it never has to scan the list pages.
func findChangeByNumber(ctx context.Context, client *api.Client, forge, owner, repo string, number int) (*api.ChangeResource, error) {
	resp, err := client.Get(ctx, changeNumberPath(forge, owner, repo, number))
	if err != nil {
		return nil, fmt.Errorf("get change %d: %w", number, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil //nolint:nilnil // nil, nil means "not found" to selector callers
	}
	if err := checkChangeResponse(resp); err != nil {
		return nil, err
	}

	found, err := decodeChangeResource(resp)
	if err != nil {
		return nil, fmt.Errorf("decode change %d: %w", number, err)
	}
	if found.ID == "" && found.Number == 0 {
		// A 2xx carrying nothing recognizable is "not found", not a change whose
		// every field is zero: selector callers would otherwise act on a phantom.
		return nil, nil //nolint:nilnil // nil, nil means "not found" to selector callers
	}
	return &found, nil
}

func findChange(ctx context.Context, client *api.Client, forge, owner, repo string, match func(api.ChangeResource) bool) (*api.ChangeResource, error) {
	// Walk bounded opaque-cursor pages so selector lookups do not silently miss
	// changes beyond the first entire-api page.
	pageToken := ""
	seenTokens := map[string]bool{}
	for range changeFindMaxPages {
		resp, err := client.Get(ctx, changesBasePath(forge, owner, repo)+changeListPageQuery(nil, changeListServerMaxLimit, pageToken))
		if err != nil {
			return nil, fmt.Errorf("list changes: %w", err)
		}

		var listResp api.ChangeListResponse
		decodeErr := func() error {
			defer resp.Body.Close()
			if err := checkChangeResponse(resp); err != nil {
				return err
			}
			if err := api.DecodeJSON(resp, &listResp); err != nil {
				return fmt.Errorf("decode change list: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return nil, decodeErr
		}

		for i := range listResp.Changes {
			if match(listResp.Changes[i]) {
				return &listResp.Changes[i], nil
			}
		}
		if listResp.NextPageToken == nil || strings.TrimSpace(*listResp.NextPageToken) == "" {
			break
		}
		pageToken = strings.TrimSpace(*listResp.NextPageToken)
		if seenTokens[pageToken] {
			break
		}
		seenTokens[pageToken] = true
	}
	return nil, nil //nolint:nilnil // nil, nil means "not found" — callers check both
}

// changesBasePath returns the API path prefix for changes endpoints
// (e.g., "/api/v1/changes/gh/org/repo").
func changesBasePath(forge, owner, repo string) string {
	return fmt.Sprintf("/api/v1/changes/%s/%s/%s", url.PathEscape(forge), url.PathEscape(owner), url.PathEscape(repo))
}

// changeNumberPath returns the single-change API path keyed by integer change
// number (e.g. "/api/v1/changes/gh/acme/repo/575"). The server validates an
// integer here and rejects the change UUID, so callers must pass Number, not ID.
func changeNumberPath(forge, owner, repo string, number int) string {
	return changesBasePath(forge, owner, repo) + "/" + strconv.Itoa(number)
}

// changeBodyPath returns the route that writes a change's description
// (e.g. "/api/v1/changes/gh/acme/repo/575/body"). It is the only route that does;
// see api.ChangeBodyRequest.
func changeBodyPath(forge, owner, repo string, number int) string {
	return changeNumberPath(forge, owner, repo, number) + "/body"
}

// resolveChangeRemote resolves the origin remote and ensures the forge is
// known to the changes API. Without this guard, an unmapped host (e.g.
// gitlab.com, or a misconfigured entire:// URL with no forge prefix)
// produces a malformed `/api/v1/changes//owner/repo` path that the server
// rejects with an opaque error instead of a clear "unsupported forge" one.
func resolveChangeRemote(ctx context.Context) (forge, owner, repo string, err error) {
	forge, owner, repo, err = gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return "", "", "", fmt.Errorf("failed to resolve repository: %w", err)
	}
	if forge == "" {
		return "", "", "", errors.New("origin remote is not on a forge supported by Entire changes (supported: github.com)")
	}
	return forge, owner, repo, nil
}

// resolveChangeRepoOrRemote resolves the forge/owner/repo triple used to build
// change API paths. An explicit --repo override (repoOverride) wins; otherwise
// it derives the triple from the origin remote of the current clone.
func resolveChangeRepoOrRemote(ctx context.Context, repoOverride string) (forge, owner, repo string, err error) {
	if repoOverride != "" {
		return parseChangeRepoArg(repoOverride)
	}
	return resolveChangeRemote(ctx)
}

// resolveChangeBranch returns the branch a change lookup should use: an explicit
// --branch override (branchOverride) when given, otherwise the current branch.
func resolveChangeBranch(ctx context.Context, branchOverride string) (string, error) {
	if branchOverride != "" {
		return branchOverride, nil
	}
	return GetCurrentBranch(ctx)
}

// defaultChangePushRemote is where a change branch goes when git config declares
// nothing — git's own fallback for a bare push. Not defaultMirrorRemote, which
// shares the value but means "the remote `mirror use` repoints".
const defaultChangePushRemote = "origin"

// resolveChangePushRemote returns the remote a change's branch is delivered to,
// following git's own push precedence for that branch: branch.<name>.pushRemote,
// then remote.pushDefault, then branch.<name>.remote, and "origin" when nothing
// is declared.
//
// A hardcoded "origin" delivered the branch to the wrong remote, with no warning,
// in any repo whose branch pushes somewhere else — a fork workflow holding the
// upstream as origin, or any repo with remote.pushDefault set. A branch being
// newly created has no branch.<name>.* config yet, so it resolves through
// remote.pushDefault to "origin": unchanged for the common case.
//
// Scope: this fixes delivery only. resolveChangeRemote still reads the change's
// forge/owner/repo from "origin", so the two can name different repos in a fork
// setup, and a repo with no "origin" at all fails there before reaching here.
//
// Not strategy.ResolveCheckpointSyncRemote — see
// strategy.DeclaredPushRemoteForBranch for why those two questions differ.
func resolveChangePushRemote(ctx context.Context, branch string) (string, error) {
	remote := strategy.DeclaredPushRemoteForBranch(ctx, branch)
	if remote == "" {
		return defaultChangePushRemote, nil
	}
	// The name reaches git as an argv element, so the hazard is a leading "-"
	// being read as a flag (`--upload-pack=...`), not shell metacharacters.
	//
	// Deliberately narrower than validateGitRemoteName: that whitelist vets names
	// Entire is about to WRITE into .git/config and is stricter than git itself.
	// Applied to config the user already has, it rejects values git accepts
	// wherever a <repository> goes — "." (the local repo) and a bare URL both fail
	// its leading-alphanumeric rule — turning a working repo into a hard failure.
	if strings.HasPrefix(remote, "-") {
		return "", fmt.Errorf("branch %q declares push remote %q, which git would read as a command-line flag", branch, remote)
	}
	return remote, nil
}

// parseChangeRepoArg parses an explicit --repo value into the forge/owner/repo
// triple. It accepts the canonical "forge/owner/repo" form (e.g. gh/acme/app)
// as well as a full clone URL (https://, git@, or entire://) that gitremote
// can parse. A trailing ".git" on the repo is stripped.
func parseChangeRepoArg(raw string) (forge, owner, repo string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", errors.New("empty --repo value")
	}
	// Bare path form: forge/owner/repo. URLs (with a scheme or an SCP "@")
	// fall through to the URL parser, which understands hosts and schemes.
	if !strings.Contains(raw, "://") && !strings.Contains(raw, "@") {
		parts := strings.Split(strings.Trim(raw, "/"), "/")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return "", "", "", fmt.Errorf("invalid --repo %q: expected forge/owner/repo (e.g. gh/acme/app) or a clone URL", raw)
		}
		// parts[0] must be a short forge id ("gh"), not a hostname. A host-like
		// value (github.com/acme/app) would otherwise be forwarded verbatim and
		// the server would reject the malformed path with an opaque error.
		if !gitremote.IsSupportedForge(parts[0]) {
			return "", "", "", fmt.Errorf("invalid --repo %q: %q is not a supported forge id (use a forge id like \"gh\", or pass a clone URL such as https://github.com/%s/%s)", raw, parts[0], parts[1], parts[2])
		}
		return parts[0], parts[1], strings.TrimSuffix(parts[2], ".git"), nil
	}
	info, perr := gitremote.ParseURL(raw)
	if perr != nil {
		return "", "", "", fmt.Errorf("invalid --repo %q: %w", raw, perr)
	}
	if info.Forge == "" {
		return "", "", "", fmt.Errorf("invalid --repo %q: unsupported forge host (supported: github.com)", raw)
	}
	return info.Forge, info.Owner, info.Repo, nil
}

// checkChangeResponse checks the API response and returns user-friendly errors.
// For auth failures, it appends a hint to re-authenticate while preserving the server's error message.
func checkChangeResponse(resp *http.Response) error {
	if err := api.CheckResponse(resp); err != nil {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w — run 'entire login' to re-authenticate", err)
		}
		return fmt.Errorf("change API: %w", err)
	}
	return nil
}

// resolveCreateBranch picks the branch a non-interactive `change create` targets.
// An explicit --branch always wins. Otherwise, on a feature branch it uses the
// checked-out branch; when the checked-out branch IS the base (the change's
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

func cleanupCreatedChangeBranch(ctx context.Context, repo *git.Repository, remote, branchName string, localCreated, remotePushed bool, errW io.Writer) {
	localRemoved := !localCreated
	if localCreated {
		branchRef := plumbing.NewBranchReferenceName(branchName)
		if head, err := repo.Head(); err == nil && head.Name() == branchRef {
			fmt.Fprintf(errW, "Warning: not deleting local branch %s after change creation failed because it is checked out; switch branches and run 'git branch -D %s' if you do not need it\n", branchName, branchName)
		} else if err := repo.Storer.RemoveReference(branchRef); err != nil {
			fmt.Fprintf(errW, "Warning: failed to delete local branch %s after change creation failed: %v; run 'git branch -D %s' if you do not need it\n", branchName, err, branchName)
		} else {
			localRemoved = true
		}
	}
	if remotePushed {
		if !localRemoved {
			fmt.Fprintf(errW, "Warning: not deleting remote branch %s after change creation failed because local cleanup did not complete; run 'git push %s --delete %s' if you do not need it\n", branchName, remote, branchName)
			return
		}
		if err := deleteBranchFromRemote(ctx, remote, branchName); err != nil {
			fmt.Fprintf(errW, "Warning: failed to delete remote branch %s after change creation failed: %v\n", branchName, err)
		}
	}
}

func fetchBranchFromRemote(ctx context.Context, remote, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ValidateBranchName(ctx, branchName); err != nil {
		return err
	}
	refspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branchName, branchName)
	cmd := exec.CommandContext(ctx, "git", "fetch", "--no-tags", remote, refspec)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// pushBranchToRemote pushes a branch to remote, which callers resolve through
// resolveChangePushRemote rather than assuming "origin".
func pushBranchToRemote(ctx context.Context, remote, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "push", "--no-verify", "-u", remote, branchName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// remoteHasBranch reports whether remote already has a branch with the given
// name, so callers can avoid treating a pre-existing remote branch as one they
// created.
//
// Deliberately not the exported BranchExistsOnRemote in git_operations.go, whose
// name this would otherwise shadow by case alone: that one is origin-only and
// reports an ls-remote failure as "branch absent". Here a failed check must stay
// distinguishable from a definite "absent", because callers use the answer to
// decide whether they created the remote branch — and therefore whether cleanup
// may delete it.
func remoteHasBranch(ctx context.Context, remote, branchName string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", remote, branchName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return strings.TrimSpace(string(output)) != "", nil
}

func deleteBranchFromRemote(ctx context.Context, remote, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "push", "--no-verify", remote, "--delete", branchName)
	if output, err := cmd.CombinedOutput(); err != nil {
		outputText := strings.TrimSpace(string(output))
		if strings.Contains(outputText, "remote ref does not exist") {
			return nil
		}
		return fmt.Errorf("%s: %w", outputText, err)
	}
	return nil
}
