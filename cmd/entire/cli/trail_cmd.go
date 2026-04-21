package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trail"

	"github.com/charmbracelet/huh"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

func newTrailCmd() *cobra.Command {
	var insecureHTTPAuth bool

	cmd := &cobra.Command{
		Use:    "trail",
		Short:  "Manage trails for your branches",
		Hidden: true,
		Long: `Trails are branch-centric work tracking abstractions. They describe the
"why" and "what" of your work, while checkpoints capture the "how" and "when".

Running 'entire trail' without a subcommand shows the trail for the current
branch, or lists all trails if no trail exists for the current branch.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrailShow(cmd.Context(), cmd.OutOrStdout(), insecureHTTPAuth)
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

	return cmd
}

// trailInsecureHTTP reads the persistent --insecure-http-auth flag from the trail root command.
func trailInsecureHTTP(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("insecure-http-auth") //nolint:errcheck // flag is always registered
	return v
}

// runTrailShow shows the trail for the current branch, or falls through to list.
func runTrailShow(ctx context.Context, w io.Writer, insecureHTTP bool) error {
	branch, err := GetCurrentBranch(ctx)
	if err != nil {
		return runTrailListAll(ctx, w, "", false, false, insecureHTTP)
	}

	client, err := NewAuthenticatedAPIClient(insecureHTTP)
	if err != nil {
		return fmt.Errorf("authentication required: %w", err)
	}

	host, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return fmt.Errorf("failed to resolve repository: %w", err)
	}

	found, err := findTrailByBranch(ctx, client, host, owner, repo, branch)
	if err != nil {
		return err
	}
	if found == nil {
		return runTrailListAll(ctx, w, "", false, false, insecureHTTP)
	}

	printTrailDetails(w, found.ToMetadata())
	return nil
}

func printTrailDetails(w io.Writer, m *trail.Metadata) {
	fmt.Fprintf(w, "Trail: %s\n", m.Title)
	fmt.Fprintf(w, "  ID:      %s\n", m.TrailID)
	fmt.Fprintf(w, "  Branch:  %s\n", m.Branch)
	fmt.Fprintf(w, "  Base:    %s\n", m.Base)
	fmt.Fprintf(w, "  Status:  %s\n", m.Status)
	fmt.Fprintf(w, "  Author:  %s\n", m.Author)
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
	var statusFilter string
	var jsonOutput bool
	var showAll bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all trails",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrailListAll(cmd.Context(), cmd.OutOrStdout(), statusFilter, jsonOutput, showAll, trailInsecureHTTP(cmd))
		},
	}

	cmd.Flags().StringVar(&statusFilter, "status", "", "Filter by status (draft, open, in_progress, in_review, merged, closed)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "Include merged and closed trails")

	return cmd
}

func runTrailListAll(ctx context.Context, w io.Writer, statusFilter string, jsonOutput, showAll, insecureHTTP bool) error {
	client, err := NewAuthenticatedAPIClient(insecureHTTP)
	if err != nil {
		return fmt.Errorf("authentication required: %w", err)
	}

	host, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return fmt.Errorf("failed to resolve repository: %w", err)
	}

	resp, err := client.Get(ctx, trailsBasePath(host, owner, repo))
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

	totalCount := len(trails)

	// Apply status filter
	if statusFilter != "" {
		status := trail.Status(statusFilter)
		if !status.IsValid() {
			return fmt.Errorf("invalid status %q: valid values are %s", statusFilter, formatValidStatuses())
		}
		var filtered []*trail.Metadata
		for _, t := range trails {
			if t.Status == status {
				filtered = append(filtered, t)
			}
		}
		trails = filtered
	} else if !showAll {
		// By default, hide merged and closed trails
		var filtered []*trail.Metadata
		for _, t := range trails {
			if t.Status != trail.StatusMerged && t.Status != trail.StatusClosed {
				filtered = append(filtered, t)
			}
		}
		trails = filtered
	}

	// Sort by updated_at descending
	sort.Slice(trails, func(i, j int) bool {
		return trails[i].UpdatedAt.After(trails[j].UpdatedAt)
	})

	if jsonOutput {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(trails); err != nil {
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
		return nil
	}

	if len(trails) == 0 {
		hiddenCount := totalCount - len(trails)
		if hiddenCount > 0 {
			fmt.Fprintf(w, "No active trails found. %d merged/closed trail(s) hidden — use --all to show.\n", hiddenCount)
		} else {
			fmt.Fprintln(w, "No trails found.")
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Commands:")
			fmt.Fprintln(w, "  entire trail create   Create a trail for the current branch")
			fmt.Fprintln(w, "  entire trail list     List all trails")
			fmt.Fprintln(w, "  entire trail update   Update trail metadata")
		}
		return nil
	}

	// Table output
	fmt.Fprintf(w, "%-30s %-40s %-13s %-15s %s\n", "BRANCH", "TITLE", "STATUS", "AUTHOR", "UPDATED")
	for _, t := range trails {
		branch := stringutil.TruncateRunes(t.Branch, 30, "...")
		title := stringutil.TruncateRunes(t.Title, 40, "...")
		fmt.Fprintf(w, "%-30s %-40s %-13s %-15s %s\n",
			branch, title, t.Status, stringutil.TruncateRunes(t.Author, 15, "..."), timeAgo(t.UpdatedAt))
	}

	return nil
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

	client, err := NewAuthenticatedAPIClient(trailInsecureHTTP(cmd))
	if err != nil {
		return fmt.Errorf("authentication required: %w", err)
	}

	host, owner, repoName, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return fmt.Errorf("failed to resolve repository: %w", err)
	}

	createReq := api.TrailCreateRequest{
		Title:      title,
		Body:       body,
		BranchName: branch,
		Base:       base,
		Status:     statusStr,
	}

	resp, err := client.Post(ctx, trailsBasePath(host, owner, repoName), createReq)
	if err != nil {
		return fmt.Errorf("failed to create trail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return err
	}

	var createResp api.TrailCreateResponse
	if err := api.DecodeJSON(resp, &createResp); err != nil {
		return fmt.Errorf("failed to decode create response: %w", err)
	}

	fmt.Fprintf(w, "Created trail %q for branch %s (ID: %s)\n", createResp.Trail.Title, createResp.Trail.Branch, createResp.Trail.TrailID)

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
	_ = errW // reserved for future warnings

	client, err := NewAuthenticatedAPIClient(insecureHTTP)
	if err != nil {
		return fmt.Errorf("authentication required: %w", err)
	}

	host, owner, repoName, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return fmt.Errorf("failed to resolve repository: %w", err)
	}

	// Determine branch
	if branch == "" {
		branch, err = GetCurrentBranch(ctx)
		if err != nil {
			return fmt.Errorf("failed to determine current branch: %w", err)
		}
	}

	// Find the trail by branch
	found, err := findTrailByBranch(ctx, client, host, owner, repoName, branch)
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

	resp, err := client.Patch(ctx, trailsBasePath(host, owner, repoName)+"/"+found.TrailID, updateReq)
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
func findTrailByBranch(ctx context.Context, client *api.Client, host, owner, repo, branch string) (*api.TrailResource, error) {
	resp, err := client.Get(ctx, trailsBasePath(host, owner, repo))
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
		if listResp.Trails[i].Branch == branch {
			return &listResp.Trails[i], nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means "not found" — callers check both
}

// apiHostAlias maps git host domains to short aliases used by the trails API.
var apiHostAlias = map[string]string{
	"github.com": "gh",
}

// trailsBasePath returns the API path prefix for trails endpoints (e.g., "/api/v1/trails/gh/org/repo").
func trailsBasePath(host, owner, repo string) string {
	if alias, ok := apiHostAlias[host]; ok {
		host = alias
	}
	return fmt.Sprintf("/api/v1/trails/%s/%s/%s", host, owner, repo)
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
