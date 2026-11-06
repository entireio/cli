package cli

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSAGrant(t *testing.T) {
	route := "POST " + saBase + "/" + saTestID + "/grants"

	t.Run("a bare repo name resolves in the account's project", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "grant", saTestID, "--repo", "web", "--role", "writer")
		require.NoError(t, err)
		require.Contains(t, out, "✓ Granted writer on repo /et/acme/web ("+saTestRepoID+") to "+saTestID)
		require.JSONEq(t, `{"resourceType":"repo","resourceId":"`+saTestRepoID+`","role":"writer"}`, f.body(route))
		require.Contains(t, f.callList(), "GET "+saBase+"/"+saTestID, "a typed ID needs a lookup for its project")
	})

	t.Run("a repo path and a project/name ref", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "/et/acme/web", "--role", "reader")
		require.NoError(t, err)
		require.Contains(t, out, "to acme/ci ("+saTestID+")")
		require.JSONEq(t, `{"resourceType":"repo","resourceId":"`+saTestRepoID+`","role":"reader"}`, f.body(route))
	})

	t.Run("a ULID-shaped value is a name in the account's project", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "01HZX7QABCDEFGHJKMNPQRSTVW", "--role", "reader")
		require.NoError(t, err)
		require.Contains(t, f.callList(), "GET /api/v1/projects/"+saTestProjectID+"/repos")
		require.JSONEq(t, `{"resourceType":"repo","resourceId":"`+saTestRepoID+`","role":"reader"}`, f.body(route))
	})

	t.Run("a ULID-shaped miss does not suggest a ULID", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.fail("GET /api/v1/projects/"+saTestProjectID+"/repos", http.StatusNotFound, "repo not found", "")
		_, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "01HZX7QABCDEFGHJKMNPQRSTVW", "--role", "reader")
		require.EqualError(t, err, `no repo named "01HZX7QABCDEFGHJKMNPQRSTVW" in the service account's project; --repo takes a repo name or its /et/<project>/<repo> path, not a ULID`)
		require.NotContains(t, f.callList(), route)
	})

	t.Run("a forgeless path suggests the native one", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "acme/web", "--role", "reader")
		require.EqualError(t, err, `repo ref "acme/web" must name its forge — did you mean /et/acme/web?`)
		require.NotContains(t, f.callList(), route)
	})

	t.Run("project grant", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "grant", "acme/ci", "--project", "acme", "--role", "admin")
		require.NoError(t, err)
		require.Contains(t, out, "✓ Granted admin on project acme ("+saTestProjectID+")")
		require.JSONEq(t, `{"resourceType":"project","resourceId":"`+saTestProjectID+`","role":"admin"}`, f.body(route))
	})

	t.Run("422 outside the project passes through", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.fail(route, http.StatusUnprocessableEntity, "resource is outside the service account's project", "")
		_, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "/et/acme/web", "--role", "writer")
		require.EqualError(t, err, "resource is outside the service account's project")
	})

	t.Run("403 passes through", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.fail(route, http.StatusForbidden, "permission denied", "")
		_, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "web", "--role", "writer")
		require.EqualError(t, err, "permission denied")
	})

	t.Run("a bad role sends nothing", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "web", "--role", "owner")
		require.EqualError(t, err, "--role must be one of reader, writer, admin")
		require.Empty(t, f.callList())
	})

	t.Run("repo and project are exclusive", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		_, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", "web", "--project", "acme", "--role", "reader")
		require.ErrorContains(t, err, "none of the others can be")
	})
}

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSARevoke(t *testing.T) {
	route := "DELETE " + saBase + "/" + saTestID + "/grants/repo/" + saTestRepoID

	t.Run("revokes a repo grant", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "revoke", "acme/ci", "--repo", "web")
		require.NoError(t, err)
		require.Contains(t, out, "✓ Revoked repo /et/acme/web ("+saTestRepoID+") from acme/ci ("+saTestID+")")
		require.Contains(t, f.callList(), route)
	})

	t.Run("a missing grant is not an error", func(t *testing.T) {
		f, srv := newSAFakeCore(t)
		f.fail(route, http.StatusNotFound, "grant not found", "")
		out, err := runSA(t, srv.URL, "revoke", "acme/ci", "--repo", "web")
		require.NoError(t, err)
		require.Contains(t, out, "no such grant; nothing to revoke")
	})

	t.Run("lists grants", func(t *testing.T) {
		_, srv := newSAFakeCore(t)
		out, err := runSA(t, srv.URL, "grants", saTestID)
		require.NoError(t, err)
		require.Contains(t, out, "TYPE")
		require.Contains(t, out, saTestRepoID)
		require.Contains(t, out, "writer")
	})
}
