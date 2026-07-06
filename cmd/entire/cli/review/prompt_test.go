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
	want := "/x\n\nbe thorough\n\nfocus on auth\n\nScope: review the commits unique to this branch vs main, plus any uncommitted changes in the working tree. Ignore code outside this scope."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeReviewPrompt_TaskAddsFindingOutputFormat(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Task: "Review for real defects.",
	}
	got := ComposeReviewPrompt(cfg)
	for _, want := range []string{
		"Task: Review for real defects.",
		"Each finding MUST be a separate top-level Markdown bullet",
		"starting with [high], [medium], or [low]",
		"Do not combine multiple defects",
		"Do not emit severity-heading paragraphs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
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
		"Scope: review the commits unique to this branch vs main, plus any uncommitted changes in the working tree. Ignore code outside this scope.",
		"Commits in scope (newest first):",
		"abc123 checkpoint data",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestComposeReviewPrompt_ScopeIncludesUncommittedChanges(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		ScopeBaseRef: "origin/main",
	}
	got := ComposeReviewPrompt(cfg)
	// The scope clause must explicitly include uncommitted changes — without
	// this, agents (correctly) ignored working-tree edits that hadn't been
	// committed yet, surprising users iterating on a feature branch who
	// expected their in-progress work to be reviewed.
	for _, want := range []string{
		"origin/main",
		"uncommitted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scope clause must mention %q so agents include uncommitted changes; got:\n%s", want, got)
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

func TestComposeReviewPrompt_IncludesScopeContext(t *testing.T) {
	t.Parallel()
	// A populated ScopeContext must land in the prompt so agents never
	// re-derive (or mis-derive) the review scope themselves.
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		ScopeBaseRef: "origin/main",
		ScopeContext: reviewtypes.ScopeContext{
			Commits:     []string{"abc1234 add feature", "def5678 fix bug"},
			Files:       []string{"M\tcmd/foo.go", "A\tcmd/bar.go"},
			Uncommitted: []string{" M docs/readme.md"},
			Diff:        "diff --git a/cmd/foo.go b/cmd/foo.go\n+added line",
		},
	}
	got := ComposeReviewPrompt(cfg)
	for _, want := range []string{
		"Commits under review",
		"abc1234 add feature",
		"def5678 fix bug",
		"Files under review",
		"M\tcmd/foo.go",
		"A\tcmd/bar.go",
		"Uncommitted working-tree changes:",
		" M docs/readme.md",
		"out of scope",
		"```diff",
		"+added line",
		"```",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestComposeReviewPrompt_ScopeContextOmittedDiffPointsAtGitCommand(t *testing.T) {
	t.Parallel()
	// When the diff was too large to inline, the prompt must give the agent
	// the exact three-dot command instead of a diff fence, so scope stays
	// authoritative even without inline content.
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		ScopeBaseRef: "origin/main",
		ScopeContext: reviewtypes.ScopeContext{
			Files:       []string{"M\tcmd/foo.go"},
			DiffOmitted: true,
		},
	}
	got := ComposeReviewPrompt(cfg)
	if !strings.Contains(got, "git diff origin/main...HEAD") {
		t.Errorf("prompt missing exact three-dot diff command:\n%s", got)
	}
	if strings.Contains(got, "```diff") {
		t.Errorf("prompt must not contain a diff fence when the diff was omitted:\n%s", got)
	}
}

func TestComposeReviewPrompt_ScopeContextTruncationNoted(t *testing.T) {
	t.Parallel()
	// Truncated lists must say so, so agents know the enumeration is partial
	// and consult git for the remainder instead of treating it as exhaustive.
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/x"},
		ScopeBaseRef: "origin/main",
		ScopeContext: reviewtypes.ScopeContext{
			Commits:          []string{"abc1234 add feature"},
			CommitsTruncated: true,
			Files:            []string{"M\tcmd/foo.go"},
			FilesTruncated:   true,
		},
	}
	got := ComposeReviewPrompt(cfg)
	if !strings.Contains(got, "list truncated") {
		t.Errorf("prompt missing truncation note:\n%s", got)
	}
}
