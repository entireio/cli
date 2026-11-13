package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/coreapi"
)

// Not parallel: the control-plane and cell-client constructors are test seams.
// The two forges share a name, but must use their own resolved ID and cell.
func TestTrailChangeRoutesUseResolvedPlacement(t *testing.T) {
	const nativeID = "01NATIVEREPOSITORY00000000"
	for _, tc := range []struct {
		forge string
		id    string
	}{
		{"gh", "processing-placement"},
		{"et", nativeID},
	} {
		t.Run(tc.forge, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/api/v1/repos/" + tc.id + "/changes":
					_, _ = fmt.Fprint(w, `{"items":[],"total_count":0}`)
				case "/api/v1/repos/" + tc.id + "/changes/7":
					_, _ = fmt.Fprint(w, `{"id":"work-id","number":7,"branch":"feature/work"}`)
				case "/api/v1/repos/" + tc.id + "/changes/7/reviews/comments":
					_, _ = fmt.Fprint(w, `{"comments":[]}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			clusters := clustersWithSlugs()
			for i := range clusters {
				if clusters[i].Slug == euClusterSlug {
					clusters[i].ApiUrl = coreapi.NewOptString(server.URL)
				}
			}
			withFakeCellCore(t, &fakeCellCore{
				repos: reposOutput(repoIndexFixture("entirehq/marvin", "processing-placement",
					placementFixture{id: "non-processing-mirror", slug: usClusterSlug},
					placementFixture{id: "processing-placement", slug: euClusterSlug})),
				resolution: nativeResolution("entirehq/marvin", nativeID),
				repo:       &coreapi.Repo{ID: nativeID, Name: "marvin", ClusterHost: coreapi.NewOptString("eu.entire.io")},
				clusters:   clusters,
			})
			factory := &countingTrailCellFactory{}
			old := newCellClientBuilder
			newCellClientBuilder = func(context.Context, bool) (cellClientBuilder, error) { return factory, nil }
			t.Cleanup(func() { newCellClientBuilder = old })

			client, repoID, err := newTrailAPIClient(t.Context(), false, tc.forge, "entirehq", "marvin")
			require.NoError(t, err)
			require.Equal(t, tc.id, repoID)
			require.Equal(t, []string{server.URL}, factory.targets)
			repoRef := tc.forge + "/entirehq/marvin"
			require.NoError(t, runTrailListAllWithClient(t.Context(), io.Discard, client, repoID, trailListOptions{Repo: repoRef, Limit: 10}, nil))
			target, err := resolveTrailReviewTarget(t.Context(), client, repoID, "7", repoRef, "")
			require.NoError(t, err)
			_, err = fetchAllTrailReviewComments(t.Context(), client, target.Trail.ID, trailReviewSummaryOptions())
			require.NoError(t, err)
			base := "/api/v1/repos/" + tc.id + "/changes"
			assert.Equal(t, []string{base, base + "/7", base + "/7/reviews/comments"}, paths)
		})
	}
}

func TestTrailChangeRoutesRejectMissingRepoID(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"gh", "et"} {
		t.Run(forge, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("missing ID must not fall back to any wire route: %s", r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			client := api.NewClientWithBaseURL("token", server.URL)
			repoRef := forge + "/acme/widget"
			for _, id := range []string{"", "  "} {
				err := runTrailListAllWithClient(t.Context(), io.Discard, client, id, trailListOptions{Repo: repoRef, Limit: 10}, nil)
				require.Error(t, err)
				_, err = resolveTrailReviewTarget(t.Context(), client, id, "7", repoRef, "")
				require.Error(t, err)
				_, err = trailRepoIDNumberPath(id, 7)
				require.Error(t, err)
			}
		})
	}
}
