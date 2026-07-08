package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const (
	defaultTrailReviewLimit     = 100
	trailReviewStatusAny        = "any"
	trailReviewFreshnessCurrent = "current"
	trailReviewFreshnessStale   = "stale"
	trailReviewFreshnessAny     = "any"
	trailReviewStatusOpen       = "open"
	trailReviewStatusResolved   = "resolved"
	trailReviewStatusDismissed  = "dismissed"
	// Per-finding batch-create outcome returned by the reviews/{id}/comments
	// endpoint that the CLI must surface as a failure. "created" and "existing"
	// are both success outcomes and need no dedicated handling.
	trailReviewBatchResultError = "error"
	trailReviewSeverityHigh     = "high"
	trailReviewSeverityMedium   = "medium"
	trailReviewSeverityLow      = "low"
)

var errTrailReviewDefaultTargetNotFound = errors.New("default trail finding target not found")

type trailReviewListOptions struct {
	Status           string
	StatusChanged    bool
	Severity         string
	Freshness        string
	IncludeDismissed bool
	Limit            int
	Offset           int
	JSON             bool
}

type trailReviewTargetOptions struct {
	Selector string
	Branch   string
}

type trailReviewTarget struct {
	Host  string
	Owner string
	Repo  string
	Trail api.TrailResource
}

func newTrailFindingCmd() *cobra.Command {
	opts := defaultTrailReviewListOptions()
	targetOpts := trailReviewTargetOptions{}

	cmd := &cobra.Command{
		Use:   "finding [<trail>]",
		Short: "Manage a trail's agent findings",
		Long: `Manage a trail's agent-native findings.

Running 'entire trail finding' shows the finding dashboard for the current
branch's trail. Pass a trail selector (number, id, or branch) to inspect another
trail in the same repo. Use 'entire trail list --status any' when you need to
discover a trail selector first.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalTrailSelector(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			opts.StatusChanged = cmd.Flags().Changed("status")
			return runTrailReviewDashboard(cmd, selector, opts)
		},
	}
	cmd.PersistentFlags().StringVar(&targetOpts.Selector, "trail", "", "Trail selector (number, id, or branch); defaults to the current branch's trail")
	cmd.PersistentFlags().StringVar(&targetOpts.Branch, "branch", "", "Resolve the trail for this branch instead of the current branch; cannot be combined with a trail selector")
	addTrailReviewListFlags(cmd, &opts)

	cmd.AddCommand(newTrailFindingListCmd(&targetOpts))
	cmd.AddCommand(newTrailFindingAddCmd(&targetOpts))
	cmd.AddCommand(newTrailReviewShowCmd(&targetOpts))
	cmd.AddCommand(newTrailReviewUpdateCmd(&targetOpts))
	cmd.AddCommand(newTrailReviewApplyCmd(&targetOpts))
	cmd.AddCommand(newTrailReviewStatusCmd(&targetOpts, "resolve", trailReviewStatusResolved, "Resolve a finding"))
	cmd.AddCommand(newTrailReviewStatusCmd(&targetOpts, "dismiss", trailReviewStatusDismissed, "Dismiss a finding"))
	cmd.AddCommand(newTrailReviewStatusCmd(&targetOpts, "reopen", trailReviewStatusOpen, "Reopen a finding"))

	return cmd
}

func defaultTrailReviewListOptions() trailReviewListOptions {
	return trailReviewListOptions{
		Status:    trailReviewStatusOpen,
		Freshness: trailReviewFreshnessCurrent,
		Limit:     defaultTrailReviewLimit,
	}
}

func addTrailReviewListFlags(cmd *cobra.Command, opts *trailReviewListOptions) {
	cmd.Flags().StringVar(&opts.Status, "status", opts.Status, "Filter by lifecycle status(es): open,resolved,dismissed; use 'any' for all")
	cmd.Flags().StringVar(&opts.Severity, "severity", "", "Filter by comma-separated severity value(s): high,medium,low")
	cmd.Flags().StringVar(&opts.Freshness, "freshness", opts.Freshness, "Filter code-version freshness: current,stale,any")
	cmd.Flags().BoolVar(&opts.IncludeDismissed, "include-dismissed", false, "Include dismissed findings")
	cmd.Flags().IntVarP(&opts.Limit, "limit", "n", opts.Limit, "Maximum number of findings to show")
	cmd.Flags().IntVar(&opts.Offset, "offset", 0, "Pagination offset")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
}

func newTrailFindingListCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	opts := defaultTrailReviewListOptions()
	cmd := &cobra.Command{
		Use:   "list [<trail>]",
		Short: "List findings for a trail",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalTrailSelector(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			opts.StatusChanged = cmd.Flags().Changed("status")
			return runTrailReviewComments(cmd, selector, opts)
		},
	}
	addTrailReviewListFlags(cmd, &opts)
	return cmd
}

type trailReviewCommentAddOptions struct {
	Body        string
	Severity    string
	Confidence  float64
	FilePath    string
	Line        int
	StartLine   int
	EndLine     int
	ClientID    string
	Patch       string
	PatchFile   string
	Instruction string
	JSON        bool
}

func newTrailFindingAddCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	var opts trailReviewCommentAddOptions
	cmd := &cobra.Command{
		Use:   "add [<trail>]",
		Short: "Create a finding on a trail",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalTrailSelector(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runTrailReviewCommentAdd(cmd, selector, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.Body, "body", "m", "", "Finding body")
	cmd.Flags().StringVar(&opts.Severity, "severity", "", "Finding severity: high,medium,low")
	cmd.Flags().Float64Var(&opts.Confidence, "confidence", -1, "Finding confidence from 0.0 to 1.0")
	cmd.Flags().StringVar(&opts.FilePath, "file", "", "File path for the finding location")
	cmd.Flags().IntVar(&opts.Line, "line", 0, "Line number for the finding location")
	cmd.Flags().IntVar(&opts.StartLine, "start-line", 0, "Start line for the finding location")
	cmd.Flags().IntVar(&opts.EndLine, "end-line", 0, "End line for the finding location")
	cmd.Flags().StringVar(&opts.ClientID, "client-id", "", "Client-provided idempotency key for this finding")
	cmd.Flags().StringVar(&opts.Patch, "patch", "", "Unified-diff suggested change to attach")
	cmd.Flags().StringVar(&opts.PatchFile, "patch-file", "", "Read unified-diff suggested change from file; use '-' for stdin")
	cmd.Flags().StringVar(&opts.Instruction, "instruction", "", "Manual suggested-change instruction to attach")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
	return cmd
}

func newTrailReviewShowCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [<trail>] <finding-id>",
		Short: "Show a finding",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, commentID, err := parseTrailSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runTrailReviewShow(cmd, selector, commentID)
		},
	}
	return cmd
}

type trailReviewUpdateOptions struct {
	Body              string
	BodyChanged       bool
	Severity          string
	SeverityChanged   bool
	Confidence        float64
	ConfidenceChanged bool
	JSON              bool
}

func newTrailReviewUpdateCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	opts := trailReviewUpdateOptions{Confidence: -1}
	cmd := &cobra.Command{
		Use:   "update [<trail>] <finding-id>",
		Short: "Update a finding's metadata",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, commentID, err := parseTrailSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			opts.BodyChanged = cmd.Flags().Changed("body")
			opts.SeverityChanged = cmd.Flags().Changed("severity")
			opts.ConfidenceChanged = cmd.Flags().Changed("confidence")
			return runTrailReviewUpdate(cmd, selector, commentID, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.Body, "body", "m", "", "Finding body")
	cmd.Flags().StringVar(&opts.Severity, "severity", "", "Finding severity: high,medium,low")
	cmd.Flags().Float64Var(&opts.Confidence, "confidence", -1, "Finding confidence from 0.0 to 1.0")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
	return cmd
}

type trailReviewApplyOptions struct {
	Resolve bool
	Check   bool
}

func newTrailReviewApplyCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	var opts trailReviewApplyOptions
	cmd := &cobra.Command{
		Use:   "apply [<trail>] <finding-id>",
		Short: "Apply a finding's unified-diff suggestion",
		Long: `Apply a finding's unified-diff suggestion to the current worktree.

By default this only changes files. Pass --resolve to update the finding's
lifecycle status after the patch applies successfully.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureNoTrailRepoOverride(cmd, "trail finding apply"); err != nil {
				return err
			}
			selector, commentID, err := parseTrailSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runTrailReviewApply(cmd, selector, commentID, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.Resolve, "resolve", false, "Mark the finding resolved after applying")
	cmd.Flags().BoolVar(&opts.Check, "check", false, "Only check whether the patch applies; do not modify files")
	return cmd
}

func newTrailReviewStatusCmd(targetOpts *trailReviewTargetOptions, use, status, short string) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   use + " [<trail>] <finding-id>",
		Short: short,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, commentID, err := parseTrailSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runTrailReviewSetStatus(cmd, selector, commentID, status, message)
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Status reason to record")
	return cmd
}

