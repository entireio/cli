package agentcheck

import (
	"strings"
	"testing"
)

func TestEvaluateIntentBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ctx           Context
		wantVerdict   Verdict
		wantCategory  FindingCategory
		wantEvidence  []string
		wantNoFinding bool
	}{
		{
			name: "satisfied requirements are trusted",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Git:             GitEvidence{Diff: "func GoogleOAuthLogin() {}"},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "preserve requirement is evaluated",
			ctx: Context{
				DeveloperPrompt: "Preserve existing authentication.",
				ChangedFiles:    []FileChange{{Path: "docs/release-notes.md", Status: "M"}},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryRequirementMiss,
			wantEvidence: []string{"Preserve existing authentication", "docs/release-notes.md"},
		},
		{
			name: "requirement miss when available implementation evidence does not satisfy explicit requirement",
			ctx: Context{
				DeveloperPrompt: "Implement password reset email.",
				ChangedFiles:    []FileChange{{Path: "docs/release-notes.md", Status: "M"}},
				Git:             GitEvidence{DiffUnavailableReason: "diff too large"},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryRequirementMiss,
			wantEvidence: []string{"Implement password reset email", "docs/release-notes.md", "diff too large"},
		},
		{
			name: "explicit prohibited schema change fails",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.\nPreserve existing authentication.\nDo NOT modify the database schema.\nKeep the implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "migrations/004_google_oauth.sql", Status: "A"}},
			},
			wantVerdict:  Verdict(VerdictFail),
			wantCategory: CategoryBoundaryViolation,
			wantEvidence: []string{"Do NOT modify the database schema", "migrations/004_google_oauth.sql"},
		},
		{
			name: "scope creep flags unrelated file alongside in-scope work",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles: []FileChange{
					{Path: "internal/auth/google_oauth.go", Status: "A"},
					{Path: "internal/billing/invoice.go", Status: "M"},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryScopeCreep,
			wantEvidence: []string{"Implement Google OAuth login", "internal/billing/invoice.go"},
		},
		{
			name: "intent deviation from only scope",
			ctx: Context{
				DeveloperPrompt: "Only update documentation.",
				ChangedFiles:    []FileChange{{Path: "cmd/entire/cli/root.go", Status: "M"}},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryIntentDeviation,
			wantEvidence: []string{"Only update documentation", "cmd/entire/cli/root.go"},
		},
		{
			name: "scoped prompt intent participates in evaluation",
			ctx: Context{
				ScopedPrompts: []Prompt{{PromptIndex: 2, Text: "Do not edit tests."}},
				ChangedFiles:  []FileChange{{Path: "cmd/entire/cli/agentcheck/evaluate_test.go", Status: "M"}},
			},
			wantVerdict:  Verdict(VerdictFail),
			wantCategory: CategoryBoundaryViolation,
			wantEvidence: []string{"Do not edit tests", "ScopedPrompts[2]", "evaluate_test.go"},
		},
		{
			name: "permitted file change is not a violation",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "supporting auth file is not scope creep for oauth login",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles: []FileChange{
					{Path: "internal/auth/google_oauth.go", Status: "A"},
					{Path: "internal/auth/session.go", Status: "M"},
				},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "migration is allowed unless schema changes are prohibited",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "migrations/004_google_oauth.sql", Status: "A"}},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "unspecified requirement is not reported missing",
			ctx: Context{
				DeveloperPrompt: "Keep the implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/session.go", Status: "M"}},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "missing context does not fabricate evidence",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "files touched and git changed files are consumed",
			ctx: Context{
				DeveloperPrompt: "Do not edit tests.",
				FilesTouched:    []string{"README.md"},
				Git:             GitEvidence{ChangedFiles: []FileChange{{Path: "internal/foo/foo_test.go", Status: "M"}}},
			},
			wantVerdict:  Verdict(VerdictFail),
			wantCategory: CategoryBoundaryViolation,
			wantEvidence: []string{"internal/foo/foo_test.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := EvaluateIntentBoundary(tt.ctx)
			if got.Verdict != tt.wantVerdict {
				t.Fatalf("EvaluateIntentBoundary() verdict = %q, want %q; findings = %#v", got.Verdict, tt.wantVerdict, got.Findings)
			}
			if tt.wantNoFinding {
				if len(got.Findings) != 0 {
					t.Fatalf("EvaluateIntentBoundary() findings = %#v, want none", got.Findings)
				}
				return
			}
			finding := findCategory(got.Findings, tt.wantCategory)
			if finding == nil {
				t.Fatalf("EvaluateIntentBoundary() findings = %#v, want category %q", got.Findings, tt.wantCategory)
			}
			for _, want := range tt.wantEvidence {
				if !findingHasEvidence(*finding, want) {
					t.Fatalf("finding evidence = %#v, want detail containing %q", finding.Evidence, want)
				}
			}
		})
	}
}

func TestDetermineVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		findings []Finding
		want     Verdict
	}{
		{
			name: "no findings trusted",
			want: Verdict(VerdictTrusted),
		},
		{
			name: "requirement miss needs review",
			findings: []Finding{{
				Category: CategoryRequirementMiss,
				Severity: Severity(SeverityHigh),
			}},
			want: Verdict(VerdictReviewRequired),
		},
		{
			name: "scope creep needs review",
			findings: []Finding{{
				Category: CategoryScopeCreep,
				Severity: Severity(SeverityMedium),
			}},
			want: Verdict(VerdictReviewRequired),
		},
		{
			name: "critical boundary violation fails",
			findings: []Finding{{
				Category: CategoryBoundaryViolation,
				Severity: Severity(SeverityCritical),
			}},
			want: Verdict(VerdictFail),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := DetermineVerdict(tt.findings); got != tt.want {
				t.Fatalf("DetermineVerdict() = %q, want %q", got, tt.want)
			}
		})
	}
}

func findCategory(findings []Finding, category FindingCategory) *Finding {
	for i := range findings {
		if findings[i].Category == category {
			return &findings[i]
		}
	}
	return nil
}

func findingHasEvidence(finding Finding, want string) bool {
	for _, evidence := range finding.Evidence {
		if strings.Contains(evidence.Kind, want) || strings.Contains(evidence.Reference, want) || strings.Contains(evidence.Detail, want) {
			return true
		}
	}
	return false
}
