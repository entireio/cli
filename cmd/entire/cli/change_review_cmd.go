package cli

import (
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
	defaultChangeReviewLimit     = 100
	changeReviewStatusAny        = "any"
	changeReviewFreshnessCurrent = "current"
	changeReviewFreshnessStale   = "stale"
	changeReviewFreshnessAny     = "any"
	changeReviewStatusOpen       = "open"
	changeReviewStatusResolved   = "resolved"
	changeReviewStatusDismissed  = "dismissed"
	// Per-finding batch-create outcome returned by the reviews/{id}/comments
	// endpoint that the CLI must surface as a failure. "created" and "existing"
	// are both success outcomes and need no dedicated handling.
	changeReviewBatchResultError = "error"
	changeReviewSeverityHigh     = "high"
	changeReviewSeverityMedium   = "medium"
	changeReviewSeverityLow      = "low"
)

var errChangeReviewDefaultTargetNotFound = errors.New("default change finding target not found")

type changeReviewListOptions struct {
	Status           string
	StatusChanged    bool
	Severity         string
	Freshness        string
	IncludeDismissed bool
	Limit            int
	Offset           int
	JSON             bool
}

type changeReviewTargetOptions struct {
	Selector string
	Branch   string
}

type changeReviewTarget struct {
	Host   string
	Owner  string
	Repo   string
	Change api.ChangeResource
}

func newChangeFindingCmd() *cobra.Command {
	opts := defaultChangeReviewListOptions()
	targetOpts := changeReviewTargetOptions{}

	cmd := &cobra.Command{
		Use:   "finding [<change>]",
		Short: "Manage a change's agent findings",
		Long: `Manage a change's agent-native findings.

Running 'entire change finding' shows the finding dashboard for the current
branch's change. Pass a change selector (number, id, or branch) to inspect another
change in the same repo. Use 'entire change list --status any' when you need to
discover a change selector first.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalChangeSelector(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			opts.StatusChanged = cmd.Flags().Changed("status")
			return runChangeReviewDashboard(cmd, selector, opts)
		},
	}
	cmd.PersistentFlags().StringVar(&targetOpts.Selector, "change", "", "Change selector (number, id, or branch); defaults to the current branch's change")
	cmd.PersistentFlags().StringVar(&targetOpts.Branch, "branch", "", "Resolve the change for this branch instead of the current branch; cannot be combined with a change selector")
	addChangeReviewListFlags(cmd, &opts)

	cmd.AddCommand(newChangeFindingListCmd(&targetOpts))
	cmd.AddCommand(newChangeFindingAddCmd(&targetOpts))
	cmd.AddCommand(newChangeReviewShowCmd(&targetOpts))
	cmd.AddCommand(newChangeReviewUpdateCmd(&targetOpts))
	cmd.AddCommand(newChangeReviewApplyCmd(&targetOpts))
	cmd.AddCommand(newChangeReviewStatusCmd(&targetOpts, "resolve", changeReviewStatusResolved, "Resolve a finding"))
	cmd.AddCommand(newChangeReviewStatusCmd(&targetOpts, "dismiss", changeReviewStatusDismissed, "Dismiss a finding"))
	cmd.AddCommand(newChangeReviewStatusCmd(&targetOpts, "reopen", changeReviewStatusOpen, "Reopen a finding"))

	return cmd
}

func defaultChangeReviewListOptions() changeReviewListOptions {
	return changeReviewListOptions{
		Status:    changeReviewStatusOpen,
		Freshness: changeReviewFreshnessCurrent,
		Limit:     defaultChangeReviewLimit,
	}
}

func addChangeReviewListFlags(cmd *cobra.Command, opts *changeReviewListOptions) {
	cmd.Flags().StringVar(&opts.Status, "status", opts.Status, "Filter by lifecycle status(es): open,resolved,dismissed; use 'any' for all")
	cmd.Flags().StringVar(&opts.Severity, "severity", "", "Filter by comma-separated severity value(s): high,medium,low")
	cmd.Flags().StringVar(&opts.Freshness, "freshness", opts.Freshness, "Filter code-version freshness: current,stale,any")
	cmd.Flags().BoolVar(&opts.IncludeDismissed, "include-dismissed", false, "Include dismissed findings")
	cmd.Flags().IntVarP(&opts.Limit, "limit", "n", opts.Limit, "Maximum number of findings to show")
	cmd.Flags().IntVar(&opts.Offset, "offset", 0, "Pagination offset")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
}

func newChangeFindingListCmd(targetOpts *changeReviewTargetOptions) *cobra.Command {
	opts := defaultChangeReviewListOptions()
	cmd := &cobra.Command{
		Use:   "list [<change>]",
		Short: "List findings for a change",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalChangeSelector(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			opts.StatusChanged = cmd.Flags().Changed("status")
			return runChangeReviewComments(cmd, selector, opts)
		},
	}
	addChangeReviewListFlags(cmd, &opts)
	return cmd
}

type changeReviewCommentAddOptions struct {
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

func newChangeFindingAddCmd(targetOpts *changeReviewTargetOptions) *cobra.Command {
	var opts changeReviewCommentAddOptions
	cmd := &cobra.Command{
		Use:   "add [<change>]",
		Short: "Create a finding on a change",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalChangeSelector(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runChangeReviewCommentAdd(cmd, selector, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.Body, "body", "m", "", "Finding body")
	cmd.Flags().StringVar(&opts.Severity, "severity", "", "Finding severity: high,medium,low")
	cmd.Flags().Float64Var(&opts.Confidence, "confidence", -1, "Finding confidence from 0.0 to 1.0")
	cmd.Flags().StringVar(&opts.FilePath, "file", "", "File path for the finding location; defaults to the file a --patch modifies")
	cmd.Flags().IntVar(&opts.Line, "line", 0, "Line number for the finding location")
	cmd.Flags().IntVar(&opts.StartLine, "start-line", 0, "Start line for the finding location")
	cmd.Flags().IntVar(&opts.EndLine, "end-line", 0, "End line for the finding location")
	cmd.Flags().StringVar(&opts.ClientID, "client-id", "", "Client-provided idempotency key for this finding")
	cmd.Flags().StringVar(&opts.Patch, "patch", "", "Unified-diff suggested change to attach; must modify a single existing file in this worktree")
	cmd.Flags().StringVar(&opts.PatchFile, "patch-file", "", "Read unified-diff suggested change from file; use '-' for stdin")
	cmd.Flags().StringVar(&opts.Instruction, "instruction", "", "Manual suggested-change instruction to attach")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
	return cmd
}

func newChangeReviewShowCmd(targetOpts *changeReviewTargetOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [<change>] <finding-id>",
		Short: "Show a finding",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, commentID, err := parseChangeSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runChangeReviewShow(cmd, selector, commentID)
		},
	}
	return cmd
}

type changeReviewUpdateOptions struct {
	Body              string
	BodyChanged       bool
	Severity          string
	SeverityChanged   bool
	Confidence        float64
	ConfidenceChanged bool
	JSON              bool
}

func newChangeReviewUpdateCmd(targetOpts *changeReviewTargetOptions) *cobra.Command {
	opts := changeReviewUpdateOptions{Confidence: -1}
	cmd := &cobra.Command{
		Use:   "update [<change>] <finding-id>",
		Short: "Update a finding's metadata",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, commentID, err := parseChangeSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			opts.BodyChanged = cmd.Flags().Changed("body")
			opts.SeverityChanged = cmd.Flags().Changed("severity")
			opts.ConfidenceChanged = cmd.Flags().Changed("confidence")
			return runChangeReviewUpdate(cmd, selector, commentID, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.Body, "body", "m", "", "Finding body")
	cmd.Flags().StringVar(&opts.Severity, "severity", "", "Finding severity: high,medium,low")
	cmd.Flags().Float64Var(&opts.Confidence, "confidence", -1, "Finding confidence from 0.0 to 1.0")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
	return cmd
}

type changeReviewApplyOptions struct {
	Resolve bool
	Check   bool
}

func newChangeReviewApplyCmd(targetOpts *changeReviewTargetOptions) *cobra.Command {
	var opts changeReviewApplyOptions
	cmd := &cobra.Command{
		Use:   "apply [<change>] <finding-id>",
		Short: "Apply a finding's unified-diff suggestion",
		Long: `Apply a finding's unified-diff suggestion to the current worktree.

By default this only changes files. Pass --resolve to update the finding's
lifecycle status after the patch applies successfully.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureNoChangeRepoOverride(cmd, "change finding apply"); err != nil {
				return err
			}
			selector, commentID, err := parseChangeSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runChangeReviewApply(cmd, selector, commentID, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.Resolve, "resolve", false, "Mark the finding resolved after applying")
	cmd.Flags().BoolVar(&opts.Check, "check", false, "Only check whether the patch applies; do not modify files")
	return cmd
}

func newChangeReviewStatusCmd(targetOpts *changeReviewTargetOptions, use, status, short string) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   use + " [<change>] <finding-id>",
		Short: short,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, commentID, err := parseChangeSelectorAndCommentID(args, targetOpts.Selector)
			if err != nil {
				return err
			}
			return runChangeReviewSetStatus(cmd, selector, commentID, status, message)
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Status reason to record")
	return cmd
}