func runTrailReviewDashboard(cmd *cobra.Command, selector string, opts trailReviewListOptions) error {
	var err error
	opts, err = normalizeTrailReviewListOptions(opts)
	if err != nil {
		return err
	}
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		if strings.TrimSpace(selector) == "" && errors.Is(err, errTrailReviewDefaultTargetNotFound) {
			fmt.Fprintln(cmd.OutOrStdout(), "No trail found for the current branch; showing trails in this repo.")
			fmt.Fprintln(cmd.OutOrStdout())
			return runTrailListAll(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailListOptions{Status: trailListStatusAny, Limit: defaultTrailListLimit, InsecureHTTP: trailInsecureHTTP(cmd), Repo: trailRepoFlag(cmd)})
		}
		return err
	}
	comments, hasMore, err := fetchTrailReviewComments(cmd.Context(), client, target.Trail.ID, opts)
	if err != nil {
		return err
	}
	summaryComments, err := fetchAllTrailReviewComments(cmd.Context(), client, target.Trail.ID, trailReviewSummaryOptions())
	if err != nil {
		return err
	}
	counts := countTrailReviewComments(summaryComments)
	if opts.JSON {
		return encodeTrailReviewJSON(cmd.OutOrStdout(), target, comments, hasMore, counts)
	}
	printTrailReviewDashboard(cmd.OutOrStdout(), target, comments, hasMore, opts, counts)
	return nil
}

func runTrailReviewComments(cmd *cobra.Command, selector string, opts trailReviewListOptions) error {
	var err error
	opts, err = normalizeTrailReviewListOptions(opts)
	if err != nil {
		return err
	}
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comments, hasMore, err := fetchTrailReviewComments(cmd.Context(), client, target.Trail.ID, opts)
	if err != nil {
		return err
	}
	if opts.JSON {
		return encodeTrailReviewJSON(cmd.OutOrStdout(), target, comments, hasMore, countTrailReviewComments(comments))
	}
	printTrailReviewComments(cmd.OutOrStdout(), comments, hasMore)
	return nil
}

func runTrailReviewCommentAdd(cmd *cobra.Command, selector string, opts trailReviewCommentAddOptions) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	opts, err = loadTrailReviewCommentPatchFile(opts, cmd.InOrStdin())
	if err != nil {
		return err
	}
	input, err := buildTrailReviewCommentInput(opts)
	if err != nil {
		return err
	}
	created, err := createTrailReviewFinding(cmd.Context(), client, target.Trail.ID, input)
	if err != nil {
		return err
	}
	if opts.JSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(created); err != nil {
			return fmt.Errorf("encode created finding: %w", err)
		}
		return nil
	}
	printTrailReviewCommentCreated(cmd.OutOrStdout(), target, created)
	return nil
}

func runTrailReviewShow(cmd *cobra.Command, selector string, commentID string) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return err
	}
	if hydrated, _, hydrateErr := hydrateTrailReviewCommentWithState(cmd.Context(), client, target.Trail.ID, comment); hydrateErr == nil {
		comment = hydrated
	}
	printTrailReviewCommentDetail(cmd.OutOrStdout(), comment)
	return nil
}

func runTrailReviewUpdate(cmd *cobra.Command, selector string, commentID string, opts trailReviewUpdateOptions) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return err
	}
	req, err := buildTrailReviewCommentPatchRequest(opts)
	if err != nil {
		return err
	}
	updated, err := patchTrailReviewComment(cmd.Context(), client, target.Trail.ID, comment, req)
	if err != nil {
		return err
	}
	if opts.JSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(updated); err != nil {
			return fmt.Errorf("encode updated finding: %w", err)
		}
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated finding %s\n", updated.ID)
	return nil
}

func runTrailReviewApply(cmd *cobra.Command, selector string, commentID string, opts trailReviewApplyOptions) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return err
	}
	comment, state, err := hydrateTrailReviewCommentWithState(cmd.Context(), client, target.Trail.ID, comment)
	if err != nil {
		return err
	}
	if err := verifyTrailReviewHead(cmd.Context(), state); err != nil {
		return err
	}
	applied, err := applyTrailReviewSuggestions(cmd.Context(), comment, opts.Check, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	if opts.Check {
		fmt.Fprintf(cmd.OutOrStdout(), "%d suggested change(s) apply cleanly.\n", applied)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Applied %d suggested change(s).\n", applied)
	if opts.Resolve {
		updated, err := patchTrailReviewCommentStatus(cmd.Context(), client, target.Trail.ID, comment, trailReviewStatusResolved, "Applied via Entire CLI")
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Resolved finding %s after apply: %s → %s\n", updated.ID, comment.Status, updated.Status)
	}
	return nil
}

func runTrailReviewSetStatus(cmd *cobra.Command, selector string, commentID, status, message string) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return err
	}
	oldStatus := comment.Status
	if message == "" {
		message = defaultTrailReviewStatusReason(status)
	}
	updated, err := patchTrailReviewCommentStatus(cmd.Context(), client, target.Trail.ID, comment, status, message)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated finding %s: %s → %s\n", updated.ID, oldStatus, updated.Status)
	return nil
}

