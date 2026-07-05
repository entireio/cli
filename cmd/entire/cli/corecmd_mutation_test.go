package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// newCreateOrgServer answers POST /api/v1/orgs with a created org. The 201
// status is load-bearing: the generated decodeCreateOrgResponse only accepts
// http.StatusCreated — a default 200 makes CreateOrg return an error.
func newCreateOrgServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := writeJSON(w, &coreapi.Org{ID: testDeleteULID, Name: "acme", Region: "us"}); err != nil {
			t.Errorf("encode org: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgCreate_HumanByDefault(t *testing.T) {
	srv := newCreateOrgServer(t)
	out, errOut, err := runCoreCmd(t, newOrgCmd, srv.URL, "create", "acme")
	require.NoError(t, err)
	require.Contains(t, out, "✓ Created org acme ("+testDeleteULID+")")
	require.NotContains(t, out, "{", "default output must not be JSON")
	require.Empty(t, errOut)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgCreate_JSONOnRequest(t *testing.T) {
	srv := newCreateOrgServer(t)
	// org create's --json is persistent on the group root, so drive the
	// full group command with "create" as a subcommand arg.
	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "create", "acme", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"name": "acme"`)
	require.Contains(t, out, `"id": "`+testDeleteULID+`"`)
	require.NotContains(t, out, "✓ Created")
}

// testRepoCreateProjectULID is the --project value for the repo-create tests
// below: a syntactically valid ULID so resolveProjectRef skips the by-name
// lookup and the fake server only needs to answer POST /api/v1/repos.
const testRepoCreateProjectULID = "01HZX7QABCDEFGHJKMNPQRSTV2"

// newCreateRepoServer answers POST /api/v1/repos with a created repo whose
// clusterHost/path resolve to a clone URL. The 201 status is load-bearing,
// same as newCreateOrgServer.
func newCreateRepoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		repo := &coreapi.Repo{
			ID:              testDeleteULID,
			Name:            "web",
			OwningProjectId: testRepoCreateProjectULID,
			ClusterHost:     coreapi.NewOptString("c.example.com"),
			Path:            coreapi.NewOptString("/gh/o/web"),
		}
		if err := writeJSON(w, repo); err != nil {
			t.Errorf("encode repo: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoCreate_HumanByDefault(t *testing.T) {
	srv := newCreateRepoServer(t)
	out, errOut, err := runCoreCmd(t, newRepoCmd, srv.URL, "create", "web", "--project", testRepoCreateProjectULID)
	require.NoError(t, err)
	require.Contains(t, out, "✓ Created repository web ("+testDeleteULID+")")
	require.Contains(t, out, "Remote: entire://c.example.com/gh/o/web")
	require.Empty(t, errOut)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoCreate_JSONOnRequest(t *testing.T) {
	srv := newCreateRepoServer(t)
	out, _, err := runCoreCmd(t, newRepoCmd, srv.URL, "create", "web", "--project", testRepoCreateProjectULID, "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"remote": "entire://c.example.com/gh/o/web"`)
	require.NotContains(t, out, "✓ Created")
}