func runChangeReviewDashboard(cmd *cobra.Command, selector string, opts changeReviewListOptions) error {
	var err error
	opts, err = normalizeChangeReviewListOptions(opts)
	if err != nil {
		return err
	}
	client, target, err := authenticatedChangeReviewTarget(cmd, selector)
	if err != nil {
		if strings.TrimSpace(selector) == "" && errors.Is(err, errChangeReviewDefaultTargetNotFound) {
			fmt.Fprintln(cmd.OutOrStdout(), "No change found for the current branch; showing changes in this repo.")
			fmt.Fprintln(cmd.OutOrStdout())
			return runChangeListAll(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), changeListOptions{Status: changeListStatusAny, Limit: defaultChangeListLimit, InsecureHTTP: changeInsecureHTTP(cmd), Repo: changeRepoFlag(cmd)})
		}
		return err
	}
	comments, hasMore, err := fetchChangeReviewComments(cmd.Context(), client, target.Change.ID, opts)
	if err != nil {
		return err
	}
	summaryComments, err := fetchAllChangeReviewComments(cmd.Context(), client, target.Change.ID, changeReviewSummaryOptions())
	if err != nil {
		return err
	}
	counts := countChangeReviewComments(summaryComments)
	if opts.JSON {
		return encodeChangeReviewJSON(cmd.OutOrStdout(), target, comments, hasMore, counts)
	}
	printChangeReviewDashboard(cmd.OutOrStdout(), target, comments, hasMore, opts, counts)
	return nil
}

func runChangeReviewComments(cmd *cobra.Command, selector string, opts changeReviewListOptions) error {
	var err error
	opts, err = normalizeChangeReviewListOptions(opts)
	if err != nil {
		return err
	}
	client, target, err := authenticatedChangeReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comments, hasMore, err := fetchChangeReviewComments(cmd.Context(), client, target.Change.ID, opts)
	if err != nil {
		return err
	}
	if opts.JSON {
		return encodeChangeReviewJSON(cmd.OutOrStdout(), target, comments, hasMore, countChangeReviewComments(comments))
	}
	printChangeReviewComments(cmd.OutOrStdout(), comments, hasMore)
	return nil
}

func runChangeReviewCommentAdd(cmd *cobra.Command, selector string, opts changeReviewCommentAddOptions) error {
	client, target, err := authenticatedChangeReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	opts, err = loadChangeReviewCommentPatchFile(opts, cmd.InOrStdin())
	if err != nil {
		return err
	}
	// An --instruction-only finding needs no anchor; a patch always does.
	var anchor *changeReviewPatchAnchor
	if patch := strings.TrimSpace(opts.Patch); patch != "" {
		if anchor, err = resolveChangeReviewPatchAnchor(cmd.Context(), patch); err != nil {
			return err
		}
	}
	input, err := buildChangeReviewCommentInput(opts, anchor)
	if err != nil {
		return err
	}
	created, err := createChangeReviewFinding(cmd.Context(), client, target.Change.ID, input)
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
	printChangeReviewCommentCreated(cmd.OutOrStdout(), target, created)
	return nil
}

