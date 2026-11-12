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
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"

	"charm.land/lipgloss/v2"
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
	// trailListServerMaxLimit is entire-api's maximum perPage.
	trailListServerMaxLimit = 100
	// trailFindMaxPages bounds a branch/ID lookup at a 2,000-trail search budget.
	trailFindMaxPages = 20
)

func trailContextBlurb() string {
	return "A trail captures project intent across repositories and branches. Manage intent and discussions on the whole trail; use --repo and --branch to select context for checkout, sessions, findings, and approvals."
}

func legacyTrailContextBlurb() string {
	return "A trail ties together the context for a branch. Use `entire trail` to view, create, update, or watch it; use `entire trail finding` to manage agent findings."
}

func newTrailCmd() *cobra.Command {
	return newTrailCmdForMode(projectTrailsEnabled())
}

// newTrailCmdForMode is where the trail model is chosen: each model registers
// its own intent-level commands, and the shared branch-level commands receive
// the model's trailMode.
func newTrailCmdForMode(project bool) *cobra.Command {
	mode := trailModeFor(project)
	var insecureHTTPAuth bool
	var repoOverride string

	cmd := &cobra.Command{
		Use:         cmdTrail,
		Short:       mode.help("Manage trails for your branches", "Manage trails across repositories and branches"),
		Annotations: map[string]string{},
		Args:        cobra.NoArgs,
		Long:        mode.help(legacyTrailContextBlurb(), trailContextBlurb()),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().BoolVar(&insecureHTTPAuth, "insecure-http-auth", false,
		"Allow API calls over plain HTTP (insecure, for local development only)")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}

	cmd.PersistentFlags().StringVar(&repoOverride, "repo", "", mode.help(
		"Target repository as forge/owner/repo (e.g. gh/acme/app) or a clone URL; defaults to the origin remote",
		"Target repository as forge/owner/repo (e.g. gh/acme/app) or a clone URL; for list, only filters the required --project"))

	if project {
		// Project intent can exist without any repository or repo-trails
		// toggle, so the project tree stays discoverable regardless.
		cmd.Annotations[projectTrailsAnnotation] = agentHelpAnnotationEnabled
		cmd.PersistentFlags().String("project", "", "Project as gh/<owner> or et/<project>; required for list, otherwise defaults to the repository's namespace")
		cmd.AddCommand(newProjectTrailShowCmd(), newProjectTrailListCmd(), newProjectTrailCreateCmd(), newProjectTrailUpdateCmd())
		cmd.AddCommand(newTrailLinkCmd(), newTrailUnlinkCmd(), newProjectTrailCommentCmd(), newProjectTrailAttachmentCmd())
	} else {
		// Advertised to agents only when trails are enabled for the repo, so
		// we never point agents at trails they can't use.
		cmd.Annotations[agentHelpRequiresTrailsAnnotation] = agentHelpAnnotationEnabled
		cmd.AddCommand(newTrailShowCmd(), newTrailListCmd(), newTrailDeleteCmd(), newTrailCommentCmd())
	}
	cmd.AddCommand(newTrailCheckoutCmd(mode), newTrailResumeCmd(mode), newTrailFindingCmd(mode), newTrailWatchCmd(mode))
	cmd.AddCommand(newTrailTimelineCmd(mode), newTrailSessionsCmd(mode), newTrailStackCmd(mode))
	cmd.AddCommand(newTrailApproveCmd(mode), newTrailRequestChangesCmd(), newTrailApprovalsCmd(mode), newTrailMergeCmd(mode))
	cmd.AddCommand(newTrailReadyCmd(mode), newTrailRetargetCmd(mode))
	cmd.AddCommand(newTrailUpdateBranchCmd(mode), newTrailCIPullRequestCmd(mode), newTrailRerunReviewCmd(mode))

	return cmd
}

// trailInsecureHTTP reads the persistent --insecure-http-auth flag from the trail root command.
func trailInsecureHTTP(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("insecure-http-auth") //nolint:errcheck // flag is always registered
	return v
}

// trailRepoFlag reads the persistent --repo flag from the trail command tree.
// It is always registered on the trail root, so a missing flag (empty string)
// just means "derive from origin".
func trailRepoFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("repo") //nolint:errcheck // flag is always registered on the trail root
	return strings.TrimSpace(v)
}

// trailBranchFlag reads an optional --branch flag. Only some subcommands
// register it; on the rest GetString errors and we treat it as unset.
func trailBranchFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("branch") //nolint:errcheck // absent on commands that don't register --branch
	return strings.TrimSpace(v)
}

// ensureNoTrailRepoOverride rejects --repo for commands that operate on the
// local clone and so cannot target an arbitrary repository.
func ensureNoTrailRepoOverride(cmd *cobra.Command, op string) error {
	if trailRepoFlag(cmd) != "" {
		return fmt.Errorf("--repo is not supported for %q because it operates on the local clone", op)
	}
	return nil
}

// ensureTrailRepoHasTarget requires an explicit branch or selector when --repo
// targets another repo; otherwise the command would resolve the local branch
// against the wrong repo. hint names the acceptable targets.
func ensureTrailRepoHasTarget(cmd *cobra.Command, hasTarget bool, hint string) error {
	if trailRepoFlag(cmd) != "" && !hasTarget {
		return fmt.Errorf("--repo requires an explicit target: %s", hint)
	}
	return nil
}

