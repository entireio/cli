package copilotcli

import (
	"encoding/json"
	"testing"
)

func TestDetectHookHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want HookHost
	}{
		{
			name: "copilot cli numeric timestamp",
			raw:  `{"timestamp":1771480081360,"sessionId":"sess-123","prompt":"hi"}`,
			want: HostCopilotCLI,
		},
		{
			name: "vscode hook event field",
			raw:  `{"timestamp":"2026-02-09T10:30:00.000Z","sessionId":"sess-123","hookEventName":"UserPromptSubmit","prompt":"hi"}`,
			want: HostVSCode,
		},
		{
			name: "vscode transcript_path",
			raw:  `{"timestamp":1771480081360,"sessionId":"sess-123","transcript_path":"/tmp/transcript.json"}`,
			want: HostVSCode,
		},
		{
			name: "null hookEventName is not vscode",
			raw:  `{"timestamp":1771480081360,"sessionId":"sess-123","hookEventName":null}`,
			want: HostCopilotCLI,
		},
		{
			name: "null transcript_path is not vscode",
			raw:  `{"timestamp":1771480081360,"sessionId":"sess-123","transcript_path":null}`,
			want: HostCopilotCLI,
		},
		{
			name: "unknown payload",
			raw:  `{"sessionId":"sess-123"}`,
			want: HostUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.raw), &raw); err != nil {
				t.Fatalf("unmarshal test fixture: %v", err)
			}

			if got := detectHookHost(raw); got != tt.want {
				t.Fatalf("detectHookHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseTimestamp_NullAndZeroFallBackToNow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "null timestamp", raw: `{"timestamp":null,"sessionId":"s"}`},
		{name: "zero timestamp", raw: `{"timestamp":0,"sessionId":"s"}`},
		{name: "missing timestamp", raw: `{"sessionId":"s"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env, err := parseHookEnvelope([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseHookEnvelope() error = %v", err)
			}
			if env.Timestamp.IsZero() {
				t.Fatal("expected time.Now() fallback, got zero time")
			}
			if env.Timestamp.Year() < 2025 {
				t.Fatalf("expected recent timestamp from time.Now(), got %v", env.Timestamp)
			}
		})
	}
}

func TestDetectHookHost_NullTimestampIsNotCopilotCLI(t *testing.T) {
	t.Parallel()

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"timestamp":null,"sessionId":"s"}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := detectHookHost(raw); got == HostCopilotCLI {
		t.Fatalf("null timestamp should not classify as HostCopilotCLI, got %q", got)
	}
}

func TestParseHookEnvelope_AcceptsAlternateTranscriptPathAndTimestampFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		host HookHost
		path string
	}{
		{
			name: "copilot cli fields",
			raw:  `{"timestamp":1771480085412,"sessionId":"sess-123","transcriptPath":"/tmp/copilot.jsonl"}`,
			host: HostCopilotCLI,
			path: "/tmp/copilot.jsonl",
		},
		{
			name: "vscode fields",
			raw:  `{"timestamp":"2026-02-09T10:30:00.000Z","sessionId":"sess-123","hookEventName":"Stop","transcript_path":"/tmp/vscode.json"}`,
			host: HostVSCode,
			path: "/tmp/vscode.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env, err := parseHookEnvelope([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseHookEnvelope() error = %v", err)
			}
			if env.Host != tt.host {
				t.Fatalf("Host = %q, want %q", env.Host, tt.host)
			}
			if env.TranscriptPath != tt.path {
				t.Fatalf("TranscriptPath = %q, want %q", env.TranscriptPath, tt.path)
			}
			if env.Timestamp.IsZero() {
				t.Fatal("Timestamp should be populated")
			}
		})
	}
}

func TestParseHookEnvelope_AcceptsSnakeCaseSessionID(t *testing.T) {
	t.Parallel()

	env, err := parseHookEnvelope([]byte(`{"timestamp":"2026-02-09T10:30:00.000Z","session_id":"sess-456","hookEventName":"UserPromptSubmit","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("parseHookEnvelope() error = %v", err)
	}
	if env.SessionID != "sess-456" {
		t.Fatalf("SessionID = %q, want %q", env.SessionID, "sess-456")
	}
}
