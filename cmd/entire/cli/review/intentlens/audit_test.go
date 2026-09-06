package intentlens

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestExpectedFixturePassesValidationAndContainsEveryStatus(t *testing.T) {
	t.Parallel()
	audit, err := ParseAuditJSON(DemoAuditJSON())
	if err != nil {
		t.Fatalf("ParseAuditJSON: %v", err)
	}
	seen := map[Status]bool{}
	for _, requirement := range audit.Requirements {
		seen[requirement.Status] = true
	}
	for _, status := range []Status{StatusImplemented, StatusIncomplete, StatusUncertain} {
		if !seen[status] {
			t.Errorf("fixture does not contain %s", status)
		}
	}
	if !json.Valid(Schema()) {
		t.Fatal("embedded JSON Schema is not valid JSON")
	}
}

func TestInvalidAuditResultsFail(t *testing.T) {
	t.Parallel()
	base := `{"summary":"ok","requirements":[{"id":"R1","requirement":"behavior","status":"IMPLEMENTED","confidence":0.9,"evidence":[{"type":"code","explanation":"exists"},{"type":"test","explanation":"verified","result":"passed"}],"recommendation":""}]}`
	evidence := `[{"type":"code","explanation":"exists"},{"type":"test","explanation":"verified","result":"passed"}]`
	tests := map[string]string{
		"invalid status":           strings.Replace(base, "\"IMPLEMENTED\"", "\"DONE\"", 1),
		"confidence above one":     strings.Replace(base, "0.9", "1.01", 1),
		"empty evidence":           strings.Replace(base, evidence, "[]", 1),
		"unknown evidence type":    strings.Replace(base, "\"type\":\"code\"", "\"type\":\"database\"", 1),
		"misspelled evidence type": strings.Replace(base, "\"type\":\"code\"", "\"type\":\"git-diff\"", 1),
		"uppercase evidence type":  strings.Replace(base, "\"type\":\"code\"", "\"type\":\"CODE\"", 1),
		"empty evidence type":      strings.Replace(base, "\"type\":\"code\"", "\"type\":\"\"", 1),
		"malformed output":         "{\"summary\":",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseAuditJSON([]byte(input)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateSemanticsRejectsUnknownEvidenceTypeWithLocation(t *testing.T) {
	t.Parallel()
	audit := Audit{Summary: "summary", Requirements: []Requirement{{
		ID: "R7", Requirement: "behavior", Status: StatusUncertain, Confidence: 0.5,
		Evidence:       []Evidence{{Type: EvidenceType("Code"), Explanation: "unsupported type"}},
		Recommendation: "Collect valid evidence.",
	}}}
	err := ValidateSemantics(audit)
	if err == nil {
		t.Fatal("expected unknown evidence type to fail")
	}
	for _, want := range []string{"R7", "evidence[0]", `type "Code" is not allowed`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error %q missing %q", err, want)
		}
	}
}

func TestImplementedRequiresImplementationAndPassingTestEvidence(t *testing.T) {
	t.Parallel()
	audit := Audit{Summary: "summary", Requirements: []Requirement{{
		ID: "R1", Requirement: "behavior", Status: StatusImplemented, Confidence: 0.9,
		Evidence: []Evidence{{Type: EvidenceCode, Explanation: "exists"}}, Recommendation: "",
	}}}
	if err := ValidateSemantics(audit); err == nil {
		t.Fatal("expected missing verification evidence to fail")
	}
}

func TestNonImplementedRequiresRecommendation(t *testing.T) {
	t.Parallel()
	for _, status := range []Status{StatusIncomplete, StatusUncertain} {
		audit := Audit{Summary: "summary", Requirements: []Requirement{{
			ID: "R1", Requirement: "behavior", Status: status, Confidence: 0.5,
			Evidence: []Evidence{{Type: EvidenceCheckpoint, Explanation: "intent only"}},
		}}}
		if err := ValidateSemantics(audit); err == nil {
			t.Errorf("expected %s without recommendation to fail", status)
		}
	}
}

func TestEvidencePackageIsExplicitlySynthetic(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/evidence-package.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\"synthetic\": true") || !strings.Contains(string(data), "no live Gemini request") {
		t.Fatal("fixture must disclose its synthetic origin")
	}
}

func TestPromptsSeparateExtractionFromEvidenceEvaluation(t *testing.T) {
	t.Parallel()
	extraction := RequirementExtractionPrompt("Keep five retries.")
	for _, want := range []string{"Do not add unstated requirements", "do not evaluate implementation", "Return JSON only", "Keep five retries."} {
		if !strings.Contains(extraction, want) {
			t.Errorf("extraction prompt missing %q", want)
		}
	}
	evaluation := EvidenceEvaluationPrompt([]byte("{\"synthetic\":true}"))
	for _, want := range []string{"using only the supplied evidence package", "Confidence never replaces evidence", "Never invent files", "BEGIN JSON SCHEMA", "\"synthetic\":true"} {
		if !strings.Contains(evaluation, want) {
			t.Errorf("evaluation prompt missing %q", want)
		}
	}
}

func TestRenderStates(t *testing.T) {
	t.Parallel()
	var output strings.Builder
	Render(&output, ViewState{Loading: true})
	Render(&output, ViewState{})
	Render(&output, ViewState{Err: errors.New("bad data")})
	audit, err := ParseAuditJSON(DemoAuditJSON())
	if err != nil {
		t.Fatal(err)
	}
	Render(&output, ViewState{Audit: &audit, Demo: true})
	for _, want := range []string{"Loading audit result", "No audit result", "bad data", DemoNotice, "R1  IMPLEMENTED", "Recommendation:"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}