// trailListOptions are the inputs to runTrailListAll. Keeping them on a
// struct avoids a long positional argument list at the two call sites.
type trailListOptions struct {
	Author       string
	Status       string
	JSON         bool
	Limit        int
	InsecureHTTP bool
	// Repo is an optional --repo override (forge/owner/repo or a clone URL);
	// empty means derive the repo from the origin remote.
	Repo string
}

// trailShowOptions are the inputs to runTrailShow. Keeping them on a struct
// avoids a long positional argument list, matching trailListOptions.
type trailShowOptions struct {
	// Selector picks the trail by number, id, or branch. Empty means "the trail
	// for Branch", and an empty Branch in turn means the current branch.
	Selector string
	// Branch shows the trail for this branch instead of the current branch.
	Branch string
	// Repo is an optional --repo override (forge/owner/repo or a clone URL);
	// empty means derive the repo from the origin remote.
	Repo         string
	JSON         bool
	InsecureHTTP bool
}

func newTrailShowCmd() *cobra.Command {
	var opts trailShowOptions
	// branchFlag is bound only so cobra has somewhere to write --branch; RunE
	// reads it back through trailBranchFlag (which trims) along with every other
	// field, so a value reaches opts exactly one way.
	var branchFlag string
	cmd := &cobra.Command{
		Use:   "show [<trail>]",
		Short: "Show a trail",
		Long: `Show a trail.

If <trail> is omitted, shows the trail for the current branch (or --branch).
Otherwise, <trail> may be a trail number, id, or branch in the target repo.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			if selector != "" && trailBranchFlag(cmd) != "" {
				return errors.New("pass a trail selector or --branch, not both")
			}
			if err := ensureTrailRepoHasTarget(cmd, selector != "" || trailBranchFlag(cmd) != "", "pass a trail selector or --branch"); err != nil {
				return err
			}
			opts.Selector = selector
			opts.Branch = trailBranchFlag(cmd)
			opts.Repo = trailRepoFlag(cmd)
			opts.InsecureHTTP = trailInsecureHTTP(cmd)
			return runTrailShow(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Show the trail for this branch instead of the current branch; cannot be combined with a trail selector")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output the trail as JSON (one entry of 'trail list --json' plus a mergeability snapshot, null when unknown)")
	return cmd
}

// runTrailShow shows one trail, defaulting to the current branch's trail.
func runTrailShow(ctx context.Context, w, errW io.Writer, opts trailShowOptions) error {
	return runAuthenticatedTrailAPI(ctx, errW, opts.InsecureHTTP, opts.Repo, func(ctx context.Context, client *api.Client, repoID string) error {
		forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, opts.Repo)
		if err != nil {
			return err
		}
		basePath, err := trailRepoBasePath(forge, owner, repo, repoID)
		if err != nil {
			return err
		}
		return runTrailShowWithClientAtPath(ctx, w, errW, client, basePath, forge, owner, repo, opts)
	})
}

// runTrailShowWithClientAtPath renders one trail once the repo route and API
// client are resolved. Warnings go to errW, so --json keeps stdout parseable
// even when the best-effort description fetch fails.
func runTrailShowWithClientAtPath(ctx context.Context, w, errW io.Writer, client *api.Client, basePath, forge, owner, repo string, opts trailShowOptions) error {
	found, err := resolveTrailBySelectorAtPath(ctx, client, basePath, forge, owner, repo, opts.Selector, opts.Branch)
	if err != nil {
		return err
	}

	// Enrich the list result with the detail endpoint, which carries the
	// rendered description (trail.body_document.text_snapshot) and the
	// mergeability snapshot the list omits, and surface a browser URL. The
	// detail fetch is best-effort: the core metadata already came from the
	// list, so a detail failure falls back to the list body, leaves
	// mergeability unknown, and warns rather than failing.
	m := found.ToMetadata()
	m.URL = trailDisplayURL(*found, forge, owner, repo)
	// Seed the description from the list body so a failed (or skipped)
	// detail fetch still shows something; a successful detail fetch
	// supersedes it with the richer body_document text below.
	bodyText := found.Body
	descriptionLoaded := strings.TrimSpace(found.Body) != ""
	// A numeric selector already resolved through the detail route, so the
	// detail is in hand — re-requesting the same URL would double the round
	// trips for every `trail show <number>`.
	var detail *api.TrailResource
	switch {
	case found.FromDetail:
		detail = found
	case found.Number > 0:
		if d, derr := fetchTrailDetailAtPath(ctx, client, basePath, found.Number); derr == nil {
			detail = &d
		} else {
			// Best-effort: warn but still render metadata + URL (and the
			// list body) rather than failing the whole command.
			fmt.Fprintf(errW, "Warning: could not load trail detail (description, mergeability): %v\n", derr)
		}
	}
	var mergeability *api.TrailMergeability
	if detail != nil {
		// A successful fetch means we authoritatively consulted the
		// description, but it only supersedes the seeded list body when it
		// actually carries text: an older/partial server that omits
		// body_document must not blank out a list body that is present.
		descriptionLoaded = true
		if detail.BodyDocument != nil {
			if snapshot := strings.TrimSpace(detail.BodyDocument.TextSnapshot); snapshot != "" {
				bodyText = snapshot
			}
		}
		// Best-effort like the description: a snapshot the CLI cannot decode
		// leaves the verdict unknown rather than failing the command.
		if mg, merr := detail.DecodeMergeability(); merr == nil {
			mergeability = mg
		} else {
			fmt.Fprintf(errW, "Warning: could not read trail mergeability: %v\n", merr)
		}
	}
	// The list body is the weaker source; carry the resolved description on the
	// metadata so JSON callers read the same text the human view renders.
	m.Body = bodyText

	if opts.JSON {
		// Emit the raw body — never the "no description" placeholder, which is
		// display text, not data. A single object mirrors one entry of
		// `trail list --json` so both can feed the same parser; the
		// detail-only mergeability snapshot is the one addition.
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(trailShowJSON{Metadata: m, Mergeability: toTrailMergeabilityJSON(mergeability)}); err != nil {
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
		return nil
	}

	printTrailDetails(w, m, m.URL, mergeability, trailDescriptionForDisplay(bodyText, descriptionLoaded))
	return nil
}

// resolveTrailBySelectorAtPath resolves a trail by an optional selector (trail
// number, id, or branch). An empty selector falls back to the current branch's
// trail. It returns an actionable error (never a nil trail with a nil error)
// when nothing matches, so callers can rely on a non-nil result.
func resolveTrailBySelectorAtPath(ctx context.Context, client *api.Client, basePath, forge, owner, repo, selector, branchOverride string) (*api.TrailResource, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		branch, err := resolveTrailBranch(ctx, branchOverride)
		if err != nil {
			return nil, fmt.Errorf("no trail selector given and current branch is unknown: %w\nhint: run 'entire trail list --status any' or pass a trail number, id, or branch", err)
		}
		found, err := findTrailByBranchAtPath(ctx, client, basePath, branch)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, fmt.Errorf("no trail found for current branch %q\nhint: run 'entire trail create' or 'entire trail list --status any'", branch)
		}
		return found, nil
	}
	found, err := findTrailBySelectorAtPath(ctx, client, basePath, selector)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("no trail %q found in %s/%s/%s (run 'entire trail list --status any')", selector, forge, owner, repo)
	}
	return found, nil
}

// printTrailDetails renders the human `trail show` view. A nil mergeability
// means the detail could not be loaded, and renders as unknown.
func printTrailDetails(w io.Writer, m *trail.Metadata, webURL string, mergeability *api.TrailMergeability, bodyText string) {
	// Color the same fields as the list view (STATUS/PHASE/AUTHOR); everything
	// else stays plain. Values are pre-colored, so alignment is unaffected.
	styles := newStatusStyles(w)
	status := string(m.Status)
	if style, ok := trailStatusColor(styles, m.Status); ok {
		status = styles.render(style, status)
	}
	author := m.AuthorLogin()
	if styles.colorEnabled && author != "" {
		author = styles.render(styles.cyan, author)
	}

	// Field labels and the title line render yellow (matching the list header).
	label := func(s string) string { return styles.render(styles.yellow, s) }

	fmt.Fprintf(w, "%s\n", styles.render(styles.yellow, "Trail: "+m.Title))
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
		phase := trailPhaseDisplay(m.Phase)
		if styles.colorEnabled {
			phase = styles.render(styles.yellow, phase)
		}
		fmt.Fprintf(w, "  %s%s\n", label("Phase:   "), phase)
	}
	if webURL != "" {
		fmt.Fprintf(w, "  %s%s\n", label("URL:     "), tuiutil.SanitizeTerminalText(webURL))
	}
	if m.ProjectTrailURL != "" {
		fmt.Fprintf(w, "  %s%s\n", label("Project trail: "), tuiutil.SanitizeTerminalText(m.ProjectTrailURL))
	}
	if len(m.Labels) > 0 {
		fmt.Fprintf(w, "  %s%s\n", label("Labels:  "), strings.Join(m.Labels, ", "))
	}
	if strings.TrimSpace(string(m.Type)) != "" {
		fmt.Fprintf(w, "  %s%s\n", label("Type:      "), m.Type)
	}
	if p := strings.TrimSpace(string(m.Priority)); p != "" && p != string(trail.PriorityNone) {
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
	printTrailMergeability(w, styles, label, mergeability)
	if strings.TrimSpace(bodyText) != "" {
		fmt.Fprintf(w, "\n%s\n%s\n", label("Description:"), bodyText)
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

// trailDisplayURL returns the trail's browser URL. It prefers the canonical URL
// the server now returns, so the CLI tracks any route change without being
// updated in lockstep, and falls back to a locally constructed URL only for
// older servers that omit the field.
func trailDisplayURL(t api.TrailResource, forge, owner, repo string) string {
	if strings.TrimSpace(t.URL) != "" {
		return t.URL
	}
	base, err := auth.WebBaseURL()
	if t.Number > 0 && err == nil {
		return trailWebURL(base, forge, owner, repo, t.Number)
	}
	return ""
}

// trailWebURL builds a fallback browser URL for a trail used only when the
// server does not supply one (older servers):
// <web-origin>/<forge>/<owner>/<repo>/trails/<number>. The web origin comes
// from auth.WebBaseURL; a split local-dev setup (API and frontend on different
// ports) sets ENTIRE_WEB_BASE_URL to reach the dev frontend.
func trailWebURL(base, forge, owner, repo string, number int) string {
	return strings.TrimRight(base, "/") + "/" + forge + "/" + owner + "/" + repo + "/trails/" + strconv.Itoa(number)
}

// fetchTrailDetailAtPath fetches a trail's detail resource by integer number.
// The detail carries what the list endpoint omits: the rendered description
// (body_document) and the mergeability snapshot.
func fetchTrailDetailAtPath(ctx context.Context, client *api.Client, basePath string, number int) (api.TrailResource, error) {
	resp, err := client.Get(ctx, trailNumberPathForBase(basePath, number))
	if err != nil {
		return api.TrailResource{}, fmt.Errorf("failed to fetch trail detail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailResource{}, err
	}
	detail, err := decodeTrailResource(resp)
	if err != nil {
		return api.TrailResource{}, fmt.Errorf("failed to decode trail detail: %w", err)
	}
	return detail, nil
}

// decodeTrailResource decodes entire-api's direct detail resource and marks it
// FromDetail.
func decodeTrailResource(resp *http.Response) (api.TrailResource, error) {
	var resource api.TrailResource
	if err := api.DecodeJSON(resp, &resource); err != nil {
		return api.TrailResource{}, fmt.Errorf("decode trail resource: %w", err)
	}
	resource.FromDetail = true
	return resource, nil
}

func runTrailListAll(ctx context.Context, w, errW io.Writer, opts trailListOptions) error {
	statusFilters, err := validateTrailListOptions(opts)
	if err != nil {
		return err
	}
	return runAuthenticatedTrailAPI(ctx, errW, opts.InsecureHTTP, opts.Repo, func(ctx context.Context, client *api.Client, repoID string) error {
		return runTrailListAllWithClient(ctx, w, client, repoID, opts, statusFilters)
	})
}

func validateTrailListOptions(opts trailListOptions) ([]trail.Status, error) {
	if opts.Limit <= 0 {
		return nil, errors.New("limit must be greater than 0")
	}
	return parseTrailStatusFilter(opts.Status)
}

func runTrailListAllWithClient(ctx context.Context, w io.Writer, client *api.Client, repoID string, opts trailListOptions, statusFilters []trail.Status) error {
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

	forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, opts.Repo)
	if err != nil {
		return err
	}

	// entire-api uses opaque cursor pagination and a 100-row page cap. Walk as
	// many pages as needed for --limit. Author login is filtered client-side:
	// the new backend's author query is account-ID based, while the CLI's public
	// flag has always accepted GitHub logins.
	basePath, err := trailRepoBasePath(forge, owner, repo, repoID)
	if err != nil {
		return err
	}
	resources, totalMatched, err := listTrailResources(ctx, client, basePath, statusFilters, authorFilter, opts.Limit)
	if err != nil {
		return err
	}

	// Convert to metadata for display, attaching the browser URL (entire-api does
	// not compose the web-app URL on this lower-level surface).
	trails := make([]*trail.Metadata, 0, len(resources))
	for i := range resources {
		m := resources[i].ToMetadata()
		m.URL = trailDisplayURL(resources[i], forge, owner, repo)
		trails = append(trails, m)
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

	return nil
}

func listTrailResources(ctx context.Context, client *api.Client, basePath string, statuses []trail.Status, author string, limit int) ([]api.TrailResource, int, error) {
	if limit <= 0 {
		return nil, 0, errors.New("limit must be greater than 0")
	}
	items := make([]api.TrailResource, 0, min(limit, trailListServerMaxLimit))
	cursor := ""
	seenTokens := map[string]bool{}
	totalMatched := 0
	for {
		perPage := trailListServerMaxLimit
		if author == "" && limit-len(items) < perPage {
			perPage = limit - len(items)
		}
		resp, err := client.Get(ctx, basePath+trailListPageQuery(statuses, perPage, cursor))
		if err != nil {
			return nil, 0, fmt.Errorf("failed to list trails: %w", err)
		}
		var page api.TrailListResponse
		decodeErr := func() error {
			defer resp.Body.Close()
			if err := checkTrailResponse(resp); err != nil {
				return err
			}
			if err := api.DecodeJSON(resp, &page); err != nil {
				return fmt.Errorf("failed to decode trail list: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return nil, 0, decodeErr
		}

		for _, resource := range page.Trails {
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
		// satisfied rather than walking the repository's complete trail history.
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
		if page.NextCursor == nil || strings.TrimSpace(*page.NextCursor) == "" {
			break
		}
		cursor = strings.TrimSpace(*page.NextCursor)
		if seenTokens[cursor] {
			return nil, 0, fmt.Errorf("trail list pagination repeated cursor %q", cursor)
		}
		seenTokens[cursor] = true
	}
	return items, totalMatched, nil
}

// trailListPageQuery builds entire-api's cursor-paginated list query. Empty
// statuses (--status any) omit the filter. Author is intentionally absent: the
// CLI accepts a login while this API's author filter accepts account ULIDs.
//
// Filters ride only the FIRST page on purpose (RFD-026 §8): the opaque cursor
// carries the active filters, the server restores them on every continuation,
// and it 400s a continuation whose repeated filters conflict with the
// cursor's. Later pages are therefore filtered by the cursor, not unfiltered.
func trailListPageQuery(statusFilters []trail.Status, perPage int, cursor string) string {
	q := url.Values{}
	if cursor == "" && len(statusFilters) > 0 {
		parts := make([]string, len(statusFilters))
		for i, status := range statusFilters {
			parts[i] = string(status)
		}
		q.Set("status[eq]", strings.Join(parts, ","))
	}
	if perPage <= 0 || perPage > trailListServerMaxLimit {
		perPage = trailListServerMaxLimit
	}
	q.Set("per_page", strconv.Itoa(perPage))
	if strings.TrimSpace(cursor) != "" {
		q.Set("cursor", strings.TrimSpace(cursor))
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
	// TotalMatched is the best available match count. It is exact when the
	// backend returns a filtered total; login-filtered entire-api lists stop at
	// --limit and therefore report only the displayed count.
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
	styles := newStatusStyles(w)
	showPhase := trailListHasPhase(trails)
	showURL := trailListHasURL(trails)

	// The leading two-space indent is folded into the first column so the shared
	// table renderer (columnWidths/writeTableRow) reproduces the list's layout.
	headers := []string{"  NUM", colHeaderBranch, colHeaderTitle}
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

	rows := make([][]string, len(trails))
	for i, t := range trails {
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
			status := trailStatusDisplay(t.Status)
			if style, ok := trailStatusColor(styles, t.Status); ok {
				status = styles.render(style, status)
			}
			fields = append(fields, status)
		}
		if showPhase {
			phase := trailPhaseDisplay(t.Phase)
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

// trailStatusColor returns the style for a trail status: open green, merged
// magenta, closed red. draft (the in-progress/building state) and any unknown
// status stay uncolored. The colors avoid AUTHOR's cyan and PHASE's yellow so
// the columns stay distinguishable.
func trailStatusColor(styles statusStyles, status trail.Status) (lipgloss.Style, bool) {
	switch status {
	case trail.StatusOpen:
		return styles.green, true
	case trail.StatusMerged:
		return styles.magenta, true
	case trail.StatusClosed:
		return styles.red, true
	case trail.StatusDraft:
		return lipgloss.Style{}, false
	default:
		return lipgloss.Style{}, false
	}
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

func ensureTrailCreateBranchExists(ctx context.Context, w io.Writer, repo *git.Repository, remote, branch string, existedOnRemote bool) error {
	if existedOnRemote {
		if err := fetchBranchFromRemote(ctx, remote, branch); err != nil {
			return fmt.Errorf("failed to fetch branch %q from %q: %w", branch, remote, err)
		}
		fmt.Fprintf(w, "Fetched branch %s from %s\n", branch, remote)
		return nil
	}
	if err := createBranch(repo, branch); err != nil {
		return fmt.Errorf("failed to create branch %q: %w", branch, err)
	}
	fmt.Fprintf(w, "Created branch %s\n", branch)
	return nil
}

func validateTrailCreateEnums(cmd *cobra.Command, typeStr, priorityStr string) error {
	if cmd.Flags().Changed("type") {
		if !trail.Type(strings.TrimSpace(typeStr)).IsValid() {
			return fmt.Errorf("invalid type %q: valid values are %s", typeStr, formatValidTypes())
		}
	}
	if cmd.Flags().Changed("priority") {
		if !trail.Priority(strings.TrimSpace(priorityStr)).IsValid() {
			return fmt.Errorf("invalid priority %q: valid values are %s", priorityStr, formatValidPriorities())
		}
	}
	return nil
}

// formatValidTypes lists the valid trail types for error messages.
func formatValidTypes() string {
	parts := make([]string, 0, len(trail.ValidTypes()))
	for _, t := range trail.ValidTypes() {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ", ")
}

// formatValidPriorities lists the valid trail priorities for error messages.
func formatValidPriorities() string {
	parts := make([]string, 0, len(trail.ValidPriorities()))
	for _, p := range trail.ValidPriorities() {
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

// sendTrailPatch sends a metadata update and returns the trail as the server
// saved it.
func sendTrailPatch(ctx context.Context, client *api.Client, path string, req api.TrailUpdateRequest) (*api.TrailResource, error) {
	resp, err := client.Patch(ctx, path, req)
	if err != nil {
		return nil, fmt.Errorf("failed to update trail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}
	var updateResp api.TrailUpdateResponse
	if err := api.DecodeJSON(resp, &updateResp); err != nil {
		return nil, fmt.Errorf("failed to decode update response: %w", err)
	}
	return &updateResp.Trail, nil
}

func newTrailCheckoutCmd(mode *trailMode) *cobra.Command {
	var trailSelector string
	var force bool
	var worktree bool
	var branch string

	cmd := &cobra.Command{
		Use:   "checkout [<trail>]",
		Short: "Check out a trail's branch",
		Long: "Check out the branch of a trail.\n\n" + mode.help(`The trail may be given as the first argument or via --trail, as a number, id, or
branch. Without one, the trail for the current branch is used. The trail's branch
is checked out, fetching it from origin first when it only exists there.`, `The trail may be given as the first argument or via --trail, as a project number,
ID, or <repo>/<number>. Without one, the current branch's parent is used. --branch selects the
working branch; otherwise the current branch or sole branch in this repository
is selected. Otherwise the open changes are listed; pass one as <repo>/<number>.
Remote-only branches are fetched.`) + `

With --worktree, the branch is checked out into a git worktree under
.entire/worktrees at the repo root instead of switching this checkout, and the
command prints a cd command for the new worktree. Gitignored files matching
.worktreeinclude patterns are copied into the worktree. When stdout is not a
terminal, only the worktree path is printed, so scripts can use
cd "$(entire trail checkout <trail> --worktree)".

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
			if err := ensureNoTrailRepoOverride(cmd, "trail checkout"); err != nil {
				return err
			}
			selected, err := mode.workingContext(cmd, selector, branch, true)
			if err != nil {
				return err
			}
			return checkoutTrailBranch(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &selected.Work, trailCheckoutOptions{Force: force, Worktree: worktree})
		},
	}

	cmd.Flags().StringVar(&trailSelector, "trail", "", mode.help(
		"Trail to check out (number, id, or branch; defaults to the current branch's trail)",
		"Project trail number, ID, or <repo>/<number> (defaults to the current branch's trail)"))
	cmd.Flags().StringVar(&branch, "branch", "", mode.help(
		"Resolve the trail for this branch instead of the current branch",
		"Select a branch within the trail in this repository"))
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip the prompt before fetching a remote-only branch")
	cmd.Flags().BoolVar(&worktree, "worktree", false, "Check out the trail branch in a worktree under .entire/worktrees instead of switching this checkout")

	return cmd
}

