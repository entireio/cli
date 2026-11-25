package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

// Codex re-asks for approval whenever a hook's definition changes: each
// `[hooks.state."<file>:<event>:<group>:<handler>"]` record keeps the
// trusted_hash of the definition the user approved, and Codex runs the
// handler only while that hash equals the hash of its current definition.
// A record that merely exists therefore does not prove the hook runs.
//
// codexHookDeclarationHash mirrors Codex's hook_hash (codex-rs
// hooks/src/engine/discovery.rs) for command handlers: the normalized handler
// and its matcher, under the event's trust label, serialized as JSON with
// sorted keys and no insignificant whitespace, then SHA-256'd. Anything it
// cannot reproduce exactly reports ok=false, so callers treat the hook as not
// approved rather than guess.

// codexMatcherEvents are the managed events whose matcher is part of the
// hook identity; Codex drops the matcher for the others (Stop,
// UserPromptSubmit).
var codexMatcherEvents = map[string]bool{
	"PreToolUse": true, "PermissionRequest": true, "PostToolUse": true,
	"SessionStart": true, "SessionEnd": true, "SubagentStart": true,
	"SubagentStop": true, "PreCompact": true, "PostCompact": true,
}

// codexCommandHandlerFields are the handler keys the hash reproduces. Other
// keys (onFailure, additionalContextLimit, ...) change the identity in ways
// not mirrored here.
var codexCommandHandlerFields = map[string]bool{
	"type": true, "command": true, "commandWindows": true, "command_windows": true,
	"timeout": true, "async": true, "statusMessage": true,
}

const codexCommandHandlerType = "command"

type codexCommandHandler struct {
	Type           string  `json:"type"`
	Command        string  `json:"command"`
	CommandWindows *string `json:"commandWindows"`
	CommandWinAlt  *string `json:"command_windows"`
	Timeout        *uint64 `json:"timeout"`
	Async          bool    `json:"async"`
	StatusMessage  *string `json:"statusMessage"`
}

func codexHookDeclarationHash(event, label string, matcher *string, rawHandler json.RawMessage) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(rawHandler, &fields) != nil {
		return "", false
	}
	for name := range fields {
		if !codexCommandHandlerFields[name] {
			return "", false
		}
	}
	var h codexCommandHandler
	if json.Unmarshal(rawHandler, &h) != nil || h.Type != codexCommandHandlerType {
		return "", false
	}
	command := h.Command
	windowsCommand := h.CommandWindows
	if windowsCommand == nil {
		windowsCommand = h.CommandWinAlt
	}
	if runtime.GOOS == "windows" && windowsCommand != nil {
		command = *windowsCommand
	}

	identity := codexHookIdentity{
		EventName: label,
		Hooks: []codexHandlerIdentity{{
			Async:         h.Async,
			Command:       command,
			StatusMessage: h.StatusMessage,
			Timeout:       codexNormalizedTimeout(event, h.Timeout),
			Type:          codexCommandHandlerType,
		}},
	}
	if codexMatcherEvents[event] {
		identity.Matcher = matcher
	}
	serialized, ok := codexCanonicalJSON(identity)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256(serialized)
	return "sha256:" + hex.EncodeToString(sum[:]), true
}

// codexHookIdentity and codexHandlerIdentity declare their fields in sorted
// key order, so encoding/json writes the sorted object Codex hashes. Absent
// optional fields are omitted, as Codex's TOML round trip drops them.
type codexHookIdentity struct {
	EventName string                 `json:"event_name"`
	Hooks     []codexHandlerIdentity `json:"hooks"`
	Matcher   *string                `json:"matcher,omitempty"`
}

type codexHandlerIdentity struct {
	Async         bool    `json:"async"`
	Command       string  `json:"command"`
	StatusMessage *string `json:"statusMessage,omitempty"`
	Timeout       uint64  `json:"timeout"`
	Type          string  `json:"type"`
}

// codexNormalizedTimeout mirrors Codex's normalize_command_hook.
func codexNormalizedTimeout(event string, timeout *uint64) uint64 {
	if event == "SessionEnd" {
		const sessionEndDefault, sessionEndMax = 1, 3
		if timeout == nil {
			return sessionEndDefault
		}
		return min(max(*timeout, 1), sessionEndMax)
	}
	if timeout == nil {
		return 600
	}
	return max(*timeout, 1)
}

// codexCanonicalJSON encodes like serde_json's compact writer. Go and
// serde_json agree on everything except a few escapes; strings that would
// hit one of those report ok=false.
func codexCanonicalJSON(identity codexHookIdentity) ([]byte, bool) {
	texts := []string{identity.EventName, identity.Hooks[0].Command}
	if identity.Matcher != nil {
		texts = append(texts, *identity.Matcher)
	}
	if message := identity.Hooks[0].StatusMessage; message != nil {
		texts = append(texts, *message)
	}
	for _, text := range texts {
		if strings.ContainsAny(text, "\u2028\u2029") || strings.ContainsFunc(text, func(r rune) bool {
			return r < 0x20 && r != '\n' && r != '\r' && r != '\t'
		}) {
			return nil, false
		}
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(identity); err != nil {
		return nil, false
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), true
}

type codexRawMatcherGroup struct {
	Matcher *string           `json:"matcher"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// entireHooksApprovedAsDeclared reports whether Codex has approved the
// current definition of every Entire handler in the hooks file at hooksPath.
// Unknown approval state, a missing record, a stale hash, or a definition the
// hash cannot reproduce all report false.
func entireHooksApprovedAsDeclared(hooksPath string, document *hooksDocument) bool {
	configPath := codexConfigPath()
	if configPath == "" {
		return false
	}
	records, ok := readCodexTrustRecords(configPath)
	if !ok {
		return false
	}
	approved := approvedHashesForFile(records, hooksPath)
	found := false
	for _, h := range managedHooks {
		data, present := document.rawHooks[h.event]
		if !present {
			continue
		}
		var groups []codexRawMatcherGroup
		if json.Unmarshal(data, &groups) != nil {
			return false
		}
		for gi, group := range groups {
			for hi, raw := range group.Hooks {
				var entry HookEntry
				if json.Unmarshal(raw, &entry) != nil || !isEntireHook(entry.Command) {
					continue
				}
				found = true
				want, ok := codexHookDeclarationHash(h.event, h.label, group.Matcher, raw)
				if !ok || approved[fmt.Sprintf("%s:%d:%d", h.label, gi, hi)] != want {
					return false
				}
			}
		}
	}
	return found
}

// approvedHashesForFile narrows Codex's approval records to the hooks file at
// hooksPath, keyed by "<label>:<group>:<handler>".
func approvedHashesForFile(records map[string]string, hooksPath string) map[string]string {
	approved := make(map[string]string)
	canonicalHooksPath, err := canonicalPath(hooksPath)
	if err != nil {
		return approved
	}
	for key, hash := range records {
		trustedHooksPath, _, ok := parseCodexTrustKey(key)
		if !ok || hash == "" {
			continue
		}
		canonicalTrustedPath, err := canonicalPath(trustedHooksPath)
		if err != nil || canonicalTrustedPath != canonicalHooksPath {
			continue
		}
		approved[key[len(trustedHooksPath)+1:]] = hash
	}
	return approved
}
