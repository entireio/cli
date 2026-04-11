package copilotcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// HookHost identifies which host format produced a copilot-compatible hook payload.
type HookHost string

const (
	HostUnknown    HookHost = "unknown"
	HostCopilotCLI HookHost = "copilot-cli"
	HostVSCode     HookHost = "vscode"
)

// VS Code hookEventName values (from official VS Code docs).
// See: https://code.visualstudio.com/docs/copilot/customization/hooks
const (
	VSCodeEventSessionStart     = "SessionStart"
	VSCodeEventUserPromptSubmit = "UserPromptSubmit"
	VSCodeEventStop             = "Stop"
	VSCodeEventPreToolUse       = "PreToolUse"
	VSCodeEventPostToolUse      = "PostToolUse"
	VSCodeEventPreCompact       = "PreCompact"
	VSCodeEventSubagentStart    = "SubagentStart"
	VSCodeEventSubagentStop     = "SubagentStop"
)

// vsCodeEventToHookNames maps each VS Code hookEventName to the CLI hook name(s)
// that are allowed to carry that event. "Stop" maps to both agent-stop and
// session-end because VS Code uses a single Stop event where Copilot CLI
// distinguishes the two.
var vsCodeEventToHookNames = map[string][]string{
	VSCodeEventUserPromptSubmit: {HookNameUserPromptSubmitted},
	VSCodeEventSessionStart:     {HookNameSessionStart},
	VSCodeEventStop:             {HookNameAgentStop, HookNameSessionEnd},
	VSCodeEventSubagentStop:     {HookNameSubagentStop},
	VSCodeEventPreToolUse:       {HookNamePreToolUse},
	VSCodeEventPostToolUse:      {HookNamePostToolUse},
	VSCodeEventPreCompact:       {},
	VSCodeEventSubagentStart:    {},
}

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
		return nil, errors.New("empty hook input")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	env := &hookEnvelope{
		Host:           detectHookHost(raw),
		SessionID:      firstString(raw, "sessionId", "session_id"),
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
	if isJSONString(raw["hookEventName"]) {
		return HostVSCode
	}
	if isJSONString(raw["transcript_path"]) {
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
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}

	var millis int64
	if err := json.Unmarshal(raw, &millis); err == nil {
		if millis == 0 {
			return time.Time{}, nil // Treat epoch as missing — triggers time.Now() fallback.
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

func isJSONString(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '"' {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil
}

func isJSONNumber(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] == 'n' {
		return false
	}
	var n int64
	return json.Unmarshal(raw, &n) == nil
}

// validateVSCodeEvent checks whether the hookEventName is consistent with the
// CLI hook subcommand that was invoked. Returns true if the event should be
// processed, false if it should be silently skipped (mismatch or unknown event).
func validateVSCodeEvent(env *hookEnvelope, hookName string) bool {
	hookEventName := env.HookEventName
	allowedHooks, known := vsCodeEventToHookNames[hookEventName]
	if !known {
		return false
	}
	if !slices.Contains(allowedHooks, hookName) {
		return false
	}

	// VS Code overloads "Stop" for both end-of-turn and terminal session-stop
	// payloads. Route them by reason to avoid ending sessions on ordinary turns.
	if hookEventName == VSCodeEventStop {
		isTerminal := isTerminalVSCodeStop(env)
		switch hookName {
		case HookNameAgentStop:
			return !isTerminal
		case HookNameSessionEnd:
			return isTerminal
		}
	}

	return true
}

func isTerminalVSCodeStop(env *hookEnvelope) bool {
	if env.Reason != "" {
		return true
	}

	switch env.StopReason {
	case "", "end_turn":
		return false
	default:
		return true
	}
}