type trailCheckoutOptions struct {
	Force    bool
	Worktree bool
}

func checkoutTrailBranch(ctx context.Context, w, errW io.Writer, found *api.TrailResource, opts trailCheckoutOptions) error {
	branch := strings.TrimSpace(found.Branch)
	if branch == "" {
		return fmt.Errorf("%s has no branch to check out", describeTrailRef(found))
	}
	if opts.Worktree {
		fmt.Fprintf(errW, "Checking out %s in a worktree\n", describeTrailRef(found))
		// The branch work's own number, not a project parent's: every branch
		// of a project trail shares the parent number, and sanitized branch
		// names collide (feature/x, feature-x).
		return checkoutTrailWorktree(ctx, w, errW, branch, opts.Force, found.Number)
	}
	currentBranch, _ := GetCurrentBranch(ctx) //nolint:errcheck // detached HEAD means not already on the branch
	if currentBranch == branch {
		fmt.Fprintf(w, "Already on branch %s for %s.\n", branch, describeTrailRef(found))
		return nil
	}
	fmt.Fprintf(w, "Checking out %s\n", describeTrailRef(found))
	proceed, err := switchToBranchForResume(ctx, w, errW, branch, opts.Force)
	if err != nil {
		return err
	}
	if !proceed {
		fmt.Fprintf(w, "Checkout of branch %s cancelled.\n", branch)
	}
	return nil
}

