package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	trailMergeLookupBase = "/api/v1/repos/repo_example/trails"
)

// trailMergeability builds a mergeability body in the server's shape
// (entire-api TrailMergeabilityResponse), starting from a mergeable trail
// whose checks gate passed, then applying edits.
func trailMergeability(t *testing.T, edits ...func(map[string]any)) string {
	t.Helper()
	m := map[string]any{
		"bypass_policy": "admins", "mergeable": true, "approval_gate_passed": true,
		"behind_by": 0, "comparison_status": "available", "conflict_status": "clean",
		"head_sha": trailMergeTestHead,
		"gates": []any{
			map[string]any{"gate_type": "checks", "gate_key": "checks", "blocking": true, "status": "passed", "state": "evaluated", "rationale": nil, "value": nil},
		},
		"checks": map[string]any{"availability": "available", "runs": []any{
			map[string]any{"name": "build", "status": "completed", "conclusion": "success"},
		}},
	}
	for _, edit := range edits {
		edit(m)
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return string(b)
}

func withGate(g map[string]any) func(map[string]any) {
	return func(m map[string]any) {
		m["gates"] = append(m["gates"].([]any), g) //nolint:forcetypeassert // test fixture
	}
}

func withField(key string, value any) func(map[string]any) {
	return func(m map[string]any) { m[key] = value }
}

var (
	blocked         = withField("mergeable", false)
	baseChecksRed   = withGate(map[string]any{"gate_type": "base_checks", "gate_key": "base_checks", "blocking": true, "status": "failed", "state": "evaluated", "rationale": "trunk is red: acme/entire-api build #1234 failed", "value": map[string]any{"web_url": "https://buildkite.com/acme/entire-api/builds/1234", "build_number": 1234}})
	findingsAdvisry = withGate(map[string]any{"gate_type": "findings", "gate_key": "findings", "blocking": false, "status": "failed", "state": "evaluated", "rationale": "advisory findings open"})
)

type trailMergeStub struct {
	t            *testing.T
	lookupBase   string
	mergeability string
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
	stub := &trailMergeStub{t: t, lookupBase: trailMergeLookupBase, mergeability: trailMergeability(t, blocked, baseChecksRed)}
	out, err := runTrailMergeTest(t, stub, "et/acme/widget")

	require.Error(t, err)
	require.Contains(t, out, "trunk is red: acme/entire-api build #1234 failed")
	require.Contains(t, out, "https://buildkite.com/acme/entire-api/builds/1234")
	require.Contains(t, err.Error(), "trail #7 is not mergeable")
	require.Contains(t, err.Error(), "--force")
	require.Empty(t, stub.mergePosts(), "a blocked merge without --force must not POST")
}

func TestTrailMerge_BlockersComeFromMergeabilityGates(t *testing.T) {
	stub := &trailMergeStub{t: t, lookupBase: trailMergeLookupBase, mergeability: trailMergeability(t, blocked, baseChecksRed, findingsAdvisry)}
	out, err := runTrailMergeTest(t, stub, "et/acme/widget")

	require.Error(t, err)
	blockedBy := out[strings.Index(out, "Blocked by:"):]
	require.Contains(t, blockedBy, "base_checks: trunk is red")
	require.NotContains(t, blockedBy, "findings", "a non-blocking gate does not block the merge")
	require.NotContains(t, blockedBy, "checks: passed", "a passed gate does not block the merge")
}

func TestTrailMerge_ChecksLineReadsChecksGateAndRuns(t *testing.T) {
	failedChecks := func(m map[string]any) {
		m["gates"] = []any{map[string]any{"gate_type": "checks", "gate_key": "checks", "blocking": true, "status": "failed", "state": "evaluated", "rationale": "build failed"}}
		m["checks"] = map[string]any{"availability": "available", "runs": []any{
			map[string]any{"name": "build", "status": "completed", "conclusion": "failure"},
			map[string]any{"name": "lint", "status": "completed", "conclusion": "success"},
			map[string]any{"name": "e2e", "status": "in_progress", "conclusion": nil},
		}}
	}
	for _, tc := range []struct {
		name  string
		edits []func(map[string]any)
		want  string
	}{
		{name: "passed", want: "Checks:     ✓ passed (1 run: 1 passed)"},
		{name: "failed", edits: []func(map[string]any){blocked, failedChecks}, want: "Checks:     ✗ failed (3 runs: 1 failed, 1 pending, 1 passed)"},
		{name: "no checks gate or CI", edits: []func(map[string]any){withField("gates", []any{}), withField("checks", map[string]any{"availability": "not_applicable", "runs": []any{}})}, want: "Checks:     ✓ none"},
		{name: "CI unavailable", edits: []func(map[string]any){withField("gates", []any{}), withField("checks", map[string]any{"availability": "unavailable", "runs": []any{}})}, want: "Checks:     ✗ unknown (CI evidence unavailable)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &trailMergeStub{t: t, lookupBase: trailMergeLookupBase, mergeability: trailMergeability(t, tc.edits...), mergeBody: `{"ok":true,"mergeCommitSha":"merge-example"}`}
			out, _ := runTrailMergeTest(t, stub, "et/acme/widget", "--dry-run")
			require.Contains(t, out, tc.want)
		})
	}
}

