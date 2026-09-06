package agentcheck

import (
	"reflect"
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
			name: "low severity material finding needs review",
			findings: []Finding{{
				Category: CategoryUnnecessaryFile,
				Severity: Severity(SeverityLow),
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

func TestEvaluateCodeQualityBloat(t *testing.T) {
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
			name: "simple necessary implementation has no bloat finding",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login. Keep the implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Git:             GitEvidence{Diff: "+func GoogleOAuthLogin() error { return nil }"},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "unnecessary abstraction for minimal task",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login. Keep the implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Git:             GitEvidence{Diff: "+type OAuthProvider interface { Login() error }"},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "abstraction",
						Paths:  []string{"internal/auth/google_oauth.go"},
						Detail: "current change introduced unnecessary abstraction with no reuse",
					}},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryUnnecessaryAbstraction,
			wantEvidence: []string{"Keep the implementation minimal", "OAuthProvider interface", "no reuse"},
		},
		{
			name: "duplicate implementation from graph evidence",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "duplicate",
						Paths:  []string{"internal/auth/google_oauth.go", "internal/auth/oauth.go"},
						Detail: "new GoogleOAuth helper duplicates existing OAuth helper",
					}},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryDuplication,
			wantEvidence: []string{"duplicates existing OAuth helper", "internal/auth/oauth.go"},
		},
		{
			name: "reinvented repository utility from graph evidence",
			ctx: Context{
				DeveloperPrompt: "Normalize checkpoint paths.",
				ChangedFiles:    []FileChange{{Path: "cmd/entire/cli/agentcheck/path.go", Status: "A"}},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "utility",
						Paths:  []string{"cmd/entire/cli/agentcheck/path.go", "cmd/entire/cli/paths"},
						Detail: "new helper reinvents existing utility paths.WorktreeRoot",
					}},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryReinventedRepositoryUtil,
			wantEvidence: []string{"reinvents existing utility", "cmd/entire/cli/paths"},
		},
		{
			name: "unnecessary dependency when minimal task evidence supports it",
			ctx: Context{
				DeveloperPrompt: "Implement title trim. Keep implementation minimal.",
				ChangedFiles: []FileChange{
					{Path: "go.mod", Status: "M"},
					{Path: "internal/title/trim.go", Status: "A"},
				},
				Git: GitEvidence{Diff: "+\tgithub.com/samber/lo v1.0.0"},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "dependency",
						Paths:  []string{"go.mod"},
						Detail: "current change introduced unused dependency github.com/samber/lo",
					}},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryUnnecessaryDependency,
			wantEvidence: []string{"github.com/samber/lo", "go.mod", "unused dependency"},
		},
		{
			name: "unnecessary file outside minimal task",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login. Keep implementation minimal.",
				ChangedFiles: []FileChange{
					{Path: "internal/auth/google_oauth.go", Status: "A"},
					{Path: "examples/playground.go", Status: "A"},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryUnnecessaryFile,
			wantEvidence: []string{"Implement Google OAuth login", "examples/playground.go"},
		},
		{
			name: "unrelated refactor outside task scope",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles: []FileChange{
					{Path: "internal/auth/google_oauth.go", Status: "A"},
					{Path: "internal/billing/invoice.go", Status: "M"},
				},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "refactor",
						Paths:  []string{"internal/billing/invoice.go"},
						Detail: "current change introduced unrelated refactor in billing",
					}},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryUnrelatedRefactor,
			wantEvidence: []string{"Implement Google OAuth login", "internal/billing/invoice.go", "unrelated refactor"},
		},
		{
			name: "dead code from graph evidence",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "dead_code",
						Paths:  []string{"internal/auth/google_oauth.go"},
						Detail: "new function unused by any call path",
					}},
				},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryDeadCode,
			wantEvidence: []string{"unused by any call path", "internal/auth/google_oauth.go"},
		},
		{
			name: "disproportionate complexity for minimal task",
			ctx: Context{
				DeveloperPrompt: "Implement title trim. Keep it simple.",
				ChangedFiles:    []FileChange{{Path: "internal/title/trim.go", Status: "A"}},
				Git: GitEvidence{Diff: strings.Join([]string{
					"+type TrimProvider interface { Trim(string) string }",
					"+type TrimFactory struct{}",
					"+var trimRegistry = map[string]TrimProvider{}",
				}, "\n")},
			},
			wantVerdict:  Verdict(VerdictReviewRequired),
			wantCategory: CategoryDisproportionateComplexity,
			wantEvidence: []string{"Keep it simple", "TrimProvider interface"},
		},
		{
			name: "line count alone does not create finding",
			ctx: Context{
				DeveloperPrompt: "Implement generated lookup table. Keep implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "internal/lookup/table.go", Status: "A"}},
				Git:             GitEvidence{Diff: largePlainDiff()},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "new file is not inherently unnecessary",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "new dependency is not inherently unnecessary",
			ctx: Context{
				DeveloperPrompt: "Implement OAuth login. Keep implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "go.mod", Status: "M"}},
				Git:             GitEvidence{Diff: "+\tgithub.com/coreos/go-oidc/v3 v3.0.0"},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "new abstraction is not inherently unnecessary",
			ctx: Context{
				DeveloperPrompt: "Support multiple OAuth providers.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/oauth_provider.go", Status: "A"}},
				Git:             GitEvidence{Diff: "+type OAuthProvider interface { Login() error }"},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "minimal task with abstraction but insufficient evidence is not enough",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login. Keep implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/oauth_provider.go", Status: "A"}},
				Git:             GitEvidence{Diff: "+type OAuthProvider interface { Login() error }"},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "required refactor is not unrelated",
			ctx: Context{
				DeveloperPrompt: "Refactor authentication for Google OAuth login.",
				ChangedFiles: []FileChange{
					{Path: "internal/auth/google_oauth.go", Status: "A"},
					{Path: "internal/auth/session.go", Status: "M"},
				},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "modified out of scope file without graph support is not unrelated refactor",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles: []FileChange{
					{Path: "internal/auth/google_oauth.go", Status: "A"},
					{Path: "internal/billing/invoice.go", Status: "M"},
				},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "existing repository complexity is not blamed",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "complexity",
						Paths:  []string{"internal/legacy/auth.go"},
						Detail: "pre-existing auth registry is overly complex",
					}},
				},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "missing evidence does not fabricate quality finding",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login. Keep implementation minimal.",
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
		{
			name: "graph unavailable still works",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login. Keep implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Graph:           GraphContext{Available: false, UnavailableReason: "graph disabled"},
			},
			wantVerdict:   Verdict(VerdictTrusted),
			wantNoFinding: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := EvaluateCodeQualityBloat(tt.ctx)
			if got.Verdict != tt.wantVerdict {
				t.Fatalf("EvaluateCodeQualityBloat() verdict = %q, want %q; findings = %#v", got.Verdict, tt.wantVerdict, got.Findings)
			}
			if tt.wantNoFinding {
				if len(got.Findings) != 0 {
					t.Fatalf("EvaluateCodeQualityBloat() findings = %#v, want none", got.Findings)
				}
				return
			}
			finding := findCategory(got.Findings, tt.wantCategory)
			if finding == nil {
				t.Fatalf("EvaluateCodeQualityBloat() findings = %#v, want category %q", got.Findings, tt.wantCategory)
			}
			for _, want := range tt.wantEvidence {
				if !findingHasEvidence(*finding, want) {
					t.Fatalf("finding evidence = %#v, want detail containing %q", finding.Evidence, want)
				}
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		ctx               Context
		wantVerdict       Verdict
		wantCategories    []FindingCategory
		wantIntentStatus  string
		wantQualityStatus string
		wantEvidence      []string
	}{
		{
			name:              "no findings trusted",
			ctx:               Context{DeveloperPrompt: "Implement Google OAuth login.", ChangedFiles: []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}}},
			wantVerdict:       Verdict(VerdictTrusted),
			wantCategories:    []FindingCategory{},
			wantIntentStatus:  "clear",
			wantQualityStatus: "clear",
		},
		{
			name: "material quality finding needs review",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles:    []FileChange{{Path: "internal/auth/google_oauth.go", Status: "A"}},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "duplicate",
						Paths:  []string{"internal/auth/google_oauth.go", "internal/auth/oauth.go"},
						Detail: "new GoogleOAuth helper duplicates existing OAuth helper",
					}},
				},
			},
			wantVerdict:       Verdict(VerdictReviewRequired),
			wantCategories:    []FindingCategory{CategoryDuplication},
			wantIntentStatus:  "clear",
			wantQualityStatus: "needs_review",
			wantEvidence:      []string{"duplicates existing OAuth helper", "internal/auth/oauth.go"},
		},
		{
			name: "major requirement miss needs review",
			ctx: Context{
				DeveloperPrompt: "Implement password reset email.",
				ChangedFiles:    []FileChange{{Path: "docs/release-notes.md", Status: "M"}},
			},
			wantVerdict:       Verdict(VerdictReviewRequired),
			wantCategories:    []FindingCategory{CategoryRequirementMiss},
			wantIntentStatus:  "needs_review",
			wantQualityStatus: "clear",
			wantEvidence:      []string{"Implement password reset email", "docs/release-notes.md"},
		},
		{
			name: "critical boundary violation fails",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.\nPreserve existing authentication.\nDo NOT modify the database schema.\nKeep the implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "migrations/004_google_oauth.sql", Status: "A"}},
			},
			wantVerdict:       Verdict(VerdictFail),
			wantCategories:    []FindingCategory{CategoryBoundaryViolation},
			wantIntentStatus:  "failed",
			wantQualityStatus: "clear",
			wantEvidence:      []string{"Do NOT modify the database schema", "migrations/004_google_oauth.sql"},
		},
		{
			name: "critical boundary violation plus quality remains fail",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.\nDo NOT modify the database schema.\nKeep the implementation minimal.",
				ChangedFiles:    []FileChange{{Path: "migrations/004_google_oauth.sql", Status: "A"}},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "duplicate",
						Paths:  []string{"migrations/004_google_oauth.sql", "migrations/001_auth.sql"},
						Detail: "current change duplicates existing migration helper",
					}},
				},
			},
			wantVerdict:       Verdict(VerdictFail),
			wantCategories:    []FindingCategory{CategoryBoundaryViolation, CategoryDuplication},
			wantIntentStatus:  "failed",
			wantQualityStatus: "needs_review",
			wantEvidence:      []string{"Do NOT modify the database schema", "duplicates existing migration helper"},
		},
		{
			name: "b1 and b2 findings aggregate into one result",
			ctx: Context{
				DeveloperPrompt: "Implement Google OAuth login.",
				ChangedFiles: []FileChange{
					{Path: "internal/auth/google_oauth.go", Status: "A"},
					{Path: "internal/billing/invoice.go", Status: "M"},
				},
				Graph: GraphContext{
					Available: true,
					Evidence: []GraphEvidence{{
						Kind:   "refactor",
						Paths:  []string{"internal/billing/invoice.go"},
						Detail: "current change introduced unrelated refactor in billing",
					}},
				},
			},
			wantVerdict:       Verdict(VerdictReviewRequired),
			wantCategories:    []FindingCategory{CategoryScopeCreep, CategoryUnrelatedRefactor},
			wantIntentStatus:  "needs_review",
			wantQualityStatus: "needs_review",
			wantEvidence:      []string{"internal/billing/invoice.go", "unrelated refactor"},
		},
		{
			name:              "empty context is safe",
			ctx:               Context{},
			wantVerdict:       Verdict(VerdictTrusted),
			wantCategories:    []FindingCategory{},
			wantIntentStatus:  "clear",
			wantQualityStatus: "clear",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Evaluate(tt.ctx)
			if got.Verdict != tt.wantVerdict {
				t.Fatalf("Evaluate() verdict = %q, want %q; findings = %#v", got.Verdict, tt.wantVerdict, got.Findings)
			}
			if got.IntentAssessment.Status != tt.wantIntentStatus {
				t.Fatalf("Evaluate() intent status = %q, want %q", got.IntentAssessment.Status, tt.wantIntentStatus)
			}
			if got.QualityAssessment.Status != tt.wantQualityStatus {
				t.Fatalf("Evaluate() quality status = %q, want %q", got.QualityAssessment.Status, tt.wantQualityStatus)
			}
			if gotCategories := findingCategories(got.Findings); !reflect.DeepEqual(gotCategories, tt.wantCategories) {
				t.Fatalf("Evaluate() categories = %#v, want %#v; findings = %#v", gotCategories, tt.wantCategories, got.Findings)
			}
			for _, want := range tt.wantEvidence {
				if !resultHasEvidence(got, want) {
					t.Fatalf("Evaluate() findings = %#v, want evidence containing %q", got.Findings, want)
				}
			}
		})
	}
}

