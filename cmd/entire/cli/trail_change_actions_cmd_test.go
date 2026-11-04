package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	trailActionTestRepoRoute = "/api/v1/repos/repo-id/trails/7"
	trailActionTestAgentID   = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
)

// runTrailChangeAction runs a trail subcommand on change 7 of a repository;
// "{trail}" in args stands for the mode's selector.
// Project mode reaches gh/acme/widget through project trail 42; legacy mode
// reaches repo (forge/owner/name) directly. actions answers every request on
// the repository-ID route, and in project mode the project's agent routes,
// and records it.
func runTrailChangeAction(t *testing.T, project bool, repo string, args []string, detail api.TrailResource, actions http.HandlerFunc) (requests []string, stdout string, err error) {
	t.Helper()
	if project {
		setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
			if serveWorkingProjectRead(t, w, r) {
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/v1/gh/acme/agents/") {
				requests = append(requests, r.Method+" "+r.URL.Path)
				actions(w, r)
				return
			}
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		})
	}
	detail.ID, detail.Number, detail.Branch = projectTrailTestChange, 7, "feature/work"
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/trails/gh/acme/widget/7" || r.URL.Path == trailActionTestRepoRoute):
			assert.NoError(t, json.NewEncoder(w).Encode(detail))
		case strings.HasPrefix(r.URL.Path, trailActionTestRepoRoute+"/"):
			requests = append(requests, r.Method+" "+strings.TrimPrefix(r.URL.Path, trailActionTestRepoRoute))
			actions(w, r)
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
	full := make([]string, 0, len(args)+2)
	for _, arg := range args {
		if arg == "{trail}" {
			arg = selector
		}
		full = append(full, arg)
	}
	full = append(full, "--repo", repo)
	cmd.SetArgs(full)
	err = cmd.Execute()
	return requests, out.String(), err
}

func answerJSON(t *testing.T, body any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(body))
	}
}

func TestTrailUpdateBranch_NativeRepoMergesOnTheServer(t *testing.T) {
	// Serial: replaces global clients.
	for _, tc := range []struct {
		sha, says string
	}{
		{"abc123", "branch is now at abc123"},
		{"", "already up to date"},
	} {
		t.Run(tc.says, func(t *testing.T) {
			requests, out, err := runTrailChangeAction(t, false, "et/acme/widget", []string{"update-branch", "{trail}"}, api.TrailResource{},
				answerJSON(t, api.TrailUpdateBranchResponse{MergeCommitSHA: tc.sha}))
			require.NoError(t, err)
			require.Equal(t, []string{"POST /update-branch"}, requests)
			assert.Contains(t, out, tc.says)
		})
	}
}

func TestTrailUpdateBranch_RefusesAGitHubBackedTrailWithoutCallingTheServer(t *testing.T) {
	// Serial: replaces global clients.
	for _, project := range []bool{false, true} {
		t.Run(fmt.Sprintf("project=%t", project), func(t *testing.T) {
			requests, _, err := runTrailChangeAction(t, project, "gh/acme/widget", []string{"update-branch", "{trail}"}, api.TrailResource{Base: "main"},
				func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("unexpected action request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				})
			require.ErrorContains(t, err, "only supports Entire-native repositories")
			require.ErrorContains(t, err, "merge main into feature/work locally and push")
			assert.Empty(t, requests)
		})
	}
}

func TestTrailCIPullRequest_ReportsThePullRequest(t *testing.T) {
	// Serial: replaces global clients.
	requests, out, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"ci-pr", "{trail}"}, api.TrailResource{},
		answerJSON(t, api.TrailCIPullRequestResponse{PRNumber: 314}))
	require.NoError(t, err)
	require.Equal(t, []string{"POST /ci-pr"}, requests)
	assert.Contains(t, out, "backed by pull request #314")

	_, out, err = runTrailChangeAction(t, true, "gh/acme/widget", []string{"ci-pr", "{trail}"}, api.TrailResource{}, answerJSON(t, map[string]any{}))
	require.NoError(t, err)
	assert.Contains(t, out, "did not report its number")
	assert.NotContains(t, out, "#0")
}

