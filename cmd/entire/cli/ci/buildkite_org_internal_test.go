//go:build internal

package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// testOrgULID is a syntactically valid org ULID (26 Crockford base32 chars, no
// I/L/O/U) so ResolveOrg short-circuits and the fake server only answers the
// ci/buildkite org calls, never an org-by-name lookup.
const testOrgULID = "0123456789ABCDEFGHJKMNPQR1"

// runOrgCmd is runBuildkiteCmd's sibling that also returns stderr, so the org
// tests can assert the shell-history warning lands on stderr and that the token
// never appears on either stream. Not parallel: the activeCoreClient seam is
// package-global.
//
//nolint:dupl // deliberately mirrors runBuildkiteCmd's seam+tree preamble but returns stderr too.
func runOrgCmd(t *testing.T, srvURL string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srvURL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })

	root := &cobra.Command{Use: "entire"}
	Register(root)
	var out, errW bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errW)
	root.SetArgs(append([]string{"ci"}, args...))
	err = root.ExecuteContext(t.Context())
	return out.String(), errW.String(), err
}

func orgCredJSON(t *testing.T, bkOrg string, tokenLen int) []byte {
	t.Helper()
	b, err := json.Marshal(orgBuildkiteCredentialView{
		BKOrganization: bkOrg,
		Owner:          testOrgULID,
		CipherVersion:  1,
		TokenLen:       tokenLen,
		CreatedAt:      time.Unix(0, 0).UTC(),
		UpdatedAt:      time.Unix(0, 0).UTC(),
	})
	require.NoError(t, err)
	return b
}

func orgClusterJSON(t *testing.T, view orgBuildkiteClusterView) []byte {
	t.Helper()
	b, err := json.Marshal(view)
	require.NoError(t, err)
	return b
}

const bkCredentialPath = "/api/v1/orgs/" + testOrgULID + "/ci/buildkite/credential"

func TestBuildkiteOrgConnect_BodyAssemblyAndVerb(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantWord string
	}{
		{"fresh connect → 201", http.StatusCreated, "✓ Connected"},
		{"rotate → 200", http.StatusOK, "✓ Rotated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const token = "bkua_secret_value_987654"
			t.Setenv("BUILDKITE_API_TOKEN", token)

			var gotMethod, gotPath string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotBody = decodeBody(t, r)
				writeJSONResponse(t, w, tc.status, orgCredJSON(t, "acme-inc", len(token)))
			}))
			t.Cleanup(srv.Close)

			stdout, stderr, err := runOrgCmd(t, srv.URL, "buildkite", "org", "connect", testOrgULID, "--bk-org", "acme-inc")
			require.NoError(t, err)

			assert.Equal(t, http.MethodPost, gotMethod)
			assert.Equal(t, bkCredentialPath, gotPath)
			assert.Equal(t, "acme-inc", gotBody["bk_organization"])
			assert.Equal(t, token, gotBody["bk_api_token"], "the token must reach the server in the request body")
			assert.Contains(t, stdout, tc.wantWord)
			assert.Contains(t, stdout, "token_len=")
			// The token must never surface in user-facing output.
			assert.NotContains(t, stdout, token, "token must never appear on stdout")
			assert.NotContains(t, stderr, token, "token must never appear on stderr")
		})
	}
}

func TestBuildkiteOrgConnect_FlagTokenWarnsAboutShellHistory(t *testing.T) {
	t.Setenv("BUILDKITE_API_TOKEN", "") // force the flag path, not env

	const token = "bkua_flag_token_abcdef"
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		writeJSONResponse(t, w, http.StatusCreated, orgCredJSON(t, "acme-inc", len(token)))
	}))
	t.Cleanup(srv.Close)

	stdout, stderr, err := runOrgCmd(t, srv.URL, "buildkite", "org", "connect", testOrgULID, "--bk-org", "acme-inc", "--bk-token", token)
	require.NoError(t, err)

	assert.Equal(t, token, gotBody["bk_api_token"])
	assert.Contains(t, stderr, "shell history", "using --bk-token must warn about shell history")
	assert.NotContains(t, stdout, token, "token must never appear on stdout")
}

func TestBuildkiteOrgConnect_NoTokenSourceFails(t *testing.T) {
	t.Setenv("BUILDKITE_API_TOKEN", "")

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	// Under `go test` CanPromptInteractively is false, so with no flag and no env
	// there is no token source and connect must fail before any HTTP call.
	_, _, err := runOrgCmd(t, srv.URL, "buildkite", "org", "connect", testOrgULID, "--bk-org", "acme-inc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Buildkite API token")
	assert.False(t, called, "a missing token must fail before any HTTP call")
}

