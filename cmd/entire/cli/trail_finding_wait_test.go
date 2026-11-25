package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	waitTestHead = "1111111111111111111111111111111111111111"
	waitTestOld  = "2222222222222222222222222222222222222222"
)

func waitTestAgent(name, status, head string, waitingFor string) api.TrailAgent {
	var a api.TrailAgent
	a.Agent.Name = name
	if status != "" {
		h := head
		a.LatestRun = &api.TrailAgentRun{ID: name + "-run", Status: status, HeadSHA: &h}
	}
	if waitingFor != "" {
		w := waitingFor
		a.WaitingFor = &w
	}
	return a
}

func TestTrailReviewProgressFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		agents               []api.TrailAgent
		serverHead, want     string
		ignoreStale          bool
		done, total          int
		headPending, settled bool
	}{
		{name: "no agents", settled: true, serverHead: waitTestHead},
		{name: "all finished on head", serverHead: waitTestHead, done: 2, total: 2, settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("review", "failed", waitTestHead, ""),
		}},
		{name: "timed out and canceled are finished", serverHead: waitTestHead, done: 2, total: 2, settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "timed_out", waitTestHead, ""), waitTestAgent("review", "canceled", waitTestHead, ""),
		}},
		{name: "one still running", serverHead: waitTestHead, done: 1, total: 2, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("review", "running", waitTestHead, ""),
		}},
		{name: "queued is pending", serverHead: waitTestHead, done: 0, total: 1, agents: []api.TrailAgent{
			waitTestAgent("risk", "queued", waitTestHead, ""),
		}},
		{name: "unknown status is pending", serverHead: waitTestHead, done: 0, total: 1, agents: []api.TrailAgent{
			waitTestAgent("risk", "brand_new_state", waitTestHead, ""),
		}},
		{name: "run on an older head is pending", serverHead: waitTestHead, done: 0, total: 1, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestOld, ""),
		}},
		{name: "after the grace a finished run on an older head is left out", serverHead: waitTestHead, ignoreStale: true, done: 1, total: 1, settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("manual", "succeeded", waitTestOld, ""),
		}},
		{name: "a running run on an older head still counts after the grace", serverHead: waitTestHead, ignoreStale: true, done: 1, total: 2, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("slow", "running", waitTestOld, ""),
		}},
		{name: "agent waiting for ready is left out", serverHead: waitTestHead, done: 1, total: 1, settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("gate", "", "", "ready"),
		}},
		{name: "agent waiting for not ready is left out", serverHead: waitTestHead, done: 1, total: 1, settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("early", "", "", "not_ready"),
		}},
		{name: "after the grace an agent with no run is left out", serverHead: waitTestHead, ignoreStale: true, done: 1, total: 1, settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("manual", "", "", "push"),
		}},
		{name: "no head to wait on settles at once", settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "running", waitTestHead, ""),
		}},
		{name: "agent waiting for push is pending", serverHead: waitTestHead, done: 0, total: 1, agents: []api.TrailAgent{
			waitTestAgent("risk", "", "", "push"),
		}},
		{name: "server behind the requested head", serverHead: waitTestOld, want: waitTestHead[:10], done: 0, total: 1, headPending: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestOld, ""),
		}},
		{name: "abbreviated requested head matches", serverHead: waitTestHead, want: waitTestHead[:7], done: 1, total: 1, settled: true, agents: []api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := trailReviewProgressFor(tt.agents, tt.serverHead, tt.want, tt.ignoreStale)
			assert.Equal(t, tt.done, p.Done)
			assert.Equal(t, tt.total, p.Total)
			assert.Equal(t, tt.headPending, p.HeadPending)
			assert.Equal(t, tt.settled, p.settled())
		})
	}
}

func TestTrailReviewProgressZeroValueIsNotFinished(t *testing.T) {
	t.Parallel()
	assert.False(t, trailReviewProgress{}.settled(), "no successful read must not read as a finished review")
	assert.False(t, toTrailReviewWaitJSON(trailReviewProgress{}).Finished)
}