func TestTrailMerge_FallbackBlockersReportCI(t *testing.T) {
	// Not mergeable, yet no blocking gate says why: describe from the summary.
	stub := &trailMergeStub{t: t, lookupBase: trailMergeLookupBase, mergeability: trailMergeability(t, blocked,
		withField("gates", []any{}), withField("approval_gate_passed", false),
		withField("checks", map[string]any{"availability": "available", "runs": []any{map[string]any{"name": "build", "status": "completed", "conclusion": "failure"}}}))}
	out, err := runTrailMergeTest(t, stub, "et/acme/widget")

	require.Error(t, err)
	require.Contains(t, out, "required approvals are missing")
	require.Contains(t, out, "CI checks failed")
	require.Empty(t, stub.mergePosts())
}

func TestTrailMerge_ForceSendsBypass(t *testing.T) {
	for _, target := range []struct{ repo, lookupBase string }{
		{"gh/acme/widget", "/api/v1/trails/gh/acme/widget"},
		{"et/acme/widget", trailMergeLookupBase},
	} {
		t.Run(target.repo, func(t *testing.T) {
			stub := &trailMergeStub{
				t: t, lookupBase: target.lookupBase, mergeability: trailMergeability(t, blocked, baseChecksRed),
				mergeBody: `{"ok":true,"mergeCommitSha":"merge-example"}`,
			}
			out, err := runTrailMergeTest(t, stub, target.repo, "--force")

			require.NoError(t, err)
			posts := stub.mergePosts()
			require.Len(t, posts, 1)
			require.JSONEq(t, `{"expectedHeadSha":"`+trailMergeTestHead+`","bypass":true}`, posts[0])
			require.Contains(t, out, "trunk is red: acme/entire-api build #1234 failed")
			require.Contains(t, out, "Merged trail #7 into trunk (merge-example), bypassing 1 blocking gate")
		})
	}
}

func TestTrailMerge_MergeableWithoutForceOmitsBypass(t *testing.T) {
	stub := &trailMergeStub{
		t: t, lookupBase: trailMergeLookupBase, mergeability: trailMergeability(t),
		mergeBody: `{"ok":true,"mergeCommitSha":"merge-example"}`,
	}
	out, err := runTrailMergeTest(t, stub, "et/acme/widget")

	require.NoError(t, err)
	posts := stub.mergePosts()
	require.Len(t, posts, 1)
	require.JSONEq(t, `{"expectedHeadSha":"`+trailMergeTestHead+`"}`, posts[0])
	require.Contains(t, out, "Merged trail #7 into trunk (merge-example)")
}

func TestTrailMerge_ForceBypassForbidden(t *testing.T) {
	stub := &trailMergeStub{
		t: t, lookupBase: trailMergeLookupBase, mergeability: trailMergeability(t, blocked, baseChecksRed),
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
		name    string
		edits   []func(map[string]any)
		args    []string
		wantErr bool
		wantOut string
	}{
		{name: "mergeable", wantOut: "Trail #7 is mergeable (dry run; no merge performed)."},
		{name: "blocked", edits: []func(map[string]any){blocked, baseChecksRed}, wantErr: true, wantOut: "trunk is red"},
		{name: "blocked with force", edits: []func(map[string]any){blocked, baseChecksRed}, args: []string{"--force"}, wantOut: "--force would bypass 1 blocking gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &trailMergeStub{t: t, lookupBase: trailMergeLookupBase, mergeability: trailMergeability(t, tc.edits...)}
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