// describeTrailRef renders a short human reference to a trail for status
// messages, e.g. "trail #575 (Add foo)" or, when the trail has no number yet,
// "trail \"Add foo\"".
func describeTrailRef(t *api.TrailResource) string {
	if t.Parent != nil {
		return fmt.Sprintf("trail #%d (branch %s)", t.Parent.Number, changeBranchName(*t))
	}
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

// Trail deletion was removed server-side (owned trails always 409). The command
// stays registered but hidden so existing scripts get a clear error pointing at
// the close workflow instead of "unknown command".
func newTrailDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "delete [<number>]",
		Short:  "Removed from the CLI",
		Args:   cobra.MaximumNArgs(1),
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return errTrailDeleteRemoved
		},
	}

	cmd.Flags().String("branch", "", "Unused")
	cmd.Flags().BoolP("force", "f", false, "Unused")

	return cmd
}

var errTrailDeleteRemoved = errors.New("trail delete was removed from the CLI")

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

func findTrailBySelectorAtPath(ctx context.Context, client *api.Client, basePath, selector string) (*api.TrailResource, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil //nolint:nilnil // empty selector means not found for this helper
	}
	if n, ok := parseTrailNumberSelector(selector); ok {
		found, err := findTrailByNumberAtPath(ctx, client, basePath, n)
		if err != nil || found != nil {
			return found, err
		}
	}
	return findTrailAtPath(ctx, client, basePath, func(t api.TrailResource) bool {
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

// findTrailByBranchAtPath looks up a trail by branch name via the list API.
func findTrailByBranchAtPath(ctx context.Context, client *api.Client, basePath, branch string) (*api.TrailResource, error) {
	return findTrailAtPath(ctx, client, basePath, func(t api.TrailResource) bool {
		return t.Branch == branch
	})
}

// findTrailByNumberAtPath looks up a trail by numeric identifier through
// entire-api's direct number route, so it never has to scan the list pages.
func findTrailByNumberAtPath(ctx context.Context, client *api.Client, basePath string, number int) (*api.TrailResource, error) {
	resp, err := client.Get(ctx, trailNumberPathForBase(basePath, number))
	if err != nil {
		return nil, fmt.Errorf("get trail %d: %w", number, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil //nolint:nilnil // nil, nil means "not found" to selector callers
	}
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}

	found, err := decodeTrailResource(resp)
	if err != nil {
		return nil, fmt.Errorf("decode trail %d: %w", number, err)
	}
	if found.ID == "" && found.Number == 0 {
		// A 2xx carrying nothing recognizable is "not found", not a trail whose
		// every field is zero: selector callers would otherwise act on a phantom.
		return nil, nil //nolint:nilnil // nil, nil means "not found" to selector callers
	}
	return &found, nil
}

func findTrailAtPath(ctx context.Context, client *api.Client, basePath string, match func(api.TrailResource) bool) (*api.TrailResource, error) {
	// Walk bounded opaque-cursor pages so selector lookups do not silently miss
	// trails beyond the first entire-api page.
	cursor := ""
	seenTokens := map[string]bool{}
	for range trailFindMaxPages {
		resp, err := client.Get(ctx, basePath+trailListPageQuery(nil, trailListServerMaxLimit, cursor))
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
		if listResp.NextCursor == nil || strings.TrimSpace(*listResp.NextCursor) == "" {
			break
		}
		cursor = strings.TrimSpace(*listResp.NextCursor)
		if seenTokens[cursor] {
			break
		}
		seenTokens[cursor] = true
	}
	return nil, nil //nolint:nilnil // nil, nil means "not found" — callers check both
}

// trailsBasePath returns the API path prefix for trails endpoints
// (e.g., "/api/v1/trails/gh/org/repo").
func trailsBasePath(forge, owner, repo string) string {
	return fmt.Sprintf("/api/v1/trails/%s/%s/%s", url.PathEscape(forge), url.PathEscape(owner), url.PathEscape(repo))
}

// trailRepoBasePath selects the repo-addressed route for Entire-native
// repositories. The host-addressed API deliberately cannot resolve et repos by
// name because the cell's full_name index has no forge discriminator; the
// opaque repo ID is unambiguous. GitHub repos retain the legacy route.
func trailRepoBasePath(forge, owner, repo, repoID string) (string, error) {
	if forge != nativeCloneForge {
		return trailsBasePath(forge, owner, repo), nil
	}
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return "", errors.New("cannot access trails for an Entire-native repo without its repo ID")
	}
	return "/api/v1/repos/" + url.PathEscape(repoID) + "/trails", nil
}

// trailNumberPathForBase returns the single-trail API path keyed by integer
// trail number. The server validates an integer here and rejects the trail UUID,
// so callers must pass Number, not ID.
func trailNumberPathForBase(basePath string, number int) string {
	return basePath + "/" + strconv.Itoa(number)
}

// resolveTrailRemote resolves the origin remote and ensures the forge is known
// to the trails API. Without this guard, an unmapped host (e.g. gitlab.com, or
// a misconfigured entire:// URL with no forge prefix) cannot select a valid
// repo-scoped trail route and would fail later with an opaque API error.
func resolveTrailRemote(ctx context.Context) (forge, owner, repo string, err error) {
	forge, owner, repo, err = gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return "", "", "", fmt.Errorf("failed to resolve repository: %w", err)
	}
	if forge == "" {
		return "", "", "", errors.New("origin remote is not on a forge supported by Entire trails (supported: github.com and Entire-native remotes)")
	}
	return forge, owner, repo, nil
}

