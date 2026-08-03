package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"

	"github.com/stretchr/testify/require"
)

// TestEncodeTrailShowJSON pins the trail show --json payload: the list-parity
// wire shape (trail_id key, normalized slices) with the detail-endpoint
// description and the browser URL the human output already surfaces.
func TestEncodeTrailShowJSON(t *testing.T) {
	t.Parallel()

	found := api.TrailResource{
		ID:     "tr_1",
		Number: 42,
		Title:  "Improve trail resume",
		Branch: "trail-resume",
		Base:   "main",
		Status: "open",
	}

	var out bytes.Buffer
	err := encodeTrailShowJSON(&out, found, "https://app.entire.io/t/42", "Long form description with <code> & links", true)
	if err != nil {
		t.Fatalf("encodeTrailShowJSON() error = %v", err)
	}

	// User-authored description text must not be HTML-escaped (jsonutil
	// convention): agents should read `<code> &` literally, not <.
	if !strings.Contains(out.String(), "<code> & links") {
		t.Errorf("output HTML-escapes the description: %s", out.String())
	}

	var payload map[string]any
	if unmarshalErr := json.Unmarshal(out.Bytes(), &payload); unmarshalErr != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", unmarshalErr, out.String())
	}
	for key, want := range map[string]any{
		"trail_id":           "tr_1",
		"number":             float64(42),
		"branch":             "trail-resume",
		"title":              "Improve trail resume",
		"body":               "Long form description with <code> & links",
		"url":                "https://app.entire.io/t/42",
		"status":             "open",
		"description_loaded": true,
	} {
		if payload[key] != want {
			t.Errorf("payload[%q] = %v, want %v", key, payload[key], want)
		}
	}
	// list-parity shape: nil slices normalize to [], and the resource "id"
	// key must not appear alongside trail_id.
	if assignees, ok := payload["assignees"].([]any); !ok || assignees == nil {
		t.Errorf("payload[assignees] = %v, want []", payload["assignees"])
	}
	if _, hasID := payload["id"]; hasID {
		t.Errorf("payload carries both id and trail_id: %s", out.String())
	}
}

// TestEncodeTrailShowJSONMarksUnloadedDescription pins the failed-fetch
// distinction: an empty body with description_loaded=false means the
// description could not be fetched, not that the trail has none — mirroring
// the text path's trailDescriptionForDisplay behavior.
func TestEncodeTrailShowJSONMarksUnloadedDescription(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := encodeTrailShowJSON(&out, api.TrailResource{Number: 7, Branch: "feat"}, "", "", false)
	if err != nil {
		t.Fatalf("encodeTrailShowJSON() error = %v", err)
	}
	var payload map[string]any
	if unmarshalErr := json.Unmarshal(out.Bytes(), &payload); unmarshalErr != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", unmarshalErr, out.String())
	}
	if payload["description_loaded"] != false {
		t.Errorf("payload[description_loaded] = %v, want false", payload["description_loaded"])
	}
	if payload["body"] != "" {
		t.Errorf("payload[body] = %v, want empty", payload["body"])
	}
	// url is omitempty: with no web URL resolvable, consumers see key
	// absence, not "".
	if _, hasURL := payload["url"]; hasURL {
		t.Errorf("payload[url] present = %v, want omitted", payload["url"])
	}
}

// TestTrailShowCmdRegistersJSONFlag closes the one read-command gap in the
// trail group: show must accept --json like list, watch, and finding do.
func TestTrailShowCmdRegistersJSONFlag(t *testing.T) {
	t.Parallel()

	cmd := newTrailShowCmd()
	flag := cmd.Flags().Lookup("json")
	if flag == nil {
		t.Fatal("trail show has no --json flag")
	}
	if !strings.Contains(strings.ToLower(flag.Usage), "json") {
		t.Errorf("--json usage = %q, want a JSON description", flag.Usage)
	}
}

// TestRunTrailShowJSON_WiresCommandToEncoder executes `trail show 42 --json`
// end to end against a fake control plane: the happy path must put the
// detail-endpoint description (not the list body) on stdout as parseable
// JSON, and a failed detail fetch must keep stdout pure JSON with
// description_loaded=false while the warning goes to stderr.
func TestRunTrailShowJSON_WiresCommandToEncoder(t *testing.T) {
	// No t.Parallel: uses t.Chdir plus auth/tokenstore package-level test seams.
	detailFails := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"exchanged-token","token_type":"Bearer","expires_in":3600}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trails/gh/acme/repo/42":
			if detailFails {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]api.TrailResource{"trail": {
				ID: "trl_show", Number: 42, Title: "Show me", Branch: "feat/show", Status: "open",
				BodyDocument: &api.TrailBodyDocument{TextSnapshot: "Detail description"},
			}}); err != nil {
				t.Errorf("encode detail response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trails/gh/acme/repo":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{
				ID: "trl_show", Number: 42, Title: "Show me", Branch: "feat/show", Status: "open",
			}}, Total: 1}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	service := tokenstore.CoreKeyringService(srv.URL)
	jwt := makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"me","exp":%d}`, srv.URL, time.Now().Add(2*time.Hour).Unix()))
	require.NoError(t, tokenstore.Set(service, "me", tokenstore.EncodeTokenWithExpiration(jwt, 7200)))
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: srv.URL, Handle: "me", KeychainService: service}
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			return ctxObj, nil
		}))

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	runGitTrailTest(t, repoDir, "remote", "add", "origin", "https://github.com/acme/repo.git")
	t.Chdir(repoDir)

	runShow := func() (string, string, error) {
		cmd := newTrailShowCmd()
		cmd.SetContext(context.Background())
		cmd.Flags().Bool("insecure-http-auth", true, "")
		require.NoError(t, cmd.Flags().Set("insecure-http-auth", "true"))
		cmd.SetArgs([]string{"42", "--json"})
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		err := cmd.Execute()
		return out.String(), errOut.String(), err
	}

	// Happy path: detail body supersedes the (empty) list body.
	out, errOut, err := runShow()
	require.NoError(t, err, "stderr: %s", errOut)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &payload), "stdout must be pure JSON: %s", out)
	require.Equal(t, "trl_show", payload["trail_id"])
	require.Equal(t, "Detail description", payload["body"])
	require.Equal(t, true, payload["description_loaded"])

	// Detail fetch failure: warning on stderr, stdout still pure JSON, and
	// with no list body the description is marked unloaded.
	detailFails = true
	out, errOut, err = runShow()
	require.NoError(t, err, "stderr: %s", errOut)
	payload = map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(out), &payload), "stdout must be pure JSON: %s", out)
	require.Empty(t, payload["body"])
	require.Equal(t, false, payload["description_loaded"])
	require.Contains(t, errOut, "could not load trail description")
}
