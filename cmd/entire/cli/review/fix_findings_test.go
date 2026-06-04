package review

import (
	"strings"
	"testing"
)

func TestReviewFindingTitle_RecognisesRealSeverityFormats(t *testing.T) {
	t.Parallel()

	matches := []string{
		"- **Medium** [cmd.go](/x/cmd.go:1): desc", // codex bolded-severity bullet
		"**Low** [f](/x): x",
		"Medium: something is wrong",
		"High - something",
		"Critical. something",
		"Blocker: data loss",
		"H1. severity-numbered title",
	}
	for _, line := range matches {
		if _, ok := reviewFindingTitle(line); !ok {
			t.Errorf("expected a finding title for %q", line)
		}
	}

	nonMatches := []string{
		"Lower the timeout to 5s", // "low" is a prefix of a real word
		"highlight the regression",
		"**Findings**",
		"No tests run; review only.",
		"- just a bullet of prose with no severity",
		"",
	}
	for _, line := range nonMatches {
		if _, ok := reviewFindingTitle(line); ok {
			t.Errorf("did not expect a finding title for %q", line)
		}
	}
}

// TestExtractSourceFindings_CodexBulletFormat pins parsing against a real codex
// `$code-reviewer` output (bolded-severity bullets) — the format that the
// previous matcher silently collapsed to a single full-output finding, breaking
// the [s] select-findings picker.
func TestExtractSourceFindings_CodexBulletFormat(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"**Findings**",
		"- **Medium** [cmd.go](/x/cmd.go:571): runMultiAgentPath skips validation.",
		"",
		"- **Low** [post_review.go](/x/post_review.go:119): footer points to a wrong command.",
		"",
		"- **Low** [t1.txt](/x/t1.txt:1): stray test artifact, remove it.",
		"",
		"No tests run; review only.",
	}, "\n")

	findings := extractSourceFindings(reviewFixSource{Kind: reviewFixSourceAgent, Label: "codex", Output: output}, 0)
	if len(findings) != 3 {
		t.Fatalf("findings = %d, want 3 (one per severity bullet):\n%+v", len(findings), findings)
	}
	// Each finding carries its own body so a [s] selection scopes the fix prompt.
	for i, f := range findings {
		if strings.TrimSpace(f.Body) == "" {
			t.Errorf("finding %d has empty body", i)
		}
	}
}
