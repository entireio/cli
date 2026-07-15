//go:build internal

package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// testRepoULID is a syntactically valid repo ULID (26 Crockford base32 chars,
// no I/L/O/U) so ResolveNativeRepo short-circuits and the fake server only
// needs to answer the ci-webhooks calls, never a project/repo name lookup.
const (
	testRepoULID = "0123456789ABCDEFGHJKMNPQR5"
	testSubID    = "01HSUBBKWEBHOOK00000000001"
)

// runBuildkiteCmd points the activeCoreClient seam at srvURL and runs the ci
// command tree with the given args (prefixed with "ci"), returning the leaf's
// stdout and error. Building the tree via Register exercises the full cobra
// wiring including the hidden `ci` group and its persistent flags.
// Not parallel: the seam is package-global.
func runBuildkiteCmd(t *testing.T, srvURL string, args ...string) (stdout string, err error) {
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
	return out.String(), err
}

// writeJSONResponse writes status + body as application/json, reporting a write
// error without aborting the handler goroutine (a require/Goexit inside a
// handler is a testifylint go-require violation and would surface to the client
// as a confusing EOF).
func writeJSONResponse(t *testing.T, w http.ResponseWriter, status int, body []byte) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// viewJSON encodes a CIWebhookView fixture. events is always non-nil (the
// generated response validator rejects a nil events array). It marshals via
// pointer so the generated (*CIWebhookView).MarshalJSON is used; a value would
// fall back to field encoding and break on the unset $schema (OptURI) field.
func viewJSON(t *testing.T, v coreapi.CIWebhookView) []byte {
	t.Helper()
	if v.Events == nil {
		v.Events = []string{}
	}
	b, err := json.Marshal(&v)
	require.NoError(t, err)
	return b
}

// listJSON encodes a ListRepoCIWebhooksOutputBody fixture with the given
// subscriptions (non-nil so the response validator accepts it).
func listJSON(t *testing.T, subs []coreapi.CIWebhookView) []byte {
	t.Helper()
	if subs == nil {
		subs = []coreapi.CIWebhookView{}
	}
	out := coreapi.ListRepoCIWebhooksOutputBody{Subscriptions: subs}
	b, err := json.Marshal(&out)
	require.NoError(t, err)
	return b
}

func sampleView() coreapi.CIWebhookView {
	return coreapi.CIWebhookView{
		ID:             testSubID,
		RepoID:         testRepoULID,
		Provider:       "buildkite",
		DisplayName:    "web CI",
		BkOrganization: "acme",
		BkPipeline:     "web",
		RefFilter:      "refs/heads/main",
		Events:         []string{"create", "update"},
		Enabled:        true,
		CreatedAt:      time.Unix(0, 0).UTC(),
		UpdatedAt:      time.Unix(0, 0).UTC(),
	}
}

// decodeBody reads r's JSON body into a generic map for presence/absence
// assertions on the fields the CLI sent.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func TestBuildkiteEnroll_BodyAssemblyAndFreshVsReenroll(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantWord string
	}{
		{"fresh enroll → 201", http.StatusCreated, "✓ Enrolled"},
		{"re-enroll → 200", http.StatusOK, "✓ Re-enrolled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotBody = decodeBody(t, r)
				writeJSONResponse(t, w, tc.status, viewJSON(t, sampleView()))
			}))
			t.Cleanup(srv.Close)

			out, err := runBuildkiteCmd(t, srv.URL,
				"buildkite", "enroll", testRepoULID, "web",
				"--bk-org", "acme", "--bk-cluster-id", "cluster-1",
				"--ref-filter", "refs/heads/main", "--events", "create,update",
				"--display-name", "web CI")
			require.NoError(t, err)

			assert.Equal(t, http.MethodPost, gotMethod)
			assert.Equal(t, "/api/v1/repos/"+testRepoULID+"/ci-webhooks", gotPath)
			assert.Equal(t, "buildkite", gotBody["provider"])
			assert.Equal(t, "acme", gotBody["bk_organization"])
			assert.Equal(t, "cluster-1", gotBody["bk_cluster_id"])
			assert.Equal(t, "web", gotBody["bk_pipeline"])
			assert.Equal(t, "refs/heads/main", gotBody["ref_filter"])
			assert.Equal(t, "web CI", gotBody["display_name"])
			assert.Equal(t, []any{"create", "update"}, gotBody["events"])
			assert.Contains(t, out, tc.wantWord)
			assert.Contains(t, out, testSubID)
		})
	}
}

func TestBuildkiteEnroll_OmitsUnsetOptionalFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		writeJSONResponse(t, w, http.StatusCreated, viewJSON(t, sampleView()))
	}))
	t.Cleanup(srv.Close)

	// No positional pipeline, no ref-filter/events/display-name.
	_, err := runBuildkiteCmd(t, srv.URL,
		"buildkite", "enroll", testRepoULID, "--bk-org", "acme", "--bk-cluster-id", "cluster-1")
	require.NoError(t, err)

	assert.Equal(t, "acme", gotBody["bk_organization"])
	assert.NotContains(t, gotBody, "bk_pipeline", "unset pipeline must be omitted so the server defaults it")
	assert.NotContains(t, gotBody, "ref_filter")
	assert.NotContains(t, gotBody, "events")
	assert.NotContains(t, gotBody, "display_name")
}

