package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	saTestProjectID  = "01HZX7QABCDEFGHJKMNPQRSTPJ"
	saTestRepoID     = "01HZX7QABCDEFGHJKMNPQRSTRP"
	saTestID         = "svc_01HZX7QABCDEFGHJKMNPQRSTSA"
	saTestBindingID  = "eib_01HZX7QABCDEFGHJKMNPQRSTBD"
	saTestProviderID = "wip_01HGHA00000000000GHACT10NS"

	saBase = "/api/v1/service-accounts"
)

// saReply overrides one route's answer.
type saReply struct {
	status int
	detail string
	typ    string
}

// saFakeCore serves the routes these commands call.
type saFakeCore struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string
	bodies   map[string]string
	replies  map[string]saReply
	accounts []coreapi.ServiceAccountWithGrants
	noGitHub bool
}

func newSAFakeCore(t *testing.T) (*saFakeCore, *httptest.Server) {
	t.Helper()
	f := &saFakeCore{
		t:       t,
		bodies:  map[string]string{},
		replies: map[string]saReply{},
		accounts: []coreapi.ServiceAccountWithGrants{{
			AccountId: saTestID, Name: "ci", Status: "active",
			ProjectID: coreapi.NewOptString(saTestProjectID), Grants: []string{},
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

// fail makes route answer with a problem.
func (f *saFakeCore) fail(route string, status int, detail, typ string) {
	f.replies[route] = saReply{status: status, detail: detail, typ: typ}
}

func (f *saFakeCore) serve(w http.ResponseWriter, r *http.Request) {
	t := f.t
	route := r.Method + " " + r.URL.Path
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read body: %v", err)
	}
	f.mu.Lock()
	f.calls = append(f.calls, route)
	f.bodies[route] = string(body)
	reply, failed := f.replies[route]
	f.mu.Unlock()
	if failed {
		writeSAProblem(t, w, reply)
		return
	}
	sa := coreapi.ServiceAccount{
		AccountId: saTestID, Name: "ci", Status: "active",
		ProjectID: coreapi.NewOptString(saTestProjectID), CreatedAt: time.Unix(0, 0).UTC(),
	}
	binding := coreapi.Binding{
		ID: saTestBindingID, AccountId: saTestID, ProviderId: saTestProviderID,
		AttributeFilter: []byte(`{"repository": "acme/web"}`),
	}
	switch route {
	case "GET /api/v1/projects":
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{
			ID: saTestProjectID, Name: "acme", OwnerType: coreapi.ProjectOwnerTypeOrg,
		})})
	case "GET /api/v1/projects/" + saTestProjectID + "/repos":
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{
			ID: saTestRepoID, Name: "web", Path: coreapi.NewOptString("/et/acme/web"), OwningProjectId: saTestProjectID,
		})})
	case "POST /api/v1/repos/resolve":
		writeJSONResponse(t, w, http.StatusOK, nativeResolution("acme/web", saTestRepoID))
	case "GET " + saBase:
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListServiceAccountsOutputBody{ServiceAccounts: f.accounts})
	case "POST " + saBase:
		writeJSONResponse(t, w, http.StatusCreated, &sa)
	case "GET " + saBase + "/" + saTestID:
		writeJSONResponse(t, w, http.StatusOK, &sa)
	case "POST " + saBase + "/" + saTestID + "/grants":
		writeJSONResponse(t, w, http.StatusCreated, &coreapi.GrantServiceAccountAccessOutputBody{Status: "granted"})
	case "GET " + saBase + "/" + saTestID + "/grants":
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListServiceAccountGrantsOutputBody{Grants: []coreapi.ServiceAccountGrant{{
			ResourceType: "repo", ResourceId: saTestRepoID, ResourceName: coreapi.NewOptString("web"), Role: "writer",
		}}})
	case "GET /api/v1/oidc-providers":
		providers := []coreapi.OIDCProvider{{ID: "wip_other", Issuer: "https://example.com"}}
		if !f.noGitHub {
			providers = append(providers, coreapi.OIDCProvider{ID: saTestProviderID, Issuer: "https://token.actions.githubusercontent.com"})
		}
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListOIDCProvidersOutputBody{Providers: providers})
	case "POST " + saBase + "/" + saTestID + "/bindings":
		writeJSONResponse(t, w, http.StatusCreated, &binding)
	case "GET " + saBase + "/" + saTestID + "/bindings":
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListBindingsOutputBody{Bindings: []coreapi.Binding{binding}})
	case "DELETE " + saBase + "/" + saTestID,
		"DELETE " + saBase + "/" + saTestID + "/grants/repo/" + saTestRepoID,
		"DELETE " + saBase + "/" + saTestID + "/bindings/" + saTestBindingID:
		w.WriteHeader(http.StatusNoContent)
	default:
		t.Errorf("unexpected %s", route)
		w.WriteHeader(http.StatusTeapot)
	}
}

