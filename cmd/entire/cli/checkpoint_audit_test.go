package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review/intentlens"
	"github.com/spf13/cobra"
)

type checkpointAuditFakeTransport struct {
	called bool
	prompt string
}

func (f *checkpointAuditFakeTransport) Generate(_ context.Context, _ string, prompt string, _ json.RawMessage) ([]byte, error) {
	f.called = true
	f.prompt = prompt
	return nil, errors.New("unexpected provider request")
}
func checkpointAuditTestCommand(report AuditReport, transport *checkpointAuditFakeTransport) *cobra.Command {
	return newCheckpointAuditCmdWithDeps(checkpointAuditDeps{
		collect: func(_ *cobra.Command, target string, _ int, _ string) (AuditReport, error) {
			report.Intent.CheckpointID = target
			return report, nil
		},
		evaluator: intentlens.NewGeminiEvaluator(transport),
	})
}
func checkpointAuditReportFixture() AuditReport {
	return AuditReport{
		Intent:         IntentPacket{Prompts: []string{"Users can log in.\nLock login after five failures.\nPreserve existing sessions."}, DeclaredFilesTouched: []string{"auth/login.go"}},
		Implementation: ImplementationEvidence{LinkedCommits: []string{"abc123"}, ActualFilesTouched: []string{"auth/login.go"}, FocusedTests: []string{"auth/login_test.go"}},
	}
}
func executeAudit(t *testing.T, report AuditReport, args ...string) (string, *checkpointAuditFakeTransport, error) {
	t.Helper()
	transport := &checkpointAuditFakeTransport{}
	cmd := checkpointAuditTestCommand(report, transport)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), transport, err
}
func TestCheckpointAuditCommandWiresEvaluatorAndRendersDashboard(t *testing.T) {
	t.Parallel()
	out, transport, err := executeAudit(t, checkpointAuditReportFixture(), "checkpoint-123")
	if err != nil {
		t.Fatal(err)
	}
	if transport.called {
		t.Fatal("test file existence incorrectly triggered model evaluation")
	}
	for _, s := range []string{"IntentLens Audit", "R1  ? UNCERTAIN", "R2  ? UNCERTAIN", "R3  ? UNCERTAIN", "Requirements: 3", "Implemented: 0", "Checkpoint", "checkpoint-123"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q: %s", s, out)
		}
	}
}
func TestCheckpointAuditCommandIncompleteContextIsConservative(t *testing.T) {
	t.Parallel()
	report := checkpointAuditReportFixture()
	report.Intent.Prompts = nil
	out, transport, err := executeAudit(t, report, "checkpoint-123", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if transport.called {
		t.Fatal("incomplete context reached provider")
	}
	audit, err := intentlens.ParseAuditJSON([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Requirements) != 1 || audit.Requirements[0].Status != intentlens.StatusUncertain {
		t.Fatal(out)
	}
}
func TestCheckpointAuditCommandRawIntentSentinelDoesNotReachEvaluatorRequest(t *testing.T) {
	t.Parallel()
	report := checkpointAuditReportFixture()
	sentinel := "UNIQUE_UNRECOGNIZED_PRIVATE_TEXT_398fa"
	report.Intent.Prompts = []string{"Add login.\nBEGIN TRANSCRIPT\nUser: " + sentinel}
	report.Implementation.Diffs = map[string]string{"commit": "diff --git\n+" + sentinel}
	report.Implementation.GraphEvidence = json.RawMessage("{\"raw\":\"" + sentinel + "\"}")
	out, transport, err := executeAudit(t, report, "checkpoint-123", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if transport.called || strings.Contains(transport.prompt, sentinel) || strings.Contains(out, sentinel) {
		t.Fatal("raw sentinel escaped local collector")
	}
}
func TestCheckpointAuditCommandJSONOutputsValidatedAudit(t *testing.T) {
	t.Parallel()
	out, _, err := executeAudit(t, checkpointAuditReportFixture(), "checkpoint-123", "--json")
	if err != nil {
		t.Fatal(err)
	}
	audit, err := intentlens.ParseAuditJSON([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Requirements) != 3 {
		t.Fatal(out)
	}
	for _, r := range audit.Requirements {
		if r.Status != intentlens.StatusUncertain || r.Confidence > 0.25 {
			t.Fatal(out)
		}
	}
}
func TestCheckpointAuditCommandRequirementFilterShowsOnlySelectedDetails(t *testing.T) {
	t.Parallel()
	out, _, err := executeAudit(t, checkpointAuditReportFixture(), "checkpoint-123", "--requirement", "R2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Requirement Detail: R2") || strings.Contains(out, "Requirement Detail: R1") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "Evidence:") || !strings.Contains(out, "Recommendation:") {
		t.Fatal(out)
	}
	_, _, err = executeAudit(t, checkpointAuditReportFixture(), "checkpoint-123", "--requirement", "R99")
	if err == nil {
		t.Fatal("unknown requirement accepted")
	}
}
func TestCheckpointAuditCommandRegisteredWithJSONFlag(t *testing.T) {
	t.Parallel()
	cmd, _, err := newCheckpointGroupCmd().Find([]string{"audit"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Use != "audit <checkpoint-id>" || cmd.Flags().Lookup("json") == nil {
		t.Fatal("missing built-in command")
	}
}
func TestCheckpointAuditRedactionAndBoundsMarkContextIncomplete(t *testing.T) {
	t.Parallel()
	for _, prompts := range [][]string{{"Keep login.\nUser: private transcript"}, {strings.Repeat("word ", 100)}, {strings.Repeat("Behavior.\n", 30)}} {
		req, incomplete := sanitizedCheckpointAuditRequirements(prompts)
		if !incomplete || len(req) > 25 {
			t.Fatal("lost/redacted intent did not mark incomplete context")
		}
	}
}
func TestCheckpointAuditMissingGraphEvidencePreservesIntent(t *testing.T) {
	t.Parallel()
	report := checkpointAuditReportFixture()
	report.Implementation.Warnings = []string{"Entire Graph evidence unavailable"}
	evidence := buildIntentLensEvidencePackage(report)
	if len(evidence.Requirements) != 3 {
		t.Fatal("missing graph erased intent")
	}
	out, transport, err := executeAudit(t, report, "checkpoint-123")
	if err != nil || transport.called || !strings.Contains(out, "UNCERTAIN") {
		t.Fatalf("%s %v", out, err)
	}
}