func authenticatedTrailReviewTarget(cmd *cobra.Command, selector string) (*api.Client, trailReviewTarget, error) {
	repoOverride := trailRepoFlag(cmd)
	branchOverride := trailBranchFlag(cmd)
	if selector != "" && branchOverride != "" {
		return nil, trailReviewTarget{}, errors.New("pass a trail selector or --branch, not both")
	}
	if repoOverride != "" && selector == "" && branchOverride == "" {
		return nil, trailReviewTarget{}, errors.New("--repo requires an explicit target: pass a trail selector or --branch")
	}
	var target trailReviewTarget
	var resolvedClient *api.Client
	err := runAuthenticatedTrailAPI(cmd.Context(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), repoOverride, func(ctx context.Context, client *api.Client) error {
		var err error
		resolvedClient = client
		target, err = resolveTrailReviewTarget(ctx, client, selector, repoOverride, branchOverride)
		return err
	})
	if err != nil {
		return nil, trailReviewTarget{}, err
	}
	return resolvedClient, target, nil
}

func resolveTrailReviewTarget(ctx context.Context, client *api.Client, selector, repoOverride, branchOverride string) (trailReviewTarget, error) {
	host, owner, repo, err := resolveTrailRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return trailReviewTarget{}, err
	}

	selector = strings.TrimSpace(selector)
	var found *api.TrailResource
	if selector != "" {
		found, err = findTrailBySelector(ctx, client, host, owner, repo, selector)
		if err != nil {
			return trailReviewTarget{}, err
		}
		if found == nil {
			return trailReviewTarget{}, fmt.Errorf("no trail %q found in %s/%s/%s (run 'entire trail list --status any')", selector, host, owner, repo)
		}
	} else {
		branch, branchErr := resolveTrailBranch(ctx, branchOverride)
		if branchErr != nil {
			return trailReviewTarget{}, fmt.Errorf("%w: no trail selector given and current branch is unknown: %w\nhint: run 'entire trail list --status any' or pass --trail <number|id|branch>", errTrailReviewDefaultTargetNotFound, branchErr)
		}
		found, err = findTrailByBranch(ctx, client, host, owner, repo, branch)
		if err != nil {
			return trailReviewTarget{}, err
		}
		if found == nil {
			return trailReviewTarget{}, fmt.Errorf("%w: no trail found for branch %q\nhint: run 'entire trail create', 'entire trail list --status any', or pass --trail <number|id|branch>", errTrailReviewDefaultTargetNotFound, branch)
		}
	}
	if found.ID == "" {
		return trailReviewTarget{}, errors.New("trail has no id yet")
	}
	return trailReviewTarget{Host: host, Owner: owner, Repo: repo, Trail: *found}, nil
}

func normalizeTrailReviewListOptions(opts trailReviewListOptions) (trailReviewListOptions, error) {
	if opts.Limit <= 0 {
		return opts, errors.New("limit must be greater than 0")
	}
	if opts.Offset < 0 {
		return opts, errors.New("offset must be non-negative")
	}
	status, err := normalizeTrailReviewStatusFilter(opts.Status)
	if err != nil {
		return opts, err
	}
	opts.Status = status
	severity, err := normalizeCommaSet(opts.Severity, "severity", map[string]bool{
		trailReviewSeverityHigh: true, trailReviewSeverityMedium: true, trailReviewSeverityLow: true,
	})
	if err != nil {
		return opts, err
	}
	opts.Severity = severity
	if freshness := strings.TrimSpace(opts.Freshness); freshness != "" {
		switch freshness {
		case trailReviewFreshnessCurrent, trailReviewFreshnessStale, trailReviewFreshnessAny:
		default:
			return opts, fmt.Errorf("invalid freshness filter %q: valid values are current, stale, any", opts.Freshness)
		}
		opts.Freshness = freshness
	}
	// `--include-dismissed` should do what it says for the common case: when the
	// caller did not explicitly choose a status, do not keep the default open-only
	// status filter that would still hide dismissed findings.
	if opts.IncludeDismissed && !opts.StatusChanged && strings.TrimSpace(opts.Status) == trailReviewStatusOpen {
		opts.Status = trailReviewStatusAny
	}
	return opts, nil
}

func normalizeTrailReviewStatusFilter(filter string) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" || filter == trailReviewStatusAny {
		return filter, nil
	}
	return normalizeCommaSet(filter, "status", map[string]bool{
		trailReviewStatusOpen: true, trailReviewStatusResolved: true, trailReviewStatusDismissed: true,
	})
}

func normalizeCommaSet(filter, name string, valid map[string]bool) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "", nil
	}
	parts := strings.Split(filter, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return "", fmt.Errorf("invalid %s filter %q: empty value", name, filter)
		}
		if !valid[value] {
			vals := make([]string, 0, len(valid))
			for v := range valid {
				vals = append(vals, v)
			}
			sort.Strings(vals)
			return "", fmt.Errorf("invalid %s %q: valid values are %s", name, value, strings.Join(vals, ", "))
		}
		out = append(out, value)
	}
	return strings.Join(out, ","), nil
}

func fetchTrailReviewComments(ctx context.Context, client *api.Client, trailID string, opts trailReviewListOptions) ([]api.TrailReviewComment, bool, error) {
	var err error
	opts, err = normalizeTrailReviewListOptions(opts)
	if err != nil {
		return nil, false, err
	}
	resp, err := client.Get(ctx, trailReviewCommentsPath(trailID, opts))
	if err != nil {
		return nil, false, fmt.Errorf("list findings: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, false, err
	}
	var out api.TrailReviewCommentsResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return nil, false, fmt.Errorf("decode findings: %w", err)
	}
	return out.Comments, out.HasMore, nil
}

