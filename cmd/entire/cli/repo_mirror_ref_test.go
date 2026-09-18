package cli

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestParseGitHubMirrorRepoRef(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"/gh/Acme/Widget.git", "gh/acme/widget"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := parseGitHubMirrorRepoRef(ref)
			require.NoError(t, err)
			require.Equal(t, "acme", owner)
			require.Equal(t, "widget", repo)
		})
	}
	// A repository is named /<forge>/<a>/<b> and no other way. A GitHub URL is
	// unambiguous about its forge but is still a second spelling, so it is
	// refused and answered with the ref it should have been.
	for _, ref := range []string{"https://github.com/acme/widget.git", "git@github.com:acme/widget.git", "github.com/acme/widget"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseGitHubMirrorRepoRef(ref)
			require.ErrorContains(t, err, "pass GitHub repositories as /gh/acme/widget")
		})
	}
	for _, ref := range []string{"acme/widget", "/gh/acme/..", "https://gitlab.com/acme/widget"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseGitHubMirrorRepoRef(ref)
			require.ErrorContains(t, err, "invalid <repo>")
		})
	}
}

func TestMirrorCommands_NativeRepoUnsupported(t *testing.T) {
	t.Parallel()
	for name, newCmd := range map[string]func() *cobra.Command{
		"mirror add":    newRepoMirrorAddCmd,
		"mirror remove": newRepoMirrorRemoveCmd,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := newCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"/et/project/widget"})
			err := cmd.ExecuteContext(t.Context())
			require.ErrorContains(t, err, "does not support Entire repository")
			require.NotContains(t, err.Error(), "invalid")
		})
	}
	t.Run("remote use", func(t *testing.T) {
		t.Parallel()
		_, _, err := resolveMirrorUseUpstream(t.Context(), t.TempDir(), "origin", "/et/project/widget")
		require.ErrorContains(t, err, "does not support Entire repository")
		require.NotContains(t, err.Error(), "invalid")
	})
}

// TestBareRefSuggestions_OnlyOffersRefsTheCallerAccepts pins the helper's
// contract: a suggestion is never itself a ref that would fail on the next
// line. `repo clone` serves both forges and names both; the mirror verbs are
// GitHub-only and must name only that one.
func TestBareRefSuggestions_OnlyOffersRefsTheCallerAccepts(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"/et/octocat/hello-world", "/gh/octocat/hello-world"},
		bareRefSuggestions("octocat/hello-world"),
		"naming no forge means every grammar, which is repo clone")
	require.Equal(t, []string{"/gh/octocat/hello-world"},
		bareRefSuggestions("octocat/hello-world", mirrorCloneForge))
	require.Equal(t, []string{"/et/octocat/hello-world"},
		bareRefSuggestions("octocat/hello-world", nativeCloneForge))

	// The suggestion a GitHub-only verb prints must parse there, which is the
	// property that was broken: /et/... was suggested and then refused.
	for _, suggestion := range bareRefSuggestions("octocat/hello-world", mirrorCloneForge) {
		_, _, err := parseGitHubMirrorRepoRef(suggestion)
		require.NoErrorf(t, err, "suggested %q must be accepted by the same parser", suggestion)
	}
}

// TestParseGitHubMirrorRepoRef_NativeRefIsRefused pins that declaring the
// native forge is the whole answer: every /et/ ref gets the same refusal, so
// callers can attach their own pointer to the verb that does serve Entire
// repositories.
//
// The spelling of the project and repo must not change the answer. These verbs
// refuse the ref either way, so reporting a name rule would send the reader to
// fix something that would be refused again.
func TestParseGitHubMirrorRepoRef_NativeRefIsRefused(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"/et/my-project/my-repo", // well-formed
		"/et/p/r",                // project too short for the server's rules
		"/et/project/..",         // repo is not a name at all
		"/et/foo",                // not even two segments
		"et/my-project/my-repo",  // leading slash is optional
	} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseGitHubMirrorRepoRef(ref)
			require.ErrorContains(t, err, "does not support Entire repository")
			require.NotContains(t, err.Error(), "is not a name the server accepts",
				"a verb that refuses every native ref must not teach the name rule")
		})
	}
}

// TestForgeQualifiedRefError_NamesOnlyTheForgesServed pins the helper's whole
// contract, suggestions and shape list alike: a ref a GitHub-only verb cannot
// read must never be answered with a native shape that same verb refuses.
func TestForgeQualifiedRefError_NamesOnlyTheForgesServed(t *testing.T) {
	t.Parallel()

	// A GitLab URL parses as neither grammar and is not a bare pair, so it
	// falls through to the shape list — the branch that used to name both.
	mirrorOnly := forgeQualifiedRefError("https://gitlab.com/acme/widget", mirrorCloneForge)
	require.ErrorContains(t, mirrorOnly, "/gh/<owner>/<repo>")
	require.NotContains(t, mirrorOnly.Error(), nativeCloneForge+"/<project>/<repo>")

	bothForges := forgeQualifiedRefError("https://gitlab.com/acme/widget")
	require.ErrorContains(t, bothForges, "/gh/<owner>/<repo>")
	require.ErrorContains(t, bothForges, "/et/<project>/<repo>")
}
