// Package agentcheck contains isolated presentation helpers for the future
// `entire agentcheck` command.
package agentcheck

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	VerdictTrusted        = "TRUSTED"
	VerdictReviewRequired = "REVIEW REQUIRED"
	VerdictFail           = "FAIL"

	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// RenderResult is a presentation-only model for terminal output.
//
// This is deliberately local to the renderer. It is not the shared AgentCheck
// result contract and should be adapted or replaced when that contract lands.
type RenderResult struct {
	CheckpointID   string
	Verdict        string
	TrustScore     *int
	Summary        string
	Findings       []RenderFinding
	Verification   *RenderVerification
	Recommendation string
}

// RenderFinding is a presentation-only finding for terminal output.
type RenderFinding struct {
	Severity    string
	Title       string
	Description string
	Evidence    []string
}

// RenderVerification is a presentation-only verification summary.
type RenderVerification struct {
	Status  string
	Summary string
}

// Render writes a terminal-friendly AgentCheck report.
func Render(w io.Writer, result RenderResult) error {
	if err := validateVerdict(result.Verdict); err != nil {
		return err
	}

	fmt.Fprintln(w, "AgentCheck")
	fmt.Fprintln(w, "─────────────────────────────────")
	fmt.Fprintln(w)

	if strings.TrimSpace(result.CheckpointID) != "" {
		fmt.Fprintf(w, "Checkpoint: %s\n", strings.TrimSpace(result.CheckpointID))
	}
	fmt.Fprintf(w, "Verdict:    %s\n", result.Verdict)
	if result.TrustScore != nil {
		fmt.Fprintf(w, "Trust Score: %d/100\n", *result.TrustScore)
	}

	if summary := strings.TrimSpace(result.Summary); summary != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Summary:")
		writeIndented(w, summary, "  ")
	}

	if len(result.Findings) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Findings:")
		findings := sortedFindings(result.Findings)
		for _, finding := range findings {
			writeFinding(w, finding)
		}
	}

	if result.Verification != nil {
		if summary := verificationSummary(*result.Verification); summary != "" {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Verification:")
			writeIndented(w, summary, "  ")
		}
	}

	if recommendation := strings.TrimSpace(result.Recommendation); recommendation != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Recommended action:")
		writeIndented(w, recommendation, "  ")
	}

	return nil
}

func validateVerdict(verdict string) error {
	switch verdict {
	case VerdictTrusted, VerdictReviewRequired, VerdictFail:
		return nil
	default:
		return fmt.Errorf("unsupported AgentCheck verdict %q", verdict)
	}
}

func sortedFindings(findings []RenderFinding) []RenderFinding {
	out := make([]RenderFinding, len(findings))
	copy(out, findings)
	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 3
	default:
		return 4
	}
}

func writeFinding(w io.Writer, finding RenderFinding) {
	severity := strings.ToUpper(strings.TrimSpace(finding.Severity))
	if severity == "" {
		severity = "INFO"
	}
	title := strings.TrimSpace(finding.Title)
	if title == "" {
		title = "Finding"
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s  %s\n", severity, title)

	if description := strings.TrimSpace(finding.Description); description != "" {
		writeIndented(w, description, "    ")
	}
	for _, evidence := range finding.Evidence {
		if evidence = strings.TrimSpace(evidence); evidence != "" {
			fmt.Fprintf(w, "    Evidence: %s\n", evidence)
		}
	}
}

func verificationSummary(verification RenderVerification) string {
	status := strings.TrimSpace(verification.Status)
	summary := strings.TrimSpace(verification.Summary)
	switch {
	case status != "" && summary != "":
		return status + " - " + summary
	case status != "":
		return status
	default:
		return summary
	}
}

func writeIndented(w io.Writer, text, indent string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprintln(w, indent+strings.TrimRight(line, " \t"))
	}
}
