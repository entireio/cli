package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/coreapi"
)

func TestProjectAgentSetupRequiresExplicitConsent(t *testing.T) {
	t.Parallel()
	cmd := newProjectAgentSetupCmd()
	cmd.SetArgs([]string{"--name", "runner"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, cmd.Execute(), "requires confirmation")
}

func TestProjectAgentSetup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                         string
		create, noConsent, wrongProject, bindFailure bool
		wantError                                    string
	}{
		{name: "existing account preserves writer grant"},
		{name: "create account and reader grant", create: true},
		{name: "refuse without confirmation", noConsent: true, wantError: "requires confirmation"},
		{name: "refuse cross project", wrongProject: true, wantError: "must belong"},
		{name: "partial failure names recovery account", create: true, bindFailure: true, wantError: "retry with --service-account " + saTestID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake, coreServer := newSAFakeCore(t)
			core, err := coreapi.NewWithBearer(coreServer.URL, "token")
			require.NoError(t, err)
			var binds int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET " + agentTestBase + "/environments":
					writeJSONResponse(t, w, http.StatusOK, map[string]any{"items": []map[string]any{{"id": "env", "managed": true, "runs_on": "self_hosted"}}})
				case "PUT " + agentTestBase + "/environments/env/runners/" + saTestID:
					binds++
					if tc.bindFailure {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					writeJSONResponse(t, w, http.StatusOK, map[string]any{"service_account_id": saTestID})
				default:
					t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			target := &projectAgentTarget{client: api.NewClientWithBaseURL("token", srv.URL), projectID: saTestProjectID, host: "gh", project: "acme", base: agentTestBase}
			if tc.wrongProject {
				target.projectID = "other"
			}
			cmd := newProjectAgentSetupCmd()
			cmd.SetContext(t.Context())
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			require.NoError(t, cmd.Flags().Set("json", "true"))
			if !tc.noConsent {
				require.NoError(t, cmd.Flags().Set("yes", "true"))
			}
			opts := projectAgentSetupOptions{serviceAccount: saTestID}
			if tc.create {
				opts = projectAgentSetupOptions{name: "runner"}
			}
			err = setupProjectAgent(cmd, core, target, saTestRepoID, "gh/acme/web", opts)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
				var result map[string]string
				require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
				require.Equal(t, saTestID, result["service_account_id"])
				require.Contains(t, stderr.String(), "ALL self-hosted agents")
				require.Contains(t, stderr.String(), "No credentials were created")
			}
			if tc.noConsent || tc.wrongProject {
				require.Zero(t, binds)
				for _, call := range fake.callList() {
					require.True(t, strings.HasPrefix(call, "GET "), call)
				}
			} else {
				require.Equal(t, 1, binds)
				grantRoute := "POST " + saBase + "/" + saTestID + "/grants"
				if tc.create {
					require.JSONEq(t, `{"project_id":"`+saTestProjectID+`","name":"runner"}`, fake.body("POST "+saBase))
					require.JSONEq(t, `{"resourceType":"repo","resourceId":"`+saTestRepoID+`","role":"reader"}`, fake.body(grantRoute))
				} else {
					require.NotContains(t, fake.callList(), grantRoute)
				}
			}
			for _, call := range fake.callList() {
				require.NotContains(t, call, "/secrets")
			}
		})
	}
}

func TestProjectAgentSetupEnvironmentLookup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, want, wantError string }{
		{"managed only", `{"items":[{"id":"legacy","runs_on":"self_hosted"},{"id":"hosted","managed":true,"runs_on":"entire"},{"id":"good","managed":true,"runs_on":"self_hosted"}]}`, "good", ""},
		{"missing", `{"items":[]}`, "", "create a custom agent"},
		{"archived", `{"items":[{"id":"old","managed":true,"runs_on":"self_hosted","archived_at":"2026-01-01T00:00:00Z"}]}`, "", "no managed"},
		{"ambiguous", `{"items":[{"id":"a","managed":true,"runs_on":"self_hosted"},{"id":"b","managed":true,"runs_on":"self_hosted"}]}`, "", "multiple managed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			defer srv.Close()
			target := &projectAgentTarget{client: api.NewClientWithBaseURL("token", srv.URL), base: agentTestBase}
			got, err := projectAgentSetupEnvironmentID(t.Context(), target)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
		})
	}
}