func fetchAllTrailReviewComments(ctx context.Context, client *api.Client, trailID string, opts trailReviewListOptions) ([]api.TrailReviewComment, error) {
	if opts.Limit <= 0 {
		opts.Limit = defaultTrailReviewLimit
	}
	var all []api.TrailReviewComment
	for {
		comments, hasMore, err := fetchTrailReviewComments(ctx, client, trailID, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if !hasMore {
			break
		}
		opts.Offset += opts.Limit
	}
	return all, nil
}

func trailReviewSummaryOptions() trailReviewListOptions {
	return trailReviewListOptions{
		Status:           trailReviewStatusAny,
		Freshness:        trailReviewFreshnessAny,
		IncludeDismissed: true,
		Limit:            defaultTrailReviewLimit,
	}
}

func trailReviewCommentsPath(trailID string, opts trailReviewListOptions) string {
	q := url.Values{}
	if opts.Status != "" && opts.Status != trailReviewStatusAny {
		q.Set("status", opts.Status)
	}
	if opts.Severity != "" {
		q.Set("severity", opts.Severity)
	}
	// Unlike status=any (which the API treats as no status filter), stale=any is
	// semantically significant: omitting stale defaults to current-only server-side.
	if opts.Freshness != "" {
		q.Set("stale", opts.Freshness)
	}
	if opts.IncludeDismissed {
		q.Set("include_dismissed", "true")
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := trailReviewListCommentsPath(trailID)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

func loadTrailReviewCommentPatchFile(opts trailReviewCommentAddOptions, stdin io.Reader) (trailReviewCommentAddOptions, error) {
	patchFile := strings.TrimSpace(opts.PatchFile)
	if patchFile == "" {
		return opts, nil
	}
	if strings.TrimSpace(opts.Patch) != "" {
		return opts, errors.New("pass either --patch or --patch-file, not both")
	}
	var (
		data []byte
		err  error
	)
	if patchFile == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(patchFile) //nolint:gosec // --patch-file is an explicit user-selected input path.
	}
	if err != nil {
		return opts, fmt.Errorf("read patch file %q: %w", patchFile, err)
	}
	opts.Patch = string(data)
	return opts, nil
}

func buildTrailReviewCommentPatchRequest(opts trailReviewUpdateOptions) (api.TrailReviewCommentPatchRequest, error) {
	var req api.TrailReviewCommentPatchRequest
	if opts.BodyChanged {
		body := strings.TrimSpace(opts.Body)
		if body == "" {
			return req, errors.New("finding body is required (pass --body)")
		}
		req.Body = stringPtr(body)
	}
	if opts.SeverityChanged {
		severity := strings.ToLower(strings.TrimSpace(opts.Severity))
		switch severity {
		case trailReviewSeverityHigh, trailReviewSeverityMedium, trailReviewSeverityLow:
			req.Severity = stringPtr(severity)
		default:
			return req, fmt.Errorf("invalid severity %q: valid values are high, medium, low", opts.Severity)
		}
	}
	if opts.ConfidenceChanged {
		if opts.Confidence < 0 || opts.Confidence > 1 {
			return req, errors.New("--confidence must be between 0.0 and 1.0")
		}
		confidence := opts.Confidence
		req.Confidence = &confidence
	}
	if !trailReviewCommentPatchHasFields(req) {
		return req, errors.New("at least one finding field is required (pass --body, --severity, or --confidence)")
	}
	return req, nil
}

func trailReviewCommentPatchHasFields(req api.TrailReviewCommentPatchRequest) bool {
	return req.Body != nil || req.Severity != nil || req.Confidence != nil || req.Status != "" || req.StatusReason != nil
}

func buildTrailReviewCommentInput(opts trailReviewCommentAddOptions) (api.TrailReviewCommentInput, error) {
	body := strings.TrimSpace(opts.Body)
	if body == "" {
		return api.TrailReviewCommentInput{}, errors.New("finding body is required (pass --body)")
	}
	severity := strings.ToLower(strings.TrimSpace(opts.Severity))
	if severity != "" {
		switch severity {
		case trailReviewSeverityHigh, trailReviewSeverityMedium, trailReviewSeverityLow:
		default:
			return api.TrailReviewCommentInput{}, fmt.Errorf("invalid severity %q: valid values are high, medium, low", opts.Severity)
		}
	}
	confidence, hasConfidence, err := buildTrailReviewCommentConfidence(opts.Confidence)
	if err != nil {
		return api.TrailReviewCommentInput{}, err
	}
	loc, err := buildTrailReviewCommentLocation(opts)
	if err != nil {
		return api.TrailReviewCommentInput{}, err
	}
	// client_id is the API's idempotency key and is now required. Honour an
	// explicit --client-id when given, otherwise generate a unique one so a
	// retried add doesn't create duplicate findings within the same key.
	clientID := strings.TrimSpace(opts.ClientID)
	if clientID == "" {
		clientID = generateTrailReviewClientID()
	}
	input := api.TrailReviewCommentInput{
		ClientID: clientID,
		Body:     stringPtr(body),
		Severity: optionalStringPtr(severity),
		Location: loc,
	}
	if hasConfidence {
		input.Confidence = &confidence
	}
	if patch := strings.TrimSpace(opts.Patch); patch != "" {
		input.SuggestedChange = &api.TrailReviewSuggestedChangeCreateRequest{
			ChangeType:  "unified_diff",
			Patch:       stringPtr(patch),
			Instruction: optionalStringPtr(strings.TrimSpace(opts.Instruction)),
		}
	} else if instruction := strings.TrimSpace(opts.Instruction); instruction != "" {
		input.SuggestedChange = &api.TrailReviewSuggestedChangeCreateRequest{
			ChangeType:  "manual_instruction",
			Instruction: stringPtr(instruction),
		}
	}
	return input, nil
}

// generateTrailReviewClientID builds a unique idempotency key for a CLI-created
// finding when the caller did not supply --client-id.
func generateTrailReviewClientID() string {
	return "entire-cli:" + uuid.NewString()
}

func buildTrailReviewCommentConfidence(confidence float64) (float64, bool, error) {
	if confidence < 0 {
		return 0, false, nil
	}
	if confidence > 1 {
		return 0, false, errors.New("--confidence must be between 0.0 and 1.0")
	}
	return confidence, true, nil
}

func buildTrailReviewCommentLocation(opts trailReviewCommentAddOptions) (api.TrailReviewLocationCreateRequest, error) {
	filePath := strings.TrimSpace(opts.FilePath)
	line := opts.Line
	if line < 0 || opts.StartLine < 0 || opts.EndLine < 0 {
		return api.TrailReviewLocationCreateRequest{}, errors.New("line numbers must be non-negative")
	}
	if opts.StartLine > 0 {
		if line > 0 {
			return api.TrailReviewLocationCreateRequest{}, errors.New("pass either --line or --start-line, not both")
		}
		line = opts.StartLine
	}
	if filePath == "" && (line > 0 || opts.EndLine > 0) {
		return api.TrailReviewLocationCreateRequest{}, errors.New("--line/--start-line/--end-line require --file")
	}
	if opts.EndLine > 0 && line == 0 {
		return api.TrailReviewLocationCreateRequest{}, errors.New("--end-line requires --line or --start-line")
	}
	if opts.EndLine > 0 && opts.EndLine < line {
		return api.TrailReviewLocationCreateRequest{}, errors.New("--end-line must be greater than or equal to the start line")
	}

	loc := api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange}
	if filePath == "" {
		return loc, nil
	}
	loc.Granularity = reviewTrailGranularityFile
	loc.FilePath = stringPtr(filePath)
	if line > 0 {
		loc.Granularity = reviewTrailGranularityLine
		loc.StartLine = &line
		if opts.EndLine > 0 {
			loc.EndLine = &opts.EndLine
			if opts.EndLine != line {
				loc.Granularity = reviewTrailGranularityRange
			}
		}
	}
	return loc, nil
}

// createTrailReviewFinding posts a single finding through the current API flow:
// start a review session, then submit a one-item comment batch under it. It
// returns the created (or already-existing) finding.
func prepareTrailReviewCommentInputsForCreate(worktreeRoot string, inputs []api.TrailReviewCommentInput) []api.TrailReviewCommentInput {
	out := make([]api.TrailReviewCommentInput, len(inputs))
	copy(out, inputs)
	for i := range out {
		out[i].Location = prepareTrailReviewLocationForCreate(worktreeRoot, out[i].Location)
	}
	return out
}

func prepareTrailReviewLocationForCreate(worktreeRoot string, loc api.TrailReviewLocationCreateRequest) api.TrailReviewLocationCreateRequest {
	switch loc.Granularity {
	case reviewTrailGranularityLine, reviewTrailGranularityRange:
		if loc.SelectedText != nil && strings.TrimSpace(*loc.SelectedText) != "" {
			return loc
		}
		filePath := ""
		if loc.FilePath != nil {
			filePath = strings.TrimSpace(*loc.FilePath)
		}
		startLine := 0
		if loc.StartLine != nil {
			startLine = *loc.StartLine
		}
		endLine := startLine
		if loc.Granularity == reviewTrailGranularityRange && loc.EndLine != nil && *loc.EndLine > startLine {
			endLine = *loc.EndLine
		}
		selected, fileOK, selectedOK := trailReviewSelectedTextFromWorktree(worktreeRoot, filePath, startLine, endLine)
		if selectedOK {
			loc.SelectedText = stringPtr(selected)
			return loc
		}
		if fileOK {
			return api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityFile, FilePath: stringPtr(filePath)}
		}
		return api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange}
	case reviewTrailGranularityFile:
		if loc.FilePath != nil && strings.TrimSpace(*loc.FilePath) != "" {
			return loc
		}
	}
	return api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange}
}

