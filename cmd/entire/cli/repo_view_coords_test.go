package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repo view --json carries the repo's entire-api coordinates at the top level
// (repoId, apiUrl) resolved exactly as the shared placement resolver does: the processing placement's id paired with that placement's
// cell. Review feedback on PR #2758.

func TestRepoViewCoords_MirrorSelectsProcessingPlacement(t *testing.T) {
	t.Parallel()
	// Processing placement deliberately NOT first: selection is by id.
	entry := repoIndexFixture("acme/widget", "mirror-eu",
		placementFixture{id: "mirror-us", slug: usClusterSlug},
		placementFixture{id: "mirror-eu", slug: euClusterSlug},
	)

	repoID, apiURL, err := repoViewCoords(entry, clustersWithSlugs())
	require.NoError(t, err)
	if repoID != "mirror-eu" {
		t.Fatalf("repoID = %q, want mirror-eu (the processing placement)", repoID)
	}
	if apiURL != euCellAPIURL {
		t.Fatalf("apiURL = %q, want %q (the processing placement's cell)", apiURL, euCellAPIURL)
	}
}

func TestRepoViewCoords_NotOnboardedYieldsNoCoords(t *testing.T) {
	t.Parallel()
	entry := repoIndexFixture("acme/widget", "",
		placementFixture{id: "mirror-us", slug: usClusterSlug},
	)

	_, _, err := repoViewCoords(entry, clustersWithSlugs())
	require.ErrorIs(t, err, errRepoNotOnboarded)
}

func TestRepoViewCoords_Unresolved(t *testing.T) {
	t.Parallel()
	for scenario, wantError := range map[string]string{
		"candidate":            "not onboarded",
		"missing primary":      "not in the repo's placement list",
		"failed":               "processing placement is failed",
		"suspended":            "processing placement is suspended",
		"missing cluster":      "no cluster matches",
		"missing api URL":      "missing apiUrl/jurisdiction",
		"missing jurisdiction": "missing apiUrl/jurisdiction",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			entry := repoIndexFixture("acme/widget", "mirror-eu", placementFixture{id: "mirror-eu", slug: euClusterSlug})
			clusters := clustersWithSlugs()
			switch scenario {
			case "candidate":
				entry.Candidate = coreapi.NewOptRepoCandidate(coreapi.RepoCandidate{})
			case "missing primary":
				entry.Placements = nil
			case "failed":
				entry.Placements[0].Status = coreapi.RepoPlacementStatusFailed
			case "suspended":
				entry.Placements[0].Status = coreapi.RepoPlacementStatusSuspended
			case "missing cluster":
				clusters = nil
			case "missing api URL":
				clusters[1].ApiUrl = coreapi.OptString{}
			case "missing jurisdiction":
				clusters[1].Jurisdiction = ""
			}
			repoID, apiURL, err := repoViewCoords(entry, clusters)
			require.ErrorContains(t, err, wantError)
			require.Empty(t, repoID)
			require.Empty(t, apiURL)
		})
	}
}

// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_JSONCoordinates(t *testing.T) {
	for _, forge := range []string{"gh", "et"} {
		t.Run(forge, func(t *testing.T) {
			var catalogReads, indexReads atomic.Int32
			entry := repoIndexFixture("acme/web", "mirror-eu",
				placementFixture{id: "mirror-us", slug: usClusterSlug},
				placementFixture{id: "mirror-eu", slug: euClusterSlug})
			for i := range entry.Placements {
				entry.Placements[i].Mirror = true
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/repos":
					indexReads.Add(1)
					assert.Equal(t, "acme/web", r.URL.Query().Get("filter"))
					writeJSONResponse(t, w, http.StatusOK, reposOutput(entry))
				case "/api/v1/clusters":
					catalogReads.Add(1)
					writeJSONResponse(t, w, http.StatusOK, &coreapi.ListClustersOutputBody{Clusters: clustersWithSlugs()})
				case "/api/v1/repos/resolve":
					writeJSONResponse(t, w, http.StatusOK, nativeResolution("acme/web", testDeleteULID))
				case "/api/v1/repos/" + testDeleteULID:
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","clusterHost":"eu.entire.io","clusterSlug":%q,"state":"active"}`, testDeleteULID, testProjectULID, euClusterSlug)
				case "/api/v1/repos/" + testDeleteULID + "/native-mirrors":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"nativeMirrors":[]}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			ref := "/" + forge + "/acme/web"
			out, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, ref, "--json")
			require.NoError(t, err)
			require.Empty(t, stderr)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			wantID := "mirror-eu"
			if forge == "et" {
				wantID = testDeleteULID
			}
			require.Equal(t, wantID, got["repoId"])
			require.Equal(t, euCellAPIURL, got["apiUrl"])
			require.Equal(t, ref, got["repo"])
			require.Contains(t, got, "placements")
			require.Equal(t, 1, int(catalogReads.Load()), "coordinates reuse the view's catalog")
			if forge == "gh" {
				require.Equal(t, 1, int(indexReads.Load()), "coordinates reuse the view's index entry")
			}
			// The same view and coordinates are available from origin without
			// duplicating the placement lookup in the current-repo path.
			origins := []string{"https://github.com/acme/web.git", "git@github.com:acme/web.git", "entire://eu.entire.io/gh/acme/web"}
			if forge == "et" {
				origins = []string{"entire://eu.entire.io/et/acme/web"}
			}
			for _, origin := range origins {
				t.Run(origin, func(t *testing.T) {
					dir := t.TempDir()
					testutil.InitRepo(t, dir)
					testutil.AddRemote(t, dir, "origin", origin)
					t.Chdir(dir)
					previous := clusterCoreClient
					var selectedHost string
					clusterCoreClient = func(_ context.Context, host string) (*coreapi.Client, error) {
						selectedHost = host
						return coreapi.NewWithBearer(srv.URL, "tok")
					}
					t.Cleanup(func() { clusterCoreClient = previous })
					current, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, "--json")
					require.NoError(t, err)
					require.Empty(t, stderr)
					require.JSONEq(t, out, current)
					if strings.HasPrefix(origin, "entire://") {
						require.Equal(t, "eu.entire.io", selectedHost)
					} else {
						require.Empty(t, selectedHost)
					}
				})
			}
		})
	}
}

