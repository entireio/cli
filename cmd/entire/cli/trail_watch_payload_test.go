package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

func TestWatchDecodesDocumentedCellEvents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ event, want string }{
		{"code_version.created", "code version target-example created (head head-example)"},
		{"review.started", "session started by actor-example (code version cv-example)"},
		{"comment.created", "finding created by actor-example on example.go (low)"},
		{"comment.updated", "finding target-example updated by actor-example"},
		{"comment.status_changed", "finding target-example status open → resolved"},
		{"comment.stale_checked", "finding target-example marked current (unchanged)"},
		{"suggested_change.created", "suggested change target-example created for finding comment-example (manual_instruction)"},
		{"gate.updated", "gate.updated resource/target-example by actor-example"},
		{"monitor.updated", "monitor.updated resource/target-example by actor-example"},
		{"runner.run", "runner.run resource/target-example by actor-example"},
		{"runner.status", "runner.status resource/target-example by actor-example"},
		{"runner.done", "runner.done resource/target-example by actor-example"},
		{"runner.error", "runner.error resource/target-example by actor-example"},
		{"discussion_updated", "discussion_updated resource/target-example by actor-example"},
		{"future.event", "future.event resource/target-example by actor-example"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			t.Parallel()
			data := fmt.Sprintf(`{"id":42,"trail_id":"trail-example","review_id":"review-example","actor_id":"actor-example","event_type":%q,"target_type":"resource","target_id":"target-example","created_at":"2026-09-01T00:00:00Z","payload":{"head_sha":"head-example","code_version_id":"cv-example","file_path":"example.go","severity":"low","from":"open","to":"resolved","outcome":"current","reason":"unchanged","review_comment_id":"comment-example","change_type":"manual_instruction"}}`, tc.event)
			var out, errOut bytes.Buffer
			printSSEEvent(&out, &errOut, tc.event, data, false)
			require.Contains(t, out.String(), tc.want)
			require.Empty(t, errOut.String())
			out.Reset()
			printSSEEvent(&out, &errOut, tc.event, data, true)
			var envelope struct {
				Event string          `json:"event"`
				Data  json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(out.Bytes(), &envelope))
			require.Equal(t, tc.event, envelope.Event)
			require.JSONEq(t, data, string(envelope.Data))
		})
	}
}

func TestWatchCellRoutesPreserveReconnectCursor(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"/api/v1/trails/gh/acme/widget/7", "/api/v1/repos/repo_example/trails/7"} {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != base+"/events" || r.Header.Get("Accept") != "text/event-stream" {
					t.Errorf("request = %s, Accept %s", r.URL.Path, r.Header.Get("Accept"))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if requests == 1 {
					_, _ = fmt.Fprint(w, "event: ready\ndata: {\"trail_id\":\"trail-example\",\"cursor\":0}\n\nid: 42\nevent: review.started\ndata: {\"trail_id\":\"trail-example\",\"actor_id\":\"actor-example\",\"event_type\":\"review.started\",\"payload\":{\"code_version_id\":\"cv-example\"}}\n\nevent: reconnect\ndata: {}\n\n")
				} else {
					if r.Header.Get("Last-Event-ID") != "42" {
						t.Errorf("Last-Event-ID = %q", r.Header.Get("Last-Event-ID"))
					}
					_, _ = fmt.Fprint(w, "id: 43\nevent: discussion_updated\ndata: {\"event_type\":\"discussion_updated\",\"actor_id\":\"actor-example\",\"target_type\":\"discussion\",\"target_id\":\"th1\",\"payload\":{\"discussion_id\":\"th1\",\"title\":\"Thread safety\"}}\n\nevent: forbidden\ndata: {}\n\n")
				}
			}))
			defer srv.Close()
			client := api.NewClientWithBaseURL("tok", srv.URL)
			client.SetTrailRoute("trail-example", base)
			var out, errOut bytes.Buffer
			reason, cursor, err := streamOnce(t.Context(), client, reviewEventsPath("trail-example"), "", false, false, &out, &errOut)
			require.NoError(t, err)
			require.Equal(t, streamCloseReconnect, reason)
			require.Equal(t, "42", cursor)
			require.Contains(t, out.String(), "connected to trail trail-example")
			out.Reset()
			reason, cursor, err = streamOnce(t.Context(), client, reviewEventsPath("trail-example"), cursor, true, false, &out, &errOut)
			require.NoError(t, err)
			require.Equal(t, streamCloseForbidden, reason)
			require.Equal(t, "43", cursor)
			require.Equal(t, 2, requests)
			var event json.RawMessage
			require.NoError(t, json.NewDecoder(&out).Decode(&event))
			require.JSONEq(t, `{"event":"discussion_updated","data":{"event_type":"discussion_updated","actor_id":"actor-example","target_type":"discussion","target_id":"th1","payload":{"discussion_id":"th1","title":"Thread safety"}}}`, string(event))
		})
	}
}