func trailReviewSelectedTextFromWorktree(worktreeRoot, filePath string, startLine, endLine int) (selected string, fileOK bool, selectedOK bool) {
	if strings.TrimSpace(worktreeRoot) == "" || strings.TrimSpace(filePath) == "" || startLine <= 0 || endLine < startLine {
		return "", false, false
	}
	fullPath, ok := safeWorktreeFilePath(worktreeRoot, filePath)
	if !ok {
		return "", false, false
	}
	data, err := os.ReadFile(fullPath) //nolint:gosec // path is constrained to the current worktree root.
	if err != nil {
		return "", false, false
	}
	contents := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(contents, "\n")
	if startLine > len(lines) || endLine > len(lines) {
		return "", true, false
	}
	text := strings.Join(lines[startLine-1:endLine], "\n")
	if strings.TrimSpace(text) == "" {
		return "", true, false
	}
	return text, true, true
}

func safeWorktreeFilePath(worktreeRoot, filePath string) (string, bool) {
	if filepath.IsAbs(filePath) {
		return "", false
	}
	root := filepath.Clean(worktreeRoot)
	cleanRel := filepath.Clean(filepath.FromSlash(filePath))
	if cleanRel == "." || cleanRel == "" || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	fullPath := filepath.Join(root, cleanRel)
	rel, err := filepath.Rel(root, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return fullPath, true
}

func createTrailReviewFinding(ctx context.Context, client *api.Client, trailID string, input api.TrailReviewCommentInput) (api.TrailReviewComment, error) {
	findings, err := createTrailReviewFindings(ctx, client, trailID, []api.TrailReviewCommentInput{input})
	if err != nil {
		return api.TrailReviewComment{}, err
	}
	if len(findings) == 0 {
		return api.TrailReviewComment{}, errors.New("create finding: server returned no results")
	}
	return findings[0], nil
}

// createTrailReviewFindings posts findings through one trail review session,
// chunking by the server-advertised batch limit when present.
func createTrailReviewFindings(ctx context.Context, client *api.Client, trailID string, inputs []api.TrailReviewCommentInput) ([]api.TrailReviewComment, error) {
	if len(inputs) == 0 {
		return nil, errors.New("create finding: no findings to post")
	}
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = ""
	}
	inputs = prepareTrailReviewCommentInputsForCreate(worktreeRoot, inputs)
	review, err := startTrailReview(ctx, client, trailID)
	if err != nil {
		return nil, err
	}
	limit := review.Limits.MaxCommentsPerBatch
	if limit <= 0 || limit > len(inputs) {
		limit = len(inputs)
	}
	findings := make([]api.TrailReviewComment, 0, len(inputs))
	for start := 0; start < len(inputs); start += limit {
		end := start + limit
		if end > len(inputs) {
			end = len(inputs)
		}
		batchFindings, err := postTrailReviewFindingBatch(ctx, client, trailID, review.ReviewID, inputs[start:end])
		if err != nil {
			return nil, err
		}
		findings = append(findings, batchFindings...)
	}
	return findings, nil
}

