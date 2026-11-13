package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const trailAgentsTestJSON = `[
	{"agent":{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAA","name":"Entire Review","preset":null},"signal":{"value_type":"percent","polarity":"higher_is_better"},
	 "reading":{"percent_value":82,"summary":"Looks right","run_id":"run-1","head_sha":"4a028793e32beed4"},"trend_delta":6,
	 "latest_run":{"id":"run-1","status":"succeeded","head_sha":"4a028793e32beed4","created_at":"2026-11-03T10:00:00Z","error":null},
	 "waiting_for":null,"history":[]},
	{"agent":{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAB","name":"Drift","preset":null},"signal":{"value_type":"size","polarity":"lower_is_better"},
	 "reading":null,"trend_delta":null,"latest_run":null,"waiting_for":"push","history":[],"future_field":true}
]`

// runTrailFindingDashboard runs `trail finding` on change 7. In project mode
// the change read carries agents; in legacy mode the dashboard reads the
// change's detail for them, which answers detailAgents.
func runTrailFindingDashboard(t *testing.T, project bool, agents string, args ...string) (string, error) {
	t.Helper()
	if project {
		setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == projectTrailTestPath+"/changes/"+projectTrailTestChange {
				change := map[string]any{"id": projectTrailTestChange, "number": 7, "trailId": projectTrailTestID, "repositoryId": "repo-id", "branch": "feature/work", "base": "main", "status": "open"}
				if agents != "" {
					change["agents"] = json.RawMessage(agents)
				}
				assert.NoError(t, json.NewEncoder(w).Encode(change))
				return
			}
			if !serveWorkingProjectRead(t, w, r) {
				t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		})
	}
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			t.Errorf("must not write: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/reviews/comments"):
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"comments": []any{}}))
		case r.URL.Path == "/api/v1/repos/repo-id/changes":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"trails": []any{map[string]any{"id": projectTrailTestChange, "number": 7, "branch": "feature/work", "base": "main", "status": "open"}}}))
		case r.URL.Path == "/api/v1/repos/repo-id/changes/7":
			detail := map[string]any{"id": projectTrailTestChange, "number": 7, "branch": "feature/work", "base": "main", "status": "open"}
			if agents != "" {
				detail["agents"] = json.RawMessage(agents)
			}
			assert.NoError(t, json.NewEncoder(w).Encode(detail))
		default:
			t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	selector := "7"
	if project {
		selector = "42"
	}
	cmd := newTrailCmdForMode(project)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"finding", selector, "--repo", "gh/acme/widget"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestTrailFindingDashboard_ShowsTheChangesAgents(t *testing.T) {
	// Serial: replaces global clients.
	for _, project := range []bool{true, false} {
		out, err := runTrailFindingDashboard(t, project, trailAgentsTestJSON)
		require.NoError(t, err, "project=%t", project)
		assert.Contains(t, out, "Agents:")
		assert.Regexp(t, `Entire Review\s+82%\s+\+6\s+succeeded\s+4a028793\s+run-1`, out)
		assert.Regexp(t, `Drift\s+-\s+waits for the next push`, out)
	}
}

func TestTrailFindingDashboard_JSONPassesTheAgentsThrough(t *testing.T) {
	// Serial: replaces global clients.
	out, err := runTrailFindingDashboard(t, true, trailAgentsTestJSON, "--json")
	require.NoError(t, err)
	var got struct {
		Agents []map[string]any `json:"agents"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Agents, 2)
	assert.Equal(t, true, got.Agents[1]["future_field"], "fields the CLI does not model reach --json")
}

func TestTrailFindingDashboard_NoAgents(t *testing.T) {
	// Serial: replaces global clients.
	out, err := runTrailFindingDashboard(t, true, `[]`)
	require.NoError(t, err)
	assert.Contains(t, out, "Agents: no project agent is on this change")

	// A server that sends no agents, or null, leaves the section out of the
	// text and the JSON; null falls back to the change's detail read.
	for _, agents := range []string{"", "null"} {
		out, err = runTrailFindingDashboard(t, true, agents)
		require.NoError(t, err, "agents %q", agents)
		assert.NotContains(t, out, "Agents", "agents %q", agents)
		out, err = runTrailFindingDashboard(t, true, agents, "--json")
		require.NoError(t, err, "agents %q", agents)
		assert.NotContains(t, out, `"agents"`, "agents %q", agents)
	}
}

func TestTrailFindingDashboard_AgentsFailuresKeepTheFindings(t *testing.T) {
	t.Parallel()
	// A failed detail read warns and leaves the agents out.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	var errOut bytes.Buffer
	agents := fetchTrailChangeAgents(t.Context(), &errOut, api.NewClientWithBaseURL("token", srv.URL), api.TrailResource{ID: projectTrailTestChange})
	assert.Nil(t, agents)
	assert.Contains(t, errOut.String(), "Warning: could not read the change's agents")

	// Agents that are not a list say so instead of failing the dashboard.
	var out bytes.Buffer
	require.NoError(t, printTrailChangeAgents(&out, json.RawMessage(`{"not":"a list"}`)))
	assert.Contains(t, out.String(), "Agents: unreadable")
}