func TestBuildkiteOrgConnect_RequiresBkOrg(t *testing.T) {
	t.Setenv("BUILDKITE_API_TOKEN", "tok")
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runOrgCmd(t, srv.URL, "buildkite", "org", "connect", testOrgULID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bk-org")
	assert.False(t, called, "a missing required flag must fail before any HTTP call")
}

func TestBuildkiteOrgList_TableAndJSON(t *testing.T) {
	const token = "bkua_should_never_render_123456"
	newServer := func() *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			switch {
			case strings.HasSuffix(r.URL.Path, "/ci/buildkite/credentials"):
				body := []byte(`{"credentials":` + string(orgCredsListJSON(t)) + `}`)
				writeJSONResponse(t, w, http.StatusOK, body)
			case strings.HasSuffix(r.URL.Path, "/ci/buildkite/clusters"):
				assert.Equal(t, "acme-inc", r.URL.Query().Get("bk_organization"), "clusters must be queried per connected bk org")
				cl := orgBuildkiteClusterView{
					BKOrganization: "acme-inc", Owner: testOrgULID, BKClusterID: "cluster-uuid-1",
					Label: "web", AuthPluginRef: "entire-io/entire-core-auth#v1.2.3",
					CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
				}
				body := []byte(`{"clusters":[` + string(orgClusterJSON(t, cl)) + `]}`)
				writeJSONResponse(t, w, http.StatusOK, body)
			default:
				t.Errorf("unexpected path %q", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("human table", func(t *testing.T) {
		stdout, _, err := runOrgCmd(t, newServer().URL, "buildkite", "org", "list", testOrgULID)
		require.NoError(t, err)
		assert.Contains(t, stdout, "CREDENTIALS")
		assert.Contains(t, stdout, "CLUSTERS")
		assert.Contains(t, stdout, "acme-inc")
		assert.Contains(t, stdout, "cluster-uuid-1")
		assert.Contains(t, stdout, "web")
		assert.Contains(t, stdout, "TOKEN_LEN")
		assert.NotContains(t, stdout, token, "no token material must ever render")
	})

	t.Run("json", func(t *testing.T) {
		stdout, _, err := runOrgCmd(t, newServer().URL, "buildkite", "org", "list", testOrgULID, "--json")
		require.NoError(t, err)
		assert.Contains(t, stdout, `"credentials"`)
		assert.Contains(t, stdout, `"clusters"`)
		assert.Contains(t, stdout, `"cluster-uuid-1"`)
		assert.NotContains(t, stdout, `"bk_api_token"`, "the wire view has no token field")
		assert.NotContains(t, stdout, token)
	})
}

func TestBuildkiteOrgList_Empty(t *testing.T) {
	var clustersCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ci/buildkite/clusters") {
			clustersCalled = true
		}
		writeJSONResponse(t, w, http.StatusOK, []byte(`{"credentials":[]}`))
	}))
	t.Cleanup(srv.Close)

	stdout, _, err := runOrgCmd(t, srv.URL, "buildkite", "org", "list", testOrgULID)
	require.NoError(t, err)
	assert.Contains(t, stdout, "No Buildkite credentials connected")
	assert.False(t, clustersCalled, "with no credentials there is nothing to query clusters for")
}

func TestBuildkiteOrgClusterAdd_BodyAndVerb(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantWord string
	}{
		{"insert → 201", http.StatusCreated, "✓ Registered"},
		{"update → 200", http.StatusOK, "✓ Updated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotBody = decodeBody(t, r)
				cl := orgBuildkiteClusterView{
					BKOrganization: "acme-inc", Owner: testOrgULID, BKClusterID: "cluster-uuid-1",
					AuthPluginRef: "entire-io/entire-core-auth#v1.2.3",
					CreatedAt:     time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
				}
				writeJSONResponse(t, w, tc.status, orgClusterJSON(t, cl))
			}))
			t.Cleanup(srv.Close)

			stdout, _, err := runOrgCmd(t, srv.URL, "buildkite", "org", "cluster", "add", testOrgULID,
				"--bk-org", "acme-inc", "--bk-cluster-id", "cluster-uuid-1",
				"--auth-plugin-ref", "entire-io/entire-core-auth#v1.2.3")
			require.NoError(t, err)

			assert.Equal(t, http.MethodPost, gotMethod)
			assert.Equal(t, "/api/v1/orgs/"+testOrgULID+"/ci/buildkite/clusters", gotPath)
			assert.Equal(t, "acme-inc", gotBody["bk_organization"])
			assert.Equal(t, "cluster-uuid-1", gotBody["bk_cluster_id"])
			assert.Equal(t, "entire-io/entire-core-auth#v1.2.3", gotBody["auth_plugin_ref"])
			assert.Contains(t, stdout, tc.wantWord)
			assert.Contains(t, stdout, "cluster-uuid-1")
		})
	}
}

func TestBuildkiteOrgClusterAdd_RequiresFlags(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runOrgCmd(t, srv.URL, "buildkite", "org", "cluster", "add", testOrgULID, "--bk-org", "acme-inc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bk-cluster-id")
	assert.False(t, called, "a missing required flag must fail before any HTTP call")
}

func TestBuildkiteOrgDisconnect_Path(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	stdout, _, err := runOrgCmd(t, srv.URL, "buildkite", "org", "disconnect", testOrgULID, "--bk-org", "acme-inc")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, bkCredentialPath+"/acme-inc", gotPath)
	assert.Contains(t, stdout, "✓ Disconnected Buildkite org")
	assert.Contains(t, stdout, "acme-inc")
}

// orgCredsListJSON is the single-credential fixture used inside the list
// handler's credentials envelope.
func orgCredsListJSON(t *testing.T) []byte {
	t.Helper()
	return []byte("[" + string(orgCredJSON(t, "acme-inc", 24)) + "]")
}