func TestEvaluateOrdersFindingsDeterministically(t *testing.T) {
	t.Parallel()

	ctx := Context{
		DeveloperPrompt: "Only update documentation. Keep it simple.",
		ChangedFiles: []FileChange{
			{Path: "internal/title/trim.go", Status: "A"},
		},
		Git: GitEvidence{Diff: strings.Join([]string{
			"+type TrimProvider interface { Trim(string) string }",
			"+type TrimFactory struct{}",
			"+var trimRegistry = map[string]TrimProvider{}",
		}, "\n")},
		Graph: GraphContext{
			Available: true,
			Evidence: []GraphEvidence{
				{Kind: "utility", Paths: []string{"internal/title/trim.go", "cmd/entire/cli/textutil"}, Detail: "new helper reinvents existing utility textutil.Trim"},
				{Kind: "dead_code", Paths: []string{"internal/title/trim.go"}, Detail: "current change introduced unused helper"},
				{Kind: "duplicate", Paths: []string{"internal/title/trim.go", "cmd/entire/cli/textutil"}, Detail: "current change duplicates existing trim helper"},
			},
		},
	}

	got := Evaluate(ctx)
	want := []FindingCategory{
		CategoryDeadCode,
		CategoryDisproportionateComplexity,
		CategoryDuplication,
		CategoryIntentDeviation,
		CategoryReinventedRepositoryUtil,
	}
	if gotCategories := findingCategories(got.Findings); !reflect.DeepEqual(gotCategories, want) {
		t.Fatalf("Evaluate() categories = %#v, want %#v; findings = %#v", gotCategories, want, got.Findings)
	}
}

