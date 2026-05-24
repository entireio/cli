package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// fakeSSEServer streams a fixed sequence of frames with optional terminal
// behavior and records the Last-Event-ID header sent by the client.
func fakeSSEServer(t *testing.T, frames []string) (*httptest.Server, *string) {
	t.Helper()
	var lastEventID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastEventID = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		for _, f := range frames {
			if _, err := fmt.Fprint(w, f); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	return srv, &lastEventID
}

func TestReviewEventsPath(t *testing.T) {
	got := reviewEventsPath("trail id/with slash")
	want := "/api/v1/trails/trail%20id%2Fwith%20slash/reviews/events"
	if got != want {
		t.Fatalf("reviewEventsPath = %q, want %q", got, want)
	}
}

func TestStreamOnce_PrintsReadyAndReviewEvents(t *testing.T) {
	frames := []string{
		"event: ready\ndata: {\"trail_id\":\"trl_1\",\"cursor\":0}\n\n",
		"id: 1\nevent: session.started\ndata: {\"id\":\"1\",\"trail_id\":\"trl_1\",\"review_session_id\":\"ses_123\",\"actor_id\":\"agent:reviewer\",\"event_type\":\"session.started\",\"target_type\":\"review_session\",\"target_id\":\"ses_123\",\"payload\":{\"code_version_id\":\"cv_1\"},\"created_at\":\"2026-01-01T00:00:00Z\"}\n\n",
		"id: 2\nevent: comment.created\ndata: {\"id\":\"2\",\"trail_id\":\"trl_1\",\"review_session_id\":\"ses_123\",\"actor_id\":\"agent:reviewer\",\"event_type\":\"comment.created\",\"target_type\":\"review_comment\",\"target_id\":\"c1\",\"payload\":{\"severity\":\"high\",\"file_path\":\"src/foo.ts\",\"granularity\":\"line\"},\"created_at\":\"2026-01-01T00:00:01Z\"}\n\n",
		"id: 3\nevent: session.ended\ndata: {\"id\":\"3\",\"trail_id\":\"trl_1\",\"review_session_id\":\"ses_123\",\"actor_id\":\"agent:reviewer\",\"event_type\":\"session.ended\",\"target_type\":\"review_session\",\"target_id\":\"ses_123\",\"payload\":{\"reason\":\"done\"},\"created_at\":\"2026-01-01T00:00:02Z\"}\n\n",
		"event: reconnect\ndata: {\"reason\":\"max_duration\"}\n\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reason, lastID, err := streamOnce(ctx, client, "/stream", "", false, false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}
	if reason != streamCloseReconnect {
		t.Errorf("close reason = %d, want streamCloseReconnect", reason)
	}
	if lastID != "3" {
		t.Errorf("lastID = %q, want %q", lastID, "3")
	}
	out := stdout.String()
	for _, want := range []string{"trail trl_1", "session started", "comment created", "src/foo.ts", "session ended"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got: %q", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "server requested reconnect") {
		t.Errorf("expected reconnect notice in stderr, got: %q", stderr.String())
	}
}

func TestStreamOnce_JSONOutputEnvelope(t *testing.T) {
	frames := []string{
		"event: ready\ndata: {\"trail_id\":\"trl_1\",\"cursor\":0}\n\n",
		"event: reconnect\ndata: {\"reason\":\"max_duration\"}\n\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := streamOnce(ctx, client, "/stream", "", true, false, &stdout, &stderr); err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}

	line := strings.Split(strings.TrimSpace(stdout.String()), "\n")[0]
	var env map[string]any
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("output is not JSON: %v\nline=%q", err, line)
	}
	if env["event"] != "ready" {
		t.Errorf("event = %v, want ready", env["event"])
	}
}

func TestStreamOnce_ShowPingsTrimsSSECommentWhitespace(t *testing.T) {
	frames := []string{
		": ping 123\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := streamOnce(ctx, client, "/stream", "", false, true, &stdout, &stderr)
	if err == nil {
		t.Errorf("expected EOF after fixed test stream")
	}
	if got := stderr.String(); !strings.Contains(got, "ping: ping 123\n") {
		t.Errorf("stderr = %q, want trimmed ping output", got)
	}
}

func TestStreamOnce_ReconnectEvent(t *testing.T) {
	frames := []string{
		"event: ready\ndata: {\"trail_id\":\"trl_1\",\"cursor\":0}\n\n",
		"id: 1\nevent: session.started\ndata: {\"id\":\"1\",\"event_type\":\"session.started\",\"target_type\":\"review_session\",\"target_id\":\"ses_123\",\"actor_id\":\"agent\",\"payload\":{}}\n\n",
		"event: reconnect\ndata: {\"reason\":\"max_duration\"}\n\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reason, lastID, err := streamOnce(ctx, client, "/stream", "", false, false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}
	if reason != streamCloseReconnect {
		t.Errorf("reason = %d, want streamCloseReconnect", reason)
	}
	if lastID != "1" {
		t.Errorf("lastID = %q, want 1 (reconnect frame has no id; should preserve last event id)", lastID)
	}
}

func TestStreamOnce_ForbiddenEvent(t *testing.T) {
	frames := []string{
		"event: forbidden\ndata: {\"reason\":\"access_revoked\"}\n\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reason, _, err := streamOnce(ctx, client, "/stream", "", false, false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}
	if reason != streamCloseForbidden {
		t.Errorf("reason = %d, want streamCloseForbidden", reason)
	}
	if !strings.Contains(stderr.String(), "access revoked") {
		t.Errorf("stderr = %q, want access revoked notice", stderr.String())
	}
}

func TestStreamOnce_TerminalHTTPStatusesDoNotReconnect(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
		{"not_found", http.StatusNotFound},
		{"gone", http.StatusGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = fmt.Fprintf(w, "{\"error\":\"%s\"}", http.StatusText(tc.code))
			}))
			defer srv.Close()

			t.Setenv(api.BaseURLEnvVar, srv.URL)
			client := api.NewClient("tok")

			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			reason, _, err := streamOnce(ctx, client, "/stream", "", false, false, &stdout, &stderr)
			if reason != streamCloseTerminal {
				t.Errorf("reason = %d, want streamCloseTerminal for %d", reason, tc.code)
			}
			if err == nil {
				t.Errorf("want non-nil error for HTTP %d", tc.code)
			}
		})
	}
}

func TestStreamOnce_TooManyRequestsIsRecoverable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reason, _, err := streamOnce(ctx, client, "/stream", "", false, false, &stdout, &stderr)
	if reason != streamCloseTransport {
		t.Errorf("reason = %d, want streamCloseTransport (429 should be retryable)", reason)
	}
	if err == nil {
		t.Errorf("want non-nil error so caller can log the backoff reason")
	}
}

func TestStreamOnce_SendsLastEventIDHeader(t *testing.T) {
	frames := []string{
		"event: reconnect\ndata: {\"reason\":\"max_duration\"}\n\n",
	}
	srv, gotLastID := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := streamOnce(ctx, client, "/stream", "42", false, false, &stdout, &stderr); err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}
	if *gotLastID != "42" {
		t.Errorf("server saw Last-Event-ID = %q, want %q", *gotLastID, "42")
	}
}
