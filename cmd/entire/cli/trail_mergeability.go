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
// per gate, and a checks summary listing only the failed runs. `--json`
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
	keys := make([]string, len(gates))
	statuses := make([]string, len(gates))
	keyWidth, statusWidth := 0, 0
	for i, g := range gates {
		keys[i] = tuiutil.SanitizeDisplayText(g.GateKey)
		statuses[i] = tuiutil.SanitizeDisplayText(g.Status)
		keyWidth = max(keyWidth, lipgloss.Width(keys[i]))
		statusWidth = max(statusWidth, lipgloss.Width(statuses[i]))
	}
	for i, g := range gates {
		icon, style := trailGateIcon(styles, g.Status)
		detail := ""
		if g.Rationale != nil {
			detail = tuiutil.SanitizeDisplayText(*g.Rationale)
		}
		if !g.Blocking {
			detail = strings.TrimSpace(detail + " (non-blocking)")
		}
		line := fmt.Sprintf("    %s %s  %s  %s", styles.render(style, icon),
			tuiutil.PadDisplayWidth(keys[i], keyWidth), tuiutil.PadDisplayWidth(statuses[i], statusWidth), detail)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

func trailGateIcon(styles statusStyles, status string) (string, lipgloss.Style) {
	switch status {
	case "passed":
		return "✓", styles.green
	case "failed":
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
		case run.Status != "completed":
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
