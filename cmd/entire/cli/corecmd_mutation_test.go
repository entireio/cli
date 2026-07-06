package cli

import (
	"bytes"
	"context"
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
		if err := printJSON(w, &coreapi.Org{ID: testDeleteULID, Name: "acme", Region: "us"}); err != nil {
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
		if err := printJSON(w, repo); err != nil {
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

// TestRepoCreate_ClusterHostRouting pins the routing fix: `repo create` must
// always dial the target cluster's own core (coreapi.NewForCluster), never the
// active-context core, so a repo lands on the correct regional deployment even
// when the target cluster is in a different jurisdiction than the active login.
// --cluster-host X targets cluster X; omitting it falls back to
// defaultClusterHost. The active core seam must never be used for the create.
//
// Not parallel: swaps the package-level activeCoreClient and clusterCoreClient seams.
func TestRepoCreate_ClusterHostRouting(t *testing.T) {
	newRepoServer := func(hit *bool) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*hit = true
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/repos", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			if err := printJSON(w, &coreapi.Repo{
				ID:              testDeleteULID,
				Name:            "web",
				OwningProjectId: testRepoCreateProjectULID,
				ClusterHost:     coreapi.NewOptString("c.example.com"),
				Path:            coreapi.NewOptString("/gh/o/web"),
			}); err != nil {
				t.Errorf("encode repo: %v", err)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	// run points both seams at distinct fake cores, runs `repo create` with the
	// extra args, and reports which core received the POST plus the cluster host
	// the cluster seam was asked for.
	run := func(t *testing.T, extra ...string) (activeHit, clusterHit bool, gotClusterHost, out string) {
		t.Helper()
		var aHit, cHit bool
		var host string
		activeSrv := newRepoServer(&aHit)
		clusterSrv := newRepoServer(&cHit)

		prevActive := activeCoreClient
		prevCluster := clusterCoreClient
		activeCoreClient = func(context.Context) (*coreapi.Client, error) {
			return coreapi.NewWithBearer(activeSrv.URL, "tok")
		}
		clusterCoreClient = func(_ context.Context, clusterHost string) (*coreapi.Client, error) {
			host = clusterHost
			return coreapi.NewWithBearer(clusterSrv.URL, "tok")
		}
		t.Cleanup(func() {
			activeCoreClient = prevActive
			clusterCoreClient = prevCluster
		})

		cmd := newRepoCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(append([]string{"create", "web", "--project", testRepoCreateProjectULID}, extra...))
		require.NoError(t, cmd.ExecuteContext(t.Context()))
		return aHit, cHit, host, buf.String()
	}

	t.Run("--cluster-host dials that cluster's core, not the active core", func(t *testing.T) {
		activeHit, clusterHit, gotHost, out := run(t, "--cluster-host", "aws-ap-southeast-2.entire.io")
		require.True(t, clusterHit, "the cluster core should receive the create")
		require.False(t, activeHit, "the active core must not be dialed")
		require.Equal(t, "aws-ap-southeast-2.entire.io", gotHost)
		require.Contains(t, out, "✓ Created repository web")
	})

	t.Run("no --cluster-host falls back to the default cluster's core, not the active core", func(t *testing.T) {
		activeHit, clusterHit, gotHost, out := run(t)
		require.True(t, clusterHit, "the default cluster's core should receive the create")
		require.False(t, activeHit, "the active core must not be dialed")
		require.Equal(t, defaultClusterHost, gotHost)
		require.Contains(t, out, "✓ Created repository web")
	})
}
