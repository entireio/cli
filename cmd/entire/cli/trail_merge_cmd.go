package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/spf13/cobra"
)

const (
	trailComparisonAvailable = "available"
	trailConflictConflicting = "conflicting"
	trailChecksNone          = "none"
	trailMergeUnknown        = "unknown"
	trailBypassPolicyNobody  = "nobody"

	trailChecksAvailable     = "available"
	trailChecksNotApplicable = "not_applicable"

	trailGateTypeChecks    = "checks"
	trailGateStateDisabled = "disabled"
	trailGatePassed        = "passed"
	trailGateSkipped       = "skipped"
	trailGateFailed        = "failed"
	trailGatePending       = "pending"
	trailGateError         = "error"
)

type trailMergeOptions struct {
	Selector string
	Branch   string
	DryRun   bool
	Force    bool
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
on the trail; the repo's bypass policy decides who may do it.

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
	DryRun         bool                    `json:"dryRun"`
	Merged         bool                    `json:"merged"`
	Bypassed       bool                    `json:"bypassed"`
	MergeCommitSha string                  `json:"mergeCommitSha,omitempty"`
}

type trailMergeBlockerJSON struct {
	GateKey   string `json:"gateKey"`
	GateType  string `json:"gateType"`
	Status    string `json:"status"`
	Rationale string `json:"rationale,omitempty"`
	URL       string `json:"url,omitempty"`
}