func postTrailReviewFindingBatch(ctx context.Context, client *api.Client, trailID, reviewID string, inputs []api.TrailReviewCommentInput) ([]api.TrailReviewComment, error) {
	resp, err := client.Post(ctx, trailReviewBatchCommentsPath(trailID, reviewID), api.TrailReviewCommentBatchRequest{Comments: inputs})
	if err != nil {
		return nil, fmt.Errorf("create finding: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}
	var batch api.TrailReviewCommentBatchResponse
	if err := api.DecodeJSON(resp, &batch); err != nil {
		return nil, fmt.Errorf("decode finding batch response: %w", err)
	}
	if len(batch.Results) == 0 {
		return nil, errors.New("create finding: server returned no results")
	}
	findings := make([]api.TrailReviewComment, 0, len(batch.Results))
	for _, result := range batch.Results {
		if result.Status == trailReviewBatchResultError {
			if result.Error != nil {
				return nil, fmt.Errorf("create finding: %s: %s", result.Error.Code, result.Error.Message)
			}
			return nil, errors.New("create finding: server reported an error")
		}
		if result.Comment == nil {
			return nil, errors.New("create finding: server did not return the finding")
		}
		findings = append(findings, *result.Comment)
	}
	return findings, nil
}

// startTrailReview opens a review session for a trail. The body is left empty
// so the server resolves the code version (base/head) from the trail itself.
// The API rejects a lone head_sha — base_sha and head_sha must be supplied
// together or not at all — and computing a correct base SHA client-side is out
// of scope for a single-finding add, so neither is sent.
func startTrailReview(ctx context.Context, client *api.Client, trailID string) (api.TrailReviewStartResponse, error) {
	resp, err := client.Post(ctx, trailReviewStartPath(trailID), api.TrailReviewStartRequest{})
	if err != nil {
		return api.TrailReviewStartResponse{}, fmt.Errorf("start review: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailReviewStartResponse{}, err
	}
	var out api.TrailReviewStartResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return api.TrailReviewStartResponse{}, fmt.Errorf("decode review: %w", err)
	}
	if out.ReviewID == "" {
		return api.TrailReviewStartResponse{}, errors.New("start review: server returned an empty review id")
	}
	return out, nil
}

func trailReviewStartPath(trailID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews"
}

func trailReviewBatchCommentsPath(trailID, reviewID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/" + url.PathEscape(reviewID) + "/comments"
}

func trailReviewListCommentsPath(trailID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/comments"
}

func resolveTrailReviewComment(ctx context.Context, client *api.Client, trailID, commentID string) (api.TrailReviewComment, error) {
	opts := trailReviewListOptions{
		Status:           trailReviewStatusAny,
		Freshness:        trailReviewFreshnessAny,
		IncludeDismissed: true,
		Limit:            defaultTrailReviewLimit,
	}
	var matches []api.TrailReviewComment
	for {
		comments, hasMore, err := fetchTrailReviewComments(ctx, client, trailID, opts)
		if err != nil {
			return api.TrailReviewComment{}, err
		}
		for _, comment := range comments {
			if comment.ID == commentID {
				return comment, nil
			}
			if strings.HasPrefix(comment.ID, commentID) {
				matches = append(matches, comment)
			}
		}
		if !hasMore {
			break
		}
		opts.Offset += opts.Limit
	}
	switch len(matches) {
	case 0:
		return api.TrailReviewComment{}, fmt.Errorf("no finding %q found", commentID)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i := range matches {
			ids[i] = matches[i].ID
		}
		sort.Strings(ids)
		return api.TrailReviewComment{}, fmt.Errorf("ambiguous finding %q (matches: %s)", commentID, strings.Join(ids, ", "))
	}
}

func hydrateTrailReviewCommentWithState(ctx context.Context, client *api.Client, trailID string, comment api.TrailReviewComment) (api.TrailReviewComment, api.TrailReviewStateResponse, error) {
	state, err := fetchTrailReviewState(ctx, client, trailID, comment.ReviewID)
	if err != nil {
		return api.TrailReviewComment{}, api.TrailReviewStateResponse{}, err
	}
	for _, candidate := range state.Comments {
		if candidate.ID == comment.ID {
			return candidate, state, nil
		}
	}
	return api.TrailReviewComment{}, api.TrailReviewStateResponse{}, fmt.Errorf("finding %s details were not returned by the API", comment.ID)
}

func fetchTrailReviewState(ctx context.Context, client *api.Client, trailID, reviewID string) (api.TrailReviewStateResponse, error) {
	var merged api.TrailReviewStateResponse
	cursor := ""
	seenCursors := map[string]bool{}
	for {
		resp, err := client.Get(ctx, trailReviewStatePath(trailID, reviewID, cursor))
		if err != nil {
			return api.TrailReviewStateResponse{}, fmt.Errorf("get review state: %w", err)
		}
		var page api.TrailReviewStateResponse
		decodeErr := func() error {
			defer resp.Body.Close()
			if err := checkTrailResponse(resp); err != nil {
				return err
			}
			if err := api.DecodeJSON(resp, &page); err != nil {
				return fmt.Errorf("decode review state: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return api.TrailReviewStateResponse{}, decodeErr
		}

		if cursor == "" {
			merged = page
		} else {
			merged.Comments = append(merged.Comments, page.Comments...)
			merged.NextCursor = page.NextCursor
			merged.EventCursor = page.EventCursor
		}

		if page.NextCursor == nil || strings.TrimSpace(*page.NextCursor) == "" {
			merged.NextCursor = nil
			break
		}
		cursor = strings.TrimSpace(*page.NextCursor)
		if seenCursors[cursor] {
			return api.TrailReviewStateResponse{}, fmt.Errorf("review state pagination repeated cursor %q", cursor)
		}
		seenCursors[cursor] = true
	}
	return merged, nil
}

func trailReviewStatePath(trailID, reviewID, cursor string) string {
	q := url.Values{}
	q.Set("include_dismissed", "true")
	q.Set("stale", trailReviewFreshnessAny)
	q.Set("limit", strconv.Itoa(defaultTrailReviewLimit))
	if strings.TrimSpace(cursor) != "" {
		q.Set("cursor", strings.TrimSpace(cursor))
	}
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/" + url.PathEscape(reviewID) + "?" + q.Encode()
}

func verifyTrailReviewHead(ctx context.Context, state api.TrailReviewStateResponse) error {
	want := strings.TrimSpace(stringPtrValue(state.CodeVersion.HeadSHA))
	if want == "" {
		return nil
	}
	got, err := resolveGitRev(ctx, "HEAD")
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("finding was created for head %s, but current HEAD is %s; check out that commit before applying", abbreviate12(want), abbreviate12(got))
	}
	return nil
}

func applyTrailReviewSuggestions(ctx context.Context, comment api.TrailReviewComment, checkOnly bool, w io.Writer) (int, error) {
	if len(comment.SuggestedChanges) == 0 {
		return 0, fmt.Errorf("finding %s has no suggested changes", comment.ID)
	}
	combinedPatch, supported, err := combinedSafeUnifiedDiffPatch(comment, w)
	if err != nil {
		return 0, err
	}
	if supported == 0 {
		return 0, fmt.Errorf("finding %s has no supported unified_diff suggested changes", comment.ID)
	}
	if err := runGitApply(ctx, combinedPatch, true); err != nil {
		return 0, fmt.Errorf("suggested changes for finding %s do not apply cleanly: %w", comment.ID, err)
	}
	if checkOnly {
		return supported, nil
	}
	if err := runGitApply(ctx, combinedPatch, false); err != nil {
		return 0, fmt.Errorf("apply suggested changes for finding %s: %w", comment.ID, err)
	}
	return supported, nil
}

func combinedSafeUnifiedDiffPatch(comment api.TrailReviewComment, w io.Writer) (string, int, error) {
	var combined strings.Builder
	supported := 0
	for _, change := range comment.SuggestedChanges {
		patch := strings.TrimSpace(stringPtrValue(change.Patch))
		if change.ChangeType != "unified_diff" || patch == "" {
			fmt.Fprintf(w, "Skipping suggested change %s (%s): only unified_diff patches are supported.\n", change.ID, change.ChangeType)
			continue
		}
		if err := validateUnifiedDiffPatchPaths(patch); err != nil {
			return "", 0, fmt.Errorf("suggested change %s has unsafe patch path: %w", change.ID, err)
		}
		if combined.Len() > 0 {
			combined.WriteByte('\n')
		}
		combined.WriteString(patch)
		combined.WriteByte('\n')
		supported++
	}
	return combined.String(), supported, nil
}

func validateUnifiedDiffPatchPaths(patchText string) error {
	scanner := bufio.NewScanner(strings.NewReader(patchText))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		for _, p := range patchHeaderPaths(line) {
			if err := validatePatchPath(p); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan patch: %w", err)
	}
	return nil
}

func patchHeaderPaths(line string) []string {
	switch {
	case strings.HasPrefix(line, "diff --git "):
		fields := strings.Fields(strings.TrimPrefix(line, "diff --git "))
		if len(fields) >= 2 {
			return []string{fields[0], fields[1]}
		}
	case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
		return []string{patchHeaderPath(line[4:])}
	case strings.HasPrefix(line, "rename from "):
		return []string{strings.TrimSpace(strings.TrimPrefix(line, "rename from "))}
	case strings.HasPrefix(line, "rename to "):
		return []string{strings.TrimSpace(strings.TrimPrefix(line, "rename to "))}
	case strings.HasPrefix(line, "copy from "):
		return []string{strings.TrimSpace(strings.TrimPrefix(line, "copy from "))}
	case strings.HasPrefix(line, "copy to "):
		return []string{strings.TrimSpace(strings.TrimPrefix(line, "copy to "))}
	}
	return nil
}

func patchHeaderPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if beforeTab, _, ok := strings.Cut(raw, "\t"); ok {
		raw = beforeTab
	}
	return raw
}

func validatePatchPath(raw string) error {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/dev/null" {
		return nil
	}
	if unquoted, err := strconv.Unquote(p); err == nil {
		p = unquoted
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("absolute path %q is not allowed", raw)
	}
	clean := path.Clean(p)
	if clean == "." || clean == "" {
		return nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q escapes the repository", raw)
	}
	for _, part := range strings.Split(clean, "/") {
		// EqualFold guards case-insensitive filesystems (default macOS/Windows)
		// where ".GIT/config" would otherwise slip past an exact match.
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("path %q targets .git metadata", raw)
		}
	}
	return nil
}

