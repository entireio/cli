package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// providers is what /oidc-providers lists.
	providers []coreapi.OIDCProvider
}

const saTestBuildkiteID = "wip_01HGHA00000000000BUILDK1TE"

// saDescribedProviders mirrors core's descriptors.
func saDescribedProviders() []coreapi.OIDCProvider {
	attr := func(name string, required, exact bool, pattern string) coreapi.OIDCProviderAttribute {
		a := coreapi.OIDCProviderAttribute{Name: name, Label: name, Required: required, Exact: exact}
		if pattern != "" {
			a.Pattern = coreapi.NewOptString(pattern)
		}
		return a
	}
	return []coreapi.OIDCProvider{
		{
			ID: saTestProviderID, Issuer: "https://token.actions.githubusercontent.com",
			Name:  coreapi.NewOptString("github-actions"),
			Label: coreapi.NewOptString("GitHub Actions"), DocsURL: coreapi.NewOptString("https://docs.github.com/oidc"),
			SelfServe: coreapi.NewOptBool(true),
			Attributes: []coreapi.OIDCProviderAttribute{
				attr("repository", true, true, `^[^/*]+/[^/*]+$`), attr("ref", false, false, ""),
			},
		},
		{
			ID: saTestBuildkiteID, Issuer: "https://agent.buildkite.com",
			Name: coreapi.NewOptString("buildkite"), Label: coreapi.NewOptString("Buildkite"),
			DocsURL: coreapi.NewOptString("https://buildkite.com/docs/agent/v3/cli-oidc"), SelfServe: coreapi.NewOptBool(true),
			Attributes: []coreapi.OIDCProviderAttribute{
				attr("organization", true, true, ""), attr("pipeline", true, false, ""), attr("branch", false, false, ""),
			},
		},
		{
			ID: "wip_cfaccess", Issuer: "https://acme.cloudflareaccess.com",
			Name: coreapi.NewOptString("cloudflare-access"), Label: coreapi.NewOptString("Cloudflare Access"),
			SelfServe:  coreapi.NewOptBool(false),
			Attributes: []coreapi.OIDCProviderAttribute{attr("common_name", false, false, "")},
		},
	}
}

// withDisplayName sets the slug old cores send.
func withDisplayName(p coreapi.OIDCProvider, name string) coreapi.OIDCProvider {
	p.DisplayName = coreapi.NewOptString(name) //nolint:staticcheck // old cores send only displayName
	return p
}

// saOldCoreProviders is a core without descriptors.
func saOldCoreProviders() []coreapi.OIDCProvider {
	return []coreapi.OIDCProvider{
		withDisplayName(coreapi.OIDCProvider{ID: saTestProviderID, Issuer: "https://token.actions.githubusercontent.com"}, "github-actions"),
		withDisplayName(coreapi.OIDCProvider{ID: "wip_cfaccess", Issuer: "https://acme.cloudflareaccess.com"}, "cloudflare-access"),
	}
}

func newSAFakeCore(t *testing.T) (*saFakeCore, *httptest.Server) {
	t.Helper()
	f := &saFakeCore{
		t:         t,
		bodies:    map[string]string{},
		replies:   map[string]saReply{},
		providers: saDescribedProviders(),
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
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListOIDCProvidersOutputBody{Providers: f.providers})
	case "POST " + saBase + "/" + saTestID + "/bindings":
		writeJSONResponse(t, w, http.StatusCreated, &binding)
	case "GET " + saBase + "/" + saTestID + "/bindings":
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListBindingsOutputBody{Bindings: []coreapi.Binding{binding}})
	case "POST " + saBase + "/" + saTestID + "/secrets":
		writeJSONResponse(t, w, http.StatusCreated, saTestCreatedSecret())
	case "GET " + saBase + "/" + saTestID + "/secrets":
		writeJSONResponse(t, w, http.StatusOK, saTestSecretPage(r.URL.Query().Get("cursor")))
	case "DELETE " + saBase + "/" + saTestID,
		"DELETE " + saBase + "/" + saTestID + "/grants/repo/" + saTestRepoID,
		"DELETE " + saBase + "/" + saTestID + "/bindings/" + saTestBindingID,
		"DELETE " + saBase + "/" + saTestID + "/secrets/" + saTestSecretID:
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
		require.NotContains(t, f.callList(), "DELETE "+saBase+"/"+saTestID)
	})
}

// A missing typed ID is an error.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestSAMissingAccount(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		mutation string
	}{
		{"revoke", []string{"revoke", saTestID, "--repo", "web"}, "DELETE " + saBase + "/" + saTestID + "/grants/repo/" + saTestRepoID},
		{"unbind", []string{"unbind", saTestID, saTestBindingID}, "DELETE " + saBase + "/" + saTestID + "/bindings/" + saTestBindingID},
		{"delete", []string{"delete", saTestID, "--force"}, "DELETE " + saBase + "/" + saTestID},
		{"grant", []string{"grant", saTestID, "--repo", "web", "--role", "reader"}, "POST " + saBase + "/" + saTestID + "/grants"},
		{"bind", []string{"bind", saTestID, "--provider", "github-actions", "--pin", "repository=acme/web"}, saBindRoute},
		// The account resolves before pins get checked.
		{"bind missing pins", []string{"bind", saTestID, "--provider", "buildkite"}, saBindRoute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newSAFakeCore(t)
			f.fail("GET "+saBase+"/"+saTestID, http.StatusNotFound, "service account not found", "")
			_, err := runSA(t, srv.URL, tc.args...)
			require.EqualError(t, err, "no service account "+saTestID)
			require.NotContains(t, f.callList(), tc.mutation)
		})
	}
}

func TestResolveSARef_Grammar(t *testing.T) {
	t.Parallel()

	t.Run("a typed ID is looked up", func(t *testing.T) {
		t.Parallel()
		f, srv := newSAFakeCore(t)
		c, err := coreapi.NewWithBearer(srv.URL, "tok")
		require.NoError(t, err)
		got, err := resolveSARef(t.Context(), c, " "+saTestID+" ")
		require.NoError(t, err)
		require.Equal(t, saTestID, got.ID)
		require.Equal(t, saTestProjectID, got.ProjectID)
		require.Equal(t, saTestID, got.label())
		require.Equal(t, []string{"GET " + saBase + "/" + saTestID}, f.callList())
	})

	t.Run("a lower-case typed ID is upper-cased", func(t *testing.T) {
		t.Parallel()
		f, srv := newSAFakeCore(t)
		c, err := coreapi.NewWithBearer(srv.URL, "tok")
		require.NoError(t, err)
		lower := saIDPrefix + strings.ToLower(strings.TrimPrefix(saTestID, saIDPrefix))
		got, err := resolveSARef(t.Context(), c, lower)
		require.NoError(t, err)
		require.Equal(t, saTestID, got.ID)
		require.Equal(t, []string{"GET " + saBase + "/" + saTestID}, f.callList())
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