func TestIsCommitSHA(t *testing.T) {
	t.Parallel()
	assert.True(t, isCommitSHA(waitTestHead))
	assert.True(t, isCommitSHA("abcdef1"))
	assert.False(t, isCommitSHA("abc"))
	assert.False(t, isCommitSHA("\x1b[31mred1234"))
	assert.False(t, isCommitSHA("main-branch"))
}

func TestWaitForTrailReviewAllReadsFailing(t *testing.T) {
	t.Parallel()
	clock := &fakeWaitClock{now: time.Unix(0, 0)}
	var errW bytes.Buffer
	p, err := waitForTrailReview(context.Background(), &errW, func(context.Context, bool) (trailReviewProgress, error) {
		return trailReviewProgress{}, errors.New("503 service unavailable")
	}, clock.now.Add(time.Minute), clock.Now, clock.Sleep)
	require.NoError(t, err)
	require.False(t, p.settled())
	require.Contains(t, errW.String(), "Warning: could not read the review's progress")
	require.Contains(t, trailReviewWaitResultLine(p), "Could not read the review's progress")
}

func TestSameCommit(t *testing.T) {
	t.Parallel()
	assert.True(t, sameCommit(waitTestHead, waitTestHead))
	assert.True(t, sameCommit(waitTestHead[:7], waitTestHead))
	assert.True(t, sameCommit("ABCDEF1", "abcdef1234"))
	assert.False(t, sameCommit("abcdef", "abcdef1234"), "too short to compare")
	assert.False(t, sameCommit("", waitTestHead))
	assert.False(t, sameCommit(waitTestOld, waitTestHead))
}

func TestValidateTrailFindingWaitOptions(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateTrailFindingWaitOptions(trailFindingWaitOptions{}, false, false))
	require.ErrorContains(t, validateTrailFindingWaitOptions(trailFindingWaitOptions{}, true, false), "need --wait")
	require.ErrorContains(t, validateTrailFindingWaitOptions(trailFindingWaitOptions{}, false, true), "need --wait")
	require.ErrorContains(t, validateTrailFindingWaitOptions(trailFindingWaitOptions{Enabled: true}, true, false), "positive")
	require.ErrorContains(t, validateTrailFindingWaitOptions(trailFindingWaitOptions{Enabled: true, Timeout: time.Second, Head: "main"}, false, true), "not a commit SHA")
	require.NoError(t, validateTrailFindingWaitOptions(trailFindingWaitOptions{Enabled: true, Timeout: time.Second, Head: waitTestHead[:7]}, false, true))
}

func TestTrailFindingWaitDeadline(t *testing.T) {
	t.Parallel()
	start := time.Unix(0, 0)
	assert.Equal(t, start.Add(105*time.Second), trailFindingWaitDeadline(start, 110*time.Second), "keeps time back to list the findings")
	assert.Equal(t, start.Add(3*time.Second), trailFindingWaitDeadline(start, 4*time.Second), "a short timeout keeps back at most a quarter")
}

type fakeWaitClock struct{ now time.Time }

