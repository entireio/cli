package agents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// codexHookFile mirrors the on-disk shape of .codex/hooks.json. Field types
// match Codex's serde definitions (codex-rs/config/src/hook_config.rs) so the
// trust-hash computation below stays faithful to discover_handlers.
type codexHookFile struct {
	Hooks codexHookEvents `json:"hooks"`
}

type codexHookEvents struct {
	PreToolUse        []codexHookGroup `json:"PreToolUse"`
	PermissionRequest []codexHookGroup `json:"PermissionRequest"`
	PostToolUse       []codexHookGroup `json:"PostToolUse"`
	PreCompact        []codexHookGroup `json:"PreCompact"`
	PostCompact       []codexHookGroup `json:"PostCompact"`
	SessionStart      []codexHookGroup `json:"SessionStart"`
	UserPromptSubmit  []codexHookGroup `json:"UserPromptSubmit"`
	Stop              []codexHookGroup `json:"Stop"`
}

type codexHookGroup struct {
	Matcher *string             `json:"matcher"`
	Hooks   []codexHookHandlers `json:"hooks"`
}

type codexHookHandlers struct {
	Type          string  `json:"type"`
	Command       string  `json:"command"`
	Timeout       *uint64 `json:"timeout"`
	Async         bool    `json:"async"`
	StatusMessage *string `json:"statusMessage"`
}

// codexHookEventLabels lists every event name in the order Codex emits in
// the JSON schema, paired with the snake_case label it uses for trust state
// keys (codex-rs/hooks/src/lib.rs:hook_event_key_label).
var codexHookEventLabels = []struct {
	displayName string
	keyLabel    string
}{
	{"PreToolUse", "pre_tool_use"},
	{"PermissionRequest", "permission_request"},
	{"PostToolUse", "post_tool_use"},
	{"PreCompact", "pre_compact"},
	{"PostCompact", "post_compact"},
	{"SessionStart", "session_start"},
	{"UserPromptSubmit", "user_prompt_submit"},
	{"Stop", "stop"},
}

// codexHookTrustState reads .codex/hooks.json from projectDir and returns the
// `[hooks.state.<key>]` TOML block that pre-trusts every command handler
// declared there. The hash matches Codex's command_hook_hash exactly (see
// codex-rs/hooks/src/engine/discovery.rs and config/src/fingerprint.rs):
//
//  1. Build a NormalizedHookIdentity = { event_name, matcher, hooks: [normalized_handler] }
//  2. Serialize to JSON with object keys sorted alphabetically (canonical_json)
//  3. SHA-256 the compact JSON bytes; format as "sha256:<hex>"
//
// Returns empty string when no hooks file exists.
func codexHookTrustState(projectDir string) (string, error) {
	hooksPath := filepath.Join(projectDir, ".codex", "hooks.json")
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	var file codexHookFile
	if err := json.Unmarshal(data, &file); err != nil {
		return "", fmt.Errorf("parse hooks.json: %w", err)
	}

	var sb strings.Builder
	for _, ev := range codexHookEventLabels {
		groups := codexEventGroups(&file.Hooks, ev.displayName)
		for groupIdx, group := range groups {
			for handlerIdx, handler := range group.Hooks {
				if handler.Type != "command" {
					continue
				}
				timeoutSec := uint64(600)
				if handler.Timeout != nil {
					timeoutSec = *handler.Timeout
				}
				if timeoutSec < 1 {
					timeoutSec = 1
				}
				hash := codexCommandHookHash(ev.keyLabel, group.Matcher, handler.Command, timeoutSec, handler.Async, handler.StatusMessage)
				key := fmt.Sprintf("%s:%s:%d:%d", hooksPath, ev.keyLabel, groupIdx, handlerIdx)
				fmt.Fprintf(&sb, "[hooks.state.%q]\ntrusted_hash = %q\n\n", key, hash)
			}
		}
	}
	return sb.String(), nil
}

func codexEventGroups(events *codexHookEvents, displayName string) []codexHookGroup {
	switch displayName {
	case "PreToolUse":
		return events.PreToolUse
	case "PermissionRequest":
		return events.PermissionRequest
	case "PostToolUse":
		return events.PostToolUse
	case "PreCompact":
		return events.PreCompact
	case "PostCompact":
		return events.PostCompact
	case "SessionStart":
		return events.SessionStart
	case "UserPromptSubmit":
		return events.UserPromptSubmit
	case "Stop":
		return events.Stop
	}
	return nil
}

// codexCommandHookHash mirrors command_hook_hash + version_for_toml. Codex
// builds a TOML value of NormalizedHookIdentity then serde_json::to_value
// converts it to JSON; we skip the TOML round-trip and emit the equivalent
// JSON directly. Crucially:
//   - matcher is omitted when nil (TOML can't represent null Options)
//   - timeout is always present (the normalized handler always sets Some(_))
//   - statusMessage is omitted when nil
//   - object keys are sorted alphabetically by canonicalize before hashing
func codexCommandHookHash(eventLabel string, matcher *string, command string, timeoutSec uint64, async bool, statusMessage *string) string {
	handler := map[string]any{
		"type":    "command",
		"command": command,
		"timeout": json.Number(strconv.FormatUint(timeoutSec, 10)),
		"async":   async,
	}
	if statusMessage != nil {
		handler["statusMessage"] = *statusMessage
	}
	identity := map[string]any{
		"event_name": eventLabel,
		"hooks":      []any{handler},
	}
	if matcher != nil {
		identity["matcher"] = *matcher
	}

	canonical := codexCanonicalJSON(identity)
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// codexCanonicalJSON marshals v with object keys sorted alphabetically, no
// whitespace. Go's encoding/json already sorts map[string]any keys, but we
// re-walk explicitly to make the contract obvious and to keep the output
// stable if a future stdlib change reorders.
func codexCanonicalJSON(v any) []byte {
	var buf strings.Builder
	codexWriteCanonical(&buf, v)
	return []byte(buf.String())
}

func codexWriteCanonical(buf *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(codexJSONAtom(k))
			buf.WriteByte(':')
			codexWriteCanonical(buf, t[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			codexWriteCanonical(buf, item)
		}
		buf.WriteByte(']')
	default:
		buf.Write(codexJSONAtom(t))
	}
}

// codexJSONAtom marshals a scalar matching serde_json's default escaping
// rules: only the JSON-required characters (", \, control chars) are
// escaped — `<`, `>`, `&` pass through unchanged. Go's default json.Marshal
// HTML-escapes those bytes to \u00XX, which would diverge from Codex's
// hash for any command containing shell redirection (>, &&, etc.).
func codexJSONAtom(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	// Callers pass strings, bools, and json.Number atoms only; encoding
	// can't fail for those types. Ignore the error rather than complicating
	// every call site for a static guarantee.
	_ = enc.Encode(v) //nolint:errchkjson // scalar atoms only; error is unreachable
	out := b.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out
}
