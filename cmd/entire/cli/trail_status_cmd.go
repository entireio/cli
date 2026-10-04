package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/spf13/cobra"
)

// Trail status verdicts. The exit code of `entire trail status` follows the
// verdict so scripts and agent hooks can branch on it without parsing output.
const (
	trailVerdictGreen   = "green"
	trailVerdictRed     = "red"
	trailVerdictPending = "pending"
	trailVerdictUnknown = "unknown"
)

// Per-item states. warning, skipped and plateaued are reported but never
// change the verdict.
const (
	trailItemGreen     = "green"
	trailItemRed       = "red"
	trailItemPending   = "pending"
	trailItemUnknown   = "unknown"
	trailItemWarning   = "warning"
	trailItemSkipped   = "skipped"
	trailItemPlateaued = "plateaued"
)

const (
	trailKindFinding = "finding"
	trailKindMonitor = "monitor"
	trailGateFailed  = "failed"
)

var trailSeverityRank = map[string]int{
	trailReviewSeverityLow:    1,
	trailReviewSeverityMedium: 2,
	trailReviewSeverityHigh:   3,
}

type trailStatusOptions struct {
	Selector         string
	JSON             bool
	MinSeverity      string
	IncludeApprovals bool
	NoChecks         bool
	NoFindings       bool
	AllowWarning     bool
}

// trailStatusEvalOptions are the rules evaluateTrailStatus applies. The loop
// adds Skipped (findings a person has to decide) and Plateaued (yellow
// monitors that stopped improving) on top of the command's flags.
type trailStatusEvalOptions struct {
	MinSeverity      string
	IncludeApprovals bool
	NoChecks         bool
	NoFindings       bool
	AllowWarning     bool
	Skipped          map[string]string
	Plateaued        map[string]bool
}

type trailStatusItem struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Quality and Score are set for monitors: the web app's quality bucket,
	// and the value normalized to 0-100 with higher always better.
	Quality string   `json:"quality,omitempty"`
	Score   *float64 `json:"score,omitempty"`
}

type trailStatusReport struct {
	Trail   int               `json:"trail"`
	Title   string            `json:"title,omitempty"`
	HeadSHA string            `json:"head_sha,omitempty"`
	Verdict string            `json:"verdict"`
	Items   []trailStatusItem `json:"items"`
}

func (r trailStatusReport) itemsIn(state string) []trailStatusItem {
	var out []trailStatusItem
	for _, it := range r.Items {
		if it.State == state {
			out = append(out, it)
		}
	}
	return out
}