func (c *fakeWaitClock) Now() time.Time { return c.now }
func (c *fakeWaitClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

func TestWaitForTrailReviewSettles(t *testing.T) {
	t.Parallel()
	clock := &fakeWaitClock{now: time.Unix(0, 0)}
	polls := 0
	poll := func(context.Context, bool) (trailReviewProgress, error) {
		polls++
		if polls < 6 {
			return trailReviewProgress{Read: true, Head: waitTestHead, Done: 1, Total: 2, OnHead: true}, nil
		}
		return trailReviewProgress{Read: true, Head: waitTestHead, Done: 2, Total: 2, OnHead: true}, nil
	}
	var errW bytes.Buffer
	p, err := waitForTrailReview(context.Background(), &errW, poll, clock.now.Add(10*time.Minute), clock.Now, clock.Sleep)
	require.NoError(t, err)
	require.True(t, p.settled())
	require.Equal(t, 6, polls)
	require.Contains(t, errW.String(), "1 of 2 agents done", "a progress line is printed right away")
}

func TestWaitForTrailReviewStopsAtDeadline(t *testing.T) {
	t.Parallel()
	clock := &fakeWaitClock{now: time.Unix(0, 0)}
	polls := 0
	poll := func(context.Context, bool) (trailReviewProgress, error) {
		polls++
		return trailReviewProgress{Read: true, Head: waitTestHead, Done: 0, Total: 1}, nil
	}
	p, err := waitForTrailReview(context.Background(), &bytes.Buffer{}, poll, clock.now.Add(40*time.Second), clock.Now, clock.Sleep)
	require.NoError(t, err)
	require.False(t, p.settled())
	require.Equal(t, 4, polls, "polls at 0s, 15s, 30s and 40s")
	require.Equal(t, time.Unix(40, 0), clock.now, "the last sleep is cut to the deadline")
	require.Contains(t, trailReviewWaitResultLine(p), "still running (0 of 1 agents done)")
}

// TestWaitForTrailReviewIgnoresStaleAfterGrace: an agent this push does not
// run keeps its finished run on the older head; once runs on the head have
// been seen for the grace period, the wait stops counting it.
func TestWaitForTrailReviewIgnoresStaleAfterGrace(t *testing.T) {
	t.Parallel()
	clock := &fakeWaitClock{now: time.Unix(0, 0)}
	var sawIgnore time.Time
	poll := func(_ context.Context, ignoreStale bool) (trailReviewProgress, error) {
		if ignoreStale && sawIgnore.IsZero() {
			sawIgnore = clock.now
		}
		return trailReviewProgressFor([]api.TrailAgent{
			waitTestAgent("risk", "succeeded", waitTestHead, ""), waitTestAgent("manual", "succeeded", waitTestOld, ""),
		}, waitTestHead, "", ignoreStale), nil
	}
	p, err := waitForTrailReview(context.Background(), &bytes.Buffer{}, poll, clock.now.Add(10*time.Minute), clock.Now, clock.Sleep)
	require.NoError(t, err)
	require.True(t, p.settled())
	require.Equal(t, time.Unix(60, 0), sawIgnore)
}

func TestWaitForTrailReviewRetriesFailedReads(t *testing.T) {
	t.Parallel()
	clock := &fakeWaitClock{now: time.Unix(0, 0)}
	polls := 0
	poll := func(context.Context, bool) (trailReviewProgress, error) {
		polls++
		if polls == 1 {
			return trailReviewProgress{}, errors.New("503 service unavailable")
		}
		return trailReviewProgress{Read: true, Head: waitTestHead, Done: 1, Total: 1, OnHead: true}, nil
	}
	var errW bytes.Buffer
	p, err := waitForTrailReview(context.Background(), &errW, poll, clock.now.Add(time.Minute), clock.Now, clock.Sleep)
	require.NoError(t, err)
	require.True(t, p.settled())
	require.Contains(t, errW.String(), "retrying")
}

func TestWaitForTrailReviewCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	clock := &fakeWaitClock{now: time.Unix(0, 0)}
	poll := func(context.Context, bool) (trailReviewProgress, error) {
		cancel()
		return trailReviewProgress{Read: true, Head: waitTestHead, Total: 1}, nil
	}
	_, err := waitForTrailReview(ctx, &bytes.Buffer{}, poll, clock.now.Add(time.Minute), clock.Now, clock.Sleep)
	require.ErrorIs(t, err, context.Canceled, "main re-raises the signal on context.Canceled")
}

func TestTrailReviewWaitResultLine(t *testing.T) {
	t.Parallel()
	assert.Contains(t, trailReviewWaitResultLine(trailReviewProgress{Read: true, NoAgents: true}), "does not report its review agents")
	assert.Contains(t, trailReviewWaitResultLine(trailReviewProgress{Read: true, Head: waitTestHead}), "No review agent runs on this commit")
	assert.Equal(t, "Review of 11111111 finished (2 agents).", trailReviewWaitResultLine(trailReviewProgress{Read: true, Head: waitTestHead, Done: 2, Total: 2}))
	assert.Contains(t, trailReviewWaitResultLine(trailReviewProgress{Read: true, Head: waitTestHead, HeadPending: true, Total: 1}), "has not reached 11111111")
	assert.Contains(t, trailReviewWaitResultLine(trailReviewProgress{Read: true, Head: waitTestHead, ServerHead: waitTestOld, HeadPending: true, Total: 1}), "trail is at 22222222, not 11111111")
}

