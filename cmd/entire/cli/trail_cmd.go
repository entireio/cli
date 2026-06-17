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
)

func newTrailCmd() *cobra.Command {
	var insecureHTTPAuth bool

	cmd := &cobra.Command{
		Use:    "trail",
		Short:  "Manage trails for your branches",
		Hidden: true,
		Args:   cobra.NoArgs,
		Long: `Trails are branch-centric work tracking abstractions. They describe the
"why" and "what" of your work, while checkpoints capture the "how" and "when".

Running 'entire trail' without a subcommand shows the trail for the current
branch, or lists recent trails if no trail exists for the current branch.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrailShow(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), insecureHTTPAuth)
		},
	}

	cmd.PersistentFlags().BoolVar(&insecureHTTPAuth, "insecure-http-auth", false,
		"Allow API calls over plain HTTP (insecure, for local development only)")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}

	cmd.AddCommand(newTrailListCmd())
	cmd.AddCommand(newTrailCreateCmd())
	cmd.AddCommand(newTrailUpdateCmd())
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

// runTrailShow shows the trail for the current branch, or falls through to list.
func runTrailShow(ctx context.Context, w, errW io.Writer, insecureHTTP bool) error {
	listOpts := defaultTrailListOptions(insecureHTTP)
	listStatusFilters, err := validateTrailListOptions(listOpts)
	if err != nil {
		return err
	}

	return runAuthenticatedDataAPI(ctx, errW, insecureHTTP, func(ctx context.Context, client *api.Client) error {
		branch, err := GetCurrentBranch(ctx)
		if err != nil {
			return runTrailListAllWithClient(ctx, w, client, listOpts, listStatusFilters)
		}

		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}

		found, err := findTrailByBranch(ctx, client, forge, owner, repo, branch)
		if err != nil {
			return err
		}
		if found == nil {
			return runTrailListAllWithClient(ctx, w, client, listOpts, listStatusFilters)
		}

		printTrailDetails(w, found.ToMetadata())
		return nil
	})
}

func printTrailDetails(w io.Writer, m *trail.Metadata) {
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
	if m.Body != "" {
		fmt.Fprintf(w, "  Body:    %s\n", m.Body)
	}
	if len(m.Labels) > 0 {
		fmt.Fprintf(w, "  Labels:  %s\n", strings.Join(m.Labels, ", "))
	}
	if len(m.Assignees) > 0 {
		fmt.Fprintf(w, "  Assignees: %s\n", strings.Join(m.Assignees, ", "))
	}
	fmt.Fprintf(w, "  Created: %s\n", m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(w, "  Updated: %s\n", m.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
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
	return runAuthenticatedDataAPI(ctx, errW, opts.InsecureHTTP, func(ctx context.Context, client *api.Client) error {
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

	// Convert to metadata for display
	trails := make([]*trail.Metadata, 0, len(listResp.Trails))
	for i := range listResp.Trails {
		trails = append(trails, listResp.Trails[i].ToMetadata())
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
	columns := []string{"NUM", "BRANCH", "TITLE"}
	if showStatus {
		columns = append(columns, "STATUS")
	}
	if showAuthor {
		columns = append(columns, "AUTHOR")
	}
	columns = append(columns, "UPDATED")
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
		if showAuthor {
			fields = append(fields, t.AuthorLogin())
		}
		fields = append(fields, timeAgo(t.UpdatedAt))
		fmt.Fprintln(tw, "  "+strings.Join(fields, "\t"))
	}
	_ = tw.Flush()
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
	var checkout bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a trail for the current or a new branch",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrailCreate(cmd, title, body, base, branch, status, checkout)
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "Trail title")
	cmd.Flags().StringVar(&body, "body", "", "Trail body")
	cmd.Flags().StringVar(&base, "base", "", "Base branch (defaults to detected default branch)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for the trail (defaults to current branch)")
	cmd.Flags().StringVar(&status, "status", "", "Initial status (defaults to draft)")
	cmd.Flags().BoolVar(&checkout, "checkout", false, "Check out the branch after creating it")

	return cmd
}

//nolint:cyclop // sequential steps for creating a trail — splitting would obscure the flow
func runTrailCreate(cmd *cobra.Command, title, body, base, branch, statusStr string, checkout bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	// --- Phase 1: Local git operations (no API calls) ---

	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	// Determine base branch
	if base == "" {
		base = strategy.GetDefaultBranchName(repo)
		if base == "" {
			base = defaultBaseBranch
		}
	}

	_, currentBranch, _ := IsOnDefaultBranch(ctx) //nolint:errcheck // best-effort detection
	interactive := !cmd.Flags().Changed("title") && !cmd.Flags().Changed("branch")

	if interactive {
		// Interactive flow: title → body → branch (derived) → status
		if err := runTrailCreateInteractive(&title, &body, &branch, &statusStr); err != nil {
			return handleFormCancellation(w, "Trail creation", err)
		}
	} else {
		// Non-interactive: derive missing values from provided flags
		if branch == "" {
			if cmd.Flags().Changed("title") {
				branch = slugifyTitle(title)
			} else {
				branch = currentBranch
			}
		}
		if title == "" {
			title = trail.HumanizeBranchName(branch)
		}
	}
	if branch == "" {
		return errors.New("branch name is required")
	}

	// Create the local branch if it doesn't exist
	needsCreation := branchNeedsCreation(repo, branch)
	if needsCreation {
		if err := createBranch(repo, branch); err != nil {
			return fmt.Errorf("failed to create branch %q: %w", branch, err)
		}
		fmt.Fprintf(w, "Created branch %s\n", branch)
	} else if currentBranch != branch {
		fmt.Fprintf(w, "Note: trail will be created for branch %q (not the current branch)\n", branch)
	}

	// Push the branch so the API can reference it
	if needsCreation {
		if err := pushBranchToOrigin(branch); err != nil {
			fmt.Fprintf(errW, "Warning: failed to push branch: %v\n", err)
		} else {
			fmt.Fprintf(w, "Pushed branch %s to origin\n", branch)
		}
	}

	// --- Phase 2: API operations ---

	var createResp api.TrailCreateResponse
	if err := runAuthenticatedDataAPI(ctx, cmd.ErrOrStderr(), trailInsecureHTTP(cmd), func(ctx context.Context, client *api.Client) error {
		forge, owner, repoName, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}

		createReq := api.TrailCreateRequest{
			Title:      title,
			Body:       body,
			BranchName: branch,
			Base:       base,
			Status:     statusStr,
		}

		resp, err := client.Post(ctx, trailsBasePath(forge, owner, repoName), createReq)
		if err != nil {
			return fmt.Errorf("failed to create trail: %w", err)
		}
		defer resp.Body.Close()
		if err := checkTrailResponse(resp); err != nil {
			return err
		}

		if err := api.DecodeJSON(resp, &createResp); err != nil {
			return fmt.Errorf("failed to decode create response: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Fprintf(w, "Created trail %q for branch %s (ID: %s)\n", createResp.Trail.Title, createResp.Trail.Branch, createResp.Trail.ID)

	// --- Phase 3: Post-creation local operations ---

	if needsCreation && currentBranch != branch {
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
			if formErr := form.Run(); formErr == nil && shouldCheckout {
				checkout = true
			}
		}
		if checkout {
			if err := CheckoutBranch(ctx, branch); err != nil {
				return fmt.Errorf("failed to checkout branch %q: %w", branch, err)
			}
			fmt.Fprintf(w, "Switched to branch %s\n", branch)
		}
	}

	return nil
}

func newTrailUpdateCmd() *cobra.Command {
	var statusStr, title, body, branch string
	var labelAdd, labelRemove []string

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update trail metadata",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrailUpdate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), statusStr, title, body, branch, labelAdd, labelRemove)
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

func runTrailUpdate(ctx context.Context, w, errW io.Writer, insecureHTTP bool, statusStr, title, body, branch string, labelAdd, labelRemove []string) error {
	return runAuthenticatedDataAPI(ctx, errW, insecureHTTP, func(ctx context.Context, client *api.Client) error {
		forge, owner, repoName, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}

		// Determine branch
		if branch == "" {
			branch, err = GetCurrentBranch(ctx)
			if err != nil {
				return fmt.Errorf("failed to determine current branch: %w", err)
			}
		}

		// Find the trail by branch
		found, err := findTrailByBranch(ctx, client, forge, owner, repoName, branch)
		if err != nil {
			return err
		}
		if found == nil {
			return fmt.Errorf("no trail found for branch %q", branch)
		}

		// Interactive mode when no flags are provided
		noFlags := statusStr == "" && title == "" && body == "" && labelAdd == nil && labelRemove == nil
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
		}

		// Validate status if provided
		if statusStr != "" {
			status := trail.Status(statusStr)
			if !status.IsValid() {
				return fmt.Errorf("invalid status %q: valid values are %s", statusStr, formatValidStatuses())
			}
		}

		// Build update request with only changed fields
		updateReq := buildTrailUpdateRequest(found, statusStr, title, body, labelAdd, labelRemove)

		// The single-trail endpoint is keyed by trail number, not id; the server
		// rejects an id here with "Invalid trail number format".
		if found.Number <= 0 {
			return fmt.Errorf("trail for branch %q has no number yet; cannot update", branch)
		}
		resp, err := client.Patch(ctx, trailsBasePath(forge, owner, repoName)+"/"+strconv.Itoa(found.Number), updateReq)
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

// buildTrailUpdateRequest constructs a PATCH request body from the current trail and the requested changes.
func buildTrailUpdateRequest(current *api.TrailResource, statusStr, title, body string, labelAdd, labelRemove []string) api.TrailUpdateRequest {
	var req api.TrailUpdateRequest

	if statusStr != "" {
		req.Status = &statusStr
	}
	if title != "" {
		req.Title = &title
	}
	if body != "" {
		req.Body = &body
	}

	// Handle label changes: merge adds, remove removes
	if len(labelAdd) > 0 || len(labelRemove) > 0 {
		labels := make([]string, 0, len(current.Labels)+len(labelAdd))
		labels = append(labels, current.Labels...)
		for _, l := range labelAdd {
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
		for _, l := range labelRemove {
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

// runTrailCreateInteractive runs the interactive form for trail creation.
// Prompts for title, body, branch (derived from title), and status.
func runTrailCreateInteractive(title, body, branch, statusStr *string) error {
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
	if err := form.Run(); err != nil {
		return fmt.Errorf("form cancelled: %w", err)
	}
	*title = strings.TrimSpace(*title)
	if *title == "" {
		return errors.New("trail title is required")
	}

	// Step 2: Branch (derived from title) and status
	suggested := slugifyTitle(*title)
	*branch = suggested

	// Build status options, excluding done/closed
	var statusOptions []huh.Option[string]
	for _, s := range trail.ValidStatuses() {
		if s == trail.StatusMerged || s == trail.StatusClosed {
			continue
		}
		statusOptions = append(statusOptions, huh.NewOption(string(s), string(s)))
	}
	if *statusStr == "" {
		*statusStr = string(trail.StatusDraft)
	}

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
	if err := form.Run(); err != nil {
		return fmt.Errorf("form cancelled: %w", err)
	}
	*branch = strings.TrimSpace(*branch)
	if *branch == "" {
		*branch = suggested
	}
	return nil
}

// findTrailByBranch looks up a trail by branch name via the list API.
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
	// The list endpoint paginates (default 50 rows); request the server max
	// so lookups don't miss less recently updated trails. Trails beyond the
	// first 200 are still invisible here — fixing that needs a server-side
	// branch filter or the by-number detail endpoint.
	resp, err := client.Get(ctx, trailsBasePath(forge, owner, repo)+trailListQuery(nil, "", trailListServerMaxLimit))
	if err != nil {
		return nil, fmt.Errorf("list trails: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}

	var listResp api.TrailListResponse
	if err := api.DecodeJSON(resp, &listResp); err != nil {
		return nil, fmt.Errorf("decode trail list: %w", err)
	}

	for i := range listResp.Trails {
		if match(listResp.Trails[i]) {
			return &listResp.Trails[i], nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means "not found" — callers check both
}

// trailsBasePath returns the API path prefix for trails endpoints
// (e.g., "/api/v1/trails/gh/org/repo").
func trailsBasePath(forge, owner, repo string) string {
	return fmt.Sprintf("/api/v1/trails/%s/%s/%s", forge, owner, repo)
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