func TestMirrorRepoViewJSON_TopLevelCoords(t *testing.T) {
	t.Parallel()
	row := repoDirRow{Repo: "/gh/acme/widget", Placements: []repoDirPlacement{{Cluster: "eu.entire.io"}}}
	data, err := json.Marshal(mirrorRepoViewJSON{repoDirRow: row, RepoID: "mirror-eu", APIURL: euCellAPIURL})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["repoId"] != "mirror-eu" || got["apiUrl"] != euCellAPIURL {
		t.Fatalf("top-level coords missing or wrong: %s", data)
	}
	if _, flat := got["repo"]; !flat {
		t.Fatalf("embedded row fields must stay at the top level: %s", data)
	}
}

func TestMirrorRepoViewJSON_CoordsOmittedWhenUnresolved(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(mirrorRepoViewJSON{repoDirRow: repoDirRow{Repo: "/gh/acme/widget"}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["repoId"]; present {
		t.Fatalf("repoId must be omitted when unresolved: %s", data)
	}
	if _, present := got["apiUrl"]; present {
		t.Fatalf("apiUrl must be omitted when unresolved: %s", data)
	}
}

func TestNativeRepoViewJSON_TopLevelCoords(t *testing.T) {
	t.Parallel()
	repo := &coreapi.Repo{ID: "01NATIVEREPOSITORY00000000", Name: "marvin"}
	row := repoDirRow{Repo: "/et/entirehq/marvin"}

	obj, err := nativeRepoViewJSON(repo, row, "01NATIVEREPOSITORY00000000", euCellAPIURL)
	if err != nil {
		t.Fatal(err)
	}
	var repoID, apiURL string
	if err := json.Unmarshal(obj["repoId"], &repoID); err != nil || repoID != "01NATIVEREPOSITORY00000000" {
		t.Fatalf("repoId = %q (err %v), want the repo ULID", repoID, err)
	}
	if err := json.Unmarshal(obj["apiUrl"], &apiURL); err != nil || apiURL != euCellAPIURL {
		t.Fatalf("apiUrl = %q (err %v), want %q", apiURL, err, euCellAPIURL)
	}
}

func TestNativeRepoViewJSON_CoordsOmittedWhenUnresolved(t *testing.T) {
	t.Parallel()
	obj, err := nativeRepoViewJSON(&coreapi.Repo{ID: "01NATIVEREPOSITORY00000000"}, repoDirRow{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := obj["repoId"]; present {
		t.Fatal("repoId must be omitted when unresolved")
	}
	if _, present := obj["apiUrl"]; present {
		t.Fatal("apiUrl must be omitted when unresolved")
	}
}

// Not parallel: each subtest changes CWD to an isolated repository.
func TestRepoView_OriginErrors(t *testing.T) {
	t.Run("outside a repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		out, _, err := runCoreCmd(t, newRepoViewCmd, "http://unused.invalid", "--json")
		require.ErrorIs(t, err, paths.ErrNotARepository)
		require.ErrorContains(t, err, "not in a git repository")
		require.NotContains(t, err.Error(), `remote "origin" not found`)
		require.Empty(t, out)
	})
	t.Run("repository lookup failure is preserved", func(t *testing.T) {
		t.Chdir(t.TempDir())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := currentRepoViewRef(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, paths.ErrNotARepository)
	})
	for _, origin := range []string{"", "https://gitlab.com/acme/web.git", "not-a-url", "entire://eu.entire.io/et/only-project"} {
		t.Run(origin, func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			if origin != "" {
				testutil.AddRemote(t, dir, "origin", origin)
			}
			t.Chdir(dir)
			out, _, err := runCoreCmd(t, newRepoViewCmd, "http://unused.invalid", "--json")
			require.Error(t, err)
			require.Empty(t, out)
			if origin == "" {
				require.ErrorContains(t, err, `remote "origin" not found`)
				require.NotErrorIs(t, err, paths.ErrNotARepository)
			}
		})
	}
}

func TestNativeRepoViewCoords_Unresolved(t *testing.T) {
	t.Parallel()
	for scenario, wantError := range map[string]string{
		"missing id":           "repo has no id",
		"missing host":         "repo has no cluster host",
		"unknown host":         "no cluster matches host",
		"missing api URL":      "missing apiUrl/jurisdiction",
		"missing jurisdiction": "missing apiUrl/jurisdiction",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			repo := &coreapi.Repo{ID: testDeleteULID, ClusterHost: coreapi.NewOptString("eu.entire.io")}
			clusters := clustersWithSlugs()
			switch scenario {
			case "missing id":
				repo.ID = ""
			case "missing host":
				repo.ClusterHost = coreapi.OptString{}
			case "unknown host":
				repo.ClusterHost = coreapi.NewOptString("unknown.entire.io")
			case "missing api URL":
				clusters[1].ApiUrl = coreapi.OptString{}
			case "missing jurisdiction":
				clusters[1].Jurisdiction = ""
			}
			repoID, apiURL, err := nativeRepoViewCoords(repo, clusters)
			require.ErrorContains(t, err, wantError)
			require.Empty(t, repoID)
			require.Empty(t, apiURL)
		})
	}
}
