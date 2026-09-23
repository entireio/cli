package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

const (
	trailMergeTestRepoID = "repo_example"
	trailMergeTestHead   = "head-example"
	trailMergeRepoPath   = "/api/v1/repos/" + trailMergeTestRepoID + "/trails/7"

	trailMergeableJSON = `{"bypass_policy":"admins","mergeable":true,"approval_gate_passed":true,` +
		`"checks_status":"success","behind_by":0,"comparison_status":"available","conflict_status":"clean",` +
		`"head_sha":"` + trailMergeTestHead + `","checks":[]}`
	trailBlockedJSON = `{"bypass_policy":"admins","mergeable":false,"approval_gate_passed":true,` +
		`"checks_status":"success","behind_by":0,"comparison_status":"available","conflict_status":"clean",` +
		`"head_sha":"` + trailMergeTestHead + `","checks":[]}`
	trailBlockedGatesJSON = `{"head_sha":"` + trailMergeTestHead + `","mergeable":false,"gates_enabled":true,` +
		`"gates":[],"blocking_failures":[{"gate_type":"base_checks","gate_key":"base_checks","blocking":true,` +
		`"status":"failed","rationale":"trunk is red: acme/entire-api build #1234 failed",` +
		`"value":{"web_url":"https://buildkite.com/acme/entire-api/builds/1234","build_number":1234}}]}`
)

type trailMergeStub struct {
	t            *testing.T
	lookupBase   string
	mergeability string
	gates        string
	mergeStatus  int
	mergeBody    string

	mu    sync.Mutex
	posts []string
}

func (s *trailMergeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET " + s.lookupBase + "/7":
		_, _ = fmt.Fprint(w, `{"id":"trail-example","number":7,"status":"open","branch":"feature/example","base":"trunk","title":"Example"}`)
	case "GET " + trailMergeRepoPath + "/mergeability":
		_, _ = fmt.Fprint(w, s.mergeability)
	case "GET " + trailMergeRepoPath + "/gates":
		_, _ = fmt.Fprint(w, s.gates)
	case "POST " + trailMergeRepoPath + "/merge":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Error(err)
		}
		s.mu.Lock()
		s.posts = append(s.posts, string(body))
		s.mu.Unlock()
		if s.mergeStatus != 0 {
			w.WriteHeader(s.mergeStatus)
		}
		_, _ = fmt.Fprint(w, s.mergeBody)
	default:
		s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *trailMergeStub) mergePosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.posts...)
}

// Tests using this are not parallel: it replaces the newTrailAPIClient seam.
func runTrailMergeTest(t *testing.T, stub *trailMergeStub, repo string, args ...string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	previous := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("token", srv.URL), trailMergeTestRepoID, nil
	}
	t.Cleanup(func() { newTrailAPIClient = previous })

	cmd := newTrailCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs(append([]string{"merge", "--trail", "7", "--repo", repo}, args...))
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	return out.String(), err
}

func TestTrailMerge_BlockedWithoutForce(t *testing.T) {
	stub := &trailMergeStub{t: t, lookupBase: "/api/v1/repos/repo_example/trails", mergeability: trailBlockedJSON, gates: trailBlockedGatesJSON}
	out, err := runTrailMergeTest(t, stub, "et/acme/widget")

	require.Error(t, err)
	require.Contains(t, out, "trunk is red: acme/entire-api build #1234 failed")
	require.Contains(t, out, "https://buildkite.com/acme/entire-api/builds/1234")
	require.Contains(t, err.Error(), "trail #7 is not mergeable")
	require.Contains(t, err.Error(), "--force")
	require.Empty(t, stub.mergePosts(), "a blocked merge without --force must not POST")
}

