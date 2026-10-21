package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/stretchr/testify/require"
)

// TestSemanticSearchV4Session_LookupFilterKeepsForge pins that the forge
// prefix on a repo filter reaches the control plane unchanged. ListRepos reads
// a bare owner/repo as "either forge", so stripping gh/ would let a
// GitHub-origin default scope also match a same-named Entire-native repo.
func TestSemanticSearchV4Session_LookupFilterKeepsForge(t *testing.T) {
	t.Parallel()

	const (
		githubID = "01GITHUBREPO00000000000000"
		nativeID = "01NATIVEREPO00000000000000"
	)
	var gotFilters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		filter := r.URL.Query().Get("filter")
		gotFilters = append(gotFilters, filter)
		w.Header().Set("Content-Type", "application/json")
		// Mimic the control plane: a forge-qualified name matches only its
		// namespace; a bare owner/repo matches both.
		var body coreapi.ListReposOutputBody
		github := coreapi.RepoIndexEntry{ID: githubID, FullName: "acme/thing"}
		native := coreapi.RepoIndexEntry{ID: nativeID, FullName: "et/acme/thing"}
		switch filter {
		case "gh/acme/thing":
			body.Repos = []coreapi.RepoIndexEntry{github}
		case "et/acme/thing":
			body.Repos = []coreapi.RepoIndexEntry{native}
		case "acme/thing":
			body.Repos = []coreapi.RepoIndexEntry{github, native}
		default:
			t.Errorf("unexpected filter %q", filter)
		}
		if err := printJSON(w, &body); err != nil {
			t.Errorf("encode repos: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)
	s := &semanticSearchV4Session{coreClient: client}

	entries, err := s.resolveScope(context.Background(), []string{"gh/acme/thing"})
	require.NoError(t, err)
	require.Equal(t, []string{"gh/acme/thing"}, gotFilters, "the forge-qualified filter must reach ListRepos unchanged")
	require.Len(t, entries, 1)
	require.Equal(t, githubID, entries[0].ID, "a GitHub-scoped lookup must not include the same-named native repo")

	entries, err = s.resolveScope(context.Background(), []string{"et/acme/thing"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, nativeID, entries[0].ID)

	// Cached per filter: the second gh/ lookup makes no further request.
	_, err = s.resolveScope(context.Background(), []string{"gh/acme/thing"})
	require.NoError(t, err)
	require.Equal(t, []string{"gh/acme/thing", "et/acme/thing"}, gotFilters)
}
