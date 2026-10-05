package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// The `.git` suffix rule has one shared fixture set because it has one
// meaning, and the CLI used to spell it five different ways: a HasSuffix in
// `repo create`, three TrimSuffix calls in the ref parsers, and a
// ToLower-wrapped one in the mirror-URL parse. Each carried its own ad-hoc
// list of examples, so each could pass while disagreeing with the others.
// A call site that handles only a lowercase `.git` cannot quietly ship behind
// its own narrower table if every site reads from these two.
//
// Case varies throughout because the suffix is a RESERVED SPELLING, not a
// name the user chose. Canonical git draws the same line twice over: it cuts
// the suffix case-sensitively when it is merely guessing a directory name
// from a URL (git_url_basename, dir.c), and matches it with aggressive
// case-insensitivity — plus HFS ignorable codepoints and NTFS short names —
// when the question is whether a path IS the reserved `.git` (is_hfs_dotgit,
// is_ntfs_dotgit). Entire settled on the latter for repo transport paths,
// which accept the suffix whatever its case — precisely so a `.GIT` path does
// not 404 a repo the server resolves. A client that cuts case-sensitively
// disagrees with that server about which repository a path names.

// gitSuffixCases are names carrying the suffix, paired with what remains once
// it is cut. rest is "" where the suffix is the entire name — the case that
// leaves nothing to suggest as an alternative.
var gitSuffixCases = []struct{ in, rest string }{
	{"widgets.git", "widgets"},
	{"widgets.GIT", "widgets"},
	{"widgets.Git", "widgets"},
	{"widgets.gIt", "widgets"},
	{"a.git", "a"},
	{"a.GIT", "a"},
	{"my-repo.GIT", "my-repo"},
	// Interior dots are legal in a repo name, so the cut must take the
	// trailing segment only and never every dotted suffix.
	{"trails.el.git", "trails.el"},
	{"x.y.GIT", "x.y"},
	// Cut exactly once, matching git's single strip_suffix_mem: the name
	// "widgets.git" is what a user who typed "widgets.git.git" meant, even
	// though it is itself a name Entire refuses to store.
	{"widgets.git.git", "widgets.git"},
	{"widgets.GIT.git", "widgets.GIT"},
	// The suffix alone. Nothing survives the cut, so no alternative can be
	// suggested and callers must not offer an empty one.
	{".git", ""},
	{".GIT", ""},
}

// gitSuffixNonCases are names that merely resemble the suffix: the word
// without its dot, a longer dotted extension that starts with it, and a
// different extension entirely. The rule is the exact trailing segment, so
// every one of these must survive untouched. Git pins the same near-misses in
// its own tests, and getting this wrong is worse than missing a `.GIT`: it
// silently renames a repository the user spelled correctly.
var gitSuffixNonCases = []string{
	"git",
	"gitops",
	"dotgit",
	"widgets.gitignore",
	"widgets.github",
	"widgets.el",
	"gitgit",
	"widgets-git",
}

// TestCutGitDirSuffix is the rule's own spec, stated once. Its call sites each
// wrap it in their own grammar — a name, a ref segment, a URL path — and their
// tests necessarily check that grammar too. This one checks nothing but the
// cut.
func TestCutGitDirSuffix(t *testing.T) {
	t.Parallel()

	for _, tc := range gitSuffixCases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, had := gitremote.CutGitDirSuffix(tc.in)
			if !had {
				t.Fatalf("gitremote.CutGitDirSuffix(%q): had = false, want true", tc.in)
			}
			if got != tc.rest {
				t.Errorf("gitremote.CutGitDirSuffix(%q) = %q, want %q", tc.in, got, tc.rest)
			}
		})
	}

	for _, name := range gitSuffixNonCases {
		t.Run("kept "+name, func(t *testing.T) {
			t.Parallel()
			got, had := gitremote.CutGitDirSuffix(name)
			if had {
				t.Fatalf("gitremote.CutGitDirSuffix(%q): had = true, want false (cut to %q)", name, got)
			}
			if got != name {
				t.Errorf("gitremote.CutGitDirSuffix(%q) = %q, want it returned unchanged", name, got)
			}
		})
	}

	// A name shorter than the suffix must not index out of bounds, and a
	// multi-byte final rune must not be sliced into a half-rune that some
	// looser comparison could fold into a match. Neither can reach a repo
	// name the server would store, which is exactly why they are worth
	// pinning: the helper is a string function, and the parsers hand it
	// whatever a pasted URL contained.
	for _, name := range []string{"", ".", ".g", ".gi", "git", "a", "日本語", "widgets.gi日", "widgets.日it"} {
		t.Run("unchanged "+name, func(t *testing.T) {
			t.Parallel()
			got, had := gitremote.CutGitDirSuffix(name)
			if had || got != name {
				t.Errorf("gitremote.CutGitDirSuffix(%q) = (%q, %v), want (%q, false)", name, got, had, name)
			}
		})
	}
}
