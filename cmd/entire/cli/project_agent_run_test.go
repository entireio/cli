package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const (
	agentRunTestID      = "01ARZ3NDEKTSV4RRFFQ69G5RAA"
	agentRunTestRetryID = "01ARZ3NDEKTSV4RRFFQ69G5RTY"
	agentRunTestAgentID = "01ARZ3NDEKTSV4RRFFQ69G5AGT"
)

type agentRunRequest struct {
	Method, Path, IdempotencyKey string
	Query                        string
}

// stubProjectAgentRuns points the agent commands at handler as project
// gh/acme, recording each request.
func stubProjectAgentRuns(t *testing.T, handler http.HandlerFunc) *[]agentRunRequest {
	t.Helper()
	var requests []agentRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, agentRunRequest{Method: r.Method, Path: r.URL.Path, IdempotencyKey: r.Header.Get("Idempotency-Key"), Query: r.URL.RawQuery})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	old := resolveProjectAgentTarget
	t.Cleanup(func() { resolveProjectAgentTarget = old })
	resolveProjectAgentTarget = func(*cobra.Command) (*projectAgentTarget, error) {
		return &projectAgentTarget{client: api.NewClientWithBaseURL("token", srv.URL), host: "gh", project: "acme", base: agentTestBase}, nil
	}
	return &requests
}

func agentRunTestJSON(id, status string) map[string]any {
	return map[string]any{
		"id": id, "status": status, "error": nil, "attempt": 1, "retry_of": nil,
		"agent":    map[string]any{"id": agentRunTestAgentID, "version": 3, "name": "Entire Review"},
		"trigger":  map[string]any{"kind": "push", "id": nil},
		"trail_id": "01ARZ3NDEKTSV4RRFFQ69G5TRL", "change_id": "01ARZ3NDEKTSV4RRFFQ69G5CHG", "branch": "feature/work",
		"created_at": "2026-11-03T10:00:00Z", "updated_at": "2026-11-03T10:05:00Z", "future_field": true,
	}
}

func TestProjectAgentRunList_ShowsOnePageWithItsCursor(t *testing.T) {
	// Serial: replaces the project agent target.
	requests := stubProjectAgentRuns(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"items": []any{agentRunTestJSON(agentRunTestID, "succeeded")}, "next_cursor": "c2"}))
	})
	out, _, err := executeProjectAgent(t, "run", "list", "--agent", agentRunTestAgentID, "--status", "succeeded", "--limit", "5")
	require.NoError(t, err)
	require.Len(t, *requests, 1)
	assert.Equal(t, agentTestBase+"/runs", (*requests)[0].Path)
	assert.Equal(t, "agent_id="+agentRunTestAgentID+"&per_page=5&status=succeeded", (*requests)[0].Query)
	assert.Regexp(t, agentRunTestID+`\s+Entire Review\s+succeeded\s+push\s+feature/work\s+2026-11-03T10:00:00Z`, out)
	assert.Contains(t, out, "More runs: entire project agent run list --cursor c2 --agent "+agentRunTestAgentID+" --status succeeded --limit 5\n",
		"the hint repeats the filters, which the cursor is bound to, and the page size")

	// The server refuses a cursor without the filters it was issued for.
	_, _, err = executeProjectAgent(t, "run", "list", "--cursor", "c2", "--agent", agentRunTestAgentID, "--status", "succeeded", "--limit", "5")
	require.NoError(t, err)
	assert.Equal(t, "agent_id="+agentRunTestAgentID+"&cursor=c2&per_page=5&status=succeeded", (*requests)[1].Query)

	// The cursor is bound to the project too, so a named one is repeated.
	out, _, err = executeProjectAgent(t, "run", "list", "--project", "et/other")
	require.NoError(t, err)
	assert.Contains(t, out, "More runs: entire project agent run list --cursor c2 --project et/other\n")

	out, _, err = executeProjectAgent(t, "run", "list", "--cursor", "c2", "--json")
	require.NoError(t, err)
	assert.Equal(t, "cursor=c2&per_page=20", (*requests)[3].Query)
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &page), out)
	require.Len(t, page.Items, 1)
	assert.Equal(t, true, page.Items[0]["future_field"], "--json passes the server's items through")
}