func TestEvaluateDoesNotFabricateEvidence(t *testing.T) {
	t.Parallel()

	got := Evaluate(Context{DeveloperPrompt: "Implement Google OAuth login."})
	if got.Verdict != Verdict(VerdictTrusted) {
		t.Fatalf("Evaluate() verdict = %q, want TRUSTED", got.Verdict)
	}
	if len(got.Findings) != 0 {
		t.Fatalf("Evaluate() findings = %#v, want none", got.Findings)
	}
	if resultHasEvidence(got, "checkpoint") || resultHasEvidence(got, "verification") || resultHasEvidence(got, "graph") {
		t.Fatalf("Evaluate() fabricated evidence in %#v", got.Findings)
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	t.Parallel()

	ctx := Context{
		DeveloperPrompt: "Implement Google OAuth login.\nDo NOT modify the database schema.\nKeep implementation minimal.",
		ChangedFiles:    []FileChange{{Path: "migrations/004_google_oauth.sql", Status: "A"}},
		Graph: GraphContext{
			Available: true,
			Evidence: []GraphEvidence{{
				Kind:   "duplicate",
				Paths:  []string{"migrations/004_google_oauth.sql", "migrations/001_auth.sql"},
				Detail: "current change duplicates existing migration helper",
			}},
		},
	}

	first := Evaluate(ctx)
	second := Evaluate(ctx)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Evaluate() not deterministic:\nfirst:  %#v\nsecond: %#v", first, second)
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

func findingCategories(findings []Finding) []FindingCategory {
	categories := make([]FindingCategory, 0, len(findings))
	for _, finding := range findings {
		categories = append(categories, finding.Category)
	}
	return categories
}

func resultHasEvidence(result EvaluationResult, want string) bool {
	for _, finding := range result.Findings {
		if findingHasEvidence(finding, want) {
			return true
		}
	}
	return false
}

func findingHasEvidence(finding Finding, want string) bool {
	for _, evidence := range finding.Evidence {
		if strings.Contains(evidence.Kind, want) || strings.Contains(evidence.Reference, want) || strings.Contains(evidence.Detail, want) {
			return true
		}
	}
	return false
}

func largePlainDiff() string {
	lines := make([]string, 0, 80)
	for i := 0; i < 80; i++ {
		lines = append(lines, "+lookupEntry")
	}
	return strings.Join(lines, "\n")
}
