package claudecode

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGenerateTextResponse_IsErrorEnvelope(t *testing.T) {
	t.Parallel()
	stdout := `{"type":"result","subtype":"success","is_error":true,"api_error_status":404,"result":"model not found"}`
	result, env, err := parseGenerateTextResponse([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "model not found" {
		t.Errorf("result = %q; want %q", result, "model not found")
	}
	if env == nil {
		t.Fatal("envelope = nil; want non-nil")
	}
	if !env.IsError {
		t.Error("IsError = false; want true")
	}
	if env.APIErrorStatus == nil || *env.APIErrorStatus != 404 {
		t.Errorf("APIErrorStatus = %v; want *404", env.APIErrorStatus)
	}
}

func TestParseGenerateTextResponse_IsErrorEnvelopeNullResult(t *testing.T) {
	t.Parallel()
	// Claude CLI can emit is_error:true with result:null on internal failures.
	// The envelope's IsError / APIErrorStatus signal must survive even though
	// Result is absent — otherwise classification falls through to a generic
	// "missing result item" parse error and the upstream typed-error path
	// loses the api_error_status.
	//
	// Both wire shapes (single object and event array) must behave the same.
	tests := []struct {
		name   string
		stdout string
	}{
		{"single object", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":500,"result":null}`},
		{"event array", `[{"type":"system"},{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":500,"result":null}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, env, err := parseGenerateTextResponse([]byte(tc.stdout))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != "" {
				t.Errorf("result = %q; want empty string", result)
			}
			if env == nil {
				t.Fatal("envelope = nil; want non-nil")
			}
			if !env.IsError {
				t.Error("IsError = false; want true")
			}
			if env.APIErrorStatus == nil || *env.APIErrorStatus != 500 {
				t.Errorf("APIErrorStatus = %v; want *500", env.APIErrorStatus)
			}
		})
	}
}

func TestParseGenerateTextResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stdout  string
		want    string
		wantErr string
	}{
		{
			name:   "legacy object result",
			stdout: `{"result":"hello"}`,
			want:   "hello",
		},
		{
			name:   "legacy object empty result",
			stdout: `{"result":""}`,
			want:   "",
		},
		{
			name:   "array result",
			stdout: `[{"type":"system"},{"type":"result","result":"hello"}]`,
			want:   "hello",
		},
		{
			name:   "array empty result",
			stdout: `[{"type":"system"},{"type":"result","result":""}]`,
			want:   "",
		},
		{
			name:    "missing result item",
			stdout:  `[{"type":"system"},{"type":"assistant","message":"working"}]`,
			wantErr: "missing result item",
		},
		{
			name:    "invalid json",
			stdout:  `not json`,
			wantErr: "unsupported Claude CLI JSON response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, _, err := parseGenerateTextResponse([]byte(tt.stdout))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseGenerateTextResponse() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStreamClaudeResponse_Success(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var phases []string
	final, malformed, err := streamClaudeResponse(bytes.NewReader(data), func(ev streamEvent) {
		switch {
		case ev.Type == streamEventTypeSystem && ev.Subtype == "status" && ev.Status == "requesting":
			phases = append(phases, "connecting")
		case ev.Type == streamEventTypeStreamEvent && ev.Event.Type == "message_start":
			phases = append(phases, "first-token")
		case ev.Type == streamEventTypeStreamEvent && ev.Event.Type == "content_block_delta":
			phases = append(phases, "generating")
		case ev.Type == streamEventTypeResult:
			phases = append(phases, streamEventTypeResult)
		}
	})

	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if malformed != 0 {
		t.Errorf("malformed = %d, want 0", malformed)
	}
	if final == nil {
		t.Fatal("expected final result event")
	}
	if final.IsError {
		t.Error("expected is_error=false on success fixture")
	}
	if final.Result == nil || *final.Result != "Hello, world." {
		t.Errorf("result = %v, want %q", final.Result, "Hello, world.")
	}
	wantPhases := []string{"connecting", "first-token", "generating", "generating", streamEventTypeResult}
	if !equalStrings(phases, wantPhases) {
		t.Errorf("phases = %v, want %v", phases, wantPhases)
	}
}

func TestStreamClaudeResponse_ErrorEnvelope(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_error_404.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	final, _, err := streamClaudeResponse(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if final == nil {
		t.Fatal("expected final result event")
	}
	if !final.IsError {
		t.Error("expected is_error=true")
	}
	if final.APIErrorStatus == nil || *final.APIErrorStatus != 404 {
		t.Errorf("api_error_status = %v, want 404", final.APIErrorStatus)
	}
}

func TestStreamClaudeResponse_MalformedLineSkipped(t *testing.T) {
	t.Parallel()

	stream := `{"type":"system","subtype":"status","status":"requesting"}
this is not json
{"type":"result","is_error":false,"result":"ok"}
`
	final, malformed, err := streamClaudeResponse(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if malformed != 1 {
		t.Errorf("malformed = %d, want 1", malformed)
	}
	if final == nil || final.Result == nil || *final.Result != "ok" {
		t.Errorf("expected result %q, got %+v", "ok", final)
	}
}

func TestStreamClaudeResponse_NoResultEvent(t *testing.T) {
	t.Parallel()

	stream := `{"type":"system","subtype":"status","status":"requesting"}
`
	_, _, err := streamClaudeResponse(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected error when stream has no result event")
	}
	if !strings.Contains(err.Error(), "without a result event") {
		t.Errorf("error = %q, want 'without a result event'", err)
	}
}

// equalStrings is a local helper to avoid pulling in reflect.DeepEqual.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
