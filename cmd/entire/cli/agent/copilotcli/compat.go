package copilotcli

import (
	"encoding/json"
	"fmt"
	"time"
)

// HookHost identifies which host format produced a copilot-compatible hook payload.
type HookHost string

const (
	HostUnknown    HookHost = "unknown"
	HostCopilotCLI HookHost = "copilot-cli"
	HostVSCode     HookHost = "vscode"
)

type hookEnvelope struct {
	Host           HookHost
	SessionID      string
	Prompt         string
	TranscriptPath string
	HookEventName  string
	Source         string
	InitialPrompt  string
	StopReason     string
	Reason         string
	Timestamp      time.Time
}

func parseHookEnvelope(data []byte) (*hookEnvelope, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty hook input")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	env := &hookEnvelope{
		Host:           detectHookHost(raw),
		SessionID:      firstString(raw, "sessionId"),
		Prompt:         firstString(raw, "prompt"),
		TranscriptPath: firstString(raw, "transcriptPath", "transcript_path"),
		HookEventName:  firstString(raw, "hookEventName"),
		Source:         firstString(raw, "source"),
		InitialPrompt:  firstString(raw, "initialPrompt"),
		StopReason:     firstString(raw, "stopReason"),
		Reason:         firstString(raw, "reason"),
	}

	ts, err := parseTimestamp(raw["timestamp"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	env.Timestamp = ts

	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}

	return env, nil
}

func detectHookHost(raw map[string]json.RawMessage) HookHost {
	if _, ok := raw["hookEventName"]; ok {
		return HostVSCode
	}
	if _, ok := raw["transcript_path"]; ok {
		return HostVSCode
	}
	if isJSONString(raw["timestamp"]) {
		return HostVSCode
	}
	if _, ok := raw["transcriptPath"]; ok {
		return HostCopilotCLI
	}
	if isJSONNumber(raw["timestamp"]) {
		return HostCopilotCLI
	}
	return HostUnknown
}

func firstString(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(value, &s); err == nil {
			return s
		}
	}
	return ""
}

func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 {
		return time.Time{}, nil
	}

	var millis int64
	if err := json.Unmarshal(raw, &millis); err == nil {
		return time.UnixMilli(millis), nil
	}

	var ts string
	if err := json.Unmarshal(raw, &ts); err != nil {
		return time.Time{}, err
	}

	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

func isJSONString(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '"' {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil
}

func isJSONNumber(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var n int64
	return json.Unmarshal(raw, &n) == nil
}