func newTrailMergeResultJSON(t *api.TrailResource, m *api.TrailMergeabilityResponse, gates []api.TrailGateResult, dryRun bool) *trailMergeResultJSON {
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

		m, err := fetchTrailMergeability(ctx, client, mergePath)
		if err != nil {
			return err
		}
		printTrailMergeability(w, found, m)

		gates := trailMergeBlockingGates(m)
		result := newTrailMergeResultJSON(found, m, gates, opts.DryRun)
		err = trailMergeAfterRead(ctx, w, client, mergePath, found, m, gates, opts, result)
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
	m *api.TrailMergeabilityResponse, gates []api.TrailGateResult, opts trailMergeOptions, result *trailMergeResultJSON,
) error {
	bypassable := len(gates)
	if !m.Mergeable {
		fmt.Fprintln(w, "Blocked by:")
		for _, b := range describeTrailMergeBlockers(m, gates) {
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
			return trailMergeBlockedError(found.Number, m, gates)
		}
		if !trailBypassAllowed(m.BypassPolicy) {
			return trailBypassNotAllowedError(found.Number, m.BypassPolicy)
		}
		if trailMergeHead(m) == "" {
			return fmt.Errorf("cannot merge trail #%d with --force: the server did not report its head commit, so the bypass cannot be pinned to the commit whose gates were checked", found.Number)
		}
	}

	if opts.DryRun {
		if bypass {
			fmt.Fprintf(w, "Trail #%d is blocked; --force would bypass %d blocking %s, subject to the repo's bypass policy (%s) (dry run; no merge performed).\n",
				found.Number, bypassable, pluralize("gate", bypassable), trailBypassPolicyDisplay(m.BypassPolicy))
		} else {
			fmt.Fprintf(w, "Trail #%d is mergeable (dry run; no merge performed).\n", found.Number)
		}
		return nil
	}

	// Bypass only what was shown: a trail that was mergeable when read
	// merges without bypass, so a gate failing in between is refused
	// rather than bypassed unseen.
	req := api.TrailMergeRequest{Bypass: bypass, ExpectedHeadSha: trailMergeHead(m)}
	res, err := postTrailMerge(ctx, client, mergePath, found.Number, req, m.BypassPolicy)
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

func trailMergeHead(m *api.TrailMergeabilityResponse) string {
	if m.HeadSHA == nil {
		return ""
	}
	return strings.TrimSpace(*m.HeadSHA)
}

// trailBypassAllowed reports whether the repo's bypass policy could let anyone
// bypass gates. Mergeability carries no per-caller eligibility, so for the
// other policies the server decides at merge time.
func trailBypassAllowed(policy string) bool {
	return strings.TrimSpace(policy) != trailBypassPolicyNobody
}

func trailBypassPolicyDisplay(policy string) string {
	policy = tuiutil.SanitizeDisplayText(strings.TrimSpace(policy))
	if policy == "" {
		return trailMergeUnknown
	}
	return policy
}

func trailBypassNotAllowedError(number int, policy string) error {
	return fmt.Errorf("cannot merge trail #%d with --force: this repo's bypass policy (%s) does not allow bypassing gates", number, trailBypassPolicyDisplay(policy))
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

func fetchTrailMergeability(ctx context.Context, client *api.Client, trailPath string) (*api.TrailMergeabilityResponse, error) {
	resp, err := client.Get(ctx, trailPath+"/mergeability")
	if err != nil {
		return nil, fmt.Errorf("failed to check mergeability: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}
	var m api.TrailMergeabilityResponse
	if err := api.DecodeJSON(resp, &m); err != nil {
		return nil, fmt.Errorf("failed to decode mergeability response: %w", err)
	}
	return &m, nil
}

func postTrailMerge(ctx context.Context, client *api.Client, trailPath string, number int, req api.TrailMergeRequest, bypassPolicy string) (*api.TrailMergeResponse, error) {
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
			if mapped := trailMergeRefusal(httpErr, number, bypassPolicy); mapped != nil {
				return nil, mapped
			}
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return nil, fmt.Errorf("%w (run 'entire login' to re-authenticate)", checkErr)
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

func trailMergeRefusal(e *api.HTTPError, number int, bypassPolicy string) error {
	code := strings.TrimSpace(e.Code)
	if code == "" {
		code = strings.TrimSpace(e.Message)
	}
	forceHint := ""
	if trailBypassAllowed(bypassPolicy) {
		forceHint = ", or rerun with --force to bypass"
	}
	switch {
	case e.StatusCode == http.StatusForbidden && code == "merge_gates_bypass_forbidden":
		return fmt.Errorf("cannot merge trail #%d with --force: this repo's bypass policy (%s) does not allow you to bypass failing gates", number, trailBypassPolicyDisplay(bypassPolicy))
	case e.StatusCode == http.StatusUnprocessableEntity && code == "merge_gates_failed":
		return fmt.Errorf("trail #%d is not mergeable: a blocking gate failed when the merge was attempted\nhint: run 'entire trail merge --dry-run' to see why%s", number, forceHint)
	case e.StatusCode == http.StatusUnprocessableEntity && code == "merge_gates_pending":
		return fmt.Errorf("trail #%d is not mergeable yet: a blocking gate is still pending\nhint: wait for it to finish%s", number, forceHint)
	case e.StatusCode == http.StatusConflict && strings.HasPrefix(code, "merge_head_mismatch"):
		return fmt.Errorf("trail #%d changed while it was being merged (%s)\nhint: rerun 'entire trail merge' to merge the new head", number, tuiutil.SanitizeDisplayText(code))
	case e.StatusCode == http.StatusConflict && code == "merge_in_progress":
		return fmt.Errorf("trail #%d is already being merged; try again shortly", number)
	}
	return nil
}

// trailMergeBlockedError explains why the merge stopped. It says "pending"
// rather than "failing" when nothing has failed, and suggests --force only
// when there are gates to bypass and the bypass policy is not nobody.
func trailMergeBlockedError(number int, m *api.TrailMergeabilityResponse, gates []api.TrailGateResult) error {
	if len(gates) == 0 {
		return fmt.Errorf("trail #%d is not mergeable\nhint: run 'entire trail show' for details", number)
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
	case !trailBypassAllowed(m.BypassPolicy) && wait != "":
		return fmt.Errorf("%s\nhint: %s; this repo's bypass policy (%s) does not allow bypassing gates", msg, wait, trailBypassPolicyDisplay(m.BypassPolicy))
	case !trailBypassAllowed(m.BypassPolicy):
		return fmt.Errorf("%s\nhint: this repo's bypass policy (%s) does not allow bypassing gates", msg, trailBypassPolicyDisplay(m.BypassPolicy))
	case wait != "":
		return fmt.Errorf("%s\nhint: %s, or rerun with --force to bypass them", msg, wait)
	default:
		return fmt.Errorf("%s\nhint: rerun with --force to merge anyway, bypassing the blocking gates", msg)
	}
}

func printTrailMergeability(w io.Writer, t *api.TrailResource, m *api.TrailMergeabilityResponse) {
	fmt.Fprintf(w, "Trail #%d (%s → %s)\n", t.Number,
		tuiutil.SanitizeDisplayText(t.Branch), tuiutil.SanitizeDisplayText(t.Base))
	fmt.Fprintf(w, "  Approvals:  %s\n", checkmark(m.ApprovalGatePassed))
	fmt.Fprintf(w, "  Checks:     %s\n", trailChecksDisplay(m))
	fmt.Fprintf(w, "  Up to date: %s\n", trailUpToDateDisplay(m))
	fmt.Fprintf(w, "  Conflicts:  %s\n", trailConflictDisplay(m.ConflictStatus))
	fmt.Fprintf(w, "  Mergeable:  %s\n", checkmark(m.Mergeable))
}

func checkmark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// trailChecksDisplay renders the checks gate's verdict with the CI runs it
// was evaluated from. Without a checks gate the runs alone decide.
func trailChecksDisplay(m *api.TrailMergeabilityResponse) string {
	counts := countTrailCheckRuns(m.Checks.Runs)
	status := ""
	if g := findTrailGate(m.Gates, trailGateTypeChecks); g != nil {
		status = tuiutil.SanitizeDisplayText(strings.TrimSpace(g.Status))
	}
	if status == "" {
		switch m.Checks.Availability {
		case trailChecksAvailable:
			status = counts.verdict()
		case trailChecksNotApplicable:
			status = trailChecksNone
		default:
			status = trailMergeUnknown
		}
	}
	ok := status == trailGatePassed || status == trailGateSkipped || status == trailChecksNone
	line := checkmark(ok) + " " + status
	switch m.Checks.Availability {
	case trailChecksAvailable:
		if counts.total > 0 {
			line += " (" + counts.summary() + ")"
		}
	case trailChecksNotApplicable:
	default:
		line += " (CI evidence unavailable)"
	}
	return line
}

type trailCheckRunCounts struct {
	total, failed, pending, passed int
}

func countTrailCheckRuns(runs []api.TrailCheckRun) trailCheckRunCounts {
	var c trailCheckRunCounts
	for _, r := range runs {
		c.total++
		conclusion := ""
		if r.Conclusion != nil {
			conclusion = strings.TrimSpace(*r.Conclusion)
		}
		switch {
		case !strings.EqualFold(strings.TrimSpace(r.Status), "completed"):
			c.pending++
		case conclusion == "success" || conclusion == "neutral" || conclusion == "skipped":
			c.passed++
		default:
			c.failed++
		}
	}
	return c
}

func (c trailCheckRunCounts) verdict() string {
	switch {
	case c.failed > 0:
		return trailGateFailed
	case c.pending > 0:
		return trailGatePending
	case c.total == 0:
		return trailChecksNone
	default:
		return trailGatePassed
	}
}

func (c trailCheckRunCounts) summary() string {
	parts := make([]string, 0, 3)
	if c.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", c.failed))
	}
	if c.pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", c.pending))
	}
	if c.passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", c.passed))
	}
	return fmt.Sprintf("%d %s: %s", c.total, pluralize("run", c.total), strings.Join(parts, ", "))
}

func findTrailGate(gates []api.TrailGateResult, gateType string) *api.TrailGateResult {
	for i := range gates {
		if gates[i].GateType == gateType {
			return &gates[i]
		}
	}
	return nil
}

func trailUpToDateDisplay(m *api.TrailMergeabilityResponse) string {
	if m.ComparisonStatus != trailComparisonAvailable {
		return trailMergeUnknown
	}
	if m.BehindBy == 0 {
		return checkmark(true)
	}
	return fmt.Sprintf("%s (%d %s behind)", checkmark(false), m.BehindBy, pluralize("commit", m.BehindBy))
}

func trailConflictDisplay(status string) string {
	switch status {
	case "clean":
		return "none"
	case trailConflictConflicting:
		return "conflicts with base"
	default:
		return trailMergeUnknown
	}
}

// trailMergeBlockingGates returns the gates that block the merge, mirroring
// the server's rollup: blocking, enabled, and failed, pending, or errored.
// They come from the mergeability read itself, which is evaluated for the
// current head (the /gates route only reports persisted results).
func trailMergeBlockingGates(m *api.TrailMergeabilityResponse) []api.TrailGateResult {
	var out []api.TrailGateResult
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

func describeTrailMergeBlockers(m *api.TrailMergeabilityResponse, gates []api.TrailGateResult) []string {
	blockers := make([]string, 0, len(gates)+1)
	for _, g := range gates {
		blockers = append(blockers, describeGateFailure(g))
	}
	if len(gates) == 0 {
		blockers = append(blockers, describeMergeabilityBlockers(m)...)
	}
	if m.ConflictStatus == trailConflictConflicting {
		blockers = append(blockers, "branch conflicts with its base; resolve the conflicts first (--force cannot bypass this)")
	}
	if len(blockers) == 0 {
		blockers = append(blockers, "the trail is not in a mergeable state")
	}
	return blockers
}

func describeGateFailure(g api.TrailGateResult) string {
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
func trailGateWebURL(g api.TrailGateResult) string {
	var value struct {
		WebURL string `json:"web_url"`
	}
	if len(g.Value) == 0 || json.Unmarshal(g.Value, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value.WebURL)
}

func describeMergeabilityBlockers(m *api.TrailMergeabilityResponse) []string {
	var reasons []string
	if !m.ApprovalGatePassed {
		reasons = append(reasons, "required approvals are missing")
	}
	if m.Checks.Availability == trailChecksAvailable {
		switch countTrailCheckRuns(m.Checks.Runs).verdict() {
		case trailGateFailed:
			reasons = append(reasons, "CI checks failed")
		case trailGatePending:
			reasons = append(reasons, "CI checks are still running")
		}
	}
	if m.ComparisonStatus == trailComparisonAvailable && m.BehindBy > 0 {
		reasons = append(reasons, fmt.Sprintf("branch is %d %s behind its base", m.BehindBy, pluralize("commit", m.BehindBy)))
	}
	if len(reasons) == 0 && m.ConflictStatus != trailConflictConflicting {
		reasons = append(reasons, "a blocking gate failed (run 'entire trail show' for details)")
	}
	return reasons
}