func (f *saFakeCore) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *saFakeCore) body(route string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[route]
}

func writeSAProblem(t *testing.T, w http.ResponseWriter, r saReply) {
	t.Helper()
	problem := map[string]any{"status": r.status, "detail": r.detail}
	if r.typ != "" {
		problem["type"] = r.typ
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(r.status)
	if err := json.NewEncoder(w).Encode(problem); err != nil {
		t.Errorf("write problem: %v", err)
	}
}

func runSA(t *testing.T, srvURL string, args ...string) (string, error) {
	t.Helper()
	stdout, _, err := runCoreCmd(t, newServiceAccountCmd, srvURL, args...)
	return stdout, err
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSACreate(t *testing.T) {
	route := "POST " + saBase

	t.Run("creates and prints the next step", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "create", "acme", "ci")
		require.NoError(t, err)
		require.Contains(t, out, "✓ Created service account acme/ci ("+saTestID+")")
		require.Contains(t, out, "Next: entire service-account grant "+saTestID+" --repo <repo> --role writer")
		require.JSONEq(t, `{"project_id":"`+saTestProjectID+`","name":"ci"}`, f.body(route))
	})

	t.Run("json prints the wire record", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "create", "acme", "ci", "--json")
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		require.Equal(t, saTestID, got["accountId"])
		require.Equal(t, saTestProjectID, got["project_id"])
	})

	refusals := []struct {
		name   string
		status int
		detail string
		typ    string
		want   string
	}{
		{"typed name in use", http.StatusConflict, "name already in use in project", saProblemNameInUse, "service account acme/ci already exists"},
		{"typed quota", http.StatusConflict, "service account limit reached", saProblemQuota, "owner of project acme reached its service account limit"},
		{"untyped 409 keeps core's prose", http.StatusConflict, "name already in use in project", "", "name already in use in project"},
		{"403 names the project", http.StatusForbidden, "permission denied", "", "you need manage access to project acme"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			f.fail(route, tc.status, tc.detail, tc.typ)
			_, err := runSA(t, srv.URL, "create", "acme", "ci")
			require.EqualError(t, err, tc.want)
		})
	}
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSAListGetDelete(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "list", "acme")
		require.NoError(t, err)
		require.Contains(t, out, saTestID)
		require.Contains(t, out, "ci")
	})

	t.Run("list 403 passes through", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.fail("GET "+saBase, http.StatusForbidden, "permission denied", "")
		_, err := runSA(t, srv.URL, "list", "acme")
		require.EqualError(t, err, "permission denied")
	})

	t.Run("get by project/name", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "get", "acme/ci")
		require.NoError(t, err)
		require.Contains(t, out, saTestProjectID)
	})

	t.Run("get misses an unknown name", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "get", "acme/deploy")
		require.EqualError(t, err, `no service account "deploy" in project acme`)
	})

	t.Run("delete with --force", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "delete", saTestID, "--force")
		require.NoError(t, err)
		require.Contains(t, out, "✓ Deleted service account "+saTestID)
		require.Contains(t, f.callList(), "DELETE "+saBase+"/"+saTestID)
	})

	t.Run("delete refuses without a prompt", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "delete", saTestID)
		require.ErrorContains(t, err, "refusing to delete")
		require.Empty(t, f.callList())
	})
}

func TestResolveSARef_Grammar(t *testing.T) {
	t.Parallel()

	t.Run("a typed ID needs no lookup", func(t *testing.T) {
		t.Parallel()
		got, err := resolveSARef(t.Context(), nil, " "+saTestID+" ")
		require.NoError(t, err)
		require.Equal(t, saRef{ID: saTestID}, got)
		require.Equal(t, saTestID, got.label())
	})

	for _, ref := range []string{"ci", "acme/", "/ci", "svc_nope"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			_, err := resolveSARef(t.Context(), nil, ref)
			require.ErrorContains(t, err, "use svc_<id> or <project>/<name>")
		})
	}

	t.Run("a name keeps its slashes", func(t *testing.T) {
		t.Parallel()
		project, name, err := parseSAPath("acme/ci/nightly")
		require.NoError(t, err)
		require.Equal(t, "acme", project)
		require.Equal(t, "ci/nightly", name)
	})
}
