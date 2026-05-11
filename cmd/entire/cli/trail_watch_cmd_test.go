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

func TestStreamOnce_PrintsReadyAndComment(t *testing.T) {
	frames := []string{
		"event: ready\ndata: {\"repo\":\"acme/web\",\"trailNumber\":42,\"commentCount\":1,\"resumed\":false}\nid: 1700000000000:ready\n\n",
		"event: comment\ndata: {\"repo\":\"acme/web\",\"trailNumber\":42,\"updatedAt\":\"2026-01-01T00:00:00Z\",\"comment\":{\"id\":\"c1\",\"author\":\"alice\",\"body\":\"hello world\",\"created_at\":\"2026-01-01T00:00:00Z\",\"resolved\":false,\"resolved_by\":null,\"resolved_at\":null}}\nid: 1700000000000:c1\n\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reason, lastID, err := streamOnce(ctx, client, "/stream", "", false, false, false, true, &stdout, &stderr)
	// `--once` with commentCount=1 should exit cleanly after seeing ready+comment.
	if err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}
	if reason != streamCloseDone {
		t.Errorf("close reason = %d, want streamCloseDone", reason)
	}
	if lastID != "1700000000000:c1" {
		t.Errorf("lastID = %q, want %q", lastID, "1700000000000:c1")
	}
	out := stdout.String()
	if !strings.Contains(out, "trail #42") {
		t.Errorf("expected ready summary in output, got: %q", out)
	}
	if !strings.Contains(out, "alice") || !strings.Contains(out, "hello world") {
		t.Errorf("expected comment line in output, got: %q", out)
	}
}

func TestStreamOnce_JSONOutputEnvelope(t *testing.T) {
	frames := []string{
		"event: ready\ndata: {\"commentCount\":0}\nid: x\n\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := streamOnce(ctx, client, "/stream", "", false, true, false, true, &stdout, &stderr); err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}

	line := strings.TrimSpace(stdout.String())
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

	_, _, err := streamOnce(ctx, client, "/stream", "", false, false, true, false, &stdout, &stderr)
	if err == nil {
		t.Errorf("expected EOF after fixed test stream")
	}
	if got := stderr.String(); !strings.Contains(got, "ping: ping 123\n") {
		t.Errorf("stderr = %q, want trimmed ping output", got)
	}
}

func TestStreamOnce_ReconnectEvent(t *testing.T) {
	frames := []string{
		"event: ready\ndata: {\"commentCount\":0}\nid: r1\n\n",
		"event: reconnect\ndata: {\"reason\":\"max_duration\"}\n\n",
	}
	srv, _ := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reason, lastID, err := streamOnce(ctx, client, "/stream", "", false, false, false, false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}
	if reason != streamCloseReconnect {
		t.Errorf("reason = %d, want streamCloseReconnect", reason)
	}
	if lastID != "r1" {
		t.Errorf("lastID = %q, want r1 (reconnect frame has no id; should preserve last ready id)", lastID)
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

			reason, _, err := streamOnce(ctx, client, "/stream", "", false, false, false, false, &stdout, &stderr)
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

	reason, _, err := streamOnce(ctx, client, "/stream", "", false, false, false, false, &stdout, &stderr)
	if reason != streamCloseTransport {
		t.Errorf("reason = %d, want streamCloseTransport (429 should be retryable)", reason)
	}
	if err == nil {
		t.Errorf("want non-nil error so caller can log the backoff reason")
	}
}

func TestStreamOnce_SendsLastEventIDHeader(t *testing.T) {
	frames := []string{
		"event: deleted\ndata: {}\n\n",
	}
	srv, gotLastID := fakeSSEServer(t, frames)
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := streamOnce(ctx, client, "/stream", "abc:c1", true, false, false, false, &stdout, &stderr); err != nil {
		t.Fatalf("streamOnce error: %v", err)
	}
	if *gotLastID != "abc:c1" {
		t.Errorf("server saw Last-Event-ID = %q, want %q", *gotLastID, "abc:c1")
	}
}
