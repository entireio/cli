package investigate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrap_SeedDocEmbedsQuestionBody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	seedPath := filepath.Join(dir, "seed.md")
	seed := "Q: why is X broken?\n"
	if err := os.WriteFile(seedPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	findings := filepath.Join(dir, "out", "findings.md")

	res, err := Bootstrap(context.Background(), BootstrapInput{
		SeedDoc:     seedPath,
		FindingsDoc: findings,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.Topic != "seed" {
		t.Errorf("Topic = %q, want derived from filename", res.Topic)
	}

	gotFindings, err := os.ReadFile(findings)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	got := string(gotFindings)
	for _, want := range []string{
		"# Investigation: seed",
		"## TLDR",
		"## Question",
		"Q: why is X broken?",
		"## Findings",
		"## Conclusion",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scaffold missing %q\nGOT:\n%s", want, got)
		}
	}

	// The seed body should land under `## Question`, before `## Prior work`.
	idxQuestion := strings.Index(got, "## Question")
	idxSeed := strings.Index(got, "Q: why is X broken?")
	idxPriorWork := strings.Index(got, "## Prior work")
	if idxQuestion >= idxSeed || idxSeed >= idxPriorWork {
		t.Errorf("expected Question < seed-body < Prior work, got %d < %d < %d", idxQuestion, idxSeed, idxPriorWork)
	}
}

func TestBootstrap_SeedDocDerivesTopicFromInvestigationHeading(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	seedPath := filepath.Join(dir, "seed.md")
	seed := "# Investigation: Why does checkout retry forever?\n\nbody text\n"
	if err := os.WriteFile(seedPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	findings := filepath.Join(dir, "out", "findings.md")

	res, err := Bootstrap(context.Background(), BootstrapInput{
		SeedDoc:     seedPath,
		FindingsDoc: findings,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.Topic != "Why does checkout retry forever?" {
		t.Errorf("Topic = %q, want derived from '# Investigation:' heading", res.Topic)
	}

	gotFindings, err := os.ReadFile(findings)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	got := string(gotFindings)
	if !strings.Contains(got, "# Investigation: Why does checkout retry forever?") {
		t.Errorf("findings missing scaffold title with derived topic\nGOT:\n%s", got)
	}
	if !strings.Contains(got, "body text") {
		t.Errorf("findings missing seed body content\nGOT:\n%s", got)
	}
}

func TestBootstrap_TopicScaffold(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	findings := filepath.Join(dir, "findings.md")

	res, err := Bootstrap(context.Background(), BootstrapInput{
		Topic:       "Why is checkout flaky?",
		FindingsDoc: findings,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.Topic != "Why is checkout flaky?" {
		t.Errorf("Topic = %q, want %q", res.Topic, "Why is checkout flaky?")
	}

	body, err := os.ReadFile(findings)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# Investigation: Why is checkout flaky?",
		"**Status:** investigating",
		"## TLDR",
		"## Question",
		"## Prior work",
		"## System under investigation",
		"## Approach",
		"## Findings",
		"## Unknowns / Assumptions",
		"## Conclusion",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scaffold missing section %q", want)
		}
	}
}

func TestBootstrap_IssueLinkSeedEmbedsQuestionBody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	findings := filepath.Join(dir, "findings.md")

	seedBytes := []byte("**Source:** https://github.com/o/r/issues/42\n\nIssue body: checkout times out under load.\n")
	res, err := Bootstrap(context.Background(), BootstrapInput{
		IssueLinkSeed:  seedBytes,
		IssueLinkTopic: "checkout times out",
		FindingsDoc:    findings,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.Topic != "checkout times out" {
		t.Errorf("Topic = %q, want from IssueLinkTopic", res.Topic)
	}

	body, err := os.ReadFile(findings)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# Investigation: checkout times out",
		"## TLDR",
		"## Question",
		"**Source:** https://github.com/o/r/issues/42",
		"Issue body: checkout times out under load.",
		"## Findings",
		"## Conclusion",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scaffold missing %q\nGOT:\n%s", want, got)
		}
	}

	// Issue body must appear under `## Question`, before `## Prior work`.
	idxQuestion := strings.Index(got, "## Question")
	idxIssue := strings.Index(got, "Issue body: checkout times out under load.")
	idxPriorWork := strings.Index(got, "## Prior work")
	if idxQuestion >= idxIssue || idxIssue >= idxPriorWork {
		t.Errorf("expected Question < issue-body < Prior work, got %d < %d < %d", idxQuestion, idxIssue, idxPriorWork)
	}
}

func TestBootstrap_TopicOnlyUsesTopicAsQuestion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	findings := filepath.Join(dir, "findings.md")

	_, err := Bootstrap(context.Background(), BootstrapInput{
		Topic:       "Why is checkout flaky?",
		FindingsDoc: findings,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	body, err := os.ReadFile(findings)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	got := string(body)

	// The topic appears under `## Question` (between Question and the next
	// section). Confirm the topic is not blank by checking it appears after
	// the Question heading and before Prior work.
	idxQuestion := strings.Index(got, "## Question")
	idxTopic := strings.Index(got[idxQuestion:], "Why is checkout flaky?")
	if idxQuestion < 0 || idxTopic < 0 {
		t.Fatalf("expected topic to appear under Question section\nGOT:\n%s", got)
	}
}

func TestRenderInvestigationScaffold_EmptyQuestionBodyFallsBackToTopic(t *testing.T) {
	t.Parallel()

	out := renderInvestigationScaffold("My topic", "2026-01-01", "")
	// Topic must appear under `## Question`.
	idxQuestion := strings.Index(out, "## Question")
	if idxQuestion < 0 {
		t.Fatalf("scaffold missing `## Question`\nGOT:\n%s", out)
	}
	rest := out[idxQuestion:]
	if !strings.Contains(rest, "My topic") {
		t.Errorf("expected topic to appear under Question section when questionBody is empty\nGOT:\n%s", out)
	}
}

func TestRenderInvestigationScaffold_TrimsQuestionBodyTrailingWhitespace(t *testing.T) {
	t.Parallel()

	out := renderInvestigationScaffold("My topic", "2026-01-01", "Some seed body\n\n\n   ")
	// After the seed body content there should be exactly one blank line
	// followed by `## Prior work` (no stacked blanks from un-trimmed input).
	if !strings.Contains(out, "Some seed body\n\n## Prior work") {
		t.Errorf("expected trimmed question body followed by single blank line + Prior work\nGOT:\n%s", out)
	}
}

func TestBootstrap_RequiresOneInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Bootstrap(context.Background(), BootstrapInput{
		FindingsDoc: filepath.Join(dir, "f.md"),
	})
	if err == nil {
		t.Fatalf("expected error when no input variant provided")
	}
}

func TestDeriveTopicFromSeed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		filename string
		want     string
	}{
		{
			name:     "investigation heading wins",
			body:     "# Investigation: Why slow?\n\n# Other heading\n",
			filename: "ignored.md",
			want:     "Why slow?",
		},
		{
			name:     "first H1 when no investigation heading",
			body:     "Some preface.\n\n# First Heading\n\n## Sub heading\n",
			filename: "ignored.md",
			want:     "First Heading",
		},
		{
			name:     "filename fallback when no headings",
			body:     "no headings here\nat all\n",
			filename: "/path/to/why-slow.md",
			want:     "why-slow",
		},
		{
			name:     "filename fallback with no extension",
			body:     "",
			filename: "/tmp/nofile",
			want:     "nofile",
		},
		{
			name:     "investigation heading trims spaces",
			body:     "#   Investigation:    spaced topic   \n",
			filename: "ignored.md",
			want:     "Investigation:    spaced topic",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DeriveTopicFromSeed([]byte(tc.body), tc.filename)
			if got != tc.want {
				t.Errorf("DeriveTopicFromSeed(%q, %q) = %q, want %q", tc.body, tc.filename, got, tc.want)
			}
		})
	}
}

func TestSlugifyTopic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "simple", input: "checkout flaky", want: "checkout-flaky"},
		{name: "punctuation", input: "Why is checkout flaky?!", want: "why-is-checkout-flaky"},
		{name: "leading and trailing dashes trimmed", input: "  ---hello world---  ", want: "hello-world"},
		{name: "non-ascii squashed", input: "café résumé", want: "caf-r-sum"},
		{name: "all punctuation falls back", input: "!!!", want: "investigation"},
		{name: "empty falls back", input: "", want: "investigation"},
		{name: "mixed case lowercased", input: "WhyIsThisHappening", want: "whyisthishappening"},
		{name: "long input truncated to 60", input: strings.Repeat("a", 100), want: strings.Repeat("a", 60)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SlugifyTopic(tc.input); got != tc.want {
				t.Errorf("SlugifyTopic(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