func TestBuildkiteEnroll_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusCreated, viewJSON(t, sampleView()))
	}))
	t.Cleanup(srv.Close)

	out, err := runBuildkiteCmd(t, srv.URL,
		"buildkite", "enroll", testRepoULID, "--bk-org", "acme", "--bk-cluster-id", "cluster-1", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"id": "`+testSubID+`"`)
	assert.NotContains(t, out, "✓ Enrolled", "JSON output must not carry the human confirmation line")
}

func TestBuildkiteEnroll_RequiresBkOrg(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := runBuildkiteCmd(t, srv.URL, "buildkite", "enroll", testRepoULID, "--bk-cluster-id", "cluster-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bk-org")
	assert.False(t, called, "a missing required flag must fail before any HTTP call")
}

func TestBuildkiteList_TableAndJSON(t *testing.T) {
	newServer := func() *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/repos/"+testRepoULID+"/ci-webhooks", r.URL.Path)
			writeJSONResponse(t, w, http.StatusOK, listJSON(t, []coreapi.CIWebhookView{sampleView()}))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("human table", func(t *testing.T) {
		out, err := runBuildkiteCmd(t, newServer().URL, "buildkite", "list", testRepoULID)
		require.NoError(t, err)
		assert.Contains(t, out, "ID")
		assert.Contains(t, out, "PROVIDER")
		assert.Contains(t, out, "EVENTS")
		assert.Contains(t, out, testSubID)
		assert.Contains(t, out, "buildkite")
		assert.Contains(t, out, "create,update")
	})

	t.Run("json", func(t *testing.T) {
		out, err := runBuildkiteCmd(t, newServer().URL, "buildkite", "list", testRepoULID, "--json")
		require.NoError(t, err)
		assert.Contains(t, out, `"id": "`+testSubID+`"`)
		assert.Contains(t, out, `"provider": "buildkite"`)
	})
}

func TestBuildkiteList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusOK, listJSON(t, nil))
	}))
	t.Cleanup(srv.Close)

	out, err := runBuildkiteCmd(t, srv.URL, "buildkite", "list", testRepoULID)
	require.NoError(t, err)
	assert.Contains(t, out, "No Buildkite subscriptions")
}

func TestBuildkiteUpdate_OnlySendsChangedFields(t *testing.T) {
	t.Run("enabled only", func(t *testing.T) {
		var gotMethod, gotPath string
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			gotBody = decodeBody(t, r)
			writeJSONResponse(t, w, http.StatusOK, viewJSON(t, sampleView()))
		}))
		t.Cleanup(srv.Close)

		_, err := runBuildkiteCmd(t, srv.URL, "buildkite", "update", testRepoULID, testSubID, "--enabled=false")
		require.NoError(t, err)
		assert.Equal(t, http.MethodPatch, gotMethod)
		assert.Equal(t, "/api/v1/repos/"+testRepoULID+"/ci-webhooks/"+testSubID, gotPath)
		assert.Equal(t, false, gotBody["enabled"])
		assert.NotContains(t, gotBody, "ref_filter")
		assert.NotContains(t, gotBody, "events")
		assert.NotContains(t, gotBody, "display_name")
	})

	t.Run("ref-filter only", func(t *testing.T) {
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody = decodeBody(t, r)
			writeJSONResponse(t, w, http.StatusOK, viewJSON(t, sampleView()))
		}))
		t.Cleanup(srv.Close)

		_, err := runBuildkiteCmd(t, srv.URL, "buildkite", "update", testRepoULID, testSubID, "--ref-filter", "refs/tags/*")
		require.NoError(t, err)
		assert.Equal(t, "refs/tags/*", gotBody["ref_filter"])
		assert.NotContains(t, gotBody, "enabled")
		assert.NotContains(t, gotBody, "events")
		assert.NotContains(t, gotBody, "display_name")
	})

	t.Run("empty events clears the list", func(t *testing.T) {
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody = decodeBody(t, r)
			writeJSONResponse(t, w, http.StatusOK, viewJSON(t, sampleView()))
		}))
		t.Cleanup(srv.Close)

		_, err := runBuildkiteCmd(t, srv.URL, "buildkite", "update", testRepoULID, testSubID, "--events", "")
		require.NoError(t, err)
		assert.Equal(t, []any{}, gotBody["events"], "an explicit empty --events must send a non-nil empty array to clear")
	})
}

func TestBuildkiteUpdate_NothingToUpdate(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := runBuildkiteCmd(t, srv.URL, "buildkite", "update", testRepoULID, testSubID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to update")
	assert.False(t, called, "a no-op update must not issue a PATCH")
}

func TestBuildkiteRemove_TeardownQuery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		wantTeardown string
		wantNote     bool
	}{
		{"default keeps identity", nil, "false", false},
		{"teardown revokes identity", []string{"--teardown"}, "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotTeardown string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotTeardown = r.URL.Query().Get("teardown")
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)

			args := append([]string{"buildkite", "remove", testRepoULID, testSubID}, tc.args...)
			out, err := runBuildkiteCmd(t, srv.URL, args...)
			require.NoError(t, err)
			assert.Equal(t, http.MethodDelete, gotMethod)
			assert.Equal(t, "/api/v1/repos/"+testRepoULID+"/ci-webhooks/"+testSubID, gotPath)
			assert.Equal(t, tc.wantTeardown, gotTeardown)
			assert.Contains(t, out, "✓ Removed subscription "+testSubID)
			if tc.wantNote {
				assert.Contains(t, out, "torn down")
			}
		})
	}
}