func runGitApply(ctx context.Context, patch string, checkOnly bool) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	args := []string{"-C", root, "apply"}
	if checkOnly {
		args = append(args, "--check")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = bytes.NewBufferString(patch + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("git apply: %w: %s", err, msg)
		}
		return fmt.Errorf("git apply: %w", err)
	}
	return nil
}

func patchTrailReviewCommentStatus(ctx context.Context, client *api.Client, trailID string, comment api.TrailReviewComment, status, reason string) (api.TrailReviewComment, error) {
	body := api.TrailReviewCommentPatchRequest{Status: status}
	if strings.TrimSpace(reason) != "" {
		body.StatusReason = stringPtr(reason)
	}
	return patchTrailReviewComment(ctx, client, trailID, comment, body)
}

func patchTrailReviewComment(ctx context.Context, client *api.Client, trailID string, comment api.TrailReviewComment, body api.TrailReviewCommentPatchRequest) (api.TrailReviewComment, error) {
	resp, err := client.Patch(ctx, trailReviewCommentPath(trailID, comment.ReviewID, comment.ID), body)
	if err != nil {
		return api.TrailReviewComment{}, fmt.Errorf("update finding: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailReviewComment{}, err
	}
	var updated api.TrailReviewComment
	if err := api.DecodeJSON(resp, &updated); err != nil {
		return api.TrailReviewComment{}, fmt.Errorf("decode updated finding: %w", err)
	}
	return updated, nil
}

func trailReviewCommentPath(trailID, reviewID, commentID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/" + url.PathEscape(reviewID) + "/comments/" + url.PathEscape(commentID)
}

func resolveGitRev(ctx context.Context, ref string) (string, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("git rev-parse %s: empty output", ref)
	}
	return sha, nil
}

func encodeTrailReviewJSON(w io.Writer, target trailReviewTarget, comments []api.TrailReviewComment, hasMore bool, counts trailReviewCommentCounts) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"trail":    target.Trail,
		"counts":   counts,
		"findings": comments,
		"has_more": hasMore,
	}); err != nil {
		return fmt.Errorf("encode trail findings JSON: %w", err)
	}
	return nil
}

func printTrailReviewDashboard(w io.Writer, target trailReviewTarget, comments []api.TrailReviewComment, hasMore bool, opts trailReviewListOptions, counts trailReviewCommentCounts) {
	trail := target.Trail
	if trail.Number > 0 {
		fmt.Fprintf(w, "  Trail #%d  %s\n", trail.Number, trail.Title)
	} else {
		fmt.Fprintf(w, "  Trail %s  %s\n", trail.ID, trail.Title)
	}
	fmt.Fprintf(w, "  Status: %s · Branch: %s · Base: %s\n", trail.Status, trail.Branch, trail.Base)
	fmt.Fprintf(w, "  ID: %s\n\n", trail.ID)

	fmt.Fprintf(w, "  Open findings: %d  high %d  medium %d  low %d\n", counts.Open, counts.OpenHigh, counts.OpenMedium, counts.OpenLow)
	fmt.Fprintf(w, "  Resolved: %d        Dismissed: %d     Stale: %d\n", counts.Resolved, counts.Dismissed, counts.Stale)
	if hasMore {
		fmt.Fprintf(w, "  Showing first %d findings; rerun with --offset for more.\n", opts.Limit)
	}
	fmt.Fprintln(w)

	if len(comments) == 0 {
		fmt.Fprintln(w, "No findings match the current filters.")
		return
	}
	printTrailReviewCommentsTable(w, comments)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  entire trail finding show <finding-id>")
	fmt.Fprintln(w, "  entire trail finding add -m \"finding body\" --file path --line 42")
	fmt.Fprintln(w, "  entire trail finding update <finding-id> -m \"updated body\"")
	fmt.Fprintln(w, "  entire trail finding apply <finding-id> --resolve")
	fmt.Fprintln(w, "  entire trail finding resolve <finding-id> -m \"fixed in <sha>\"")
	fmt.Fprintln(w, "  entire trail finding dismiss <finding-id> -m \"not applicable\"")
	fmt.Fprintln(w, "  entire trail watch")
}

func printTrailReviewComments(w io.Writer, comments []api.TrailReviewComment, hasMore bool) {
	if len(comments) == 0 {
		fmt.Fprintln(w, "No findings found.")
		return
	}
	printTrailReviewCommentsTable(w, comments)
	if hasMore {
		fmt.Fprintln(w, "More findings available; rerun with --offset for the next page.")
	}
}

func printTrailReviewCommentsTable(w io.Writer, comments []api.TrailReviewComment) {
	var table bytes.Buffer
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSEV\tSTATUS\tFRESHNESS\tLOCATION\tSUMMARY")
	for _, comment := range comments {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			abbreviate12(comment.ID),
			severityTableDisplay(comment.Severity),
			comment.Status,
			trailReviewFreshnessDisplay(comment),
			trailReviewLocationDisplay(comment.Location),
			trailReviewCommentSummary(comment),
		)
	}
	_ = tw.Flush()
	printIndentedBlock(w, table.String(), "  ")
}