// resolveTrailRepoOrRemote resolves the forge/owner/repo triple used to build
// trail API paths. An explicit --repo override (repoOverride) wins; otherwise
// it derives the triple from the origin remote of the current clone.
func resolveTrailRepoOrRemote(ctx context.Context, repoOverride string) (forge, owner, repo string, err error) {
	if repoOverride != "" {
		return parseTrailRepoArg(repoOverride)
	}
	return resolveTrailRemote(ctx)
}

// resolveTrailBranch returns the branch a trail lookup should use: an explicit
// --branch override (branchOverride) when given, otherwise the current branch.
func resolveTrailBranch(ctx context.Context, branchOverride string) (string, error) {
	if branchOverride != "" {
		return branchOverride, nil
	}
	return GetCurrentBranch(ctx)
}

// defaultTrailPushRemote is where a trail branch goes when git config declares
// nothing — git's own fallback for a bare push. Not defaultMirrorRemote, which
// shares the value but means "the remote `remote add` repoints".
const defaultTrailPushRemote = "origin"

// resolveTrailPushRemote returns the remote a trail's branch is delivered to,
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
// Scope: this fixes delivery only. resolveTrailRemote still reads the trail's
// forge/owner/repo from "origin", so the two can name different repos in a fork
// setup, and a repo with no "origin" at all fails there before reaching here.
//
// Not strategy.ResolveCheckpointSyncRemote — see
// strategy.DeclaredPushRemoteForBranch for why those two questions differ.
func resolveTrailPushRemote(ctx context.Context, branch string) (string, error) {
	remote := strategy.DeclaredPushRemoteForBranch(ctx, branch)
	if remote == "" {
		return defaultTrailPushRemote, nil
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

// parseTrailRepoArg parses an explicit --repo value into the forge/owner/repo
// triple. It accepts the canonical "forge/owner/repo" form (e.g. gh/acme/app)
// as well as a full clone URL (https://, git@, or entire://) that gitremote
// can parse. A trailing ".git" on the repo is stripped, on every forge: the
// suffix is never part of a name (see gitDirSuffix).
func parseTrailRepoArg(raw string) (forge, owner, repo string, err error) {
	return parseTrailRepoShape(raw)
}

// parseTrailRepoShape parses the two spellings --repo accepts — a bare
// forge/owner/repo triple, or a clone URL — without judging whether trails can
// act on the forge it found.
func parseTrailRepoShape(raw string) (forge, owner, repo string, err error) {
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
		if !gitremote.IsForgePathToken(parts[0]) {
			return "", "", "", fmt.Errorf("invalid --repo %q: %q is not a supported forge id (use a forge id like \"gh\", or pass a clone URL such as https://github.com/%s/%s)", raw, parts[0], parts[1], parts[2])
		}
		// Re-check the repo AFTER the trim: the emptiness check above ran on the
		// name as typed, and dropping the suffix can empty it (".git") or turn
		// it dot-only ("..git" → "."). Either would otherwise be forwarded as a
		// repo coordinate the suffix manufactured. See dotOnlyRe.
		repo, _ := gitremote.CutGitDirSuffix(parts[2])
		if repo == "" || dotOnlyRe.MatchString(repo) {
			return "", "", "", fmt.Errorf("invalid --repo %q: %q is not a repo name once the %s suffix is dropped", raw, parts[2], gitDirSuffix)
		}
		return parts[0], parts[1], repo, nil
	}
	info, perr := gitremote.ParseURL(raw)
	if perr != nil {
		return "", "", "", fmt.Errorf("invalid --repo %q: %w", raw, perr)
	}
	if info.Forge == "" {
		return "", "", "", fmt.Errorf("invalid --repo %q: unsupported forge host (supported: github.com and Entire-native remotes)", raw)
	}
	return info.Forge, info.Owner, info.Repo, nil
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

// trailBranchPushTimeout bounds the trail branch delivery, which now covers the
// pre-push hook as well as the network push.
//
// The hook's compute cost is smaller than it looks: OPF makes exactly one
// shell-out per push (see manual_commit_opf_rewrite.go and
// manual_commit_opf_refs.go), bounded by
// redaction.openai_privacy_filter.timeout_seconds — 30s by default — and an
// oversized first run is rejected outright by BootstrapTooLargeError or
// OPFRawBytesTooLargeError rather than allowed to run long. What is genuinely
// unbounded is the OPF prompt, which waits on a person. Ten minutes is sized to
// leave room for someone to answer it, not to cover a slow scan; two minutes
// was not enough for either.
//
// Known limitation: timeout_seconds is validated only as >= 0, so a value
// configured above this bound is truncated and the push is cut short. Deriving
// the bound from that setting is the fix if anyone hits it.
//
// What expiry kills is git, and only git: exec.CommandContext's default Cancel
// is Process.Kill() on the child's PID, no Setpgid is set, and no group signal
// is sent. The OPF re-redaction and the checkpoint ref CAS-updates do not run
// there — they run in the pre-push hook, a grandchild (.git/hooks/pre-push,
// which execs `entire hooks pre-push`). So the hook is orphaned, not
// interrupted: expiry cannot leave a half-rewritten ref or a stale lock the way
// StatusWalkBudget's SIGKILL can, because that one kills the process holding
// the lock and this one does not. Nor is expiry likely to land on a local write
// at all — the reason it fires is the OPF prompt, and pre-push runs before any
// transfer or ref update, so git is parked holding nothing.
//
// The orphan is the real cost, and it is deliberately accepted: the hook keeps
// the inherited terminal and can go on publishing checkpoint refs and printing
// after trail create has failed and begun retracting the branch. Killing the
// process group instead would trade that for the mid-rewrite kill this note
// used to describe, which is the worse of the two.
//
// A bound exists at all only because trail create is a multi-step command and a
// wedged push must not hang it forever. A plain `git push` has none — the hook
// runs undeadlined — so this ceiling is Entire's, not git's.
const trailBranchPushTimeout = 10 * time.Minute

// pushBranchToRemote pushes a branch to remote, which callers resolve through
// resolveTrailPushRemote rather than assuming "origin". This must run Git's
// pre-push hooks: Entire's hook publishes any checkpoint data that was captured
// before the trail branch was created.
//
// Because the hook runs, this has to be a real push rather than an internal
// subprocess whose output nobody reads, so stdout and stderr stream to the
// caller's writers instead of into a CombinedOutput() buffer read only on
// failure. Two things went wrong while that buffer swallowed them:
//
//   - Entire's hook reports on stderr both when it pushes checkpoint refs and
//     when it withholds them, and on the git-refs backend it withholds them
//     while still exiting zero. So trail create could print "Pushed branch" for
//     a push that published nothing — the failure this path exists to prevent,
//     with its only diagnostic discarded.
//   - The OPF prompt became unanswerable-in-practice rather than skipped.
//     interactive.CanPromptInteractively() decides from a /dev/tty probe and
//     never consults stdio, so it still said yes; bubbletea then falls back to
//     /dev/tty for prompt INPUT when stdin is not a terminal but has no such
//     fallback for OUTPUT, which is unconditionally os.Stdout. The form
//     therefore rendered into the buffer while reading real keystrokes: a push
//     that blocks on a prompt the user cannot see.
//
// Both fixes rest on the writers arriving here being real *os.File values:
// os/exec hands an *os.File's descriptor straight to the child, so the hook
// inherits the terminal, whereas any other io.Writer gets an os.Pipe and the
// child's stdout stops being a tty. Today they are — cobra's OutOrStdout and
// ErrOrStderr fall through to os.Stdout/os.Stderr, and nothing on the path to
// trail create calls SetOut/SetErr — so a pager or output tee added to the root
// command would silently undo this.
//
// It would undo more than the tty. Once os/exec has to interpose a pipe,
// Cmd.Wait blocks on its copy goroutine until every holder of the write end
// closes it — and an orphaned hook (see trailBranchPushTimeout) holds one.
// WaitDelay is unset, so that wait has no ceiling: the timeout above would stop
// bounding this call at all, which is the one thing it exists to do.
//
// stdin is deliberately NOT inherited, though a real push would inherit it,
// because nothing downstream reads it and /dev/null is what makes a
// non-interactive trail create fail fast rather than block to the timeout. The
// justification this note used to give — git prompting for credentials — does
// not hold: git push has no --stdin mode, git's credential prompt uses /dev/tty
// or askpass, the OPF prompt reads /dev/tty per bubbletea's fallback above, and
// the hook never receives ours anyway, since git hands it a pipe carrying the
// ref list. Inheriting it only converted an immediate EOF into a real blocking
// read for an agent whose stdin is a long-lived pipe.
func pushBranchToRemote(ctx context.Context, out, errOut io.Writer, remote, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, trailBranchPushTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "push", "-u", remote, branchName)
	cmd.Stdout = out
	cmd.Stderr = errOut
	// The hook only records the checkpoint upload; it runs once git push has
	// returned, so "Pushed branch" means the checkpoints went too. git's own
	// message already reached errOut on failure, so the error names the
	// command and carries the exit status rather than repeating the diagnostic.
	return strategy.RunPushThenUploadCheckpoints(ctx, cmd, remote, errOut) //nolint:wrapcheck // already names git push
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
