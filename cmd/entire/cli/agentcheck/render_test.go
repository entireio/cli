package agentcheck

import (
	"bytes"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	t.Parallel()

	score := 61
	tests := []struct {
		name    string
		result  RenderResult
		want    []string
		wantNot []string
	}{
		{
			name: "trusted result with no findings",
			result: RenderResult{
				CheckpointID:   "abc123",
				Verdict:        VerdictTrusted,
				Summary:        "All requested changes match the checkpoint evidence.",
				Recommendation: "Proceed.",
			},
			want: []string{
				"AgentCheck",
				"Checkpoint: abc123",
				"Verdict:    TRUSTED",
				"Summary:",
				"All requested changes match the checkpoint evidence.",
				"Recommended action:",
				"Proceed.",
			},
			wantNot: []string{"Findings:", "Verification:"},
		},
		{
			name: "review required with one important finding",
			result: RenderResult{
				CheckpointID: "def456",
				Verdict:      VerdictReviewRequired,
				Summary:      "One boundary needs human review.",
				Findings: []RenderFinding{{
					Severity:    SeverityHigh,
					Title:       "Possible scope creep",
					Description: "The task was scoped to auth but billing code changed.",
					Evidence:    []string{"billing/invoice.go"},
				}},
				Verification: &RenderVerification{Status: "not run", Summary: "Verification was unavailable."},
			},
			want: []string{
				"Verdict:    REVIEW REQUIRED",
				"HIGH  Possible scope creep",
				"The task was scoped to auth but billing code changed.",
				"Evidence: billing/invoice.go",
				"Verification:",
				"not run - Verification was unavailable.",
			},
		},
		{
			name: "fail with critical finding",
			result: RenderResult{
				CheckpointID: "fed789",
				Verdict:      VerdictFail,
				Findings: []RenderFinding{{
					Severity:    SeverityCritical,
					Title:       "Prohibited schema change",
					Description: "The developer explicitly prohibited database schema edits.",
					Evidence:    []string{"migrations/004_google_oauth.sql"},
				}},
			},
			want: []string{
				"Verdict:    FAIL",
				"CRITICAL  Prohibited schema change",
				"Evidence: migrations/004_google_oauth.sql",
			},
		},
		{
			name: "multiple findings sorted by severity",
			result: RenderResult{
				Verdict: VerdictReviewRequired,
				Findings: []RenderFinding{
					{Severity: SeverityLow, Title: "Low item"},
					{Severity: SeverityCritical, Title: "Critical item"},
					{Severity: SeverityMedium, Title: "Medium item"},
					{Severity: SeverityHigh, Title: "High item"},
				},
			},
			want: []string{"CRITICAL  Critical item", "HIGH  High item", "MEDIUM  Medium item", "LOW  Low item"},
		},
		{
			name: "missing verification omitted",
			result: RenderResult{
				Verdict: VerdictTrusted,
				Summary: "No verification section should render.",
			},
			want:    []string{"Verdict:    TRUSTED"},
			wantNot: []string{"Verification:"},
		},
		{
			name: "missing evidence omitted",
			result: RenderResult{
				Verdict: VerdictReviewRequired,
				Findings: []RenderFinding{{
					Severity:    SeverityMedium,
					Title:       "Needs review",
					Description: "The finding has no supplied evidence reference.",
				}},
			},
			want:    []string{"MEDIUM  Needs review", "The finding has no supplied evidence reference."},
			wantNot: []string{"Evidence:"},
		},
		{
			name: "empty summary and recommendation omitted",
			result: RenderResult{
				Verdict: VerdictTrusted,
			},
			want:    []string{"Verdict:    TRUSTED"},
			wantNot: []string{"Summary:", "Recommended action:"},
		},
		{
			name: "score is secondary when present",
			result: RenderResult{
				Verdict:    VerdictReviewRequired,
				TrustScore: &score,
			},
			want: []string{"Verdict:    REVIEW REQUIRED", "Trust Score: 61/100"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			if err := Render(&buf, tt.result); err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			got := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("Render() missing %q in:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.wantNot {
				if strings.Contains(got, unwanted) {
					t.Errorf("Render() included %q in:\n%s", unwanted, got)
				}
			}
			if tt.name == "multiple findings sorted by severity" {
				assertOrdered(t, got, tt.want)
			}
		})
	}
}

func assertOrdered(t *testing.T, got string, parts []string) {
	t.Helper()
	last := -1
	for _, part := range parts {
		idx := strings.Index(got, part)
		if idx < 0 {
			t.Fatalf("output missing %q in:\n%s", part, got)
		}
		if idx < last {
			t.Fatalf("%q appeared before an earlier severity in:\n%s", part, got)
		}
		last = idx
	}
}

func TestRenderSupportedVerdicts(t *testing.T) {
	t.Parallel()

	for _, verdict := range []string{VerdictTrusted, VerdictReviewRequired, VerdictFail} {
		t.Run(verdict, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			if err := Render(&buf, RenderResult{Verdict: verdict}); err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if got := buf.String(); !strings.Contains(got, "Verdict:    "+verdict) {
				t.Fatalf("Render() output = %q, want verdict %q", got, verdict)
			}
		})
	}
}

func TestRenderUnsupportedVerdict(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := Render(&buf, RenderResult{Verdict: "MAYBE"})
	if err == nil {
		t.Fatal("Render() error = nil, want unsupported verdict error")
	}
	if !strings.Contains(err.Error(), `unsupported AgentCheck verdict "MAYBE"`) {
		t.Fatalf("Render() error = %q", err)
	}
}
