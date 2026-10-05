package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// printTrailMonitors renders one row per monitor for the human `trail show`
// view: label, value, and the quality bucket the web app would color it with.
// Monitors still running or evaluated on an older head say so. Nothing is
// printed when the server served no monitors. Server-supplied strings are
// sanitized because they are printed straight to the terminal.
func printTrailMonitors(w io.Writer, styles statusStyles, label func(string) string, monitors []api.TrailMonitor) {
	if len(monitors) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s\n", label("Monitors:"))
	labels := make([]string, len(monitors))
	values := make([]string, len(monitors))
	labelWidth, valueWidth := 0, 0
	for i, m := range monitors {
		labels[i] = tuiutil.SanitizeDisplayText(m.Label)
		if labels[i] == "" {
			labels[i] = tuiutil.SanitizeDisplayText(m.Key)
		}
		values[i] = trailMonitorValueDisplay(m)
		labelWidth = max(labelWidth, lipgloss.Width(labels[i]))
		valueWidth = max(valueWidth, lipgloss.Width(values[i]))
	}
	for i, m := range monitors {
		quality := m.Quality()
		icon, style := trailMonitorIcon(styles, quality)
		var notes []string
		switch {
		case m.Evaluating || m.State == api.TrailAutomationRunning:
			notes = append(notes, "evaluating")
		case m.State == api.TrailAutomationStale:
			notes = append(notes, "stale")
		case m.State == api.TrailAutomationConfigurationError:
			notes = append(notes, "configuration error")
		}
		detail := quality
		if len(notes) > 0 {
			detail += " (" + strings.Join(notes, ", ") + ")"
		}
		line := fmt.Sprintf("    %s %s  %s  %s", styles.render(style, icon),
			tuiutil.PadDisplayWidth(labels[i], labelWidth), tuiutil.PadDisplayWidth(values[i], valueWidth), detail)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

const (
	trailMonitorYes = "yes"
	trailMonitorNo  = "no"
)

func trailMonitorValueDisplay(m api.TrailMonitor) string {
	switch {
	case m.ValueType == "percent" && m.PercentValue != nil:
		return strconv.FormatFloat(*m.PercentValue, 'f', -1, 64) + "%"
	case m.ValueType == "size" && m.SizeValue != nil:
		return tuiutil.SanitizeDisplayText(*m.SizeValue)
	case m.ValueType == "boolean" && m.BooleanValue != nil:
		if *m.BooleanValue {
			return trailMonitorYes
		}
		return trailMonitorNo
	}
	return "-"
}

func trailMonitorIcon(styles statusStyles, quality string) (string, lipgloss.Style) {
	switch quality {
	case api.TrailMonitorQualitySuccess:
		return "✓", styles.green
	case api.TrailMonitorQualityWarning:
		return "!", styles.yellow
	case api.TrailMonitorQualityDanger:
		return "✗", styles.red
	default:
		return "-", styles.dim
	}
}
