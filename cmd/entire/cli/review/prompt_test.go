package review

import (
	"strings"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

func TestComposeReviewPrompt_SkillsOnly(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills: []string{"/skill-a", "/skill-b"},
	}
	got := ComposeReviewPrompt(cfg)
	want := "/skill-a\n/skill-b"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_SkillsPlusAlwaysPrompt(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/skill-a", "/skill-b"},
		AlwaysPrompt: "be thorough",
	}
	got := ComposeReviewPrompt(cfg)
	want := "/skill-a\n/skill-b\n\nbe thorough"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_SkillsPlusAlwaysPlusPerRun(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/skill-a", "/skill-b"},
		AlwaysPrompt: "be thorough",
		PerRunPrompt: "focus on auth",
	}
	got := ComposeReviewPrompt(cfg)
	want := "/skill-a\n/skill-b\n\nbe thorough\n\nfocus on auth"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_AllSectionsWithScope(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		AlwaysPrompt: "be thorough",
		PerRunPrompt: "focus on auth",
		ScopeBaseRef: "main",
	}
	got := ComposeReviewPrompt(cfg)
	want := "/x\n\nbe thorough\n\nfocus on auth\n\nScope: review only the commits unique to this branch vs main."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_IncludesCheckpointContext(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:            []string{"/x"},
		ScopeBaseRef:      "main",
		CheckpointContext: "Commits in scope (newest first):\n  abc123 checkpoint data\n",
	}
	got := ComposeReviewPrompt(cfg)
	for _, want := range []string{
		"/x",
		"Scope: review only the commits unique to this branch vs main.",
		"Commits in scope (newest first):",
		"abc123 checkpoint data",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestComposeReviewPrompt_PromptOverrideIsVerbatim(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:            []string{"/review"},
		AlwaysPrompt:      "always-on instructions",
		PerRunPrompt:      "per-run focus",
		ScopeBaseRef:      "main",
		CheckpointContext: "Commits in scope (newest first):\n  abc123 checkpoint data\n",
		PromptOverride:    "custom prompt\nleave untouched",
	}
	got := ComposeReviewPrompt(cfg)
	want := "custom prompt\nleave untouched"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_EmptyAlwaysPromptNoExtraBlankLine(t *testing.T) {
	t.Parallel()
	// Skills + PerRunPrompt only — empty AlwaysPrompt must not produce triple-newline.
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		PerRunPrompt: "y",
	}
	got := ComposeReviewPrompt(cfg)
	want := "/x\n\ny"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_EmptySkillsAlwaysPromptOnly(t *testing.T) {
	t.Parallel()
	// No skills, AlwaysPrompt only — must not produce a leading blank line.
	cfg := reviewtypes.RunConfig{
		AlwaysPrompt: "review carefully",
	}
	got := ComposeReviewPrompt(cfg)
	want := "review carefully"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_NoScopeBaseRef(t *testing.T) {
	t.Parallel()
	// Empty ScopeBaseRef — scope clause must be omitted entirely.
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		ScopeBaseRef: "",
	}
	got := ComposeReviewPrompt(cfg)
	want := "/x"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_TrailingWhitespaceStripped(t *testing.T) {
	t.Parallel()
	// AlwaysPrompt with trailing newlines — must not produce extra blank lines.
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		AlwaysPrompt: "be thorough\n\n",
		PerRunPrompt: "focus",
	}
	got := ComposeReviewPrompt(cfg)
	want := "/x\n\nbe thorough\n\nfocus"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