func TestTrailRerunReview_RequestsANewReview(t *testing.T) {
	// Serial: replaces global clients.
	requests, out, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"rerun-review", "{trail}"}, api.TrailResource{},
		answerJSON(t, api.TrailReviewRerunResponse{Launched: 0}))
	require.NoError(t, err)
	require.Equal(t, []string{"POST /review-rerun"}, requests)
	assert.Contains(t, out, "Requested a new review of trail #42")
	assert.NotContains(t, strings.ToLower(out), "runner")

	_, out, err = runTrailChangeAction(t, true, "gh/acme/widget", []string{"rerun-review", "{trail}", "--json"}, api.TrailResource{},
		answerJSON(t, api.TrailReviewRerunResponse{Launched: 2}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"launched":2}`, out)
}

func TestTrailRerunReview_RunsOneAgentOnTheSelectedChange(t *testing.T) {
	// Serial: replaces global clients.
	var body map[string]any
	var idempotencyKey string
	requests, out, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"rerun-review", "{trail}", "--agent", trailActionTestAgentID}, api.TrailResource{},
		func(w http.ResponseWriter, r *http.Request) {
			idempotencyKey = r.Header.Get("Idempotency-Key")
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			w.WriteHeader(http.StatusAccepted)
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": "run-1", "status": "queued", "agent": map[string]any{"id": trailActionTestAgentID, "name": "Entire Review"}}))
		})
	require.NoError(t, err)
	require.Equal(t, []string{"POST /api/v1/gh/acme/agents/" + trailActionTestAgentID + "/runs"}, requests, "the project's agent route, not the change's review-rerun")
	assert.NotEmpty(t, idempotencyKey)
	assert.Equal(t, map[string]any{"target": map[string]any{"trail_id": projectTrailTestID, "change_id": projectTrailTestChange}}, body)
	assert.Contains(t, out, "Started Entire Review on trail #42")
	assert.Contains(t, out, "run run-1, queued")
}

func TestTrailRerunReview_RefusesAnAgentThatIsNotAnID(t *testing.T) {
	// Serial: replaces global clients.
	for _, agent := range []string{"", " ", ".", "..", "trail-review"} {
		requests, _, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"rerun-review", "{trail}", "--agent", agent}, api.TrailResource{},
			func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected action request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			})
		require.ErrorContains(t, err, "--agent must be an agent ID", "agent %q", agent)
		assert.Empty(t, requests)
	}
}

func TestTrailRerunReview_AgentNeedsAProjectTrail(t *testing.T) {
	// Serial: replaces global clients.
	requests, _, err := runTrailChangeAction(t, false, "gh/acme/widget", []string{"rerun-review", "{trail}", "--agent", trailActionTestAgentID}, api.TrailResource{},
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected action request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		})
	require.ErrorContains(t, err, "--agent needs a project trail")
	assert.Empty(t, requests)
}

func TestTrailChangeActions_PrintJSON(t *testing.T) {
	// Serial: replaces global clients.
	_, out, err := runTrailChangeAction(t, false, "et/acme/widget", []string{"update-branch", "{trail}", "--json"}, api.TrailResource{},
		answerJSON(t, api.TrailUpdateBranchResponse{MergeCommitSHA: "abc123"}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"mergeCommitSha":"abc123"}`, out)

	_, out, err = runTrailChangeAction(t, true, "gh/acme/widget", []string{"ci-pr", "{trail}", "--json"}, api.TrailResource{},
		answerJSON(t, api.TrailCIPullRequestResponse{PRNumber: 314}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"prNumber":314}`, out)
}

func TestTrailUpdateBranch_ReportsTheServersRefusal(t *testing.T) {
	// Serial: replaces global clients.
	for _, tc := range []struct {
		status int
		says   string
	}{
		{http.StatusConflict, "conflicts with its base"},
		{http.StatusUnprocessableEntity, "trail is merged"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			_, out, err := runTrailChangeAction(t, false, "et/acme/widget", []string{"update-branch", "{trail}"}, api.TrailResource{},
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": tc.says}))
				})
			require.ErrorContains(t, err, tc.says)
			assert.NotContains(t, out, "Updated")
		})
	}
}

func TestTrailChangeAction_LegacyContextCarriesTheRepoID(t *testing.T) {
	// Serial: replaces global clients. Legacy contexts used to leave RepoID
	// empty, which the repository-ID route cannot address.
	requests, _, err := runTrailChangeAction(t, false, "gh/acme/widget", []string{"ci-pr", "{trail}"}, api.TrailResource{},
		answerJSON(t, api.TrailCIPullRequestResponse{PRNumber: 1}))
	require.NoError(t, err)
	require.Equal(t, []string{"POST /ci-pr"}, requests)
}

func TestTrailCIPullRequest_RefusesAnEntireNativeRepoWithoutCallingTheServer(t *testing.T) {
	// Serial: replaces global clients.
	requests, _, err := runTrailChangeAction(t, false, "et/acme/widget", []string{"ci-pr", "{trail}"}, api.TrailResource{},
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected action request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		})
	require.ErrorContains(t, err, "ci-pr only applies to GitHub-backed trails")
	assert.Empty(t, requests)
}