func runChangeReviewShow(cmd *cobra.Command, selector string, commentID string) error {
	client, target, err := authenticatedChangeReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveChangeReviewComment(cmd.Context(), client, target.Change.ID, commentID)
	if err != nil {
		return err
	}
	if hydrated, _, hydrateErr := hydrateChangeReviewCommentWithState(cmd.Context(), client, target.Change.ID, comment); hydrateErr == nil {
		comment = hydrated
	}
	printChangeReviewCommentDetail(cmd.OutOrStdout(), comment)
	return nil
}

func runChangeReviewUpdate(cmd *cobra.Command, selector string, commentID string, opts changeReviewUpdateOptions) error {
	client, target, err := authenticatedChangeReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveChangeReviewComment(cmd.Context(), client, target.Change.ID, commentID)
	if err != nil {
		return err
	}
	req, err := buildChangeReviewCommentPatchRequest(opts)
	if err != nil {
		return err
	}
	updated, err := patchChangeReviewComment(cmd.Context(), client, target.Change.ID, comment, req)
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

func runChangeReviewApply(cmd *cobra.Command, selector string, commentID string, opts changeReviewApplyOptions) error {
	client, target, err := authenticatedChangeReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveChangeReviewComment(cmd.Context(), client, target.Change.ID, commentID)
	if err != nil {
		return err
	}
	comment, state, err := hydrateChangeReviewCommentWithState(cmd.Context(), client, target.Change.ID, comment)
	if err != nil {
		return err
	}
	if err := verifyChangeReviewHead(cmd.Context(), state); err != nil {
		return err
	}
	applied, err := applyChangeReviewSuggestions(cmd.Context(), comment, opts.Check, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	if opts.Check {
		fmt.Fprintf(cmd.OutOrStdout(), "%d suggested change(s) apply cleanly.\n", applied)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Applied %d suggested change(s).\n", applied)
	if opts.Resolve {
		updated, err := patchChangeReviewCommentStatus(cmd.Context(), client, target.Change.ID, comment, changeReviewStatusResolved, "Applied via Entire CLI")
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Resolved finding %s after apply: %s → %s\n", updated.ID, comment.Status, updated.Status)
	}
	return nil
}

func runChangeReviewSetStatus(cmd *cobra.Command, selector string, commentID, status, message string) error {
	client, target, err := authenticatedChangeReviewTarget(cmd, selector)
	if err != nil {
		return err
	}
	comment, err := resolveChangeReviewComment(cmd.Context(), client, target.Change.ID, commentID)
	if err != nil {
		return err
	}
	oldStatus := comment.Status
	if message == "" {
		message = defaultChangeReviewStatusReason(status)
	}
	updated, err := patchChangeReviewCommentStatus(cmd.Context(), client, target.Change.ID, comment, status, message)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated finding %s: %s → %s\n", updated.ID, oldStatus, updated.Status)
	return nil
}

func authenticatedChangeReviewTarget(cmd *cobra.Command, selector string) (*api.Client, changeReviewTarget, error) {
	repoOverride := changeRepoFlag(cmd)
	branchOverride := changeBranchFlag(cmd)
	if selector != "" && branchOverride != "" {
		return nil, changeReviewTarget{}, errors.New("pass a change selector or --branch, not both")
	}
	if repoOverride != "" && selector == "" && branchOverride == "" {
		return nil, changeReviewTarget{}, errors.New("--repo requires an explicit target: pass a change selector or --branch")
	}
	var target changeReviewTarget
	var resolvedClient *api.Client
	err := runAuthenticatedChangeAPI(cmd.Context(), cmd.ErrOrStderr(), changeInsecureHTTP(cmd), repoOverride, func(ctx context.Context, client *api.Client) error {
		var err error
		resolvedClient = client
		target, err = resolveChangeReviewTarget(ctx, client, selector, repoOverride, branchOverride)
		return err
	})
	if err != nil {
		return nil, changeReviewTarget{}, err
	}
	return resolvedClient, target, nil
}

func resolveChangeReviewTarget(ctx context.Context, client *api.Client, selector, repoOverride, branchOverride string) (changeReviewTarget, error) {
	host, owner, repo, err := resolveChangeRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return changeReviewTarget{}, err
	}

	selector = strings.TrimSpace(selector)
	var found *api.ChangeResource
	if selector != "" {
		found, err = findChangeBySelector(ctx, client, host, owner, repo, selector)
		if err != nil {
			return changeReviewTarget{}, err
		}
		if found == nil {
			return changeReviewTarget{}, fmt.Errorf("no change %q found in %s/%s/%s (run 'entire change list --status any')", selector, host, owner, repo)
		}
	} else {
		branch, branchErr := resolveChangeBranch(ctx, branchOverride)
		if branchErr != nil {
			return changeReviewTarget{}, fmt.Errorf("%w: no change selector given and current branch is unknown: %w\nhint: run 'entire change list --status any' or pass --change <number|id|branch>", errChangeReviewDefaultTargetNotFound, branchErr)
		}
		found, err = findChangeByBranch(ctx, client, host, owner, repo, branch)
		if err != nil {
			return changeReviewTarget{}, err
		}
		if found == nil {
			return changeReviewTarget{}, fmt.Errorf("%w: no change found for branch %q\nhint: run 'entire change create', 'entire change list --status any', or pass --change <number|id|branch>", errChangeReviewDefaultTargetNotFound, branch)
		}
	}
	if found.ID == "" {
		return changeReviewTarget{}, errors.New("change has no id yet")
	}
	if found.Number <= 0 {
		return changeReviewTarget{}, errors.New("change has no number yet")
	}
	// Review helpers carry the stable change ID internally, but entire-api's
	// public routes are repo/number addressed. Register that translation once
	// when the target is resolved so findings, snapshots, and SSE all hit the
	// owning cell's native route.
	client.SetChangeRoute(found.ID, changeNumberPath(host, owner, repo, found.Number))
	return changeReviewTarget{Host: host, Owner: owner, Repo: repo, Change: *found}, nil
}

func normalizeChangeReviewListOptions(opts changeReviewListOptions) (changeReviewListOptions, error) {
	if opts.Limit <= 0 {
		return opts, errors.New("limit must be greater than 0")
	}
	if opts.Offset < 0 {
		return opts, errors.New("offset must be non-negative")
	}
	status, err := normalizeChangeReviewStatusFilter(opts.Status)
	if err != nil {
		return opts, err
	}
	opts.Status = status
	severity, err := normalizeCommaSet(opts.Severity, "severity", map[string]bool{
		changeReviewSeverityHigh: true, changeReviewSeverityMedium: true, changeReviewSeverityLow: true,
	})
	if err != nil {
		return opts, err
	}
	opts.Severity = severity
	if freshness := strings.TrimSpace(opts.Freshness); freshness != "" {
		switch freshness {
		case changeReviewFreshnessCurrent, changeReviewFreshnessStale, changeReviewFreshnessAny:
		default:
			return opts, fmt.Errorf("invalid freshness filter %q: valid values are current, stale, any", opts.Freshness)
		}
		opts.Freshness = freshness
	}
	// `--include-dismissed` should do what it says for the common case: when the
	// caller did not explicitly choose a status, do not keep the default open-only
	// status filter that would still hide dismissed findings.
	if opts.IncludeDismissed && !opts.StatusChanged && strings.TrimSpace(opts.Status) == changeReviewStatusOpen {
		opts.Status = changeReviewStatusAny
	}
	return opts, nil
}

func normalizeChangeReviewStatusFilter(filter string) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" || filter == changeReviewStatusAny {
		return filter, nil
	}
	return normalizeCommaSet(filter, "status", map[string]bool{
		changeReviewStatusOpen: true, changeReviewStatusResolved: true, changeReviewStatusDismissed: true,
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

func fetchChangeReviewComments(ctx context.Context, client *api.Client, changeID string, opts changeReviewListOptions) ([]api.ChangeReviewComment, bool, error) {
	var err error
	opts, err = normalizeChangeReviewListOptions(opts)
	if err != nil {
		return nil, false, err
	}
	resp, err := client.Get(ctx, changeReviewCommentsPath(changeID, opts))
	if err != nil {
		return nil, false, fmt.Errorf("list findings: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		return nil, false, err
	}
	var out api.ChangeReviewCommentsResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return nil, false, fmt.Errorf("decode findings: %w", err)
	}
	return out.Comments, out.HasMore, nil
}

func fetchAllChangeReviewComments(ctx context.Context, client *api.Client, changeID string, opts changeReviewListOptions) ([]api.ChangeReviewComment, error) {
	if opts.Limit <= 0 {
		opts.Limit = defaultChangeReviewLimit
	}
	var all []api.ChangeReviewComment
	seenPages := make(map[string]bool)
	for {
		comments, hasMore, err := fetchChangeReviewComments(ctx, client, changeID, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if !hasMore {
			break
		}
		signature := changeReviewCommentPageSignature(comments)
		if signature == "" {
			return nil, errors.New("finding pagination returned hasMore with an empty page")
		}
		if seenPages[signature] {
			return nil, fmt.Errorf("finding pagination repeated page at offset %d", opts.Offset)
		}
		seenPages[signature] = true
		opts.Offset += opts.Limit
	}
	return all, nil
}

func changeReviewCommentPageSignature(comments []api.ChangeReviewComment) string {
	if len(comments) == 0 {
		return ""
	}
	return comments[0].ID + ":" + comments[len(comments)-1].ID
}

func changeReviewSummaryOptions() changeReviewListOptions {
	return changeReviewListOptions{
		Status:           changeReviewStatusAny,
		Freshness:        changeReviewFreshnessAny,
		IncludeDismissed: true,
		Limit:            defaultChangeReviewLimit,
	}
}

func changeReviewCommentsPath(changeID string, opts changeReviewListOptions) string {
	// entire-api's review reads use include_dismissed plus limit/offset and
	// return hasMore/nextOffset; they are not pageSize/pageToken.
	q := url.Values{}
	if opts.Status != "" && opts.Status != changeReviewStatusAny {
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
	path := changeReviewListCommentsPath(changeID)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

func loadChangeReviewCommentPatchFile(opts changeReviewCommentAddOptions, stdin io.Reader) (changeReviewCommentAddOptions, error) {
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

func buildChangeReviewCommentPatchRequest(opts changeReviewUpdateOptions) (api.ChangeReviewCommentPatchRequest, error) {
	var req api.ChangeReviewCommentPatchRequest
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
		case changeReviewSeverityHigh, changeReviewSeverityMedium, changeReviewSeverityLow:
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
	if !changeReviewCommentPatchHasFields(req) {
		return req, errors.New("at least one finding field is required (pass --body, --severity, or --confidence)")
	}
	return req, nil
}

func changeReviewCommentPatchHasFields(req api.ChangeReviewCommentPatchRequest) bool {
	return req.Body != nil || req.Severity != nil || req.Confidence != nil || req.Status != "" || req.StatusReason != nil
}

func buildChangeReviewCommentInput(opts changeReviewCommentAddOptions, anchor *changeReviewPatchAnchor) (api.ChangeReviewCommentInput, error) {
	body := strings.TrimSpace(opts.Body)
	if body == "" {
		return api.ChangeReviewCommentInput{}, errors.New("finding body is required (pass --body)")
	}
	severity := strings.ToLower(strings.TrimSpace(opts.Severity))
	if severity != "" {
		switch severity {
		case changeReviewSeverityHigh, changeReviewSeverityMedium, changeReviewSeverityLow:
		default:
			return api.ChangeReviewCommentInput{}, fmt.Errorf("invalid severity %q: valid values are high, medium, low", opts.Severity)
		}
	}
	confidence, hasConfidence, err := buildChangeReviewCommentConfidence(opts.Confidence)
	if err != nil {
		return api.ChangeReviewCommentInput{}, err
	}
	loc, err := buildChangeReviewCommentLocation(opts, anchor)
	if err != nil {
		return api.ChangeReviewCommentInput{}, err
	}
	// client_id is the API's idempotency key and is now required. Honour an
	// explicit --client-id when given, otherwise generate a unique one so a
	// retried add doesn't create duplicate findings within the same key.
	clientID := strings.TrimSpace(opts.ClientID)
	if clientID == "" {
		clientID = generateChangeReviewClientID()
	}
	input := api.ChangeReviewCommentInput{
		ClientID: clientID,
		Body:     stringPtr(body),
		Severity: optionalStringPtr(severity),
		Location: loc,
	}
	if hasConfidence {
		input.Confidence = &confidence
	}
	if patch := strings.TrimSpace(opts.Patch); patch != "" {
		// The API rejects a unified_diff that carries no pre-image anchor, so a
		// patch without one must not reach the wire.
		if anchor == nil {
			return api.ChangeReviewCommentInput{}, errors.New("suggested-change patch has no resolved file anchor")
		}
		input.SuggestedChange = &api.ChangeReviewSuggestedChangeCreateRequest{
			ChangeType:        "unified_diff",
			Patch:             stringPtr(patch),
			Instruction:       optionalStringPtr(strings.TrimSpace(opts.Instruction)),
			ExpectedFilePath:  stringPtr(anchor.FilePath),
			ExpectedFileHash:  stringPtr(anchor.FileHash),
			ExpectedStartLine: &anchor.StartLine,
			ExpectedEndLine:   &anchor.EndLine,
			ExpectedLines:     stringPtr(anchor.Lines),
		}
	} else if instruction := strings.TrimSpace(opts.Instruction); instruction != "" {
		input.SuggestedChange = &api.ChangeReviewSuggestedChangeCreateRequest{
			ChangeType:  "manual_instruction",
			Instruction: stringPtr(instruction),
		}
	}
	return input, nil
}

// generateChangeReviewClientID builds a unique idempotency key for a CLI-created
// finding when the caller did not supply --client-id.
func generateChangeReviewClientID() string {
	return "entire-cli:" + uuid.NewString()
}

func buildChangeReviewCommentConfidence(confidence float64) (float64, bool, error) {
	if confidence < 0 {
		return 0, false, nil
	}
	if confidence > 1 {
		return 0, false, errors.New("--confidence must be between 0.0 and 1.0")
	}
	return confidence, true, nil
}

// buildChangeReviewCommentLocation resolves where the finding sits. A patch
// already names the file it rewrites and the lines it expects, so when the
// caller gave no --file the anchor supplies both rather than the finding landing
// on the change as a whole; explicit flags always win over it.
func buildChangeReviewCommentLocation(opts changeReviewCommentAddOptions, anchor *changeReviewPatchAnchor) (api.ChangeReviewLocationCreateRequest, error) {
	filePath := strings.TrimSpace(opts.FilePath)
	line := opts.Line
	if line < 0 || opts.StartLine < 0 || opts.EndLine < 0 {
		return api.ChangeReviewLocationCreateRequest{}, errors.New("line numbers must be non-negative")
	}
	if opts.StartLine > 0 {
		if line > 0 {
			return api.ChangeReviewLocationCreateRequest{}, errors.New("pass either --line or --start-line, not both")
		}
		line = opts.StartLine
	}
	if filePath == "" && (line > 0 || opts.EndLine > 0) {
		return api.ChangeReviewLocationCreateRequest{}, errors.New("--line/--start-line/--end-line require --file")
	}
	if opts.EndLine > 0 && line == 0 {
		return api.ChangeReviewLocationCreateRequest{}, errors.New("--end-line requires --line or --start-line")
	}
	if opts.EndLine > 0 && opts.EndLine < line {
		return api.ChangeReviewLocationCreateRequest{}, errors.New("--end-line must be greater than or equal to the start line")
	}

	loc := api.ChangeReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange}
	if filePath == "" {
		if anchor != nil {
			return changeReviewLocationFromAnchor(anchor), nil
		}
		return loc, nil
	}
	// The API pins a suggested change to one file, so a --file naming a
	// different one than the patch rewrites has no sensible reading: the finding
	// would point at one place and its fix at another.
	if anchor != nil && normalizeFindingPath(filePath) != normalizeFindingPath(anchor.FilePath) {
		return api.ChangeReviewLocationCreateRequest{}, fmt.Errorf(
			"--file names %s but the patch modifies %s; a suggested change must target the file the finding is on",
			filePath, anchor.FilePath)
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

// changeReviewLocationFromAnchor places the finding on the span the patch
// rewrites: the whole hunk range, so a multi-hunk fix is not misreported as
// sitting on its first line alone.
func changeReviewLocationFromAnchor(anchor *changeReviewPatchAnchor) api.ChangeReviewLocationCreateRequest {
	// Copy rather than alias the anchor's fields; they are also handed to the
	// suggested-change request.
	startLine := anchor.StartLine
	loc := api.ChangeReviewLocationCreateRequest{
		Granularity: reviewTrailGranularityLine,
		FilePath:    stringPtr(anchor.FilePath),
		StartLine:   &startLine,
	}
	if anchor.EndLine > anchor.StartLine {
		endLine := anchor.EndLine
		loc.Granularity = reviewTrailGranularityRange
		loc.EndLine = &endLine
	}
	return loc
}

// normalizeFindingPath puts a caller-supplied path into the slash-separated,
// cleaned form patch targets already use, so `./cmd/foo.go` and `cmd/foo.go`
// compare equal.
func normalizeFindingPath(p string) string {
	return path.Clean(filepath.ToSlash(strings.TrimSpace(p)))
}

// createChangeReviewFinding posts a single finding through the current API flow:
// start a review session, then submit a one-item comment batch under it. It
// returns the created (or already-existing) finding.
func prepareChangeReviewCommentInputsForCreate(worktreeRoot string, inputs []api.ChangeReviewCommentInput) []api.ChangeReviewCommentInput {
	out := make([]api.ChangeReviewCommentInput, len(inputs))
	copy(out, inputs)
	for i := range out {
		out[i].Location = prepareChangeReviewLocationForCreate(worktreeRoot, out[i].Location)
	}
	return out
}

func prepareChangeReviewLocationForCreate(worktreeRoot string, loc api.ChangeReviewLocationCreateRequest) api.ChangeReviewLocationCreateRequest {
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
		selected, fileOK, selectedOK := changeReviewSelectedTextFromWorktree(worktreeRoot, filePath, startLine, endLine)
		if selectedOK {
			loc.SelectedText = stringPtr(selected)
			return loc
		}
		if fileOK {
			return api.ChangeReviewLocationCreateRequest{Granularity: reviewTrailGranularityFile, FilePath: stringPtr(filePath)}
		}
		return api.ChangeReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange}
	case reviewTrailGranularityFile:
		if loc.FilePath != nil && strings.TrimSpace(*loc.FilePath) != "" {
			return loc
		}
	}
	return api.ChangeReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange}
}

func changeReviewSelectedTextFromWorktree(worktreeRoot, filePath string, startLine, endLine int) (selected string, fileOK bool, selectedOK bool) {
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

func createChangeReviewFinding(ctx context.Context, client *api.Client, changeID string, input api.ChangeReviewCommentInput) (api.ChangeReviewComment, error) {
	findings, err := createChangeReviewFindings(ctx, client, changeID, []api.ChangeReviewCommentInput{input})
	if err != nil {
		return api.ChangeReviewComment{}, err
	}
	if len(findings) == 0 {
		return api.ChangeReviewComment{}, errors.New("create finding: server returned no results")
	}
	return findings[0], nil
}

// createChangeReviewFindings posts findings through one change review session,
// chunking by the server-advertised batch limit when present.
func createChangeReviewFindings(ctx context.Context, client *api.Client, changeID string, inputs []api.ChangeReviewCommentInput) ([]api.ChangeReviewComment, error) {
	if len(inputs) == 0 {
		return nil, errors.New("create finding: no findings to post")
	}
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = ""
	}
	inputs = prepareChangeReviewCommentInputsForCreate(worktreeRoot, inputs)
	review, err := startChangeReview(ctx, client, changeID)
	if err != nil {
		return nil, err
	}
	limit := review.Limits.MaxCommentsPerBatch
	if limit <= 0 || limit > len(inputs) {
		limit = len(inputs)
	}
	findings := make([]api.ChangeReviewComment, 0, len(inputs))
	for start := 0; start < len(inputs); start += limit {
		end := start + limit
		if end > len(inputs) {
			end = len(inputs)
		}
		batchFindings, err := postChangeReviewFindingBatch(ctx, client, changeID, review.ReviewID, inputs[start:end])
		if err != nil {
			return nil, err
		}
		findings = append(findings, batchFindings...)
	}
	return findings, nil
}

func postChangeReviewFindingBatch(ctx context.Context, client *api.Client, changeID, reviewID string, inputs []api.ChangeReviewCommentInput) ([]api.ChangeReviewComment, error) {
	resp, err := client.Post(ctx, changeReviewBatchCommentsPath(changeID, reviewID), api.ChangeReviewCommentBatchRequest{Comments: inputs})
	if err != nil {
		return nil, fmt.Errorf("create finding: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		return nil, err
	}
	var batch api.ChangeReviewCommentBatchResponse
	if err := api.DecodeJSON(resp, &batch); err != nil {
		return nil, fmt.Errorf("decode finding batch response: %w", err)
	}
	if len(batch.Results) == 0 {
		return nil, errors.New("create finding: server returned no results")
	}
	findings := make([]api.ChangeReviewComment, 0, len(batch.Results))
	for _, result := range batch.Results {
		if result.Status == changeReviewBatchResultError {
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

// startChangeReview opens a review session for a change. The body is left empty
// so the server resolves the code version (base/head) from the change itself.
// The API rejects a lone head_sha — base_sha and head_sha must be supplied
// together or not at all — and computing a correct base SHA client-side is out
// of scope for a single-finding add, so neither is sent.
func startChangeReview(ctx context.Context, client *api.Client, changeID string) (api.ChangeReviewStartResponse, error) {
	resp, err := client.Post(ctx, changeReviewStartPath(changeID), api.ChangeReviewStartRequest{})
	if err != nil {
		return api.ChangeReviewStartResponse{}, fmt.Errorf("start review: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		return api.ChangeReviewStartResponse{}, err
	}
	var out api.ChangeReviewStartResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return api.ChangeReviewStartResponse{}, fmt.Errorf("decode review: %w", err)
	}
	if out.ReviewID == "" {
		return api.ChangeReviewStartResponse{}, errors.New("start review: server returned an empty review id")
	}
	return out, nil
}

func changeReviewStartPath(changeID string) string {
	return "/api/v1/changes/" + url.PathEscape(changeID) + "/reviews"
}

func changeReviewBatchCommentsPath(changeID, reviewID string) string {
	return "/api/v1/changes/" + url.PathEscape(changeID) + "/reviews/" + url.PathEscape(reviewID) + "/comments"
}

func changeReviewListCommentsPath(changeID string) string {
	return "/api/v1/changes/" + url.PathEscape(changeID) + "/reviews/comments"
}

func resolveChangeReviewComment(ctx context.Context, client *api.Client, changeID, commentID string) (api.ChangeReviewComment, error) {
	opts := changeReviewListOptions{
		Status:           changeReviewStatusAny,
		Freshness:        changeReviewFreshnessAny,
		IncludeDismissed: true,
		Limit:            defaultChangeReviewLimit,
	}
	var matches []api.ChangeReviewComment
	for {
		comments, hasMore, err := fetchChangeReviewComments(ctx, client, changeID, opts)
		if err != nil {
			return api.ChangeReviewComment{}, err
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
		return api.ChangeReviewComment{}, fmt.Errorf("no finding %q found", commentID)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i := range matches {
			ids[i] = matches[i].ID
		}
		sort.Strings(ids)
		return api.ChangeReviewComment{}, fmt.Errorf("ambiguous finding %q (matches: %s)", commentID, strings.Join(ids, ", "))
	}
}

func hydrateChangeReviewCommentWithState(ctx context.Context, client *api.Client, changeID string, comment api.ChangeReviewComment) (api.ChangeReviewComment, api.ChangeReviewStateResponse, error) {
	state, err := fetchChangeReviewState(ctx, client, changeID, comment.ReviewID)
	if err != nil {
		return api.ChangeReviewComment{}, api.ChangeReviewStateResponse{}, err
	}
	for _, candidate := range state.Comments {
		if candidate.ID == comment.ID {
			return candidate, state, nil
		}
	}
	return api.ChangeReviewComment{}, api.ChangeReviewStateResponse{}, fmt.Errorf("finding %s details were not returned by the API", comment.ID)
}

func fetchChangeReviewState(ctx context.Context, client *api.Client, changeID, reviewID string) (api.ChangeReviewStateResponse, error) {
	var merged api.ChangeReviewStateResponse
	cursor := ""
	seenCursors := map[string]bool{}
	for {
		resp, err := client.Get(ctx, changeReviewStatePath(changeID, reviewID, cursor))
		if err != nil {
			return api.ChangeReviewStateResponse{}, fmt.Errorf("get review state: %w", err)
		}
		var page api.ChangeReviewStateResponse
		decodeErr := func() error {
			defer resp.Body.Close()
			if err := checkChangeResponse(resp); err != nil {
				return err
			}
			if err := api.DecodeJSON(resp, &page); err != nil {
				return fmt.Errorf("decode review state: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return api.ChangeReviewStateResponse{}, decodeErr
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
			return api.ChangeReviewStateResponse{}, fmt.Errorf("review state pagination repeated cursor %q", cursor)
		}
		seenCursors[cursor] = true
	}
	return merged, nil
}

func changeReviewStatePath(changeID, reviewID, cursor string) string {
	// The entire-api snapshot route uses include_dismissed/limit/cursor rather
	// than pageSize/pageToken.
	q := url.Values{}
	q.Set("include_dismissed", "true")
	q.Set("stale", changeReviewFreshnessAny)
	q.Set("limit", strconv.Itoa(defaultChangeReviewLimit))
	if strings.TrimSpace(cursor) != "" {
		q.Set("cursor", strings.TrimSpace(cursor))
	}
	return "/api/v1/changes/" + url.PathEscape(changeID) + "/reviews/" + url.PathEscape(reviewID) + "?" + q.Encode()
}

func verifyChangeReviewHead(ctx context.Context, state api.ChangeReviewStateResponse) error {
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

func applyChangeReviewSuggestions(ctx context.Context, comment api.ChangeReviewComment, checkOnly bool, w io.Writer) (int, error) {
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

func combinedSafeUnifiedDiffPatch(comment api.ChangeReviewComment, w io.Writer) (string, int, error) {
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

// validateUnifiedDiffPatchPaths checks every path a patch's headers name. It
// walks headers only: diff content is not a path, and a deleted line such as
// "-- ../legacy" serializes as "--- ../legacy", which read as a header would
// reject a perfectly safe patch.
func validateUnifiedDiffPatchPaths(patchText string) error {
	return forEachPatchHeaderLine(patchText, func(line string) error {
		for _, p := range patchHeaderPaths(line) {
			if err := validatePatchPath(p); err != nil {
				return err
			}
		}
		return nil
	})
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

// cleanPatchPath normalizes a diff header path to the repo-relative,
// slash-separated form the working tree uses: quoted paths are unquoted, the a/
// or b/ prefix git prepends is dropped, and separators are normalized.
func cleanPatchPath(raw string) string {
	p := strings.TrimSpace(raw)
	if unquoted, err := strconv.Unquote(p); err == nil {
		p = unquoted
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	return strings.ReplaceAll(p, "\\", "/")
}

func validatePatchPath(raw string) error {
	p := strings.TrimSpace(raw)
	if p == "" || p == patchDevNull {
		return nil
	}
	p = cleanPatchPath(p)
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

func patchChangeReviewCommentStatus(ctx context.Context, client *api.Client, changeID string, comment api.ChangeReviewComment, status, reason string) (api.ChangeReviewComment, error) {
	body := api.ChangeReviewCommentPatchRequest{Status: status}
	if strings.TrimSpace(reason) != "" {
		body.StatusReason = stringPtr(reason)
	}
	return patchChangeReviewComment(ctx, client, changeID, comment, body)
}

func patchChangeReviewComment(ctx context.Context, client *api.Client, changeID string, comment api.ChangeReviewComment, body api.ChangeReviewCommentPatchRequest) (api.ChangeReviewComment, error) {
	resp, err := client.Patch(ctx, changeReviewCommentPath(changeID, comment.ReviewID, comment.ID), body)
	if err != nil {
		return api.ChangeReviewComment{}, fmt.Errorf("update finding: %w", err)
	}
	defer resp.Body.Close()
	if err := checkChangeResponse(resp); err != nil {
		return api.ChangeReviewComment{}, err
	}
	var updated api.ChangeReviewComment
	if err := api.DecodeJSON(resp, &updated); err != nil {
		return api.ChangeReviewComment{}, fmt.Errorf("decode updated finding: %w", err)
	}
	return updated, nil
}

func changeReviewCommentPath(changeID, reviewID, commentID string) string {
	return "/api/v1/changes/" + url.PathEscape(changeID) + "/reviews/" + url.PathEscape(reviewID) + "/comments/" + url.PathEscape(commentID)
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

func encodeChangeReviewJSON(w io.Writer, target changeReviewTarget, comments []api.ChangeReviewComment, hasMore bool, counts changeReviewCommentCounts) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"change":   target.Change,
		"counts":   counts,
		"findings": comments,
		"has_more": hasMore,
	}); err != nil {
		return fmt.Errorf("encode change findings JSON: %w", err)
	}
	return nil
}

func printChangeReviewDashboard(w io.Writer, target changeReviewTarget, comments []api.ChangeReviewComment, hasMore bool, opts changeReviewListOptions, counts changeReviewCommentCounts) {
	change := target.Change
	if change.Number > 0 {
		fmt.Fprintf(w, "  Change #%d  %s\n", change.Number, change.Title)
	} else {
		fmt.Fprintf(w, "  Change %s  %s\n", change.ID, change.Title)
	}
	fmt.Fprintf(w, "  Status: %s · Branch: %s · Base: %s\n", change.Status, change.Branch, change.Base)
	fmt.Fprintf(w, "  ID: %s\n\n", change.ID)

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
	printChangeReviewCommentsTable(w, comments)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  entire change finding show <finding-id>")
	fmt.Fprintln(w, "  entire change finding add -m \"finding body\" --file path --line 42")
	fmt.Fprintln(w, "  entire change finding update <finding-id> -m \"updated body\"")
	fmt.Fprintln(w, "  entire change finding apply <finding-id> --resolve")
	fmt.Fprintln(w, "  entire change finding resolve <finding-id> -m \"fixed in <sha>\"")
	fmt.Fprintln(w, "  entire change finding dismiss <finding-id> -m \"not applicable\"")
	fmt.Fprintln(w, "  entire change watch")
}

func printChangeReviewComments(w io.Writer, comments []api.ChangeReviewComment, hasMore bool) {
	if len(comments) == 0 {
		fmt.Fprintln(w, "No findings found.")
		return
	}
	printChangeReviewCommentsTable(w, comments)
	if hasMore {
		fmt.Fprintln(w, "More findings available; rerun with --offset for the next page.")
	}
}

func printChangeReviewCommentsTable(w io.Writer, comments []api.ChangeReviewComment) {
	var table bytes.Buffer
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSEV\tSTATUS\tFRESHNESS\tLOCATION\tSUMMARY")
	for _, comment := range comments {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			abbreviate12(comment.ID),
			severityTableDisplay(comment.Severity),
			comment.Status,
			changeReviewFreshnessDisplay(comment),
			changeReviewLocationDisplay(comment.Location),
			changeReviewCommentSummary(comment),
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

func printChangeReviewCommentCreated(w io.Writer, target changeReviewTarget, comment api.ChangeReviewComment) {
	fmt.Fprintf(w, "Created finding %s on %s\n", comment.ID, changeReviewTargetDisplay(target))
	fmt.Fprintf(w, "Status:   %s\n", comment.Status)
	fmt.Fprintf(w, "Severity: %s\n", severityDisplay(comment.Severity))
	fmt.Fprintf(w, "Location: %s\n", changeReviewLocationDisplay(comment.Location))
}

func printChangeReviewCommentDetail(w io.Writer, comment api.ChangeReviewComment) {
	fmt.Fprintf(w, "Finding:   %s\n", comment.ID)
	fmt.Fprintf(w, "Status:    %s\n", comment.Status)
	fmt.Fprintf(w, "Freshness: %s\n", changeReviewFreshnessDisplay(comment))
	fmt.Fprintf(w, "Severity:  %s\n", severityDisplay(comment.Severity))
	if comment.Confidence != nil {
		fmt.Fprintf(w, "Confidence: %.2f\n", *comment.Confidence)
	}
	fmt.Fprintf(w, "Location: %s\n", changeReviewLocationDisplay(comment.Location))
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

func changeReviewTargetDisplay(target changeReviewTarget) string {
	if target.Change.Number > 0 {
		return fmt.Sprintf("change #%d (%s)", target.Change.Number, target.Change.Title)
	}
	if target.Change.Branch != "" {
		return fmt.Sprintf("change %s on %s", target.Change.ID, target.Change.Branch)
	}
	return "change " + target.Change.ID
}

type changeReviewCommentCounts struct {
	Open       int
	OpenHigh   int
	OpenMedium int
	OpenLow    int
	Resolved   int
	Dismissed  int
	Stale      int
}

func countChangeReviewComments(comments []api.ChangeReviewComment) changeReviewCommentCounts {
	var counts changeReviewCommentCounts
	for _, comment := range comments {
		switch comment.Status {
		case changeReviewStatusResolved:
			counts.Resolved++
		case changeReviewStatusDismissed:
			counts.Dismissed++
		case changeReviewStatusOpen:
			counts.Open++
			switch strings.ToLower(stringPtrValue(comment.Severity)) {
			case changeReviewSeverityHigh:
				counts.OpenHigh++
			case changeReviewSeverityMedium:
				counts.OpenMedium++
			case changeReviewSeverityLow:
				counts.OpenLow++
			}
		}
		if comment.StaleOutcome == changeReviewFreshnessStale {
			counts.Stale++
		}
	}
	return counts
}

func changeReviewLocationDisplay(loc api.ChangeReviewLocation) string {
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

func changeReviewCommentSummary(comment api.ChangeReviewComment) string {
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
	case changeReviewSeverityHigh:
		return "High"
	case changeReviewSeverityMedium:
		return "Medium"
	case changeReviewSeverityLow:
		return "Low"
	case "":
		return "-"
	default:
		return value
	}
}

func changeReviewFreshnessDisplay(comment api.ChangeReviewComment) string {
	outcome := strings.TrimSpace(comment.StaleOutcome)
	if outcome == "" {
		return changeReviewFreshnessCurrent
	}
	return outcome
}

func defaultChangeReviewStatusReason(status string) string {
	switch status {
	case changeReviewStatusResolved:
		return "Resolved via Entire CLI"
	case changeReviewStatusDismissed:
		return "Dismissed via Entire CLI"
	case changeReviewStatusOpen:
		return "Reopened via Entire CLI"
	default:
		return "Updated via Entire CLI"
	}
}

func parseOptionalChangeSelector(args []string, flagSelector string) (string, error) {
	flagSelector = strings.TrimSpace(flagSelector)
	if len(args) == 0 {
		return flagSelector, nil
	}
	if flagSelector != "" {
		return "", errors.New("pass a change either positionally or with --change, not both")
	}
	selector := strings.TrimSpace(args[0])
	if selector == "" {
		return "", errors.New("change selector cannot be empty")
	}
	return selector, nil
}

func parseChangeSelectorAndCommentID(args []string, flagSelector string) (string, string, error) {
	flagSelector = strings.TrimSpace(flagSelector)
	if len(args) == 1 {
		commentID := strings.TrimSpace(args[0])
		if commentID == "" {
			return "", "", errors.New("finding id cannot be empty")
		}
		return flagSelector, commentID, nil
	}
	if flagSelector != "" {
		return "", "", errors.New("pass a change either positionally or with --change, not both")
	}
	selector := strings.TrimSpace(args[0])
	commentID := strings.TrimSpace(args[1])
	if selector == "" {
		return "", "", errors.New("change selector cannot be empty")
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
