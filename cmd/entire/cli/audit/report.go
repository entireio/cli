package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
)

// RenderConsoleReport outputs a rich CLI summary to the writer.
func RenderConsoleReport(res *AuditResult, w io.Writer, useColor bool) {
	if !useColor {
		renderPlainTextReport(res, w)
		return
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86")).MarginBottom(1)
	badgeStyle := lipgloss.NewStyle().Bold(true).Padding(0, 1)

	var gradeBadge string
	if res.ReadinessScore >= 80 {
		gradeBadge = badgeStyle.Background(lipgloss.Color("42")).Foreground(lipgloss.Color("0")).Render(fmt.Sprintf("%d/100 - %s", res.ReadinessScore, res.ReadinessGrade))
	} else if res.ReadinessScore >= 60 {
		gradeBadge = badgeStyle.Background(lipgloss.Color("214")).Foreground(lipgloss.Color("0")).Render(fmt.Sprintf("%d/100 - %s", res.ReadinessScore, res.ReadinessGrade))
	} else {
		gradeBadge = badgeStyle.Background(lipgloss.Color("196")).Foreground(lipgloss.Color("15")).Render(fmt.Sprintf("%d/100 - %s", res.ReadinessScore, res.ReadinessGrade))
	}

	fmt.Fprintln(w, titleStyle.Render("=== ENTIRE CHECKPOINT AUDIT & RELEASE READINESS REPORT ==="))
	fmt.Fprintf(w, "Branch:       %s\n", res.BranchName)
	fmt.Fprintf(w, "Head Commit:  %s\n", res.HeadCommit)
	fmt.Fprintf(w, "Checkpoints:  %d committed sessions\n", res.CheckpointsCount)
	fmt.Fprintf(w, "Readiness:    %s\n\n", gradeBadge)

	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	fmt.Fprintln(w, sectionStyle.Render("▶ INTENT VERIFICATION & PROMPT COVERAGE"))
	for _, item := range res.Intents {
		var icon string
		switch item.Status {
		case IntentStatusFulfilled:
			icon = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✔ FULFILLED")
		case IntentStatusPartial:
			icon = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("▲ PARTIAL  ")
		default:
			icon = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✖ MISSING  ")
		}
		fmt.Fprintf(w, "  [%s] %s - %s\n", icon, item.ID, item.Prompt)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, sectionStyle.Render("▶ IDENTIFIED RISKS & AUDIT WARNINGS"))
	if len(res.Risks) == 0 {
		fmt.Fprintln(w, "  ✔ No critical risks or pending unresolved tasks found.")
	} else {
		for _, r := range res.Risks {
			var sevTag string
			switch r.Severity {
			case SeverityHigh:
				sevTag = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Render("[HIGH]")
			case SeverityMedium:
				sevTag = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("[MED] ")
			default:
				sevTag = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("[LOW] ")
			}
			fmt.Fprintf(w, "  %s %s: %s\n", sevTag, r.ID, r.Title)
			if r.Location != "" {
				fmt.Fprintf(w, "         Loc: %s\n", r.Location)
			}
			fmt.Fprintf(w, "         Details: %s\n", r.Description)
		}
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, sectionStyle.Render("▶ ENTIRE GRAPH EVIDENCE"))
	for _, ge := range res.GraphEvidence {
		fmt.Fprintf(w, "  • %s\n", ge)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, sectionStyle.Render("▶ DEVELOPER / AGENT HANDOFF SUMMARY"))
	fmt.Fprintf(w, "  Goal: %s\n", res.Handoff.Goal)
	fmt.Fprintln(w, "  Completed Milestones:")
	for _, m := range res.Handoff.CompletedMilestones {
		fmt.Fprintf(w, "    - %s\n", m)
	}
	fmt.Fprintln(w, "  Recommended Next Steps:")
	for _, step := range res.Handoff.NextRecommendedSteps {
		fmt.Fprintf(w, "    - %s\n", step)
	}
	fmt.Fprintln(w)
}

func renderPlainTextReport(res *AuditResult, w io.Writer) {
	fmt.Fprintln(w, "=== ENTIRE CHECKPOINT AUDIT & RELEASE READINESS REPORT ===")
	fmt.Fprintf(w, "Branch:       %s\n", res.BranchName)
	fmt.Fprintf(w, "Head Commit:  %s\n", res.HeadCommit)
	fmt.Fprintf(w, "Checkpoints:  %d committed sessions\n", res.CheckpointsCount)
	fmt.Fprintf(w, "Readiness:    %d/100 (%s)\n\n", res.ReadinessScore, res.ReadinessGrade)

	fmt.Fprintln(w, "--- INTENT VERIFICATION ---")
	for _, item := range res.Intents {
		fmt.Fprintf(w, "  [%s] %s - %s\n", item.Status, item.ID, item.Prompt)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "--- IDENTIFIED RISKS ---")
	if len(res.Risks) == 0 {
		fmt.Fprintln(w, "  No risks identified.")
	} else {
		for _, r := range res.Risks {
			fmt.Fprintf(w, "  [%s] %s: %s (%s)\n", r.Severity, r.ID, r.Title, r.Location)
		}
	}
	fmt.Fprintln(w)
}

// RenderMarkdownReport converts the audit result to GitHub Flavored Markdown.
func RenderMarkdownReport(res *AuditResult) string {
	var sb strings.Builder

	sb.WriteString("# Entire Checkpoint Audit & Release Readiness Report\n\n")
	sb.WriteString(fmt.Sprintf("**Branch**: `%s` | **Head Commit**: `%s` | **Evaluated At**: `%s`\n\n",
		res.BranchName, res.HeadCommit, res.EvaluatedAt.Format("2006-01-02 15:04:05 MST")))

	sb.WriteString(fmt.Sprintf("> [!IMPORTANT]\n> **Overall Release Readiness Score**: **%d / 100** (%s)\n\n",
		res.ReadinessScore, res.ReadinessGrade))

	sb.WriteString("## 1. Intent Verification against Checkpoint Context\n\n")
	sb.WriteString("| Intent ID | Prompt / Requirement | Status | Evidence |\n")
	sb.WriteString("|---|---|---|---|\n")
	for _, item := range res.Intents {
		sb.WriteString(fmt.Sprintf("| `%s` | %s | **%s** | %s |\n",
			item.ID, item.Prompt, item.Status, item.Reasoning))
	}
	sb.WriteString("\n")

	sb.WriteString("## 2. Risk Matrix & Unresolved Requirements\n\n")
	if len(res.Risks) == 0 {
		sb.WriteString("✔ **No high-risk issues or unresolved tasks detected.**\n\n")
	} else {
		sb.WriteString("| Risk ID | Severity | Category | Description | Location |\n")
		sb.WriteString("|---|---|---|---|---|\n")
		for _, r := range res.Risks {
			sb.WriteString(fmt.Sprintf("| `%s` | **%s** | `%s` | %s | `%s` |\n",
				r.ID, r.Severity, r.Category, r.Description, r.Location))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## 3. Structural Graph Evidence\n\n")
	for _, ge := range res.GraphEvidence {
		sb.WriteString(fmt.Sprintf("- %s\n", ge))
	}
	sb.WriteString("\n")

	sb.WriteString("## 4. Agent & Developer Handoff Package\n\n")
	sb.WriteString(fmt.Sprintf("**Stated Goal**: %s\n\n", res.Handoff.Goal))

	sb.WriteString("### Completed Milestones:\n")
	for _, m := range res.Handoff.CompletedMilestones {
		sb.WriteString(fmt.Sprintf("- %s\n", m))
	}
	sb.WriteString("\n")

	sb.WriteString("### Recommended Next Actions:\n")
	for _, step := range res.Handoff.NextRecommendedSteps {
		sb.WriteString(fmt.Sprintf("- %s\n", step))
	}
	sb.WriteString("\n")

	return sb.String()
}

// RenderHandoffPayload outputs the handoff package in JSON or Markdown format.
func RenderHandoffPayload(res *AuditResult, asJSON bool) (string, error) {
	if asJSON {
		data, err := json.MarshalIndent(res.Handoff, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	var sb strings.Builder
	sb.WriteString("# Entire Agent Handoff Context\n\n")
	sb.WriteString(fmt.Sprintf("**Goal**: %s\n\n", res.Handoff.Goal))
	sb.WriteString("## Milestones Completed:\n")
	for _, m := range res.Handoff.CompletedMilestones {
		sb.WriteString(fmt.Sprintf("- %s\n", m))
	}
	sb.WriteString("\n## Open Risks & Warnings:\n")
	if len(res.Handoff.UnresolvedRisks) == 0 {
		sb.WriteString("- None\n")
	} else {
		for _, r := range res.Handoff.UnresolvedRisks {
			sb.WriteString(fmt.Sprintf("- %s\n", r))
		}
	}
	sb.WriteString("\n## Recommended Next Steps:\n")
	for _, s := range res.Handoff.NextRecommendedSteps {
		sb.WriteString(fmt.Sprintf("- %s\n", s))
	}
	return sb.String(), nil
}