func newTrailStatusCmd() *cobra.Command {
	opts := trailStatusOptions{MinSeverity: trailReviewSeverityLow}
	cmd := &cobra.Command{
		Use:   "status [<trail>]",
		Short: "Say whether a trail is green: findings, monitors, gates, and checks",
		Long: `Report whether a trail is done: no open findings, every monitor green,
every blocking gate passed, and CI passing, all on the current head.

Defaults to the trail for the current branch.

Exit codes:
  0  green    nothing left to do
  1  red      findings, red monitors, failed gates, or failed checks
  2  pending  reviewers, monitors, or checks are still running on this head
  3  unknown  the server did not report enough to decide (never treated as green)

The approvals gate is left out by default because only a person can pass it;
--include-approvals adds it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalTrailSelector(args, "")
			if err != nil {
				return err
			}
			opts.Selector = selector
			return runTrailStatus(cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&opts.MinSeverity, "min-severity", opts.MinSeverity, "Lowest finding severity that counts: low, medium, or high")
	cmd.Flags().BoolVar(&opts.IncludeApprovals, "include-approvals", false, "Count the human approvals gate")
	cmd.Flags().BoolVar(&opts.NoChecks, "no-checks", false, "Ignore CI check runs")
	cmd.Flags().BoolVar(&opts.NoFindings, "no-findings", false, "Ignore open findings")
	cmd.Flags().BoolVar(&opts.AllowWarning, "allow-warning", false, "Treat yellow monitors as passing")
	return cmd
}

func runTrailStatus(cmd *cobra.Command, opts trailStatusOptions) error {
	if _, ok := trailSeverityRank[opts.MinSeverity]; !ok {
		return fmt.Errorf("--min-severity must be low, medium, or high, got %q", opts.MinSeverity)
	}
	report, err := loadTrailStatus(cmd, opts.Selector, trailStatusEvalOptions{
		MinSeverity:      opts.MinSeverity,
		IncludeApprovals: opts.IncludeApprovals,
		NoChecks:         opts.NoChecks,
		NoFindings:       opts.NoFindings,
		AllowWarning:     opts.AllowWarning,
	})
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if opts.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
	} else {
		printTrailStatus(w, report)
	}
	return trailVerdictExitError(report.Verdict)
}

// trailVerdictExitError maps a verdict to the command's exit code. Green is a
// normal exit; the others exit non-zero without printing an extra error line.
func trailVerdictExitError(verdict string) error {
	code := map[string]int{trailVerdictRed: 1, trailVerdictPending: 2, trailVerdictUnknown: 3}[verdict]
	if code == 0 {
		return nil
	}
	return NewExitCodeError(code, errors.New("trail is "+verdict))
}

// loadTrailStatus resolves the trail (by selector, else the current branch),
// fetches its detail and open current-version findings, and evaluates them.
func loadTrailStatus(cmd *cobra.Command, selector string, eval trailStatusEvalOptions) (trailStatusReport, error) {
	client, target, err := authenticatedTrailReviewTarget(cmd, selector)
	if err != nil {
		return trailStatusReport{}, err
	}
	return fetchAndEvaluateTrailStatus(cmd.Context(), client, target, eval)
}

func fetchAndEvaluateTrailStatus(ctx context.Context, client *api.Client, target trailReviewTarget, eval trailStatusEvalOptions) (trailStatusReport, error) {
	resp, err := client.Get(ctx, trailReviewTrailPath(target.Trail.ID))
	if err != nil {
		return trailStatusReport{}, fmt.Errorf("failed to fetch trail detail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return trailStatusReport{}, err
	}
	var detail api.TrailResource
	if err := api.DecodeJSON(resp, &detail); err != nil {
		return trailStatusReport{}, fmt.Errorf("decode trail detail: %w", err)
	}
	// Undecodable pieces are left nil, which the evaluation reports as
	// unknown rather than green.
	mergeability, _ := detail.DecodeMergeability() //nolint:errcheck // nil snapshot reads as unknown
	monitors, _ := detail.DecodeMonitors()         //nolint:errcheck // nil monitors are simply not counted
	runners, _ := detail.DecodeRunners()           //nolint:errcheck // nil runners are simply not counted

	var findings []api.TrailReviewComment
	findingsLoaded := false
	if !eval.NoFindings {
		findings, err = fetchAllTrailReviewComments(ctx, client, target.Trail.ID, defaultTrailReviewListOptions())
		if err != nil {
			return trailStatusReport{}, err
		}
		findingsLoaded = true
	}

	report := evaluateTrailStatus(mergeability, monitors, runners, findings, findingsLoaded, eval)
	report.Trail = target.Trail.Number
	report.Title = target.Trail.Title
	return report, nil
}

// trailReviewTrailPath is the detail route for a trail whose number route was
// registered on the client by resolveTrailReviewTarget.
func trailReviewTrailPath(trailID string) string {
	return "/api/v1/trails/" + trailID
}

// evaluateTrailStatus applies the green rules. The server's mergeability
// snapshot is the source of truth for gates and checks; nothing is inferred
// as passing. Red beats pending beats unknown beats green, so an agent always
// sees what it can fix first.
func evaluateTrailStatus(mg *api.TrailMergeability, monitors []api.TrailMonitor, runners []api.TrailRunner, findings []api.TrailReviewComment, findingsLoaded bool, eval trailStatusEvalOptions) trailStatusReport {
	head := ""
	if mg != nil && mg.HeadSHA != nil {
		head = strings.TrimSpace(*mg.HeadSHA)
	}
	ev := &trailStatusEvaluator{head: head, eval: eval}
	switch {
	case mg == nil:
		ev.add("trail", "mergeability", "mergeability", trailItemUnknown, "server did not report mergeability")
	case head == "":
		ev.add("trail", "head", "head", trailItemUnknown, "trail has no head commit yet; push the branch")
	}
	for _, m := range monitors {
		ev.monitor(m)
	}
	for _, r := range runners {
		ev.runner(r)
	}
	if mg != nil {
		for _, g := range mg.Gates {
			ev.gate(g)
		}
		if !eval.NoChecks && mg.Checks.Availability == api.TrailChecksAvailable {
			for _, run := range mg.Checks.Runs {
				ev.check(run)
			}
		}
	}
	if findingsLoaded {
		for _, f := range findings {
			ev.finding(f)
		}
	}

	report := trailStatusReport{HeadSHA: head, Items: ev.items}
	has := func(state string) bool { return len(report.itemsIn(state)) > 0 }
	switch {
	case has(trailItemRed):
		report.Verdict = trailVerdictRed
	case has(trailItemPending):
		report.Verdict = trailVerdictPending
	case has(trailItemUnknown):
		report.Verdict = trailVerdictUnknown
	default:
		report.Verdict = trailVerdictGreen
	}
	return report
}

type trailStatusEvaluator struct {
	head  string
	eval  trailStatusEvalOptions
	items []trailStatusItem
}

func (ev *trailStatusEvaluator) add(kind, key, name, state, detail string) {
	ev.items = append(ev.items, trailStatusItem{Kind: kind, Key: key, Name: tuiutil.SanitizeDisplayText(name), State: state, Detail: tuiutil.SanitizeDisplayText(detail)})
}

func (ev *trailStatusEvaluator) stale(sha *string) bool {
	return sha != nil && *sha != "" && ev.head != "" && *sha != ev.head
}

func (ev *trailStatusEvaluator) monitor(m api.TrailMonitor) {
	name := m.Label
	if name == "" {
		name = m.Key
	}
	quality := m.Quality()
	before := len(ev.items)
	switch {
	case m.State == api.TrailAutomationDisabled || m.State == api.TrailAutomationNotApplicable:
		return
	case m.State == api.TrailAutomationConfigurationError || (m.Outcome != nil && *m.Outcome == api.TrailAutomationErrored):
		ev.add(trailKindMonitor, m.Key, name, trailItemUnknown, "runner errored or is misconfigured")
	case m.Evaluating || m.State == api.TrailAutomationRunning || m.State == api.TrailAutomationNotRun:
		ev.add(trailKindMonitor, m.Key, name, trailItemPending, "evaluating")
	case m.State == api.TrailAutomationStale || ev.stale(m.EvaluatedAtSHA) || ev.stale(m.HeadSHA):
		ev.add(trailKindMonitor, m.Key, name, trailItemPending, "evaluated on an older commit")
	case quality == api.TrailMonitorQualitySuccess:
		ev.add(trailKindMonitor, m.Key, name, trailItemGreen, trailMonitorValueDisplay(m))
	case quality == api.TrailMonitorQualityDanger:
		ev.add(trailKindMonitor, m.Key, name, trailItemRed, trailMonitorDetail(m))
	case quality == api.TrailMonitorQualityWarning:
		switch {
		case ev.eval.Plateaued[m.Key]:
			ev.add(trailKindMonitor, m.Key, name, trailItemPlateaued, trailMonitorDetail(m)+" (stopped improving)")
		case ev.eval.AllowWarning:
			ev.add(trailKindMonitor, m.Key, name, trailItemWarning, trailMonitorDetail(m))
		default:
			ev.add(trailKindMonitor, m.Key, name, trailItemRed, "yellow "+trailMonitorDetail(m))
		}
	default:
		ev.add(trailKindMonitor, m.Key, name, trailItemSkipped, "no pass/fail direction")
	}
	if len(ev.items) > before {
		ev.items[len(ev.items)-1].Quality = quality
		ev.items[len(ev.items)-1].Score = trailMonitorScore(m)
	}
}

// runner holds the verdict at pending while a runner is still working.
func (ev *trailStatusEvaluator) runner(r api.TrailRunner) {
	switch {
	case r.State == api.TrailAutomationRunning || r.State == api.TrailAutomationNotRun || r.State == api.TrailAutomationStale:
		ev.add("runner", r.RunnerID, r.RunnerID, trailItemPending, r.State)
	case r.State == api.TrailAutomationConfigurationError || (r.Outcome != nil && *r.Outcome == api.TrailAutomationErrored):
		ev.add("runner", r.RunnerID, r.RunnerID, trailItemUnknown, "runner errored or is misconfigured")
	case r.State == api.TrailAutomationEvaluated && ev.stale(r.EvaluatedAtSHA):
		ev.add("runner", r.RunnerID, r.RunnerID, trailItemPending, "evaluated on an older commit")
	}
}

func (ev *trailStatusEvaluator) gate(g api.TrailGate) {
	if g.GateKey == "approvals" && !ev.eval.IncludeApprovals {
		return
	}
	detail := ""
	if g.Rationale != nil {
		detail = *g.Rationale
	}
	switch {
	case ev.stale(g.EvaluatedAtSHA):
		ev.add("gate", g.GateKey, g.GateKey, trailItemPending, "evaluated on an older commit")
	case g.Status == "passed" || g.Status == "skipped":
		ev.add("gate", g.GateKey, g.GateKey, trailItemGreen, g.Status)
	case g.Status == "pending" || g.Status == api.TrailAutomationRunning:
		ev.add("gate", g.GateKey, g.GateKey, trailItemPending, detail)
	case g.Status == trailGateFailed && g.Blocking:
		ev.add("gate", g.GateKey, g.GateKey, trailItemRed, detail)
	case g.Status == trailGateFailed:
		ev.add("gate", g.GateKey, g.GateKey, trailItemWarning, strings.TrimSpace(detail+" (non-blocking)"))
	default:
		ev.add("gate", g.GateKey, g.GateKey, trailItemUnknown, g.Status)
	}
}

func (ev *trailStatusEvaluator) check(run api.TrailCheckRun) {
	url := ""
	if run.DetailsURL != nil {
		url = *run.DetailsURL
	}
	switch {
	case run.Conclusion == nil:
		ev.add("check", run.Name, run.Name, trailItemPending, run.Status)
	case trailFailedCheckConclusions[*run.Conclusion]:
		ev.add("check", run.Name, run.Name, trailItemRed, strings.TrimSpace(*run.Conclusion+" "+url))
	default:
		ev.add("check", run.Name, run.Name, trailItemGreen, *run.Conclusion)
	}
}

// finding counts an open, current-version finding at or above the severity
// floor, unless a person has been asked to decide it.
func (ev *trailStatusEvaluator) finding(f api.TrailReviewComment) {
	floor := trailSeverityRank[ev.eval.MinSeverity]
	sev := stringPtrValue(f.Severity)
	rank := trailSeverityRank[sev]
	if rank == 0 {
		rank = trailSeverityRank[trailReviewSeverityLow]
	}
	if rank < floor {
		return
	}
	title := stringPtrValue(f.Title)
	if title == "" {
		title = trailStatusSnippet(stringPtrValue(f.Body))
	}
	if reason, ok := ev.eval.Skipped[f.ID]; ok {
		ev.add(trailKindFinding, f.ID, title, trailItemSkipped, "needs a person: "+reason)
		return
	}
	ev.add(trailKindFinding, f.ID, title, trailItemRed, strings.TrimSpace(sev+" "+trailReviewLocationShort(f.Location)))
}

func trailMonitorScore(m api.TrailMonitor) *float64 {
	lower := m.Polarity != nil && *m.Polarity == "lower_is_better"
	var v float64
	switch {
	case m.ValueType == "percent" && m.PercentValue != nil:
		v = *m.PercentValue
	case m.ValueType == "size" && m.SizeValue != nil:
		rank, ok := map[string]float64{"small": 0, "medium": 50, "large": 100}[*m.SizeValue]
		if !ok {
			return nil
		}
		v = rank
	default:
		return nil
	}
	if lower {
		v = 100 - v
	}
	return &v
}

func trailMonitorDetail(m api.TrailMonitor) string {
	detail := trailMonitorValueDisplay(m)
	if m.Rationale != nil && strings.TrimSpace(*m.Rationale) != "" {
		detail += ": " + trailStatusSnippet(*m.Rationale)
	}
	return detail
}

func trailReviewLocationShort(loc api.TrailReviewLocation) string {
	if loc.FilePath == nil || *loc.FilePath == "" {
		return ""
	}
	if loc.StartLine != nil && *loc.StartLine > 0 {
		return fmt.Sprintf("%s:%d", *loc.FilePath, *loc.StartLine)
	}
	return *loc.FilePath
}

func trailStatusSnippet(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxLen = 100
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

func printTrailStatus(w io.Writer, r trailStatusReport) {
	styles := newStatusStyles(w)
	header := fmt.Sprintf("Trail #%d", r.Trail)
	if r.HeadSHA != "" {
		header += "  head " + shortSHA(r.HeadSHA)
	}
	fmt.Fprintln(w, styles.render(styles.yellow, header))

	order := map[string]int{"trail": 0, trailKindFinding: 1, trailKindMonitor: 2, "gate": 3, "check": 4, "runner": 5}
	items := append([]trailStatusItem(nil), r.Items...)
	sort.SliceStable(items, func(i, j int) bool { return order[items[i].Kind] < order[items[j].Kind] })
	for _, it := range items {
		icon, style := trailStatusItemIcon(styles, it.State)
		line := fmt.Sprintf("  %s %-8s %s  %s", styles.render(style, icon), it.Kind, it.Name, it.State)
		if it.Detail != "" {
			line += "  " + it.Detail
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}

	verdict := strings.ToUpper(r.Verdict)
	_, style := trailStatusItemIcon(styles, map[string]string{
		trailVerdictGreen: trailItemGreen, trailVerdictRed: trailItemRed,
		trailVerdictPending: trailItemPending, trailVerdictUnknown: trailItemUnknown,
	}[r.Verdict])
	fmt.Fprintf(w, "Verdict: %s\n", styles.render(style, verdict))
}

func trailStatusItemIcon(styles statusStyles, state string) (string, lipgloss.Style) {
	switch state {
	case trailItemGreen:
		return "✓", styles.green
	case trailItemRed:
		return "✗", styles.red
	case trailItemPending:
		return "…", styles.yellow
	case trailItemWarning, trailItemPlateaued:
		return "!", styles.yellow
	case trailItemSkipped:
		return "-", styles.dim
	default:
		return "?", styles.dim
	}
}
