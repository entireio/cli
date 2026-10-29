package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readTrailChange runs a read subcommand on change widget/7 of project trail
// 42 and serves body for GET <repo-ID route>/<suffix>, recording the query.
func readTrailChange(t *testing.T, suffix string, body any, args ...string) (query, stdout string, err error) {
	t.Helper()
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if !serveWorkingProjectRead(t, w, r) {
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/repo-id/trails/7/"+suffix {
			query = r.URL.RawQuery
			assert.NoError(t, json.NewEncoder(w).Encode(body))
			return
		}
		t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	cmd := newTrailCmdForMode(true)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{suffix, "42", "--repo", "gh/acme/widget"}, args...))
	err = cmd.Execute()
	return query, out.String(), err
}

const trailTimelineTestBody = `{"items":[
	{"id":"r","kind":"review_posted","actor":{"actorId":"agent:trail-review","login":null},"occurredAt":"2026-10-29T15:45:59Z","findingCount":2,"commentId":"c1"},
	{"id":"f","kind":"finding_resolved","actor":{"login":"alice"},"occurredAt":"2026-10-29T15:44:48Z","commentId":"finding-one"},
	{"id":"a","kind":"approval","actor":{"login":"bob"},"occurredAt":"2026-10-29T15:44:30Z","approvalStatus":"approved"},
	{"id":"c","kind":"commit","actor":{"login":"alice"},"occurredAt":"2026-10-29T15:44:25Z","title":"Add keys","commitSha":"4a028793e32beed4","additions":10,"deletions":2,"agents":["Claude Code"]},
	{"id":"t","kind":"trail_created","actor":{"login":"alice"},"occurredAt":"2026-10-29T12:37:44Z"},
	{"id":"n","kind":"some_future_kind","occurredAt":"2026-10-29T12:00:00Z","newField":true}
],"commitCount":1,"incomplete":true,"nextPageToken":"next-page"}`

func TestTrailTimeline_RendersEachKindAndPages(t *testing.T) {
	// Serial: replaces global clients.
	query, out, err := readTrailChange(t, "timeline", json.RawMessage(trailTimelineTestBody), "--limit", "6", "--page-token", "prev")
	require.NoError(t, err)
	assert.Equal(t, "pageSize=6&pageToken=prev", query)
	for _, want := range []string{
		"2026-10-29T15:45:59Z  review_posted       agent:trail-review  2 finding(s)\n",
		"finding_resolved    alice             finding finding-one\n",
		"approval            bob               approved\n",
		"commit              alice             4a028793 Add keys +10/-2 (Claude Code)\n",
		"trail_created       alice\n",
		"some_future_kind    -\n",
		"(incomplete: some activity could not be loaded)\n",
		"Next page: --page-token next-page\n",
	} {
		assert.Contains(t, out, want)
	}
}

func TestTrailTimeline_JSONPassesItemsThrough(t *testing.T) {
	// Serial: replaces global clients.
	_, out, err := readTrailChange(t, "timeline", json.RawMessage(trailTimelineTestBody), "--json")
	require.NoError(t, err)
	var got struct {
		Items         []map[string]any `json:"items"`
		NextPageToken string           `json:"nextPageToken"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 6)
	assert.Equal(t, true, got.Items[5]["newField"], "unknown fields survive")
	assert.Equal(t, "next-page", got.NextPageToken)

	_, out, err = readTrailChange(t, "timeline", map[string]any{"items": nil}, "--json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"items":[],"commitCount":0,"incomplete":false}`, out)
}

func TestTrailTimeline_RejectsAnOutOfRangeLimit(t *testing.T) {
	// Serial: replaces global clients.
	_, _, err := readTrailChange(t, "timeline", nil, "--limit", "0")
	require.ErrorContains(t, err, "--limit must be between 1 and 100")
}

func TestTrailSessions_ListsSessions(t *testing.T) {
	// Serial: replaces global clients.
	body := json.RawMessage(`{"sessions":[{"sessionId":"session-1","displayName":"Keys","agent":"Claude Code","model":"claude-opus-5-5","startedAt":"2026-10-29T12:37:22Z","checkpointCount":2,"prompt":"secret prompt"}]}`)
	_, out, err := readTrailChange(t, "sessions", body)
	require.NoError(t, err)
	assert.Regexp(t, `session-1\s+Claude Code\s+claude-opus-5-5\s+2026-10-29T12:37:22Z\s+2\s+Keys`, out)
	assert.NotContains(t, out, "secret prompt", "the table does not print prompts")

	_, out, err = readTrailChange(t, "sessions", body, "--json")
	require.NoError(t, err)
	var sessions []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &sessions), out)
	require.Len(t, sessions, 1)
	assert.Equal(t, "session-1", sessions[0]["sessionId"])
}

func TestTrailStack_RendersTheStackOrItsAbsence(t *testing.T) {
	// Serial: replaces global clients.
	seven, title, open := 7, "Add keys", "open"
	stack := trailStackResponse{Stack: &trailStack{Position: 2, Size: 2, Base: "main", PullRequests: []trailStackPullRequest{
		{PRNumber: 10, Status: &open},
		{PRNumber: 11, TrailNumber: &seven, Title: &title, Status: &open, Current: true},
	}}}
	_, out, err := readTrailChange(t, "stack", stack)
	require.NoError(t, err)
	assert.Contains(t, out, "Position 2 of 2 onto main:\n")
	assert.Regexp(t, `(?m)^  #10\s+open\s+-\s*$`, out)
	assert.Regexp(t, `(?m)^> #11\s+open\s+widget/7\s+Add keys$`, out)

	_, out, err = readTrailChange(t, "stack", api.TrailResource{})
	require.NoError(t, err)
	assert.Contains(t, out, "is not in a pull request stack")
}
