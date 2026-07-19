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

// Copilot CLI 1.0.71 started emitting timestamp as float epoch-millis
// (e.g. 1784283185447.0). The strict int64 parse rejected it, so every
// lifecycle hook (session-start, user-prompt-submitted, agent-stop,
// session-end) failed and no Entire session was ever created.
func TestParseHookEnvelope_AcceptsFloatTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantMillis int64
	}{
		{
			name:       "sessionStart",
			raw:        `{"sessionId":"sess-123","timestamp":1784283185447.0,"cwd":"/tmp/repo","source":"new","initialPrompt":"hi"}`,
			wantMillis: 1784283185447,
		},
		{
			name:       "userPromptSubmitted",
			raw:        `{"sessionId":"sess-123","timestamp":1784283185370.0,"cwd":"/tmp/repo","prompt":"hi"}`,
			wantMillis: 1784283185370,
		},
		{
			name:       "agentStop",
			raw:        `{"sessionId":"sess-123","timestamp":1784283190710.0,"cwd":"/tmp/repo","transcriptPath":"/tmp/events.jsonl","stopReason":"end_turn"}`,
			wantMillis: 1784283190710,
		},
		{
			name:       "sessionEnd",
			raw:        `{"sessionId":"sess-123","timestamp":1784283190784.0,"cwd":"/tmp/repo","reason":"complete"}`,
			wantMillis: 1784283190784,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env, err := parseHookEnvelope([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseHookEnvelope() error = %v", err)
			}
			if env.Host != HostCopilotCLI {
				t.Fatalf("Host = %q, want %q", env.Host, HostCopilotCLI)
			}
			if got := env.Timestamp.UnixMilli(); got != tt.wantMillis {
				t.Fatalf("Timestamp.UnixMilli() = %d, want %d", got, tt.wantMillis)
			}
			if env.SessionID != "sess-123" {
				t.Fatalf("SessionID = %q, want %q", env.SessionID, "sess-123")
			}
		})
	}
}

func TestParseTimestamp_FloatMillis(t *testing.T) {
	t.Parallel()

	ts, err := ParseTimestamp(json.RawMessage(`1784283185447.0`))
	if err != nil {
		t.Fatalf("ParseTimestamp() error = %v", err)
	}
	if got := ts.UnixMilli(); got != 1784283185447 {
		t.Fatalf("UnixMilli() = %d, want 1784283185447", got)
	}
}

func TestParseTimestamp_SubMillisecondFloatIsMissing(t *testing.T) {
	t.Parallel()

	// 0.4 truncates to 0 ms — must be treated as "missing" (zero time),
	// not as the Unix epoch.
	ts, err := ParseTimestamp(json.RawMessage(`0.4`))
	if err != nil {
		t.Fatalf("ParseTimestamp() error = %v", err)
	}
	if !ts.IsZero() {
		t.Fatalf("ParseTimestamp(0.4) = %v, want zero time", ts)
	}
}

func TestParseTimestamp_OutOfRangeFloatErrors(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{`1e300`, `-1e300`} {
		if _, err := ParseTimestamp(json.RawMessage(raw)); err == nil {
			t.Fatalf("ParseTimestamp(%s) expected out-of-range error, got nil", raw)
		}
	}
}

func TestDetectHookHost_FloatTimestampIsCopilotCLI(t *testing.T) {
	t.Parallel()

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"timestamp":1784283185447.0,"sessionId":"s","prompt":"hi"}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := detectHookHost(raw); got != HostCopilotCLI {
		t.Fatalf("detectHookHost() = %q, want %q", got, HostCopilotCLI)
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
