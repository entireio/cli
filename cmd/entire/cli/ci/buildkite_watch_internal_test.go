//go:build internal

package ci

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

func tp(t *testing.T, s string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return &parsed
}

// buildsJSON encodes a ci-builds response fixture the way entire-core would.
func buildsJSON(t *testing.T, builds ...ciBuildDTO) []byte {
	t.Helper()
	b, err := json.Marshal(ciBuildsResponse{Builds: builds})
	require.NoError(t, err)
	return b
}

// newCoreSource points a coreBuildSource at srvURL via the same bearer client
// the ci verbs use, so the hand-rolled GetJSON call is exercised end to end.
func newCoreSource(t *testing.T, srvURL, pipeline string) *coreBuildSource {
	t.Helper()
	c, err := coreapi.NewWithBearer(srvURL, "tok")
	require.NoError(t, err)
	return &coreBuildSource{client: c, repoID: testRepoULID, pipeline: pipeline}
}

func TestCoreBuildSource_BuildMapsDTO(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		writeJSONResponse(t, w, http.StatusOK, buildsJSON(t, ciBuildDTO{
			BKBuildUUID: "uuid-1", BKOrganization: "acme", BKPipeline: "web",
			BuildNumber: 42, State: "running", Branch: "main", Commit: "abcdef0123456",
			Message: "fix: the thing\n\nlong body ignored", WebURL: "https://buildkite.com/acme/web/builds/42",
			StartedAt: tp(t, "2026-06-09T12:00:00Z"),
			Jobs: []ciJobDTO{
				{Type: "script", Name: ":package: build", StepKey: "build", State: "passed", ExitStatus: new(0),
					StartedAt: tp(t, "2026-06-09T12:00:00Z"), FinishedAt: tp(t, "2026-06-09T12:00:12Z")},
				{Type: "waiter", State: "passed"},
				{Type: "script", Name: "", StepKey: "tests", State: "running", StartedAt: tp(t, "2026-06-09T12:00:12Z")},
			},
		}))
	}))
	t.Cleanup(srv.Close)

	v, err := newCoreSource(t, srv.URL, "web").Build(t.Context(), 42)
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/repos/"+testRepoULID+"/ci-builds", gotPath)
	assert.Equal(t, "web", gotQuery.Get("pipeline"))
	assert.Equal(t, "100", gotQuery.Get("limit"))

	assert.Equal(t, 42, v.Number)
	assert.Equal(t, "running", v.State)
	assert.Equal(t, "main", v.Branch)
	assert.Equal(t, "fix: the thing", v.Message, "message is trimmed to its first line")
	assert.True(t, v.HasStarted)
	require.Len(t, v.Steps, 2, "the waiter job must be dropped")
	assert.Equal(t, ":package: build", v.Steps[0].Label)
	assert.True(t, v.Steps[0].HasFinished)
	assert.Equal(t, "tests", v.Steps[1].Label, "empty name falls back to step_key")
}

func TestCoreBuildSource_BuildNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusOK, buildsJSON(t))
	}))
	t.Cleanup(srv.Close)

	_, err := newCoreSource(t, srv.URL, "").Build(t.Context(), 7)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build #7 not found")
}

func TestCoreBuildSource_ActiveBuildsFiltersAndSorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No pipeline filter set → the param is absent.
		assert.Empty(t, r.URL.Query().Get("pipeline"))
		writeJSONResponse(t, w, http.StatusOK, buildsJSON(t,
			ciBuildDTO{BuildNumber: 9, State: "passed"},
			ciBuildDTO{BuildNumber: 14, State: "running"},
			ciBuildDTO{BuildNumber: 11, State: "scheduled"},
		))
	}))
	t.Cleanup(srv.Close)

	views, err := newCoreSource(t, srv.URL, "").ActiveBuilds(t.Context())
	require.NoError(t, err)
	require.Len(t, views, 2, "the passed build is not active")
	assert.Equal(t, 14, views[0].Number, "newest active build first")
	assert.Equal(t, 11, views[1].Number)
}

// TestWatchCommand_TerminalBuildExits drives the whole `ci buildkite watch`
// verb: ResolveNativeRepo short-circuits on the ULID, the poll loop fetches a
// terminal build on the first tick, renders it, and exits without sleeping.
func TestWatchCommand_TerminalBuildExits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusOK, buildsJSON(t, ciBuildDTO{
			BuildNumber: 42, State: "passed", Branch: "main",
			Jobs: []ciJobDTO{{Type: "script", Name: "build", State: "passed", ExitStatus: new(0)}},
		}))
	}))
	t.Cleanup(srv.Close)

	out, err := runBuildkiteCmd(t, srv.URL, "buildkite", "watch", testRepoULID, "42")
	require.NoError(t, err)
	assert.Contains(t, out, "Build #42  PASSED")
	assert.Contains(t, out, "build")
}

func TestWatchCommand_NoActiveBuilds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusOK, buildsJSON(t))
	}))
	t.Cleanup(srv.Close)

	out, err := runBuildkiteCmd(t, srv.URL, "buildkite", "watch", testRepoULID, "--pipeline", "web")
	require.NoError(t, err)
	assert.Contains(t, out, "no in-progress builds")
	assert.Contains(t, out, "pipeline web")
}

func TestWatchCommand_RejectsBadBuildNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusOK, buildsJSON(t))
	}))
	t.Cleanup(srv.Close)

	_, err := runBuildkiteCmd(t, srv.URL, "buildkite", "watch", testRepoULID, "notanumber")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid build number")
}
