package cli

import (
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// Check-run conclusions counted as failed in the `trail show` checks summary.
// Anything else that has completed (success, neutral, skipped) counts as
// passed. This buckets runs for display only; the merge verdict is always the
// server's mergeability.mergeable, never derived from these counts.
var trailFailedCheckConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"cancelled":       true,
	"action_required": true,
	"startup_failure": true,
	"stale":           true,
}

// printTrailMergeability renders the mergeability snapshot for the human
// `trail show` view: the server's verdict, head, and conflict status, one row
// per gate with eval gates grouped under "Evals:", the non-blocking gates that
// need attention, and a checks summary listing only the failed runs. `--json`
// carries every run and full gate rows. Server-supplied strings are sanitized
// because they are printed straight to the terminal.
func printTrailMergeability(w io.Writer, styles statusStyles, label func(string) string, mg *api.TrailMergeability) {
	if mg == nil {
		fmt.Fprintf(w, "  %s%s\n", label("Mergeable: "), "unknown")
		return
	}

	mergeable := styles.render(styles.red, "no")
	if mg.Mergeable {
		mergeable = styles.render(styles.green, "yes")
	}
	fmt.Fprintf(w, "  %s%s\n", label("Mergeable: "), mergeable)
	if mg.HeadSHA != nil && strings.TrimSpace(*mg.HeadSHA) != "" {
		fmt.Fprintf(w, "  %s%s\n", label("Head:      "), shortSHA(tuiutil.SanitizeDisplayText(*mg.HeadSHA)))
	}
	fmt.Fprintf(w, "  %s%s\n", label("Conflicts: "), trailConflictDisplay(styles, mg.ConflictStatus))

	printTrailGates(w, styles, label, mg.Gates)
	printTrailGatesNeedingAttention(w, label, mg.Gates)
	printTrailChecks(w, styles, label, mg.Checks)
}

func trailConflictDisplay(styles statusStyles, status string) string {
	switch status {
	case "clean":
		return styles.render(styles.green, status)
	case "conflicting":
		return styles.render(styles.red, status)
	case "":
		return "unknown"
	default:
		return tuiutil.SanitizeDisplayText(status)
	}
}

func printTrailGates(w io.Writer, styles statusStyles, label func(string) string, gates []api.TrailGate) {
	if len(gates) == 0 {
		fmt.Fprintf(w, "  %s%s\n", label("Gates:     "), "none")
		return
	}
	fmt.Fprintf(w, "  %s\n", label("Gates:"))
	var others, evals []api.TrailGate
	for _, g := range gates {
		switch {
		case g.GateType != api.TrailGateTypeEval:
			others = append(others, g)
		case g.State != trailGateStateDisabled:
			evals = append(evals, g)
		}
	}
	keys := make([]string, len(others))
	statuses := make([]string, len(others))
	keyWidth, statusWidth := 0, 0
	for i, g := range others {
		keys[i] = tuiutil.SanitizeDisplayText(g.GateKey)
		statuses[i] = tuiutil.SanitizeDisplayText(g.Status)
		keyWidth = max(keyWidth, lipgloss.Width(keys[i]))
		statusWidth = max(statusWidth, lipgloss.Width(statuses[i]))
	}
	for i, g := range others {
		icon, style := trailGateIcon(styles, g.Status)
		line := fmt.Sprintf("    %s %s  %s  %s", styles.render(style, icon),
			tuiutil.PadDisplayWidth(keys[i], keyWidth), tuiutil.PadDisplayWidth(statuses[i], statusWidth), trailGateDetail(g))
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	if len(evals) > 0 {
		fmt.Fprintf(w, "    %s\n", label("Evals:"))
		printTrailEvalGates(w, styles, evals)
	}
}

// trailGateDetail is a gate row's rationale, marked when the gate does not
// block the merge.
func trailGateDetail(g api.TrailGate) string {
	detail := ""
	if g.Rationale != nil {
		detail = tuiutil.SanitizeDisplayText(*g.Rationale)
	}
	if !g.Blocking {
		detail = strings.TrimSpace(detail + " (non-blocking)")
	}
	return detail
}

// trailGatesNeedingAttention returns the non-blocking gates that failed or
// errored: they do not hold the merge, but someone should look at them.
func trailGatesNeedingAttention(gates []api.TrailGate) []api.TrailGate {
	var out []api.TrailGate
	for _, g := range gates {
		if g.Blocking || g.State == trailGateStateDisabled {
			continue
		}
		if g.Status == trailGateFailed || g.Status == trailGateError {
			out = append(out, g)
		}
	}
	return out
}

func printTrailGatesNeedingAttention(w io.Writer, label func(string) string, gates []api.TrailGate) {
	attention := trailGatesNeedingAttention(gates)
	if len(attention) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s\n", label("Needs attention:"))
	for _, g := range attention {
		fmt.Fprintf(w, "    - %s\n", describeGateFailure(g))
	}
}

func trailGateIcon(styles statusStyles, status string) (string, lipgloss.Style) {
	switch status {
	case "passed":
		return "✓", styles.green
	case "failed", "error":
		return "✗", styles.red
	case "pending":
		return "…", styles.yellow
	case "skipped":
		return "-", styles.gray
	default:
		return "?", styles.gray
	}
}

func printTrailChecks(w io.Writer, styles statusStyles, label func(string) string, checks api.TrailChecks) {
	switch checks.Availability {
	case api.TrailChecksAvailable:
	case api.TrailChecksNotApplicable:
		fmt.Fprintf(w, "  %s%s\n", label("Checks:    "), "not applicable")
		return
	case "":
		fmt.Fprintf(w, "  %s%s\n", label("Checks:    "), "unknown")
		return
	default:
		fmt.Fprintf(w, "  %s%s\n", label("Checks:    "), tuiutil.SanitizeDisplayText(checks.Availability))
		return
	}
	if len(checks.Runs) == 0 {
		fmt.Fprintf(w, "  %s%s\n", label("Checks:    "), "none reported")
		return
	}

	var passed, running int
	var failed []api.TrailCheckRun
	for _, run := range checks.Runs {
		switch {
		case run.Status != statusCompleted:
			running++
		case run.Conclusion != nil && trailFailedCheckConclusions[*run.Conclusion]:
			failed = append(failed, run)
		default:
			passed++
		}
	}
	summary := fmt.Sprintf("%d passed · %d running · %d failed (%d total)", passed, running, len(failed), len(checks.Runs))
	fmt.Fprintf(w, "  %s%s\n", label("Checks:    "), summary)

	names := make([]string, len(failed))
	nameWidth := 0
	for i, run := range failed {
		names[i] = tuiutil.SanitizeDisplayText(run.Name)
		nameWidth = max(nameWidth, lipgloss.Width(names[i]))
	}
	for i, run := range failed {
		url := ""
		if run.DetailsURL != nil {
			url = tuiutil.SanitizeDisplayText(*run.DetailsURL)
		}
		line := fmt.Sprintf("    %s %s  %s  %s", styles.render(styles.red, "✗"),
			tuiutil.PadDisplayWidth(names[i], nameWidth), tuiutil.SanitizeDisplayText(*run.Conclusion), url)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}
