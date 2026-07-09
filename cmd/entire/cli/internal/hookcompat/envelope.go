package hookcompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

// Envelope contains the common hook fields shared by CLI and plugin-style hosts.
type Envelope struct {
	SessionID      string
	Prompt         string
	TranscriptPath string
	HookEventName  string
	Model          string
	CWD            string
	Timestamp      time.Time
}

// ReadRaw reads a hook JSON payload into a raw object map.
func ReadRaw(stdin io.Reader) (map[string]json.RawMessage, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to read hook input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty hook input")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	return raw, nil
}

// EnvelopeFromRaw extracts common fields from either snake_case or camelCase payloads.
func EnvelopeFromRaw(raw map[string]json.RawMessage) (*Envelope, error) {
	ts, err := ParseTimestamp(raw["timestamp"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	if ts.IsZero() {
		ts = time.Now()
	}

	return &Envelope{
		SessionID:      FirstString(raw, "session_id", "sessionId"),
		Prompt:         FirstString(raw, "prompt"),
		TranscriptPath: FirstString(raw, "transcript_path", "transcriptPath"),
		HookEventName:  FirstString(raw, "hook_event_name", "hookEventName"),
		Model:          FirstString(raw, "model"),
		CWD:            FirstString(raw, "cwd"),
		Timestamp:      ts,
	}, nil
}

// ReadEnvelope reads and extracts common hook fields.
func ReadEnvelope(stdin io.Reader) (*Envelope, error) {
	raw, err := ReadRaw(stdin)
	if err != nil {
		return nil, err
	}
	return EnvelopeFromRaw(raw)
}

// FirstString returns the first JSON string found under keys.
func FirstString(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok || len(value) == 0 || string(value) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(value, &s); err == nil {
			return s
		}
	}
	return ""
}

// FirstRaw returns the first present raw JSON value under keys.
func FirstRaw(raw map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			return value
		}
	}
	return nil
}

// ParseTimestamp decodes epoch-millis or RFC3339Nano hook timestamps.
func ParseTimestamp(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}

	var millis int64
	if err := json.Unmarshal(raw, &millis); err == nil {
		if millis == 0 {
			return time.Time{}, nil
		}
		return time.UnixMilli(millis), nil
	}

	var ts string
	if err := json.Unmarshal(raw, &ts); err != nil {
		return time.Time{}, fmt.Errorf("unmarshal timestamp string: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", ts, err)
	}
	return parsed, nil
}

// HookEventMatches validates optional hookEventName/hook_event_name fields.
func HookEventMatches(hookEventName, hookName string, allowed map[string][]string) bool {
	if hookEventName == "" {
		return true
	}
	allowedHooks, ok := allowed[hookEventName]
	return ok && slices.Contains(allowedHooks, hookName)
}
