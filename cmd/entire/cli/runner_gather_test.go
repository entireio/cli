package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Not parallel: swaps newTrailAPIClient and changes the process working directory.
func TestGatherTrailsUsesNativeRepoBaseForFindings(t *testing.T) {
	const basePath = "/api/v1/repos/native-repo-id/trails"

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "entire://aws-us-east-2.entire.io/et/entirehq/marvin")
	t.Chdir(repoDir)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case basePath:
			if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{ID: "native-trail", Number: 3}}}); err != nil {
				t.Errorf("encode trail list response: %v", err)
			}
		case basePath + "/3/reviews/comments":
			if err := json.NewEncoder(w).Encode(api.TrailReviewCommentsResponse{Comments: []api.TrailReviewComment{{ID: "finding-1", Status: trailReviewStatusOpen}}}); err != nil {
				t.Errorf("encode findings response: %v", err)
			}
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	previous := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("token", srv.URL), "native-repo-id", nil
	}
	t.Cleanup(func() { newTrailAPIClient = previous })

	got := gatherTrails(t.Context(), io.Discard, 10, false)
	if !strings.Contains(got, "1 past review findings") {
		t.Fatalf("gatherTrails output = %q, want one finding", got)
	}
	if len(paths) != 2 || paths[0] != basePath || paths[1] != basePath+"/3/reviews/comments" {
		t.Fatalf("request paths = %v, want native list and findings routes", paths)
	}
}
