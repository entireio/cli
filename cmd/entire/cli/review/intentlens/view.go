package intentlens

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

const DemoNotice = "Demo evidence fixture — backend not connected"

type ViewState struct {
	Audit   *Audit
	Loading bool
	Demo    bool
	Err     error
}

type DashboardState struct {
	Audit         *Audit
	CheckpointID  string
	ContextStatus ContextStatus
	ContextNote   string
	Agent         string
	RequirementID string
}

func Render(w io.Writer, state ViewState) {
	if state.Loading {
		fmt.Fprintln(w, "Loading audit result...")
		return
	}
	if state.Err != nil {
		fmt.Fprintf(w, "Could not display audit result: %v\n", state.Err)
		return
	}
	if state.Audit == nil || len(state.Audit.Requirements) == 0 {
		fmt.Fprintln(w, "No audit result was provided.")
		return
	}
	if state.Demo {
		fmt.Fprintln(w, DemoNotice)
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "IntentLens Audit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, state.Audit.Summary)
	for _, requirement := range state.Audit.Requirements {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s  %s  %.0f%% confidence\n", requirement.ID, requirement.Status, requirement.Confidence*100)
		fmt.Fprintln(w, requirement.Requirement)
		fmt.Fprintln(w, "Evidence:")
		for _, evidence := range requirement.Evidence {
			fmt.Fprintf(w, "  - [%s] %s\n", evidence.Type, evidence.Explanation)
			var details []string
			for _, detail := range []struct{ label, value string }{
				{"file", evidence.File}, {"symbol", evidence.Symbol}, {"test", evidence.TestName},
				{"reference", evidence.Reference}, {"result", evidence.Result},
			} {
				if strings.TrimSpace(detail.value) != "" {
					details = append(details, detail.label+": "+detail.value)
				}
			}
			if len(details) > 0 {
				fmt.Fprintf(w, "    %s\n", strings.Join(details, " | "))
			}
		}
		if strings.TrimSpace(requirement.Recommendation) == "" {
			fmt.Fprintln(w, "Recommendation: none")
		} else {
			fmt.Fprintf(w, "Recommendation: %s\n", requirement.Recommendation)
		}
	}
}

func RenderDashboard(w io.Writer, state DashboardState) error {
	if state.Audit == nil || len(state.Audit.Requirements) == 0 {
		fmt.Fprintln(w, "No audit result was provided.")
		return nil
	}
	var selected *Requirement
	if strings.TrimSpace(state.RequirementID) != "" {
		for i := range state.Audit.Requirements {
			if state.Audit.Requirements[i].ID == state.RequirementID {
				selected = &state.Audit.Requirements[i]
				break
			}
		}
		if selected == nil {
			return fmt.Errorf("requirement %s not found in audit result", state.RequirementID)
		}
	}

	counts := countStatuses(state.Audit.Requirements)
	writeDashboardTop(w, "IntentLens Audit")
	writeDashboardTwoColumn(w, "Checkpoint", shortDashboardText(state.CheckpointID, 18), "Context", string(state.ContextStatus))
	if strings.TrimSpace(state.Agent) != "" {
		writeDashboardOneColumn(w, "Agent", state.Agent)
	}
	writeDashboardWrapped(w, "Summary  ", state.Audit.Summary)
	if strings.TrimSpace(state.ContextNote) != "" {
		writeDashboardWrapped(w, "Note     ", state.ContextNote)
	}
	writeDashboardSeparator(w)
	writeDashboardLine(w, fmt.Sprintf("Requirements: %d   ✓ Implemented: %d", len(state.Audit.Requirements), counts[StatusImplemented]))
	writeDashboardLine(w, fmt.Sprintf("                  ! Incomplete: %d   ? Uncertain: %d", counts[StatusIncomplete], counts[StatusUncertain]))
	writeDashboardSeparator(w)
	for _, requirement := range state.Audit.Requirements {
		writeDashboardLine(w, fmt.Sprintf("%-3s %s %-10s %3.0f%%  %s", requirement.ID, statusGlyph(requirement.Status), requirement.Status, requirement.Confidence*100, shortDashboardText(requirement.Requirement, 34)))
	}

	if selected != nil {
		writeDashboardSeparator(w)
		renderRequirementDetail(w, *selected)
		writeDashboardBottom(w)
		return nil
	}

	recommendations := topRecommendations(state.Audit.Requirements, 1)
	if len(recommendations) > 0 {
		writeDashboardSeparator(w)
		writeDashboardWrapped(w, "Fix next: ", recommendations[0].Text)
		if recommendations[0].RequirementID != "" {
			writeDashboardLine(w, "Use requirement details: "+recommendations[0].RequirementID)
		}
	}
	writeDashboardBottom(w)
	return nil
}

func renderRequirementDetail(w io.Writer, requirement Requirement) {
	writeDashboardLine(w, "Requirement Detail: "+requirement.ID)
	writeDashboardWrapped(w, "", fmt.Sprintf("%s %-10s %3.0f%%  %s", statusGlyph(requirement.Status), requirement.Status, requirement.Confidence*100, requirement.Requirement))
	if len(requirement.Evidence) > 0 {
		writeDashboardLine(w, "Evidence:")
		for _, evidence := range requirement.Evidence {
			writeDashboardWrapped(w, "  - ", fmt.Sprintf("[%s] %s", evidence.Type, evidence.Explanation))
			for _, detail := range evidenceDetails(evidence) {
				writeDashboardWrapped(w, "    ", detail)
			}
		}
	}
	if strings.TrimSpace(requirement.Recommendation) != "" {
		writeDashboardWrapped(w, "Recommendation: ", requirement.Recommendation)
	}
}

const dashboardContentWidth = 76
const dashboardFrameWidth = dashboardContentWidth + 2

type dashboardRecommendation struct {
	RequirementID string
	Text          string
}

func writeDashboardTop(w io.Writer, title string) {
	label := " " + title + " "
	left := (dashboardFrameWidth - runeLen(label)) / 2
	right := dashboardFrameWidth - runeLen(label) - left
	fmt.Fprintf(w, "╭%s%s%s╮\n", strings.Repeat("─", left), label, strings.Repeat("─", right))
}

func writeDashboardSeparator(w io.Writer) {
	fmt.Fprintf(w, "├%s┤\n", strings.Repeat("─", dashboardFrameWidth))
}

func writeDashboardBottom(w io.Writer) {
	fmt.Fprintf(w, "╰%s╯\n", strings.Repeat("─", dashboardFrameWidth))
}

func writeDashboardLine(w io.Writer, text string) {
	text = clipDashboardText(tuiutil.SanitizeDisplayText(text), dashboardContentWidth)
	padding := dashboardContentWidth - runeLen(text)
	if padding < 0 {
		padding = 0
	}
	fmt.Fprintf(w, "│ %s%s │\n", text, strings.Repeat(" ", padding))
}

func writeDashboardTwoColumn(w io.Writer, leftLabel, leftValue, rightLabel, rightValue string) {
	left := strings.TrimSpace(leftLabel + "  " + leftValue)
	right := strings.TrimSpace(rightLabel + "  " + rightValue)
	if rightValue == "" {
		writeDashboardLine(w, left)
		return
	}
	gap := dashboardContentWidth - runeLen(left) - runeLen(right)
	if gap < 2 {
		writeDashboardLine(w, left)
		writeDashboardLine(w, right)
		return
	}
	writeDashboardLine(w, left+strings.Repeat(" ", gap)+right)
}

func writeDashboardOneColumn(w io.Writer, label string, value string) {
	if strings.TrimSpace(value) != "" {
		writeDashboardLine(w, strings.TrimSpace(label+"  "+value))
	}
}

func writeDashboardWrapped(w io.Writer, prefix string, text string) {
	available := dashboardContentWidth - runeLen(prefix)
	for i, part := range wrapDashboardText(text, available) {
		if i == 0 {
			writeDashboardLine(w, prefix+part)
			continue
		}
		writeDashboardLine(w, strings.Repeat(" ", runeLen(prefix))+part)
	}
}

func countStatuses(requirements []Requirement) map[Status]int {
	counts := map[Status]int{
		StatusImplemented: 0,
		StatusIncomplete:  0,
		StatusUncertain:   0,
	}
	for _, requirement := range requirements {
		counts[requirement.Status]++
	}
	return counts
}

func topRecommendations(requirements []Requirement, limit int) []dashboardRecommendation {
	var recommendations []dashboardRecommendation
	for _, requirement := range requirements {
		recommendation := strings.TrimSpace(requirement.Recommendation)
		if recommendation == "" {
			continue
		}
		recommendations = append(recommendations, dashboardRecommendation{RequirementID: requirement.ID, Text: recommendation})
		if len(recommendations) == limit {
			break
		}
	}
	return recommendations
}

func evidenceDetails(evidence Evidence) []string {
	var details []string
	for _, detail := range []struct{ label, value string }{
		{"file", evidence.File},
		{"symbol", evidence.Symbol},
		{"test", evidence.TestName},
		{"reference", evidence.Reference},
		{"result", evidence.Result},
	} {
		if strings.TrimSpace(detail.value) != "" {
			details = append(details, detail.label+": "+detail.value)
		}
	}
	return details
}

func statusGlyph(status Status) string {
	switch status {
	case StatusImplemented:
		return "✓"
	case StatusIncomplete:
		return "!"
	case StatusUncertain:
		return "?"
	default:
		return "-"
	}
}

func wrapDashboardText(value string, limit int) []string {
	return tuiutil.WrapDisplayWidth(value, limit)
}

func shortDashboardText(value string, limit int) string {
	value = strings.TrimSpace(whitespacePattern.ReplaceAllString(value, " "))
	if value == "" {
		return ""
	}
	return clipDashboardText(value, limit)
}

func clipDashboardText(value string, limit int) string {
	return tuiutil.TruncateDisplayWidth(tuiutil.SanitizeDisplayText(value), limit)
}

func runeLen(value string) int {
	return ansi.StringWidth(value)
}
