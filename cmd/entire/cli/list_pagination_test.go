package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/stretchr/testify/require"
)

// TestOrgList_FollowsCursor drives `org list` against a two-page fake control
// plane and asserts the command walks every page (COR-580). Without the cursor
// loop only the first page's orgs would render, silently hiding the rest.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestOrgList_FollowsCursor(t *testing.T) {
	var gotCursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orgs" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		cursor := r.URL.Query().Get("pageToken")
		gotCursors = append(gotCursors, cursor)
		w.Header().Set("Content-Type", "application/json")
		var body coreapi.ListOrgsOutputBody
		switch cursor {
		case "":
			body.Orgs = []coreapi.Org{{ID: "01ORG1", Name: "acme"}, {ID: "01ORG2", Name: "globex"}}
			body.NextPageToken = coreapi.NewOptString("c1")
		case "c1":
			body.Orgs = []coreapi.Org{{ID: "01ORG3", Name: "initech"}}
		default:
			t.Errorf("unexpected cursor %q", cursor)
		}
		if err := writeJSON(w, &body); err != nil {
			t.Errorf("encode orgs: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })

	cmd := newOrgListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	require.NoError(t, cmd.ExecuteContext(t.Context()))

	stdout := out.String()
	require.Contains(t, stdout, "acme")
	require.Contains(t, stdout, "globex")
	// The second page only renders because the command followed nextPageToken.
	require.Contains(t, stdout, "initech")
	require.Equal(t, []string{"", "c1"}, gotCursors)
}

// TestMirrorList_FollowsCursor covers the ListMirrors endpoint paginated by
// COR-583: `repo mirror list` must walk every page the same way the other list
// commands do.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestMirrorList_FollowsCursor(t *testing.T) {
	var gotTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mirrors" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		token := r.URL.Query().Get("pageToken")
		gotTokens = append(gotTokens, token)
		w.Header().Set("Content-Type", "application/json")
		var body coreapi.ListMirrorsOutputBody
		switch token {
		case "":
			body.Mirrors = []coreapi.Mirror{
				{Owner: "acme", Repo: "web", ClusterHost: "h"},
				{Owner: "acme", Repo: "api", ClusterHost: "h"},
			}
			body.NextPageToken = coreapi.NewOptString("m1")
		case "m1":
			body.Mirrors = []coreapi.Mirror{{Owner: "acme", Repo: "cli", ClusterHost: "h"}}
		default:
			t.Errorf("unexpected pageToken %q", token)
		}
		if err := writeJSON(w, &body); err != nil {
			t.Errorf("encode mirrors: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })

	cmd := newRepoMirrorListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	require.NoError(t, cmd.ExecuteContext(t.Context()))

	stdout := out.String()
	require.Contains(t, stdout, "acme/web")
	require.Contains(t, stdout, "acme/api")
	// The second page only renders because the command followed nextPageToken.
	require.Contains(t, stdout, "acme/cli")
	require.Equal(t, []string{"", "m1"}, gotTokens)
}
