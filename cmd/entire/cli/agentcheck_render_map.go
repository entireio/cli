package cli

import (
	"fmt"
	"strings"

	contextpkg "github.com/entireio/cli/cmd/entire/cli/agentcheck"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func mapAgentCheckEvaluationToRender(
	cpID id.CheckpointID,
	evaluation contextpkg.EvaluationResult,
	verification agentCheckVerificationEvidence,
) contextpkg.RenderResult {
	return contextpkg.RenderResult{
		CheckpointID:   cpID.String(),
		Verdict:        string(evaluation.Verdict),
		Summary:        evaluation.Summary,
		Findings:       mapAgentCheckFindingsToRender(evaluation.Findings),
		Verification:   mapAgentCheckVerificationToRender(verification),
		Recommendation: agentCheckRecommendation(evaluation.Findings),
	}
}

func mapAgentCheckFindingsToRender(findings []contextpkg.Finding) []contextpkg.RenderFinding {
	out := make([]contextpkg.RenderFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, contextpkg.RenderFinding{
			Severity:    string(finding.Severity),
			Title:       finding.Title,
			Description: finding.Description,
			Evidence:    flattenAgentCheckFindingEvidence(finding.Evidence),
		})
	}
	return out
}

func flattenAgentCheckFindingEvidence(evidence []contextpkg.FindingEvidence) []string {
	out := make([]string, 0, len(evidence))
	for _, item := range evidence {
		parts := make([]string, 0, 3)
		if kind := strings.TrimSpace(item.Kind); kind != "" {
			parts = append(parts, kind)
		}
		if ref := strings.TrimSpace(item.Reference); ref != "" {
			parts = append(parts, ref)
		}
		if detail := strings.TrimSpace(item.Detail); detail != "" {
			parts = append(parts, detail)
		}
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, ": "))
		}
	}
	return out
}

func mapAgentCheckVerificationToRender(evidence agentCheckVerificationEvidence) *contextpkg.RenderVerification {
	summary := agentCheckVerificationSummary(evidence)
	if strings.TrimSpace(evidence.Status) == "" && summary == "" {
		return nil
	}
	return &contextpkg.RenderVerification{
		Status:  evidence.Status,
		Summary: summary,
	}
}

func agentCheckVerificationSummary(evidence agentCheckVerificationEvidence) string {
	switch evidence.Status {
	case agentCheckVerificationSuccess:
		if evidence.Command != "" {
			return fmt.Sprintf("%s completed successfully.", evidence.Command)
		}
		return "Verification completed successfully."
	case agentCheckVerificationFailed:
		if evidence.Command != "" {
			return fmt.Sprintf("%s exited with code %d.", evidence.Command, evidence.ExitCode)
		}
		return fmt.Sprintf("Verification exited with code %d.", evidence.ExitCode)
	case agentCheckVerificationUnableToRun:
		if evidence.Command != "" {
			return fmt.Sprintf("%s could not be run.", evidence.Command)
		}
		if detail := strings.TrimSpace(evidence.Stderr); detail != "" {
			return detail
		}
		return "Verification could not be run."
	default:
		return strings.TrimSpace(evidence.Stderr)
	}
}

func agentCheckRecommendation(findings []contextpkg.Finding) string {
	var recommendations []string
	seen := map[string]struct{}{}
	for _, finding := range findings {
		recommendation := strings.TrimSpace(finding.Recommendation)
		if recommendation == "" {
			continue
		}
		if _, ok := seen[recommendation]; ok {
			continue
		}
		seen[recommendation] = struct{}{}
		recommendations = append(recommendations, recommendation)
	}
	return strings.Join(recommendations, "\n")
}
