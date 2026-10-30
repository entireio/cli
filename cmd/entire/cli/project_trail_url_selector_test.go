package cli

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func entireComWebOrigin() (string, error) { return "https://entire.com", nil }

// useEntireComWebOrigin points command tests at the production site. Callers
// are serial: it replaces a package-level seam.
func useEntireComWebOrigin(t *testing.T) {
	t.Helper()
	old := projectTrailWebBaseURL
	projectTrailWebBaseURL = entireComWebOrigin
	t.Cleanup(func() { projectTrailWebBaseURL = old })
}

func TestParseProjectTrailURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw, project, selector string
	}{
		{"project trail", "https://entire.com/gh/acme/-/trails/42", "gh/acme", "42"},
		{"project trail trailing slash and query", "https://entire.com/gh/Acme/-/trails/42/?tab=changes#x", "gh/acme", "42"},
		{"change under project trail", "https://entire.com/gh/acme/-/trails/42/changes/widget/7", "gh/acme", "widget/7"},
		{"change under repository", "https://entire.com/gh/acme/widget/-/trails/42/changes/7", "gh/acme", "widget/7"},
		{"repository trail", "https://entire.com/gh/acme/widget/trails/7", "gh/acme", "widget/7"},
		{"native project", "https://entire.com/et/widgets/-/trails/3", "et/widgets", "3"},
		{"legacy entire.io host", "https://entire.io/gh/acme/-/trails/42", "gh/acme", "42"},
		{"dotted repository name", "https://entire.com/gh/acme/entire.io/trails/7", "gh/acme", "entire.io/7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel, isURL, err := resolveProjectTrailURLSelector(tc.raw, entireComWebOrigin)
			require.NoError(t, err)
			require.True(t, isURL)
			assert.Equal(t, tc.project, sel.Project)
			assert.Equal(t, tc.selector, sel.Selector)
		})
	}
}

func TestParseProjectTrailURLRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, raw, want string }{
		{"non-Entire URL", "https://github.com/acme/widget/pull/7", "not an Entire trail URL"},
		{"lookalike host", "https://notentire.com/gh/acme/-/trails/42", "not an Entire trail URL"},
		{"other Entire site", "https://staging.entire.com/gh/acme/-/trails/42", "your login targets"},
		{"credentials", "https://user:pw@entire.com/gh/acme/-/trails/42", "credentials"},
		{"collection page", "https://entire.com/gh/acme/-/trails", "unrecognized Entire trail URL"},
		{"non-numeric trail", "https://entire.com/gh/acme/-/trails/abc", "unrecognized Entire trail URL"},
		{"unknown forge", "https://entire.com/gl/acme/-/trails/42", "unrecognized Entire trail URL"},
		{"empty segment", "https://entire.com/gh//-/trails/42", "unrecognized Entire trail URL"},
		{"extra segments", "https://entire.com/gh/acme/-/trails/42/changes/widget/7/files", "unrecognized Entire trail URL"},
		{"scheme", "ftp://entire.com/gh/acme/-/trails/42", "unsupported URL scheme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, isURL, err := resolveProjectTrailURLSelector(tc.raw, entireComWebOrigin)
			require.True(t, isURL)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// A scheme-less "entire.io/3915" is a change selector for a repository named
// entire.io, not a URL.
func TestProjectTrailURLSelectorLeavesSchemelessSelectorsAlone(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"entire.io/3915", "42", "widget/7", "01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		_, isURL, err := resolveProjectTrailURLSelector(raw, entireComWebOrigin)
		require.NoError(t, err)
		require.False(t, isURL, raw)
	}
}

// A pasted trail URL resolves in the URL's project, not origin's namespace.
// Not parallel: changes CWD and replaces client constructors.
func TestProjectTrailShowAcceptsTrailURL(t *testing.T) {
	cloneWithOrigin(t, "git@github.com:other/elsewhere.git")
	useEntireComWebOrigin(t)
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, []string{"/api/v1/gh/acme/trails/42", projectTrailTestPath}, r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	core.wantProject = "acme"

	out, _, err := executeProjectTrailTest(t, "show", "https://entire.com/gh/acme/-/trails/42")
	require.NoError(t, err)
	assert.Contains(t, out, "Project intent")
	assert.Equal(t, 1, core.resolveCalls)
}

func TestProjectTrailURLConflictingProjectFlag(t *testing.T) {
	cloneWithOrigin(t, "git@github.com:acme/widget.git")
	useEntireComWebOrigin(t)
	setupProjectTrailTest(t, func(http.ResponseWriter, *http.Request) {
		t.Error("no request expected for a conflicting --project")
	})
	_, _, err := executeProjectTrailTest(t, "show", "https://entire.com/gh/acme/-/trails/42", "--project", "gh/other")
	require.ErrorContains(t, err, "the trail URL is in project gh/acme, but --project is gh/other")
}

// The --trail flag takes URLs too (comment subtree).
func TestProjectTrailCommentTrailFlagAcceptsURL(t *testing.T) {
	cloneWithOrigin(t, "git@github.com:other/elsewhere.git")
	useEntireComWebOrigin(t)
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/gh/acme/trails/42" || r.URL.Path == projectTrailTestPath {
			assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"discussions": []any{}}))
	})
	core.wantProject = "acme"
	_, _, err := executeProjectTrailTest(t, "comment", "list", "--trail", "https://entire.com/gh/acme/-/trails/42")
	require.NoError(t, err)
	assert.Equal(t, 1, core.resolveCalls)
}

// With several open changes the command lists them instead of guessing; the
// listing must name change selectors, never echo the pasted URL.
// Not parallel: changes CWD and replaces client constructors.
func TestProjectTrailURLSeveralChangesListsSelectors(t *testing.T) {
	cloneWithOrigin(t, "git@github.com:acme/widget.git")
	useEntireComWebOrigin(t)
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{
		{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Number: 7, Title: "Widget work", Branch: "feature/a"},
		{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", RepositoryID: "api-id", Repository: "api", Number: 9, Title: "API work", Branch: "feature/api"},
	}
	setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(parent))
	})
	repos := setupProjectMergeRepos(t, trailMergeability(t), map[string]string{"widget": "repo-id", "api": "api-id"})

	_, _, err := executeProjectTrailTest(t, "merge", "https://entire.com/gh/acme/-/trails/42")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "trail #42 has 2 open changes; choose one:")
	assert.Contains(t, msg, "widget/7 - Widget work")
	assert.NotContains(t, msg, "https://", "the listing must name selectors, not the URL")
	assert.Contains(t, msg, "trail merge widget/7 --project=gh/acme", "the retry hint carries the URL's project")
	require.Empty(t, repos.mergePosts())
}
