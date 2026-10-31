package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// Eval value types and polarities, as an eval gate's value reports them.
const (
	trailEvalPercent        = "percent"
	trailEvalSize           = "size"
	trailEvalBoolean        = "boolean"
	trailEvalHigherIsBetter = "higher_is_better"
	trailEvalLowerIsBetter  = "lower_is_better"

	trailEvalYes = "yes"
	trailEvalNo  = "no"
)

// printTrailEvalGates prints one row per eval gate, named by its agent. A
// gate with a reading shows it against the threshold and whether it passed,
// blocks the merge, or needs attention; a gate that is not enforced says why;
// any other gate (pending, error) prints like the other gate rows.
func printTrailEvalGates(w io.Writer, styles statusStyles, gates []api.TrailGate) {
	rows := make([][]string, len(gates))
	for i, g := range gates {
		rows[i] = trailEvalGateCells(g)
	}
	// A column is as wide as its widest cell among the rows that go on past
	// it, so a row's last cell never widens the column.
	var widths []int
	for _, cells := range rows {
		for c := range len(cells) - 1 {
			if c == len(widths) {
				widths = append(widths, 0)
			}
			widths[c] = max(widths[c], lipgloss.Width(cells[c]))
		}
	}
	for i, g := range gates {
		icon, style := trailGateIcon(styles, g.Status)
		cells := rows[i]
		var b strings.Builder
		for c, cell := range cells {
			if c < len(cells)-1 {
				cell = tuiutil.PadDisplayWidth(cell, widths[c]) + "  "
			}
			b.WriteString(cell)
		}
		fmt.Fprintln(w, strings.TrimRight("      "+styles.render(style, icon)+" "+b.String(), " "))
	}
}

func trailEvalGateCells(g api.TrailGate) []string {
	name := tuiutil.SanitizeDisplayText(trailGateName(g))
	if issue := trailEvalNotEnforced(g); issue != "" {
		return []string{name, "not enforced: " + tuiutil.SanitizeDisplayText(issue)}
	}
	if v, ok := trailEvalGateValue(g); ok && (g.Status == trailGatePassed || g.Status == trailGateFailed) {
		if reading := trailEvalReading(v.ValueType, v.Value); reading != "" {
			cells := []string{name, tuiutil.SanitizeDisplayText(reading)}
			if req := trailEvalRequirement(v); req != "" {
				cells = append(cells, "must be "+tuiutil.SanitizeDisplayText(req))
			}
			switch {
			case g.Status == trailGatePassed:
				return append(cells, "passed")
			case g.Blocking:
				return append(cells, "blocks merge")
			default:
				return append(cells, "needs attention")
			}
		}
	}
	cells := []string{name, tuiutil.SanitizeDisplayText(g.Status)}
	if detail := trailGateDetail(g); detail != "" {
		cells = append(cells, detail)
	}
	return cells
}

// trailEvalGateValue decodes an eval gate's value; ok is false for any
// other gate type and for a value the CLI cannot read.
func trailEvalGateValue(g api.TrailGate) (api.TrailEvalGateValue, bool) {
	var v api.TrailEvalGateValue
	if g.GateType != api.TrailGateTypeEval || len(g.Value) == 0 || json.Unmarshal(g.Value, &v) != nil {
		return api.TrailEvalGateValue{}, false
	}
	return v, true
}

// trailGateName is how a gate is named to the user: an eval gate by its
// agent's name, since its key is the agent id.
func trailGateName(g api.TrailGate) string {
	if v, ok := trailEvalGateValue(g); ok {
		if name := strings.TrimSpace(v.AgentName); name != "" {
			return name
		}
	}
	if key := strings.TrimSpace(g.GateKey); key != "" {
		return key
	}
	return strings.TrimSpace(g.GateType)
}

// trailEvalReading formats an eval reading or threshold of the given value
// type: 72%, medium, yes or no. It is "" for null and for a value of another
// type.
func trailEvalReading(valueType string, raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	switch valueType {
	case trailEvalPercent:
		var n float64
		if json.Unmarshal(raw, &n) == nil {
			return strconv.FormatFloat(n, 'f', -1, 64) + "%"
		}
	case trailEvalSize:
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s)
		}
	case trailEvalBoolean:
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return trailEvalYesNo(b)
		}
	}
	return ""
}

// trailEvalRequirement is what the reading must be to pass: "≤ 40%",
// "≥ medium", or for a yes/no eval the better answer. It is "" when the
// direction or the threshold is unknown.
func trailEvalRequirement(v api.TrailEvalGateValue) string {
	var op string
	switch v.Polarity {
	case trailEvalLowerIsBetter:
		op = "≤"
	case trailEvalHigherIsBetter:
		op = "≥"
	default:
		return ""
	}
	if v.ValueType == trailEvalBoolean {
		return trailEvalYesNo(v.Polarity == trailEvalHigherIsBetter)
	}
	threshold := trailEvalReading(v.ValueType, v.Threshold)
	if threshold == "" {
		return ""
	}
	return op + " " + threshold
}

func trailEvalYesNo(b bool) string {
	if b {
		return trailEvalYes
	}
	return trailEvalNo
}

// trailEvalNotEnforced is why an eval gate is skipped for a configuration
// issue, without the "Not enforced:" lead-in, or "" when it is enforced.
func trailEvalNotEnforced(g api.TrailGate) string {
	v, ok := trailEvalGateValue(g)
	if !ok || g.Status != trailGateSkipped || v.ConfigurationIssue == nil {
		return ""
	}
	msg := strings.TrimSpace(v.ConfigurationIssue.Message)
	const lead = "not enforced:"
	if len(msg) >= len(lead) && strings.EqualFold(msg[:len(lead)], lead) {
		msg = strings.TrimSpace(msg[len(lead):])
	}
	if msg == "" {
		msg = strings.TrimSpace(v.ConfigurationIssue.Code)
	}
	if msg == "" {
		msg = "configuration issue"
	}
	return msg
}

// printTrailMergeWarnings names what a merge goes ahead despite: the
// non-blocking gates that failed, and the eval gates that are not enforced.
func printTrailMergeWarnings(w io.Writer, gates []api.TrailGate) {
	for _, g := range trailGatesNeedingAttention(gates) {
		fmt.Fprintf(w, "Warning: needs attention: %s\n", describeGateFailure(g))
	}
	for _, g := range gates {
		if issue := trailEvalNotEnforced(g); issue != "" {
			fmt.Fprintf(w, "Warning: not enforced: %s\n", tuiutil.SanitizeDisplayText(issue))
		}
	}
}