func printIndentedBlock(w io.Writer, block, indent string) {
	for line := range strings.SplitSeq(strings.TrimRight(block, "\n"), "\n") {
		fmt.Fprintln(w, indent+line)
	}
}

func printTrailReviewCommentCreated(w io.Writer, target trailReviewTarget, comment api.TrailReviewComment) {
	fmt.Fprintf(w, "Created finding %s on %s\n", comment.ID, trailReviewTargetDisplay(target))
	fmt.Fprintf(w, "Status:   %s\n", comment.Status)
	fmt.Fprintf(w, "Severity: %s\n", severityDisplay(comment.Severity))
	fmt.Fprintf(w, "Location: %s\n", trailReviewLocationDisplay(comment.Location))
}

func printTrailReviewCommentDetail(w io.Writer, comment api.TrailReviewComment) {
	fmt.Fprintf(w, "Finding:   %s\n", comment.ID)
	fmt.Fprintf(w, "Status:    %s\n", comment.Status)
	fmt.Fprintf(w, "Freshness: %s\n", trailReviewFreshnessDisplay(comment))
	fmt.Fprintf(w, "Severity:  %s\n", severityDisplay(comment.Severity))
	if comment.Confidence != nil {
		fmt.Fprintf(w, "Confidence: %.2f\n", *comment.Confidence)
	}
	fmt.Fprintf(w, "Location: %s\n", trailReviewLocationDisplay(comment.Location))
	if body := stringPtrValue(comment.Body); body != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(body))
	}
	if len(comment.SuggestedChanges) > 0 {
		fmt.Fprintln(w, "\nSuggested changes:")
		for _, change := range comment.SuggestedChanges {
			fmt.Fprintf(w, "- %s (%s)\n", change.ID, change.ChangeType)
			if change.ExpectedFilePath != nil && *change.ExpectedFilePath != "" {
				fmt.Fprintf(w, "  file: %s\n", *change.ExpectedFilePath)
			}
			if patch := stringPtrValue(change.Patch); patch != "" {
				fmt.Fprintf(w, "  patch:\n%s\n", strings.TrimSpace(patch))
			}
			if instruction := stringPtrValue(change.Instruction); instruction != "" {
				fmt.Fprintf(w, "  instruction: %s\n", instruction)
			}
		}
	}
}

func trailReviewTargetDisplay(target trailReviewTarget) string {
	if target.Trail.Number > 0 {
		return fmt.Sprintf("trail #%d (%s)", target.Trail.Number, target.Trail.Title)
	}
	if target.Trail.Branch != "" {
		return fmt.Sprintf("trail %s on %s", target.Trail.ID, target.Trail.Branch)
	}
	return "trail " + target.Trail.ID
}

type trailReviewCommentCounts struct {
	Open       int
	OpenHigh   int
	OpenMedium int
	OpenLow    int
	Resolved   int
	Dismissed  int
	Stale      int
}

func countTrailReviewComments(comments []api.TrailReviewComment) trailReviewCommentCounts {
	var counts trailReviewCommentCounts
	for _, comment := range comments {
		switch comment.Status {
		case trailReviewStatusResolved:
			counts.Resolved++
		case trailReviewStatusDismissed:
			counts.Dismissed++
		case trailReviewStatusOpen:
			counts.Open++
			switch strings.ToLower(stringPtrValue(comment.Severity)) {
			case trailReviewSeverityHigh:
				counts.OpenHigh++
			case trailReviewSeverityMedium:
				counts.OpenMedium++
			case trailReviewSeverityLow:
				counts.OpenLow++
			}
		}
		if comment.StaleOutcome == trailReviewFreshnessStale {
			counts.Stale++
		}
	}
	return counts
}

func trailReviewLocationDisplay(loc api.TrailReviewLocation) string {
	file := stringPtrValue(loc.FilePath)
	if file == "" {
		return loc.Granularity
	}
	if loc.StartLine == nil {
		return file
	}
	if loc.EndLine != nil && *loc.EndLine != *loc.StartLine {
		return fmt.Sprintf("%s:%d-%d", file, *loc.StartLine, *loc.EndLine)
	}
	return fmt.Sprintf("%s:%d", file, *loc.StartLine)
}

func trailReviewCommentSummary(comment api.TrailReviewComment) string {
	body := strings.TrimSpace(stringPtrValue(comment.Body))
	if body == "" {
		return "(empty finding)"
	}
	return truncateOneLine(body, 80)
}

func severityDisplay(severity *string) string {
	if severity == nil || strings.TrimSpace(*severity) == "" {
		return "-"
	}
	return *severity
}

func severityTableDisplay(severity *string) string {
	value := strings.ToLower(strings.TrimSpace(stringPtrValue(severity)))
	switch value {
	case trailReviewSeverityHigh:
		return "High"
	case trailReviewSeverityMedium:
		return "Medium"
	case trailReviewSeverityLow:
		return "Low"
	case "":
		return "-"
	default:
		return value
	}
}

func trailReviewFreshnessDisplay(comment api.TrailReviewComment) string {
	outcome := strings.TrimSpace(comment.StaleOutcome)
	if outcome == "" {
		return trailReviewFreshnessCurrent
	}
	return outcome
}

func defaultTrailReviewStatusReason(status string) string {
	switch status {
	case trailReviewStatusResolved:
		return "Resolved via Entire CLI"
	case trailReviewStatusDismissed:
		return "Dismissed via Entire CLI"
	case trailReviewStatusOpen:
		return "Reopened via Entire CLI"
	default:
		return "Updated via Entire CLI"
	}
}

func parseOptionalTrailSelector(args []string, flagSelector string) (string, error) {
	flagSelector = strings.TrimSpace(flagSelector)
	if len(args) == 0 {
		return flagSelector, nil
	}
	if flagSelector != "" {
		return "", errors.New("pass a trail either positionally or with --trail, not both")
	}
	selector := strings.TrimSpace(args[0])
	if selector == "" {
		return "", errors.New("trail selector cannot be empty")
	}
	return selector, nil
}

func parseTrailSelectorAndCommentID(args []string, flagSelector string) (string, string, error) {
	flagSelector = strings.TrimSpace(flagSelector)
	if len(args) == 1 {
		commentID := strings.TrimSpace(args[0])
		if commentID == "" {
			return "", "", errors.New("finding id cannot be empty")
		}
		return flagSelector, commentID, nil
	}
	if flagSelector != "" {
		return "", "", errors.New("pass a trail either positionally or with --trail, not both")
	}
	selector := strings.TrimSpace(args[0])
	commentID := strings.TrimSpace(args[1])
	if selector == "" {
		return "", "", errors.New("trail selector cannot be empty")
	}
	if commentID == "" {
		return "", "", errors.New("finding id cannot be empty")
	}
	return selector, commentID, nil
}

func optionalStringPtr(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func stringPtr(s string) *string {
	return &s
}

func stringPtrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func abbreviate12(s string) string {
	const n = 12
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncateOneLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
