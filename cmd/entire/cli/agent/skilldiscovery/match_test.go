package skilldiscovery_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
)

func TestMatches_NameKeywords(t *testing.T) {
	t.Parallel()
	cases := []string{"/review", "/security-audit", "/inspect-pr", "/critique", "/assess"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !skilldiscovery.Matches(name, "") {
				t.Errorf("Matches(%q, \"\") = false, want true", name)
			}
		})
	}
}

// TestMatches_DescriptionIgnored pins the deliberate name-only matching.
// Skills whose descriptions mention review-adjacent words but whose names do
// not (e.g. "review the plan", "inspect recent sessions") must NOT match —
// those were the false positives that crowded the picker with non-review
// skills in real discovery runs.
func TestMatches_DescriptionIgnored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, desc string
	}{
		{"/entire:session-handoff", "inspect recent sessions or summarize a saved session"},
		{"/codex:gpt-5-4-prompting", "composing prompts for coding, review, diagnosis, research"},
		{"/superpowers:executing-plans", "execute plans in a separate session with review checkpoints"},
		{"/unrelated", "Audit test coverage on changed files"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if skilldiscovery.Matches(c.name, c.desc) {
				t.Errorf("Matches(%q, %q) = true; want false (description should not match)", c.name, c.desc)
			}
		})
	}
}

// TestMatches_PluginPrefixProvidesKeyword pins that plugin-prefix invocations
// count for matching — this is how /pr-review-toolkit:silent-failure-hunter
// still gets discovered even though its own skill name doesn't contain
// "review".
func TestMatches_PluginPrefixProvidesKeyword(t *testing.T) {
	t.Parallel()
	cases := []string{
		"/pr-review-toolkit:silent-failure-hunter",
		"/pr-review-toolkit:type-design-analyzer",
		"/security-audit-bundle:scan-for-leaks",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !skilldiscovery.Matches(name, "") {
				t.Errorf("Matches(%q, \"\") = false, want true (plugin-prefix match)", name)
			}
		})
	}
}

func TestMatches_CaseInsensitive(t *testing.T) {
	t.Parallel()
	if !skilldiscovery.Matches("/REVIEW", "") {
		t.Error("uppercase name keyword should match")
	}
	if !skilldiscovery.Matches("/Inspect-PR", "") {
		t.Error("mixed-case name keyword should match")
	}
}

func TestMatches_NoMatchWhenNeitherContains(t *testing.T) {
	t.Parallel()
	if skilldiscovery.Matches("/format", "Reformat code") {
		t.Error("format/reformat should not match review keywords")
	}
	if skilldiscovery.Matches("/lint-fix", "Auto-fix lint errors") {
		t.Error("lint is no longer in the keyword set — see spec §Keyword Set")
	}
}
