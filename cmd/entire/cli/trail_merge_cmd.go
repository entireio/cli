package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/spf13/cobra"
)

const (
	trailComparisonAvailable = "available"
	trailConflictConflicting = "conflicting"
	trailMergeUnknown        = "unknown"
	trailBypassPolicyNobody  = "nobody"

	trailActionEnabled       = "enabled"
	trailMergeOverrideDenied = "merge_gates_bypass_forbidden"

	trailGateStateDisabled = "disabled"
	trailGateFailed        = "failed"
	trailGatePending       = "pending"
	trailGateError         = "error"
)

type trailMergeOptions struct {
	Selector string
	Branch   string
	DryRun   bool
	Force    bool
	Yes      bool
	JSON     bool
}

func newTrailMergeCmd() *cobra.Command {
	var opts trailMergeOptions

	cmd := &cobra.Command{
		Use:   "merge [<trail>]",
		Short: "Merge a trail's branch into its base",
		Long: `Merge a trail's branch into its base branch.

If <trail> (a number, id, or branch) is omitted, merges the trail for the
current branch (or --branch).

The trail's gates (approvals, CI checks, the base branch's CI, being up to date,
and any other configured gates) are checked first. When a blocking gate fails
or is still pending, the blocking gates are listed and nothing is merged. A
trail that conflicts with its base is never merged; resolve the conflicts first.

Pass --force to merge anyway. This bypasses the blocking gates and is recorded
on the trail; the repo's bypass policy decides who may do it. You are asked to
confirm the bypass unless --yes is passed; without a terminal to prompt on (or
with --json), --force requires --yes.

Pass --dry-run to only report whether the trail can be merged; it exits
non-zero when the merge would be refused, so it can gate CI.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Selector = selectorFromArgs(args)
			if err := ensureTrailRepoHasTarget(cmd, opts.Selector != "" || strings.TrimSpace(opts.Branch) != "", "pass a trail selector or --branch"); err != nil {
				return err
			}
			return runTrailMerge(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), trailRepoFlag(cmd), opts)
		},
	}

	cmd.Flags().StringVar(&opts.Branch, "branch", "", "Branch of the trail (defaults to current); cannot be combined with a trail selector")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Only check whether the trail can be merged; do not merge")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "Merge even when blocking gates fail (requires bypass permission)")
	cmd.Flags().BoolVarP(&opts.Yes, "yes", "y", false, "Skip the confirmation prompt before --force bypasses gates")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")

	return cmd
}

// trailMergeResultJSON is `trail merge --json`. It is written for every
// outcome once mergeability has been read, including refusals, which still
// exit non-zero.
type trailMergeResultJSON struct {
	Number         int                     `json:"number"`
	Branch         string                  `json:"branch"`
	Base           string                  `json:"base"`
	HeadSha        *string                 `json:"headSha"`
	Mergeable      bool                    `json:"mergeable"`
	ConflictStatus string                  `json:"conflictStatus"`
	BypassPolicy   string                  `json:"bypassPolicy"`
	Blockers       []trailMergeBlockerJSON `json:"blockers"`
	// Reasons explains a blocked trail that reports no blocking gates, the
	// same fallback the human-readable report prints.
	Reasons  []string `json:"reasons,omitempty"`
	DryRun   bool     `json:"dryRun"`
	Merged   bool     `json:"merged"`
	Bypassed bool     `json:"bypassed"`
	// WouldBypass is set by --dry-run --force when the merge would go
	// through only by bypassing the blocking gates.
	WouldBypass    bool   `json:"wouldBypass"`
	MergeCommitSha string `json:"mergeCommitSha,omitempty"`
}

type trailMergeBlockerJSON struct {
	GateKey   string `json:"gateKey"`
	GateType  string `json:"gateType"`
	Status    string `json:"status"`
	Rationale string `json:"rationale,omitempty"`
	URL       string `json:"url,omitempty"`
}

func newTrailMergeResultJSON(t *api.TrailResource, m *api.TrailMergeabilityResponse, gates []api.TrailGate, dryRun bool) *trailMergeResultJSON {
	out := &trailMergeResultJSON{
		Number: t.Number, Branch: t.Branch, Base: t.Base, HeadSha: m.HeadSHA,
		Mergeable: m.Mergeable, ConflictStatus: m.ConflictStatus, BypassPolicy: m.BypassPolicy,
		Blockers: make([]trailMergeBlockerJSON, 0, len(gates)), DryRun: dryRun,
	}
	for _, g := range gates {
		b := trailMergeBlockerJSON{GateKey: g.GateKey, GateType: g.GateType, Status: g.Status, URL: trailGateWebURL(g)}
		if g.Rationale != nil {
			b.Rationale = strings.TrimSpace(*g.Rationale)
		}
		out.Blockers = append(out.Blockers, b)
	}
	if !m.Mergeable && len(gates) == 0 {
		out.Reasons = describeMergeabilityBlockers(t.Number, m)
	}
	return out
}

func runTrailMerge(ctx context.Context, out, errW io.Writer, insecureHTTP bool, repoOverride string, opts trailMergeOptions) error {
	if opts.Selector != "" && strings.TrimSpace(opts.Branch) != "" {
		return errors.New("pass a trail selector or --branch, not both")
	}
	// With --json the human-readable report is dropped and stdout carries
	// only the JSON result.
	w := out
	if opts.JSON {
		w = io.Discard
	}
	return runAuthenticatedTrailAPI(ctx, errW, insecureHTTP, repoOverride, func(ctx context.Context, client *api.Client, repoID string) error {
		forge, owner, repoName, err := resolveTrailRepoOrRemote(ctx, repoOverride)
		if err != nil {
			return err
		}
		basePath, err := trailRepoBasePath(forge, owner, repoName, repoID)
		if err != nil {
			return err
		}
		found, err := resolveNumberedTrailAtPath(ctx, client, basePath, forge, owner, repoName, opts.Selector, opts.Branch)
		if err != nil {
			return err
		}
		// repo_id-addressed for every forge: the name-addressed routes were retired.
		mergePath, err := trailRepoIDNumberPath(repoID, found.Number)
		if err != nil {
			return err
		}

		m, actions, err := fetchTrailMergeDetail(ctx, client, mergePath)
		if err != nil {
			return err
		}
		printTrailMergeReadiness(w, found, m)

		gates := trailMergeBlockingGates(m)
		result := newTrailMergeResultJSON(found, m, gates, opts.DryRun)
		err = trailMergeAfterRead(ctx, w, client, mergePath, found, m, trailBypassVerdictFor(m, actions), gates, opts, result)
		return finishTrailMerge(out, opts.JSON, result, err)
	})
}

func finishTrailMerge(out io.Writer, jsonOut bool, result *trailMergeResultJSON, err error) error {
	if !jsonOut {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(result); encErr != nil {
		return errors.Join(err, encErr)
	}
	return err
}

// trailMergeAfterRead decides and performs the merge once mergeability is in
// hand, recording the outcome on result for --json.
func trailMergeAfterRead(ctx context.Context, w io.Writer, client *api.Client, mergePath string, found *api.TrailResource,
	m *api.TrailMergeabilityResponse, v trailBypassVerdict, gates []api.TrailGate, opts trailMergeOptions, result *trailMergeResultJSON,
) error {
	bypassable := len(gates)
	if !m.Mergeable {
		fmt.Fprintln(w, "Blocked by:")
		for _, b := range describeTrailMergeBlockers(found.Number, m, gates) {
			fmt.Fprintf(w, "  - %s\n", b)
		}
	}
	// Checked before --force and --dry-run: a conflict is not a gate, so
	// no bypass can get past it and no hint may suggest one.
	if m.ConflictStatus == trailConflictConflicting {
		return trailMergeConflictError(found)
	}
	bypass := !m.Mergeable
	if bypass {
		if !opts.Force || bypassable == 0 {
			return trailMergeBlockedError(found.Number, v, gates)
		}
		if !v.allowed {
			return fmt.Errorf("cannot merge trail #%d with --force: %s", found.Number, v.denial)
		}
		if trailMergeHead(m) == "" {
			return fmt.Errorf("cannot merge trail #%d with --force: the server did not report its head commit, so the bypass cannot be pinned to the commit whose gates were checked", found.Number)
		}
	}

	if !opts.DryRun && bypass {
		proceed, err := confirmTrailMergeBypass(ctx, w, found, gates, opts.Yes, !opts.JSON && trailMergeCanPrompt())
		if err != nil || !proceed {
			return err
		}
		// The prompt can stay open indefinitely, and gates (approvals, base
		// CI) can change without the head moving, so re-read before bypassing.
		if !opts.Yes {
			if err := recheckTrailMergeBypass(ctx, client, mergePath, found.Number, m, gates); err != nil {
				return err
			}
		}
	}

	if opts.DryRun {
		switch {
		case bypass && v.confirmed:
			result.WouldBypass = true
			fmt.Fprintf(w, "Trail #%d is blocked; --force would bypass %d blocking %s (dry run; no merge performed).\n",
				found.Number, bypassable, pluralize("gate", bypassable))
		case bypass:
			result.WouldBypass = true
			fmt.Fprintf(w, "Trail #%d is blocked; --force would bypass %d blocking %s, subject to the repo's bypass policy (%s) (dry run; no merge performed).\n",
				found.Number, bypassable, pluralize("gate", bypassable), trailBypassPolicyDisplay(m.BypassPolicy))
		default:
			fmt.Fprintf(w, "Trail #%d is mergeable (dry run; no merge performed).\n", found.Number)
		}
		return nil
	}

	// A trail that was mergeable when read merges without bypass, so a gate
	// failing in between is refused rather than bypassed unseen. A bypass is
	// pinned to the head only: the server takes no gate set, so a gate that
	// changes on the same head after the recheck is bypassed too.
	req := api.TrailMergeRequest{Bypass: bypass, ExpectedHeadSha: trailMergeHead(m)}
	res, err := postTrailMerge(ctx, client, mergePath, found.Number, req, v, m.BypassPolicy)
	if err != nil {
		return err
	}
	result.Merged, result.Bypassed, result.MergeCommitSha = true, bypass, res.MergeCommitSha

	// The server sends no SHA both for a fast-forward and when the base
	// already had the head, and does not say which.
	commit := "no merge commit"
	if sha := tuiutil.SanitizeDisplayText(strings.TrimSpace(res.MergeCommitSha)); sha != "" {
		commit = sha
	}
	base := tuiutil.SanitizeDisplayText(strings.TrimSpace(found.Base))
	if base == "" {
		base = "its base"
	}
	if bypass {
		fmt.Fprintf(w, "Merged trail #%d into %s (%s), bypassing %d blocking %s\n",
			found.Number, base, commit, bypassable, pluralize("gate", bypassable))
	} else {
		fmt.Fprintf(w, "Merged trail #%d into %s (%s)\n", found.Number, base, commit)
	}
	return nil
}

// Seams for tests: whether a terminal can be prompted, and the prompt itself.
var (
	trailMergeCanPrompt    = interactive.CanPromptInteractively
	trailMergeBypassPrompt = promptTrailMergeBypass
)

// confirmTrailMergeBypass decides whether a --force bypass should proceed,
// mirroring confirmTrailDeletion: --yes proceeds silently; otherwise it needs
// an interactive terminal, and without one it refuses rather than bypassing
// unprompted. A declined or aborted prompt is a clean cancel; a cancelled
// context is an interruption, returned as an error wrapping ctx.Err().
func confirmTrailMergeBypass(ctx context.Context, w io.Writer, t *api.TrailResource, gates []api.TrailGate, yes, canPrompt bool) (bool, error) {
	if yes {
		return true, nil
	}
	n := len(gates)
	if !canPrompt {
		return false, fmt.Errorf("refusing to bypass %d blocking %s on trail #%d without confirmation; pass --yes", n, pluralize("gate", n), t.Number)
	}
	// huh opens the TTY during form startup regardless of context state.
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("trail merge confirmation cancelled: %w", err)
	}
	base := tuiutil.SanitizeDisplayText(strings.TrimSpace(t.Base))
	if base == "" {
		base = "its base"
	}
	// The gates go in the title, not a description: accessible mode renders
	// only a field's title and options.
	lines := make([]string, 0, n+2)
	lines = append(lines,
		fmt.Sprintf("Bypass %d blocking %s and merge trail #%d into %s?", n, pluralize("gate", n), t.Number, base),
		"The bypass is recorded on the trail. Gates bypassed:")
	for _, g := range gates {
		lines = append(lines, "  - "+describeGateFailure(g))
	}
	confirmed, err := trailMergeBypassPrompt(ctx, strings.Join(lines, "\n"))
	if err != nil {
		return false, err
	}
	if !confirmed {
		fmt.Fprintln(w, "Trail merge cancelled.")
		return false, nil
	}
	return true, nil
}

func promptTrailMergeBypass(ctx context.Context, title string) (bool, error) {
	confirmed := false
	form := NewAccessibleForm(
		huh.NewGroup(huh.NewConfirm().Title(title).Value(&confirmed)),
	)
	err := form.RunWithContext(ctx)
	// Before inspecting err: a cancelled context surfaces as a form error too,
	// and it is an interruption, not an answer.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("trail merge confirmation cancelled: %w", ctxErr)
	}
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, fmt.Errorf("trail merge prompt: %w", err)
	}
	return confirmed, nil
}

// recheckTrailMergeBypass refuses the bypass when the blocking gates or head
// differ from what the user confirmed.
func recheckTrailMergeBypass(ctx context.Context, client *api.Client, mergePath string, number int,
	confirmed *api.TrailMergeabilityResponse, confirmedGates []api.TrailGate,
) error {
	now, _, err := fetchTrailMergeDetail(ctx, client, mergePath)
	if err != nil {
		return err
	}
	if trailMergeHead(now) == trailMergeHead(confirmed) && !now.Mergeable &&
		trailGateFingerprint(trailMergeBlockingGates(now)) == trailGateFingerprint(confirmedGates) {
		return nil
	}
	return fmt.Errorf("trail #%d changed while you were confirming the bypass; nothing was merged\nhint: rerun 'entire trail merge %d --force' to review the current gates", number, number)
}

// trailGateFingerprint identifies a set of blocking gates by key and status.
func trailGateFingerprint(gates []api.TrailGate) string {
	parts := make([]string, 0, len(gates))
	for _, g := range gates {
		parts = append(parts, g.GateKey+"="+g.Status)
	}
	slices.Sort(parts)
	return strings.Join(parts, "\x00")
}

func trailMergeHead(m *api.TrailMergeabilityResponse) string {
	if m.HeadSHA == nil {
		return ""
	}
	return strings.TrimSpace(*m.HeadSHA)
}

// trailBypassVerdict is whether this caller may bypass the trail's gates.
// confirmed means the server said so for this caller; otherwise only the
// repo policy was known and the server decides at merge time. denial is the
// reason, when not allowed, phrased to follow "cannot ...: ".
type trailBypassVerdict struct {
	allowed   bool
	confirmed bool
	denial    string
}

// trailBypassVerdictFor reads the caller's merge_with_bypass action, falling
// back to the repo policy when the server sends none.
func trailBypassVerdictFor(m *api.TrailMergeabilityResponse, actions *api.TrailActions) trailBypassVerdict {
	policy := trailBypassPolicyDisplay(m.BypassPolicy)
	if strings.TrimSpace(m.BypassPolicy) == trailBypassPolicyNobody {
		return trailBypassVerdict{denial: fmt.Sprintf("this repo's bypass policy (%s) does not allow bypassing gates", policy)}
	}
	// A mergeable trail reports bypass_not_required, which says nothing about
	// the caller; nothing is bypassed then anyway.
	if actions == nil || strings.TrimSpace(actions.MergeWithBypass.State) == "" || m.Mergeable {
		return trailBypassVerdict{allowed: true}
	}
	a := actions.MergeWithBypass
	if strings.TrimSpace(a.State) == trailActionEnabled {
		return trailBypassVerdict{allowed: true, confirmed: true}
	}
	reason := ""
	if a.Reason != nil {
		reason = tuiutil.SanitizeDisplayText(strings.TrimSpace(*a.Reason))
	}
	if reason == trailMergeOverrideDenied {
		return trailBypassVerdict{denial: fmt.Sprintf("this repo's bypass policy (%s) does not allow you to bypass gates", policy)}
	}
	if reason == "" {
		reason = tuiutil.SanitizeDisplayText(strings.TrimSpace(a.State))
	}
	return trailBypassVerdict{denial: fmt.Sprintf("the server reports that bypassing gates is not available for this trail (%s)", reason)}
}

func trailBypassPolicyDisplay(policy string) string {
	policy = tuiutil.SanitizeDisplayText(strings.TrimSpace(policy))
	if policy == "" {
		return trailMergeUnknown
	}
	return policy
}

func trailMergeConflictError(t *api.TrailResource) error {
	base := tuiutil.SanitizeDisplayText(strings.TrimSpace(t.Base))
	if base == "" {
		base = "its base branch"
	} else {
		base = "its base branch " + base
	}
	return fmt.Errorf("trail #%d conflicts with %s; resolve the conflicts and push before merging (--force cannot bypass a merge conflict)", t.Number, base)
}

func trailRepoIDNumberPath(repoID string, number int) (string, error) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return "", errors.New("cannot merge a trail without the repo's ID")
	}
	return trailNumberPathForBase("/api/v1/repos/"+url.PathEscape(repoID)+"/trails", number), nil
}

// fetchTrailMergeDetail reads the trail detail for its mergeability snapshot
// and the caller's merge actions (nil when the server sends none).
func fetchTrailMergeDetail(ctx context.Context, client *api.Client, trailPath string) (*api.TrailMergeabilityResponse, *api.TrailActions, error) {
	resp, err := client.Get(ctx, trailPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to check mergeability: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, nil, err
	}
	var d api.TrailMergeDetail
	if err := api.DecodeJSON(resp, &d); err != nil {
		return nil, nil, fmt.Errorf("failed to decode trail detail: %w", err)
	}
	if d.Mergeability == nil {
		return nil, nil, errors.New("the server did not report the trail's mergeability; try again shortly")
	}
	return d.Mergeability, d.Actions, nil
}

func postTrailMerge(ctx context.Context, client *api.Client, trailPath string, number int, req api.TrailMergeRequest, v trailBypassVerdict, bypassPolicy string) (*api.TrailMergeResponse, error) {
	resp, err := client.Post(ctx, trailPath+"/merge", req)
	if err != nil {
		return nil, fmt.Errorf("failed to merge trail: %w", err)
	}
	defer resp.Body.Close()
	// Not checkTrailResponse: it would report a bypass denial (403) as an expired login.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		checkErr := api.CheckResponse(resp)
		var httpErr *api.HTTPError
		if errors.As(checkErr, &httpErr) {
			if mapped := trailMergeRefusal(httpErr, number, v, bypassPolicy); mapped != nil {
				return nil, mapped
			}
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return nil, fmt.Errorf("%w (run 'entire login' to re-authenticate)", checkErr)
			case http.StatusForbidden:
				// On merge a 403 is a missing permission, not an expired login.
				return nil, fmt.Errorf("%w (you may not have permission to merge trails in this repo)", checkErr)
			}
		}
		return nil, fmt.Errorf("trail API: %w", checkErr)
	}
	var res api.TrailMergeResponse
	if err := api.DecodeJSON(resp, &res); err != nil {
		return nil, fmt.Errorf("failed to decode merge response: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("trail API did not confirm merge of trail #%d", number)
	}
	return &res, nil
}

func trailMergeRefusal(e *api.HTTPError, number int, v trailBypassVerdict, bypassPolicy string) error {
	code := strings.TrimSpace(e.Code)
	if code == "" {
		code = strings.TrimSpace(e.Message)
	}
	forceHint := ""
	if v.allowed {
		forceHint = fmt.Sprintf(", or rerun 'entire trail merge %d --force' to bypass", number)
	}
	switch {
	case e.StatusCode == http.StatusForbidden && code == trailMergeOverrideDenied:
		return fmt.Errorf("cannot merge trail #%d with --force: this repo's bypass policy (%s) does not allow you to bypass failing gates", number, trailBypassPolicyDisplay(bypassPolicy))
	case e.StatusCode == http.StatusUnprocessableEntity && code == "merge_gates_failed":
		return fmt.Errorf("trail #%d is not mergeable: a blocking gate failed when the merge was attempted\nhint: run 'entire trail merge %d --dry-run' to see why%s", number, number, forceHint)
	case e.StatusCode == http.StatusUnprocessableEntity && code == "merge_gates_pending":
		return fmt.Errorf("trail #%d is not mergeable yet: a blocking gate is still pending\nhint: wait for it to finish%s", number, forceHint)
	case e.StatusCode == http.StatusConflict && strings.HasPrefix(code, "merge_head_mismatch"):
		return fmt.Errorf("trail #%d changed while it was being merged (%s)\nhint: rerun 'entire trail merge %d' to merge the new head", number, tuiutil.SanitizeDisplayText(code), number)
	case e.StatusCode == http.StatusConflict && code == "merge_in_progress":
		return fmt.Errorf("trail #%d is already being merged; try again shortly", number)
	}
	return nil
}

// trailMergeBlockedError explains why the merge stopped. It says "pending"
// rather than "failing" when nothing has failed, and suggests --force only
// when there are gates to bypass and this caller may bypass them.
func trailMergeBlockedError(number int, v trailBypassVerdict, gates []api.TrailGate) error {
	if len(gates) == 0 {
		return fmt.Errorf("trail #%d is not mergeable\nhint: run 'entire trail show %d' for details", number, number)
	}
	pending := 0
	for _, g := range gates {
		if g.Status == trailGatePending {
			pending++
		}
	}
	failing := len(gates) - pending
	var msg string
	switch {
	case failing == 0:
		msg = fmt.Sprintf("trail #%d is not mergeable yet: %d blocking %s pending", number, pending, pluralize("gate", pending))
	case pending == 0:
		msg = fmt.Sprintf("trail #%d is not mergeable: %d blocking %s failing", number, failing, pluralize("gate", failing))
	default:
		msg = fmt.Sprintf("trail #%d is not mergeable: %d blocking %s failing, %d pending", number, failing, pluralize("gate", failing), pending)
	}
	wait := ""
	if failing == 0 {
		wait = "wait for the pending gates to finish"
	}
	switch {
	case !v.allowed && wait != "":
		return fmt.Errorf("%s\nhint: %s; %s", msg, wait, v.denial)
	case !v.allowed:
		return fmt.Errorf("%s\nhint: %s", msg, v.denial)
	case wait != "":
		return fmt.Errorf("%s\nhint: %s, or rerun 'entire trail merge %d --force' to bypass them", msg, wait, number)
	default:
		return fmt.Errorf("%s\nhint: rerun 'entire trail merge %d --force' to merge anyway, bypassing the blocking gates", msg, number)
	}
}

// printTrailMergeReadiness renders the mergeability read with the same view
// `trail show` uses, plus how far the branch is behind its base.
func printTrailMergeReadiness(w io.Writer, t *api.TrailResource, m *api.TrailMergeabilityResponse) {
	styles := newStatusStyles(w)
	label := func(s string) string { return styles.render(styles.yellow, s) }
	fmt.Fprintf(w, "%s\n", styles.render(styles.yellow, fmt.Sprintf("Trail #%d (%s → %s)", t.Number,
		tuiutil.SanitizeDisplayText(t.Branch), tuiutil.SanitizeDisplayText(t.Base))))
	printTrailMergeability(w, styles, label, &m.TrailMergeability)
	if m.ComparisonStatus == trailComparisonAvailable && m.BehindBy > 0 {
		fmt.Fprintf(w, "  %s%d %s behind its base\n", label("Behind:    "), m.BehindBy, pluralize("commit", m.BehindBy))
	}
}

// trailMergeBlockingGates returns the gates that block the merge, mirroring
// the server's rollup: blocking, enabled, and failed, pending, or errored.
// They come from the mergeability read itself, which is evaluated for the
// current head (the /gates route only reports persisted results).
func trailMergeBlockingGates(m *api.TrailMergeabilityResponse) []api.TrailGate {
	var out []api.TrailGate
	for _, g := range m.Gates {
		if !g.Blocking || g.State == trailGateStateDisabled {
			continue
		}
		switch g.Status {
		case trailGateFailed, trailGatePending, trailGateError:
			out = append(out, g)
		}
	}
	return out
}

func describeTrailMergeBlockers(number int, m *api.TrailMergeabilityResponse, gates []api.TrailGate) []string {
	blockers := make([]string, 0, len(gates)+1)
	for _, g := range gates {
		blockers = append(blockers, describeGateFailure(g))
	}
	if len(gates) == 0 {
		blockers = append(blockers, describeMergeabilityBlockers(number, m)...)
	}
	if m.ConflictStatus == trailConflictConflicting {
		blockers = append(blockers, "branch conflicts with its base; resolve the conflicts first (--force cannot bypass this)")
	}
	if len(blockers) == 0 {
		blockers = append(blockers, "the trail is not in a mergeable state")
	}
	return blockers
}

func describeGateFailure(g api.TrailGate) string {
	name := strings.TrimSpace(g.GateKey)
	if name == "" {
		name = strings.TrimSpace(g.GateType)
	}
	reason := ""
	if g.Rationale != nil {
		reason = strings.TrimSpace(*g.Rationale)
	}
	if reason == "" {
		reason = strings.TrimSpace(g.Status)
	}
	line := name + ": " + reason
	if u := trailGateWebURL(g); u != "" {
		line += " (" + u + ")"
	}
	return tuiutil.SanitizeDisplayText(line)
}

// trailGateWebURL is the link a gate's value carries (e.g. the base branch's
// CI build), or "".
func trailGateWebURL(g api.TrailGate) string {
	var value struct {
		WebURL string `json:"web_url"`
	}
	if len(g.Value) == 0 || json.Unmarshal(g.Value, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value.WebURL)
}

// describeMergeabilityBlockers explains a blocked trail that reports no
// blocking gates, from the CI runs and branch comparison in the same read.
func describeMergeabilityBlockers(number int, m *api.TrailMergeabilityResponse) []string {
	var reasons []string
	if m.Checks.Availability == api.TrailChecksAvailable {
		var failed, running int
		for _, r := range m.Checks.Runs {
			switch {
			case r.Status != "completed":
				running++
			case r.Conclusion != nil && trailFailedCheckConclusions[*r.Conclusion]:
				failed++
			}
		}
		switch {
		case failed > 0:
			reasons = append(reasons, "CI checks failed")
		case running > 0:
			reasons = append(reasons, "CI checks are still running")
		}
	}
	if m.ComparisonStatus == trailComparisonAvailable && m.BehindBy > 0 {
		reasons = append(reasons, fmt.Sprintf("branch is %d %s behind its base", m.BehindBy, pluralize("commit", m.BehindBy)))
	}
	if len(reasons) == 0 && m.ConflictStatus != trailConflictConflicting {
		reasons = append(reasons, fmt.Sprintf("a blocking gate failed (run 'entire trail show %d' for details)", number))
	}
	return reasons
}
