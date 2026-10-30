package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const trailActionTestRepoRoute = "/api/v1/repos/repo-id/trails/7"

// runTrailChangeAction runs a trail subcommand on change 7 of a repository;
// "{trail}" in args stands for the mode's selector.
// Project mode reaches gh/acme/widget through project trail 42; legacy mode
// reaches repo (forge/owner/name) directly. actions answers every request on
// the repository-ID route and records it.
func runTrailChangeAction(t *testing.T, project bool, repo string, args []string, detail api.TrailResource, actions http.HandlerFunc) (requests []string, stdout string, err error) {
	t.Helper()
	if project {
		setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
			if !serveWorkingProjectRead(t, w, r) {
				t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
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
}

func TestTrailRunnerStart_ReportsTheLaunchOutcome(t *testing.T) {
	// Serial: replaces global clients.
	for _, tc := range []struct {
		outcome, says string
	}{
		{"launched", "Started runner trail-review"},
		{"already_in_progress", "already running"},
		{"already_complete", "nothing started"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			requests, out, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"runner", "start", "{trail}", "trail-review"}, api.TrailResource{},
				answerJSON(t, api.TrailRunnerLaunchResponse{RunID: "run-1", Outcome: tc.outcome}))
			require.NoError(t, err)
			require.Equal(t, []string{"POST /runs/trail-review"}, requests)
			assert.Contains(t, out, tc.says)
			assert.Contains(t, out, "run-1")
		})
	}
}

func TestTrailRunnerRerunReview_ReportsLaunchedRunners(t *testing.T) {
	// Serial: replaces global clients.
	requests, out, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"runner", "rerun-review", "{trail}"}, api.TrailResource{},
		answerJSON(t, api.TrailReviewRerunResponse{Launched: 2}))
	require.NoError(t, err)
	require.Equal(t, []string{"POST /review-rerun"}, requests)
	assert.Contains(t, out, "Started 2 review runner(s)")
}

func TestTrailRunnerList_ReadsRunnersFromTheDetail(t *testing.T) {
	// Serial: replaces global clients.
	detail := api.TrailResource{Runners: json.RawMessage(`[{"runner_id":"trail-review","state":"evaluated","outcome":"passed","evaluated_at_sha":"4a028793e32beed49d873b90c829ec4f20887bdf","started_at":"2026-10-29T15:44:49Z"}]`)}
	noActions := func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected action request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}

	_, out, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"runner", "list", "{trail}"}, detail, noActions)
	require.NoError(t, err)
	assert.Regexp(t, `trail-review\s+evaluated\s+passed\s+4a028793\s+2026-10-29T15:44:49Z`, out)

	_, out, err = runTrailChangeAction(t, true, "gh/acme/widget", []string{"runner", "list", "{trail}", "--json"}, detail, noActions)
	require.NoError(t, err)
	var runners []api.TrailRunnerState
	require.NoError(t, json.Unmarshal([]byte(out), &runners), out)
	require.Len(t, runners, 1)
	assert.Equal(t, "trail-review", runners[0].RunnerID)

	_, out, err = runTrailChangeAction(t, true, "gh/acme/widget", []string{"runner", "list", "{trail}", "--json"}, api.TrailResource{}, noActions)
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, out, "no runners is an empty list, not null")
}

func TestTrailRunnerShow_PrintsTheLatestRun(t *testing.T) {
	// Serial: replaces global clients.
	sha, failure := "4a028793e32b", "sandbox timed out"
	latest := api.TrailRunnerLatestResponse{
		Run:    &api.TrailRunnerRun{ID: "run-1", Status: "failed", UpdatedAt: "2026-10-29T15:45:59Z", HeadSHA: &sha, ErrorMessage: &failure},
		Result: &api.TrailRunnerResult{Type: "code_review_comments", Adapter: "last_json_line", Data: json.RawMessage(`{"comments":[]}`)},
	}
	requests, out, err := runTrailChangeAction(t, true, "gh/acme/widget", []string{"runner", "show", "{trail}", "trail-review"}, api.TrailResource{}, answerJSON(t, latest))
	require.NoError(t, err)
	require.Equal(t, []string{"GET /runs/trail-review"}, requests)
	for _, want := range []string{"Run:     run-1", "Status:  failed", "Head:    4a028793e32b", "Error:   sandbox timed out", "Result:  code_review_comments (last_json_line)"} {
		assert.Contains(t, out, want)
	}

	_, out, err = runTrailChangeAction(t, true, "gh/acme/widget", []string{"runner", "show", "{trail}", "trail-review"}, api.TrailResource{}, answerJSON(t, api.TrailRunnerLatestResponse{}))
	require.NoError(t, err)
	assert.Contains(t, out, "has not run")
}

func TestTrailChangeAction_LegacyContextCarriesTheRepoID(t *testing.T) {
	// Serial: replaces global clients. Legacy contexts used to leave RepoID
	// empty, which the repository-ID route cannot address.
	requests, _, err := runTrailChangeAction(t, false, "gh/acme/widget", []string{"ci-pr", "{trail}"}, api.TrailResource{},
		answerJSON(t, api.TrailCIPullRequestResponse{PRNumber: 1}))
	require.NoError(t, err)
	require.Equal(t, []string{"POST /ci-pr"}, requests)
}
