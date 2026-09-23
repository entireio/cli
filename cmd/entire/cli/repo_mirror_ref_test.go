package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseMirrorRepoRef_GitHub pins the /gh/ grammar: the token is required,
// the pair is lowercased, and a `.git` suffix is never part of a name.
func TestParseMirrorRepoRef_GitHub(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"/gh/Acme/Widget.git", "gh/acme/widget"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			got, err := parseMirrorRepoRef(ref, mirrorCloneForge)
			require.NoError(t, err)
			require.Equal(t, mirrorRepoRef{forge: mirrorCloneForge, owner: "acme", repo: "widget"}, got)
		})
	}
}

// TestParseMirrorRepoRef_Native pins the /et/ grammar behind the same entry
// point, so a verb that serves both forges reads them through one parser. Case
// is preserved here, unlike GitHub: both native lookups fold case server-side,
// and the project/repo pair is passed on as the user spelled it.
func TestParseMirrorRepoRef_Native(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"/et/my-project/my-repo", "et/my-project/my-repo"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			got, err := parseMirrorRepoRef(ref, nativeCloneForge)
			require.NoError(t, err)
			require.Equal(t, mirrorRepoRef{forge: nativeCloneForge, owner: "my-project", repo: "my-repo"}, got)
		})
	}
}

// TestParseMirrorRepoRef_NativeKeepsGitSuffix pins that `.git` on a native ref
// is part of the repo name, not decoration: the data plane resolves /et/ paths
// verbatim, so trimming here would point at a different repository. Contrast
// TestParseMirrorRepoRef_GitHub, where the suffix is dropped.
func TestParseMirrorRepoRef_NativeKeepsGitSuffix(t *testing.T) {
	t.Parallel()
	got, err := parseMirrorRepoRef("/et/my-project/my-repo.git", nativeCloneForge)
	require.NoError(t, err)
	require.Equal(t, mirrorRepoRef{forge: nativeCloneForge, owner: "my-project", repo: "my-repo.git"}, got)
}

// TestParseMirrorRepoRef_ServingBothForges pins that one call site can take
// both, which is what lets a verb act on either kind of repository without a
// second grammar.
func TestParseMirrorRepoRef_ServingBothForges(t *testing.T) {
	t.Parallel()
	native, err := parseMirrorRepoRef("/et/my-project/my-repo")
	require.NoError(t, err)
	require.Equal(t, nativeCloneForge, native.forge)

	mirror, err := parseMirrorRepoRef("/gh/acme/widget")
	require.NoError(t, err)
	require.Equal(t, mirrorCloneForge, mirror.forge)
}

// A repository is named /<forge>/<a>/<b> and no other way. A GitHub URL is
// unambiguous about its forge but is still a second spelling, so it is refused
// and answered with the ref it should have been.
func TestParseMirrorRepoRef_GitHubURLIsRefused(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"https://github.com/acme/widget.git", "git@github.com:acme/widget.git", "github.com/acme/widget"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, err := parseMirrorRepoRef(ref, mirrorCloneForge)
			require.ErrorContains(t, err, "pass GitHub repositories as /gh/acme/widget")
		})
	}
}

func TestParseMirrorRepoRef_RefusesRefsNamingNoForge(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"acme/widget", "/gh/acme/..", "https://gitlab.com/acme/widget"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, err := parseMirrorRepoRef(ref, mirrorCloneForge)
			require.ErrorContains(t, err, "invalid <repo>")
		})
	}
}

// TestParseMirrorRepoRef_UnservedForgeIsRefusedUnparsed pins that declaring a
// forge the verb does not serve is the whole answer: every such ref gets the
// same refusal, so callers can attach their own pointer to the verb that does
// serve it.
//
// The spelling of the two names must not change the answer. The verb refuses
// the ref either way, so reporting a name rule would send the reader to fix
// something that would be refused again.
func TestParseMirrorRepoRef_UnservedForgeIsRefusedUnparsed(t *testing.T) {
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
			_, err := parseMirrorRepoRef(ref, mirrorCloneForge)
			require.ErrorContains(t, err, "does not support Entire repository")
			require.ErrorContains(t, err, "supports GitHub mirrors only")
			require.NotContains(t, err.Error(), "is not a name the server accepts",
				"a verb that refuses every native ref must not teach the name rule")
		})
	}

	// The refusal is symmetric: it names whichever forge the caller does serve.
	_, err := parseMirrorRepoRef("/gh/acme/widget", nativeCloneForge)
	require.ErrorContains(t, err, "does not support GitHub mirror")
	require.ErrorContains(t, err, "supports Entire repositories only")
}

// TestMirrorCommands_NativeRepoUnsupported covers `repo access list`, the one
// verb here that still serves GitHub alone: it reads GitHub collaborators, and
// a native repo's access is grants, so the answer is a pointer to the command
// that does serve it.
func TestMirrorCommands_NativeRepoUnsupported(t *testing.T) {
	t.Parallel()
	t.Run("access list", func(t *testing.T) {
		t.Parallel()
		cmd := newRepoAccessListCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"/et/project/widget"})
		err := cmd.ExecuteContext(t.Context())
		require.ErrorContains(t, err, "does not support Entire repository")
		require.ErrorContains(t, err, "entire repo grant list")
	})
}

// TestResolveMirrorUseUpstream_BothForges pins that `repo remote use` now reads
// a native ref as readily as a GitHub one — a clone of either kind can have its
// remote repointed at another cluster.
func TestResolveMirrorUseUpstream_BothForges(t *testing.T) {
	t.Parallel()
	native, err := resolveMirrorUseUpstream(t.Context(), t.TempDir(), "origin", "/et/project/widget")
	require.NoError(t, err)
	require.Equal(t, mirrorRepoRef{forge: nativeCloneForge, owner: "project", repo: "widget"}, native)

	mirror, err := resolveMirrorUseUpstream(t.Context(), t.TempDir(), "origin", "/gh/Acme/Widget")
	require.NoError(t, err)
	require.Equal(t, mirrorRepoRef{forge: mirrorCloneForge, owner: "acme", repo: "widget"}, mirror)
}

// TestBareRefSuggestions_OnlyOffersRefsTheCallerAccepts pins the helper's
// contract: a suggestion is never itself a ref that would fail on the next
// line. Naming no forge means every grammar, which is `repo clone`; a verb that
// serves one must name it.
func TestBareRefSuggestions_OnlyOffersRefsTheCallerAccepts(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"/et/octocat/hello-world", "/gh/octocat/hello-world"},
		bareRefSuggestions("octocat/hello-world"),
		"naming no forge means every grammar, which is repo clone")
	require.Equal(t, []string{"/gh/octocat/hello-world"},
		bareRefSuggestions("octocat/hello-world", mirrorCloneForge))
	require.Equal(t, []string{"/et/octocat/hello-world"},
		bareRefSuggestions("octocat/hello-world", nativeCloneForge))

	// Every suggestion must parse for the same caller that printed it, which is
	// the property that was broken: /et/... was suggested and then refused.
	for _, forges := range [][]string{{mirrorCloneForge}, {nativeCloneForge}, nil} {
		for _, suggestion := range bareRefSuggestions("octocat/hello-world", forges...) {
			_, err := parseMirrorRepoRef(suggestion, forges...)
			require.NoErrorf(t, err, "suggested %q must be accepted by the same parser", suggestion)
		}
	}
}
