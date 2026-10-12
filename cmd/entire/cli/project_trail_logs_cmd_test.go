package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/redact"
)

func TestLocalLogNumberDoesNotReadEditorDetail(t *testing.T) {
	t.Parallel()
	const trailID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/et/team/trails/7/logs/access" {
			t.Errorf("unexpected detail lookup: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"trail_id": trailID, "project_id": "project", "number": 7}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	target := &projectTrailTarget{Client: api.NewClientWithBaseURL("test", server.URL), BasePath: "/api/v1/et/team/trails", TrailID: "7", ProjectID: "project"}
	if err := resolveProjectLogNumber(t.Context(), target); err != nil || target.TrailID != trailID {
		t.Fatalf("resolve: %s %v", target.TrailID, err)
	}
}

func TestLocalLogRedactionUsesConfiguredScanners(t *testing.T) {
	t.Parallel()
	line, err := redactLocalLogLine("connect postgres://alice:plainpass123@host/db")
	if err != nil || strings.Contains(line, "plainpass123") {
		t.Fatalf("credential was not redacted: %q %v", line, err)
	}
}

func TestLocalLogRedactionWithholdsDegradedScanner(t *testing.T) {
	// Changes the process-global scanner configuration; must remain serial.
	redact.WithScannerDegradedSole(t)
	if line, err := redactLocalLogLine("must not upload"); !errors.Is(err, redact.ErrScannerDegraded) || line != "" {
		t.Fatalf("degraded scan escaped: %q %v", line, err)
	}
}

func TestLocalLogInputRedactionAndCompletion(t *testing.T) {
	t.Parallel()
	var records []localLogRecord
	var diagnostics bytes.Buffer
	err := publishLocalLogInput(t.Context(), strings.NewReader("hello\npassword=secret\n"), &diagnostics, []*regexp.Regexp{regexp.MustCompile(`password=\S+`)}, func(_ context.Context, batch []localLogRecord) error { records = append(records, batch...); return nil })
	if err != nil || len(records) != 3 || records[1].Text != "[REDACTED]" || records[2].Kind != "complete" {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	for i, r := range records {
		if r.Sequence != int64(i+1) {
			t.Fatalf("sequence: %+v", records)
		}
	}
}
func TestLocalLogInputOversizeGap(t *testing.T) {
	t.Parallel()
	var records []localLogRecord
	err := publishLocalLogInput(t.Context(), strings.NewReader(strings.Repeat("x", 20000)+"\nlast\n"), io.Discard, nil, func(_ context.Context, b []localLogRecord) error { records = append(records, b...); return nil })
	if err != nil {
		t.Fatal(err)
	}
	gap := false
	for _, r := range records {
		gap = gap || r.Kind == "gap"
		if len(r.Text) > 4096 {
			t.Fatal("unbounded record")
		}
	}
	if !gap || records[len(records)-1].Kind != "complete" {
		t.Fatalf("missing gap/completion: %+v", records)
	}
}
func TestLocalLogInputBoundsEscapedJSON(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Repeat(strings.Repeat("<", 4096)+"\n", 15))
	err := publishLocalLogInput(t.Context(), input, io.Discard, nil, func(_ context.Context, batch []localLogRecord) error {
		encoded, err := json.Marshal(map[string]any{"records": batch})
		if err != nil {
			return err
		}
		if len(encoded) > 128*1024 {
			t.Fatalf("oversized wire batch: %d", len(encoded))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalLogWatchReconnectUsesConsumedCursor(t *testing.T) {
	t.Parallel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			if _, err := io.WriteString(w, "event: log\nid: 1\ndata: {\"sequence\":1,\"kind\":\"text\",\"text\":\"first\"}\n\n"); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Header.Get("Last-Event-ID") != "1" {
			t.Error("lost consumed cursor")
		}
		if _, err := io.WriteString(w, "event: log\nid: 2\ndata: {\"sequence\":2,\"kind\":\"text\",\"text\":\"second\"}\n\nevent: end\ndata: {}\n\n"); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	err := watchLocalLogs(t.Context(), "/watch", &out, func(context.Context) (*api.Client, error) { return api.NewClientWithBaseURL("test", server.URL), nil })
	if err != nil || out.String() != "first\nsecond\n" || calls != 2 {
		t.Fatalf("resume: %q %d %v", out.String(), calls, err)
	}
}

func TestLocalLogInputDrainsAfterUploadFailure(t *testing.T) {
	t.Parallel()
	input := bytes.NewBufferString(strings.Repeat("line\n", 10000))
	err := publishLocalLogInput(t.Context(), input, io.Discard, nil, func(context.Context, []localLogRecord) error { return errors.New("offline") })
	if err == nil || input.Len() != 0 {
		t.Fatalf("not drained: %d, %v", input.Len(), err)
	}
}
func TestLocalLogAppendLostAcknowledgement(t *testing.T) {
	t.Parallel()
	calls := 0
	var first string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("bad request %s", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if calls == 0 {
			first = string(body)
		} else if first != string(body) {
			t.Error("retry changed content")
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "stream", "cursor": 1}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	err := appendLocalLogBatch(t.Context(), "/logs/stream", "stream", []localLogRecord{{Sequence: 1, Kind: "text", Text: "one"}}, func(context.Context) (*api.Client, error) { return api.NewClientWithBaseURL("test", server.URL), nil })
	if err != nil || calls != 2 {
		t.Fatalf("retry: %d %v", calls, err)
	}
}
func TestLocalLogPublishRequiresOptIn(t *testing.T) {
	t.Parallel()
	cmd := newProjectTrailLogsCmd()
	cmd.SetArgs([]string{"publish"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--share-with-trail") {
		t.Fatalf("missing opt-in: %v", err)
	}
}
func TestLocalLogWatchBufferLost(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "event: reset_required\ndata: {\"reason\":\"log_buffer_lost\"}\n\n"); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	err := watchLocalLogs(t.Context(), "/watch", io.Discard, func(context.Context) (*api.Client, error) { return api.NewClientWithBaseURL("test", server.URL), nil })
	if err == nil || !strings.Contains(err.Error(), "start a new publishing stream") {
		t.Fatalf("missing buffer-loss explanation: %v", err)
	}
}

func TestLocalLogWatchSafeOutput(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "event: log\nid: 1\ndata: {\"sequence\":1,\"kind\":\"text\",\"text\":\"hello\"}\n\nevent: end\ndata: {}\n\n"); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	err := watchLocalLogs(t.Context(), "/watch", &out, func(context.Context) (*api.Client, error) { return api.NewClientWithBaseURL("test", server.URL), nil })
	if err != nil || out.String() != "hello\n" {
		t.Fatalf("watch: %q %v", out.String(), err)
	}
}
