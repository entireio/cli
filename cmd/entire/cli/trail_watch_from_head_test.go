package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// watchRequest is one stream request as the server saw it.
type watchRequest struct {
	from, lastEventID string
}

// recordingSSEServer answers the n-th request with responses[n] (a status, and
// frames for 200) and records each request's `from` and Last-Event-ID.
func recordingSSEServer(t *testing.T, responses []func(http.ResponseWriter)) (*httptest.Server, func() []watchRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []watchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(seen)
		seen = append(seen, watchRequest{from: r.URL.Query().Get("from"), lastEventID: r.Header.Get("Last-Event-ID")})
		mu.Unlock()
		if n >= len(responses) {
			sseFrames(w, "event: forbidden\ndata: {}\n\n")
			return
		}
		responses[n](w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []watchRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]watchRequest(nil), seen...)
	}
}

func sseFrames(w http.ResponseWriter, frames string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, frames)
}

func runWatchForTest(t *testing.T, srv *httptest.Server, jsonOutput, full bool) (string, string) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	client := api.NewClientWithBaseURL("tok", srv.URL)
	err := runTrailWatchResolved(cmd, client, "trl_1", "trail #1", jsonOutput, false, false, full)
	require.Error(t, err, "the scripted stream ends with forbidden")
	return out.String(), errOut.String()
}

// An idle first connection (ready, then the server's reconnect) resumes from
// the ready cursor instead of replaying from the start, and every request
// asks for the latest push.
func TestTrailWatchIdleReconnectResumesFromReadyCursor(t *testing.T) {
	t.Parallel()
	srv, seen := recordingSSEServer(t, []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sseFrames(w, "event: ready\ndata: {\"trail_id\":\"trl_1\",\"cursor\":136,\"start\":\"head\",\"head_sha\":\"abcdef0123456789\",\"latest_event_id\":140}\n\nevent: reconnect\ndata: {}\n\n")
		},
	})
	_, errOut := runWatchForTest(t, srv, false, false)
	require.Equal(t, []watchRequest{{from: "head"}, {from: "head", lastEventID: "136"}}, seen())
	require.Contains(t, errOut, "Showing the latest push (abcdef01); --full replays the whole history")
}

// A first connection that fails before any ready frame retries with from=head,
// not a full replay.
func TestTrailWatchRetryBeforeReadyKeepsFromHead(t *testing.T) {
	t.Parallel()
	srv, seen := recordingSSEServer(t, []func(http.ResponseWriter){
		func(w http.ResponseWriter) { http.Error(w, `{"error":"busy"}`, http.StatusServiceUnavailable) },
	})
	runWatchForTest(t, srv, false, false)
	require.Equal(t, []watchRequest{{from: "head"}, {from: "head"}}, seen())
}

// --full never asks for the latest push; a later event id still resumes.
func TestTrailWatchFullReplaysHistory(t *testing.T) {
	t.Parallel()
	srv, seen := recordingSSEServer(t, []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sseFrames(w, "event: ready\ndata: {\"trail_id\":\"trl_1\",\"cursor\":0,\"latest_event_id\":2}\n\nid: 2\nevent: x.y\ndata: {}\n\nevent: reconnect\ndata: {}\n\n")
		},
	})
	_, errOut := runWatchForTest(t, srv, false, true)
	require.Equal(t, []watchRequest{{}, {lastEventID: "2"}}, seen())
	require.NotContains(t, errOut, "latest push")
}

func TestTrailWatchStartLine(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ready, want string
	}{
		"head":               {`{"cursor":5,"start":"head","head_sha":"0123456789abcdef"}`, "Showing the latest push (01234567); --full replays the whole history\n"},
		"tail":               {`{"cursor":9,"start":"tail","head_sha":"0123456789abcdef"}`, "No events for the latest push (01234567) yet; showing new events\n"},
		"beginning":          {`{"cursor":0,"start":"beginning"}`, "The trail has no head yet; showing its whole history\n"},
		"short head sha":     {`{"cursor":1,"start":"head","head_sha":"abc"}`, "Showing the latest push (abc); --full replays the whole history\n"},
		"empty head sha":     {`{"cursor":1,"start":"head"}`, "Showing the latest push; --full replays the whole history\n"},
		"older server":       {`{"cursor":0}`, ""},
		"resumed (no start)": {`{"cursor":7}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var errOut bytes.Buffer
			noteReadyStart(&errOut, sseEventReady, tc.ready, "")
			require.Equal(t, tc.want, errOut.String())
		})
	}
}

// With --json the start line goes to stderr and stdout keeps the server's
// ready frame, start fields included.
func TestTrailWatchStartLineKeepsJSONStdout(t *testing.T) {
	t.Parallel()
	srv, _ := recordingSSEServer(t, []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sseFrames(w, "event: ready\ndata: {\"trail_id\":\"trl_1\",\"cursor\":3,\"start\":\"head\",\"head_sha\":\"0123456789abcdef\"}\n\nevent: forbidden\ndata: {}\n\n")
		},
	})
	out, errOut := runWatchForTest(t, srv, true, false)
	require.JSONEq(t, `{"event":"ready","data":{"trail_id":"trl_1","cursor":3,"start":"head","head_sha":"0123456789abcdef"}}`, strings.SplitN(out, "\n", 2)[0])
	require.Contains(t, errOut, "Showing the latest push (01234567)")
	require.NotContains(t, out, "Showing the latest push")
}

// A cursor already set by an event id is kept; the ready cursor only seeds an
// empty one.
func TestNoteReadyStartKeepsSeenCursor(t *testing.T) {
	t.Parallel()
	var errOut bytes.Buffer
	require.Equal(t, "42", noteReadyStart(&errOut, sseEventReady, `{"cursor":136}`, "42"))
	require.Equal(t, "136", noteReadyStart(&errOut, sseEventReady, `{"cursor":136}`, ""))
	require.Empty(t, noteReadyStart(&errOut, sseEventReady, `{"cursor":0}`, ""), "an older server's ready at 0 needs no cursor")
	require.Equal(t, "0", noteReadyStart(&errOut, sseEventReady, `{"cursor":0,"start":"tail"}`, ""),
		"an empty trail's from=head ready keeps 0, so a reconnect does not re-resolve the head")
	require.Empty(t, noteReadyStart(&errOut, sseEventReady, `not json`, ""))
	require.Empty(t, noteReadyStart(&errOut, "review.started", `{"cursor":136}`, ""), "only ready frames seed the cursor")
}

// A resumed connection keeps the cursor it resumed from, even when its ready
// frame (no start) reports a different one and the connection drops before
// any event.
func TestTrailWatchResumeKeepsItsCursor(t *testing.T) {
	t.Parallel()
	srv, seen := recordingSSEServer(t, []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sseFrames(w, "event: ready\ndata: {\"cursor\":5,\"start\":\"head\"}\n\nid: 6\nevent: x.y\ndata: {}\n\nevent: reconnect\ndata: {}\n\n")
		},
		func(w http.ResponseWriter) {
			sseFrames(w, "event: ready\ndata: {\"cursor\":9}\n\nevent: reconnect\ndata: {}\n\n")
		},
	})
	runWatchForTest(t, srv, false, false)
	require.Equal(t, []watchRequest{{from: "head"}, {from: "head", lastEventID: "6"}, {from: "head", lastEventID: "6"}}, seen())
}
