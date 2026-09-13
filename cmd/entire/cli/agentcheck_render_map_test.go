package cli

import (
	"strings"
	"testing"

	contextpkg "github.com/entireio/cli/cmd/entire/cli/agentcheck"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/stretchr/testify/require"
)

func TestAgentCheckMapTrustedEvaluationToRender(t *testing.T) {
	cpID := id.MustCheckpointID("acac11112222")

	render := mapAgentCheckEvaluationToRender(cpID, contextpkg.EvaluationResult{
		Verdict: contextpkg.Verdict(contextpkg.VerdictTrusted),
		Summary: "All requested changes match the checkpoint evidence.",
	}, sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess))

	require.Equal(t, cpID.String(), render.CheckpointID)
	require.Equal(t, contextpkg.VerdictTrusted, render.Verdict)
	require.Nil(t, render.TrustScore)
	require.Equal(t, "All requested changes match the checkpoint evidence.", render.Summary)
	require.Empty(t, render.Findings)
	require.NotNil(t, render.Verification)
	require.Equal(t, agentCheckVerificationSuccess, render.Verification.Status)
}

func TestAgentCheckMapReviewRequiredFindingsAndEvidence(t *testing.T) {
	cpID := id.MustCheckpointID("acac11112222")

	render := mapAgentCheckEvaluationToRender(cpID, contextpkg.EvaluationResult{
		Verdict: contextpkg.Verdict(contextpkg.VerdictReviewRequired),
		Summary: "One boundary needs review.",
		Findings: []contextpkg.Finding{{
			Severity:       contextpkg.Severity(contextpkg.SeverityHigh),
			Title:          "Possible scope creep",
			Description:    "Billing code changed during an auth task.",
			Recommendation: "Review the billing change against the task boundary.",
			Evidence: []contextpkg.FindingEvidence{{
				Kind:      "file",
				Reference: "billing/invoice.go",
				Detail:    "Changed outside requested auth scope.",
			}},
		}},
	}, sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess))

	require.Equal(t, contextpkg.VerdictReviewRequired, render.Verdict)
	require.Len(t, render.Findings, 1)
	require.Equal(t, contextpkg.SeverityHigh, render.Findings[0].Severity)
	require.Equal(t, "Possible scope creep", render.Findings[0].Title)
	require.Equal(t, []string{"file: billing/invoice.go: Changed outside requested auth scope."}, render.Findings[0].Evidence)
	require.Equal(t, "Review the billing change against the task boundary.", render.Recommendation)
}

func TestAgentCheckMapFailCriticalFindings(t *testing.T) {
	cpID := id.MustCheckpointID("acac11112222")

	render := mapAgentCheckEvaluationToRender(cpID, contextpkg.EvaluationResult{
		Verdict: contextpkg.Verdict(contextpkg.VerdictFail),
		Findings: []contextpkg.Finding{{
			Severity:    contextpkg.Severity(contextpkg.SeverityCritical),
			Title:       "Prohibited schema change",
			Description: "A migration changed schema despite an explicit prohibition.",
		}},
	}, sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess))

	require.Equal(t, contextpkg.VerdictFail, render.Verdict)
	require.Len(t, render.Findings, 1)
	require.Equal(t, contextpkg.SeverityCritical, render.Findings[0].Severity)
	require.Equal(t, "Prohibited schema change", render.Findings[0].Title)
}

func TestAgentCheckFindingEvidenceFlatteningPreservesAvailableFields(t *testing.T) {
	evidence := flattenAgentCheckFindingEvidence([]contextpkg.FindingEvidence{
		{Kind: "file", Reference: "cmd/main.go", Detail: "added command"},
		{Reference: "README.md"},
		{Detail: "diff mentions setup"},
		{},
	})

	require.Equal(t, []string{
		"file: cmd/main.go: added command",
		"README.md",
		"diff mentions setup",
	}, evidence)
}

func TestAgentCheckVerificationFailureRemainsRenderEvidence(t *testing.T) {
	cpID := id.MustCheckpointID("acac11112222")
	verification := sampleAgentCheckVerificationEvidence(agentCheckVerificationFailed)

	render := mapAgentCheckEvaluationToRender(cpID, contextpkg.EvaluationResult{
		Verdict: contextpkg.Verdict(contextpkg.VerdictTrusted),
		Summary: "Evaluation had no deterministic findings.",
	}, verification)

	require.Equal(t, contextpkg.VerdictTrusted, render.Verdict)
	require.NotNil(t, render.Verification)
	require.Equal(t, agentCheckVerificationFailed, render.Verification.Status)
	require.Contains(t, render.Verification.Summary, "go test ./...")
	require.Contains(t, render.Verification.Summary, "code 1")
}

func TestAgentCheckRecommendationUsesOnlyFindingRecommendations(t *testing.T) {
	recommendation := agentCheckRecommendation([]contextpkg.Finding{
		{Recommendation: "Review the extra file."},
		{Recommendation: "Review the extra file."},
		{Recommendation: "Add evidence for the abstraction."},
		{},
	})

	require.Equal(t, "Review the extra file.\nAdd evidence for the abstraction.", recommendation)
	require.False(t, strings.Contains(recommendation, "TRUSTED"))
}