// serveWaitTestTrail serves a legacy repository trail whose one agent's run
// finishes once detail has been read finishAfter times. Target resolution
// reads the change's detail too (the client maps /api/v1/trails/<id> to it).
func serveWaitTestTrail(t *testing.T, finishAfter int, agentsJSON func(status string) json.RawMessage) *int {
	t.Helper()
	reads := 0
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		item := api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work", Title: "Work", Status: "open"}
		var payload any
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/repos/repo-id/changes":
			payload = api.TrailListResponse{Trails: []api.TrailResource{item}}
		case "GET /api/v1/repos/repo-id/changes/7":
			reads++
			status := "running"
			if reads > finishAfter {
				status = "succeeded"
			}
			item.Agents = agentsJSON(status)
			item.Mergeability = json.RawMessage(`{"head_sha":"` + waitTestHead + `"}`)
			payload = item
		case "GET /api/v1/repos/repo-id/changes/7/reviews/comments":
			payload = map[string]any{"comments": []map[string]any{{"id": "finding-one", "review_id": "review-one", "status": "open", "body": "Check the nil case"}}}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(payload))
	})
	return &reads
}

func waitTestAgentsJSON(t *testing.T) func(string) json.RawMessage {
	t.Helper()
	return func(status string) json.RawMessage {
		raw, err := json.Marshal([]api.TrailAgent{waitTestAgent("Risk Eval", status, waitTestHead, "")})
		require.NoError(t, err)
		return raw
	}
}

func runWaitTestCommand(t *testing.T, args ...string) (string, string) {
	t.Helper()
	oldSleep := trailFindingWaitSleep
	trailFindingWaitSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { trailFindingWaitSleep = oldSleep })
	cmd := newTrailCmdForMode(false)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append(append([]string{"finding", "list", "feature/work", "--wait"}, args...), "--repo", "gh/acme/widget"))
	require.NoError(t, cmd.ExecuteContext(context.Background()), errOut.String())
	return out.String(), errOut.String()
}

// TestTrailFindingListWait runs the command end to end: the review finishes
// on a later read of the change, then the findings are listed.
func TestTrailFindingListWait(t *testing.T) {
	// Serial: replaces the package wait sleep and global clients.
	reads := serveWaitTestTrail(t, 2, waitTestAgentsJSON(t))
	out, errOut := runWaitTestCommand(t)
	require.GreaterOrEqual(t, *reads, 3, "the wait polls until the run finishes")
	require.Contains(t, errOut, "Review of 11111111 finished (1 agents).")
	require.Contains(t, out, "finding-one")
}

func TestTrailFindingListWaitJSON(t *testing.T) {
	// Serial: replaces the package wait sleep and global clients.
	serveWaitTestTrail(t, 1, waitTestAgentsJSON(t))
	out, _ := runWaitTestCommand(t, "--json")
	var got struct {
		Findings []map[string]any    `json:"findings"`
		Wait     trailReviewWaitJSON `json:"wait"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout stays one JSON document")
	require.Len(t, got.Findings, 1)
	require.Equal(t, trailReviewWaitJSON{Head: waitTestHead, Finished: true, Done: 1, Total: 1}, got.Wait)
}

func TestTrailFindingListWaitWithoutAgentsDoesNotWait(t *testing.T) {
	// Serial: replaces the package wait sleep and global clients.
	reads := serveWaitTestTrail(t, 100, func(string) json.RawMessage { return nil })
	out, errOut := runWaitTestCommand(t)
	require.LessOrEqual(t, *reads, 2, "an absent agents field is not waited on")
	require.Contains(t, errOut, "does not report its review agents")
	require.Contains(t, out, "finding-one")
}

func TestTrailFindingListWaitFlagsNeedWait(t *testing.T) {
	t.Parallel()
	cmd := newTrailCmdForMode(false)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"finding", "list", "--timeout", "1m"})
	require.ErrorContains(t, cmd.ExecuteContext(context.Background()), "need --wait")
}