func TestTrailMerge_ForceSendsBypass(t *testing.T) {
	for _, target := range []struct{ repo, lookupBase string }{
		{"gh/acme/widget", "/api/v1/trails/gh/acme/widget"},
		{"et/acme/widget", "/api/v1/repos/repo_example/trails"},
	} {
		t.Run(target.repo, func(t *testing.T) {
			stub := &trailMergeStub{
				t: t, lookupBase: target.lookupBase, mergeability: trailBlockedJSON, gates: trailBlockedGatesJSON,
				mergeBody: `{"ok":true,"mergeCommitSha":"merge-example"}`,
			}
			out, err := runTrailMergeTest(t, stub, target.repo, "--force")

			require.NoError(t, err)
			posts := stub.mergePosts()
			require.Len(t, posts, 1)
			require.JSONEq(t, `{"expectedHeadSha":"`+trailMergeTestHead+`","bypass":true}`, posts[0])
			require.Contains(t, out, "trunk is red: acme/entire-api build #1234 failed")
			require.Contains(t, out, "Merged trail #7 into trunk (merge-example), bypassing 1 failing gate")
		})
	}
}

func TestTrailMerge_MergeableWithoutForceOmitsBypass(t *testing.T) {
	stub := &trailMergeStub{
		t: t, lookupBase: "/api/v1/repos/repo_example/trails", mergeability: trailMergeableJSON,
		mergeBody: `{"ok":true,"mergeCommitSha":""}`,
	}
	out, err := runTrailMergeTest(t, stub, "et/acme/widget")

	require.NoError(t, err)
	posts := stub.mergePosts()
	require.Len(t, posts, 1)
	require.JSONEq(t, `{"expectedHeadSha":"`+trailMergeTestHead+`"}`, posts[0])
	require.Contains(t, out, "Merged trail #7 into trunk (fast-forward)")
}

func TestTrailMerge_ForceBypassForbidden(t *testing.T) {
	stub := &trailMergeStub{
		t: t, lookupBase: "/api/v1/repos/repo_example/trails", mergeability: trailBlockedJSON, gates: trailBlockedGatesJSON,
		mergeStatus: http.StatusForbidden,
		mergeBody:   `{"title":"Forbidden","status":403,"detail":"merge_gates_bypass_forbidden"}`,
	}
	_, err := runTrailMergeTest(t, stub, "et/acme/widget", "--force")

	require.Error(t, err)
	require.Contains(t, err.Error(), "bypass policy (admins) does not allow you to bypass")
	require.NotContains(t, err.Error(), "entire login", "a bypass denial is not an auth failure")
	require.Len(t, stub.mergePosts(), 1)
}

func TestTrailMerge_DryRunMakesNoPost(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mergeability string
		args         []string
		wantErr      bool
		wantOut      string
	}{
		{name: "mergeable", mergeability: trailMergeableJSON, wantOut: "Trail #7 is mergeable (dry run; no merge performed)."},
		{name: "blocked", mergeability: trailBlockedJSON, wantErr: true, wantOut: "trunk is red"},
		{name: "blocked with force", mergeability: trailBlockedJSON, args: []string{"--force"}, wantOut: "would bypass 1 failing gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &trailMergeStub{t: t, lookupBase: "/api/v1/repos/repo_example/trails", mergeability: tc.mergeability, gates: trailBlockedGatesJSON}
			out, err := runTrailMergeTest(t, stub, "et/acme/widget", append([]string{"--dry-run"}, tc.args...)...)

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Contains(t, out, tc.wantOut)
			require.Empty(t, stub.mergePosts(), "--dry-run must not POST")
		})
	}
}

func TestTrailMerge_GatesReadFailureFallsBackToSummary(t *testing.T) {
	stub := &trailMergeStub{
		t: t, lookupBase: "/api/v1/repos/repo_example/trails",
		mergeability: strings.Replace(trailBlockedJSON, `"approval_gate_passed":true`, `"approval_gate_passed":false`, 1),
		gates:        `not json`,
	}
	out, err := runTrailMergeTest(t, stub, "et/acme/widget")

	require.Error(t, err)
	require.Contains(t, out, "required approvals are missing")
	require.Empty(t, stub.mergePosts())
}