func TestProjectAgentRunList_RefusesBadFiltersBeforeAnyRequest(t *testing.T) {
	// Serial: replaces the project agent target.
	requests := stubProjectAgentRuns(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"--agent", "review"}, "--agent must be an agent ID"},
		{[]string{"--status", "done"}, "--status must be one of"},
		{[]string{"--limit", "0"}, "--limit must be between 1 and 100"},
		{[]string{"--limit", "101"}, "--limit must be between 1 and 100"},
	} {
		_, _, err := executeProjectAgent(t, append([]string{"run", "list"}, tc.args...)...)
		require.ErrorContains(t, err, tc.says, "args %v", tc.args)
	}
	assert.Empty(t, *requests)
}

func TestProjectAgentRunShow_PrintsTheRun(t *testing.T) {
	// Serial: replaces the project agent target.
	run := agentRunTestJSON(agentRunTestID, "failed")
	run["error"] = map[string]any{"code": nil, "message": "sandbox timed out"}
	run["target"] = map[string]any{"trail_id": "01ARZ3NDEKTSV4RRFFQ69G5TRL", "change_id": "01ARZ3NDEKTSV4RRFFQ69G5CHG", "ref": "feature/work", "head_sha": "4a028793e32beed4"}
	run["review_changes_result"] = map[string]any{"summary": "Two risky spots", "signal": map[string]any{"value_type": "percent", "value": 64, "rationale": "x"}, "findings": []any{map[string]any{}, map[string]any{}}}
	run["diagnostic"] = "panic: boom"
	requests := stubProjectAgentRuns(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(run))
	})
	out, _, err := executeProjectAgent(t, "run", "show", agentRunTestID)
	require.NoError(t, err)
	assert.Equal(t, []agentRunRequest{{Method: http.MethodGet, Path: agentTestBase + "/runs/" + agentRunTestID}}, *requests)
	for _, want := range []string{
		"Agent:     Entire Review (" + agentRunTestAgentID + ", version 3)",
		"Status:    failed: sandbox timed out",
		"Change:    feature/work at 4a028793",
		"Reading:   64%",
		"Findings:  2",
		"Two risky spots",
		"panic: boom",
	} {
		assert.Contains(t, out, want)
	}
}

func TestProjectAgentRunCancel(t *testing.T) {
	// Serial: replaces the project agent target.
	for _, tc := range []struct{ status, says string }{
		{"canceled", "Canceled run " + agentRunTestID},
		{"succeeded", "had already finished: succeeded"},
	} {
		requests := stubProjectAgentRuns(t, func(w http.ResponseWriter, _ *http.Request) {
			assert.NoError(t, json.NewEncoder(w).Encode(agentRunTestJSON(agentRunTestID, tc.status)))
		})
		out, _, err := executeProjectAgent(t, "run", "cancel", agentRunTestID)
		require.NoError(t, err)
		assert.Equal(t, []agentRunRequest{{Method: http.MethodPost, Path: agentTestBase + "/runs/" + agentRunTestID + "/cancel"}}, *requests)
		assert.Contains(t, out, tc.says)
	}
}

func TestProjectAgentRunRetry_SendsAnIdempotencyKey(t *testing.T) {
	// Serial: replaces the project agent target.
	retried := agentRunTestJSON(agentRunTestRetryID, "queued")
	retried["attempt"] = 2
	requests := stubProjectAgentRuns(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		assert.NoError(t, json.NewEncoder(w).Encode(retried))
	})
	out, _, err := executeProjectAgent(t, "run", "retry", agentRunTestID)
	require.NoError(t, err)
	require.Len(t, *requests, 1)
	assert.Equal(t, agentTestBase+"/runs/"+agentRunTestID+"/retry", (*requests)[0].Path)
	assert.NotEmpty(t, (*requests)[0].IdempotencyKey)
	assert.Contains(t, out, "Retrying run "+agentRunTestID+" as run "+agentRunTestRetryID+" (attempt 2, queued)")

	out, _, err = executeProjectAgent(t, "run", "retry", agentRunTestID, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"future_field": true`)
}

func TestProjectAgentRun_RefusesARunIDThatIsNotOne(t *testing.T) {
	// Serial: replaces the project agent target.
	requests := stubProjectAgentRuns(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	for _, verb := range []string{"show", "cancel", "retry"} {
		for _, id := range []string{"", ".", "..", "run-1"} {
			_, _, err := executeProjectAgent(t, "run", verb, id)
			require.ErrorContains(t, err, "expected a run ID", "%s %q", verb, id)
		}
	}
	assert.Empty(t, *requests)
}
