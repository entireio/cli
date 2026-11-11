// Package opencodev2 converts an OpenCode 2 session export to the OpenCode 1
// shape every reader of OpenCode sessions takes: the transcript library's
// Convert and OpenCode adapter, and the CLI's OpenCode agent. It imports only
// the standard library (the library's dependency guard).
package opencodev2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// OpenCode 2 exports a session as flat, typed messages:
//
//	{"info": {...}, "messages": [{"type": "user", "text": ...},
//	  {"type": "assistant", "content": [{"type": "tool", "name": "write", ...}]}, ...]}
//
// Everything downstream of the export (the CLI's prompt, file and token
// extraction, subagent slicing and checkpoint storage, Convert, the
// OpenCodeAdapter and entire.io's transcript view) reads the OpenCode 1
// shape, {"info": ..., "parts": [...]} per message, so a 2.x export is
// converted to that shape once, when it is read (Normalize).
// Sessions OpenCode 2 migrated from a 1.x database keep their 1.x tool names
// and input keys, so both naming schemes are accepted.

// The OpenCode 1 names the conversion writes.
const (
	roleUser       = "user"
	roleAssistant  = "assistant"
	partText       = "text"
	partCompaction = "compaction"
	partTool       = "tool"
	toolTask       = "task" // OpenCode 1's subagent tool
)

// openCodeV2ToolNames maps OpenCode 2 tool names to their 1.x names.
var openCodeV2ToolNames = map[string]string{
	"subagent": toolTask,
	"patch":    "apply_patch",
	"shell":    "bash",
}

// IsExport reports whether data is an OpenCode 2 export: its messages carry
// a top-level "type" rather than an "info" object. An export is written by
// one OpenCode version, so the first message decides; it is read with a
// streaming decoder, so a 1.x export (every stored transcript) costs one
// message's decode, not the whole file's. An export with no messages is the
// same in both versions.
func IsExport(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return false
		}
		if key != "messages" {
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return false
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('[') || !dec.More() {
			return false
		}
		var first map[string]json.RawMessage
		if dec.Decode(&first) != nil {
			return false
		}
		_, hasInfo := first["info"]
		_, hasType := first["type"]
		return hasType && !hasInfo
	}
	return false
}

// NativeKey is the message info key that keeps the OpenCode 2
// messages a converted message came from, verbatim. Every reader ignores it;
// the CLI's OpenCode resume uses it to give OpenCode 2 back the exact session
// it exported, which the 1.x shape alone cannot reproduce.
const NativeKey = "opencode2"

// Normalize converts an OpenCode 2 export to the OpenCode 1
// shape and returns other payloads unchanged. The conversion is
// deterministic, so re-exporting the same session yields the same message
// indices; transcript offsets stored between turns depend on that.
//
// Each converted message carries its source messages under
// info[NativeKey]: the 2.x message itself, plus any that convert to
// nothing (idle markers, kinds this version does not know) after it. Both
// messages of a converted compaction carry the key, the summary with none.
func Normalize(data []byte) ([]byte, error) {
	if !IsExport(data) {
		return data, nil
	}
	var doc struct {
		Info     map[string]any    `json:"info"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse OpenCode 2 export: %w", err)
	}
	sessionID := v2String(doc.Info, "id")
	dir := ""
	if loc, ok := doc.Info["location"].(map[string]any); ok {
		dir = v2String(loc, "directory")
	}
	info := map[string]any{}
	for k, v := range doc.Info {
		info[k] = v
	}
	if dir != "" {
		if _, ok := info["directory"]; !ok {
			info["directory"] = dir
		}
	}

	messages := make([]map[string]any, 0, len(doc.Messages))
	var unplaced []json.RawMessage // source messages before the first converted one
	for _, raw := range doc.Messages {
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("parse OpenCode 2 export message: %w", err)
		}
		converted := convertOpenCodeV2Message(msg, sessionID, dir)
		if len(converted) == 0 {
			if len(messages) > 0 {
				addNativeMessages(messages[len(messages)-1], raw)
			} else {
				unplaced = append(unplaced, raw)
			}
			continue
		}
		addNativeMessages(converted[0], append(unplaced, raw)...)
		unplaced = nil
		for _, extra := range converted[1:] {
			addNativeMessages(extra)
		}
		messages = append(messages, converted...)
	}
	out, err := json.Marshal(map[string]any{"info": info, "messages": messages})
	if err != nil {
		return nil, fmt.Errorf("encode normalized OpenCode export: %w", err)
	}
	return out, nil
}

// addNativeMessages appends source messages to a converted message's
// info[NativeKey], creating it (possibly empty).
func addNativeMessages(msg map[string]any, raw ...json.RawMessage) {
	info, ok := msg["info"].(map[string]any)
	if !ok {
		return
	}
	existing, ok := info[NativeKey].([]json.RawMessage)
	if !ok {
		existing = []json.RawMessage{}
	}
	info[NativeKey] = append(existing, raw...)
}

// convertOpenCodeV2Message returns the OpenCode 1 messages for one OpenCode 2
// message: none for an idle marker, two for a compaction (the 1.x marker and
// its summary), one otherwise.
func convertOpenCodeV2Message(msg map[string]any, sessionID, dir string) []map[string]any {
	if _, ok := msg["info"]; ok {
		return []map[string]any{msg} // already 1.x-shaped
	}
	id := v2String(msg, "id")
	typ := v2String(msg, "type")
	created := v2Time(msg, "created")
	info := map[string]any{
		"id":        id,
		"sessionID": sessionID,
		"time":      map[string]any{"created": created},
	}
	switch typ {
	case "user":
		info["role"] = roleUser
		parts := []map[string]any{}
		if text := v2String(msg, "text"); text != "" {
			parts = append(parts, v2Part(id, 0, map[string]any{"type": partText, "text": text}))
		}
		if files, ok := msg["files"].([]any); ok {
			for _, f := range files {
				if file, ok := f.(map[string]any); ok {
					part := map[string]any{"type": "file"}
					for k, v := range file {
						part[k] = v
					}
					parts = append(parts, v2Part(id, len(parts), part))
				}
			}
		}
		return []map[string]any{{"info": info, "parts": parts}}
	case "synthetic", "system":
		// Text OpenCode wrote into the conversation itself: a background
		// subagent's result, or the tool-rename notice after a 1.x migration.
		info["role"] = roleUser
		text := v2String(msg, "text")
		return []map[string]any{{"info": info, "parts": []map[string]any{
			v2Part(id, 0, map[string]any{"type": partText, "text": text, "synthetic": true}),
		}}}
	case partCompaction:
		info["role"] = roleUser
		marker := map[string]any{"info": info, "parts": []map[string]any{
			v2Part(id, 0, map[string]any{"type": partCompaction, "auto": msg["reason"] == "auto"}),
		}}
		summary := map[string]any{
			"id":        id + "-summary",
			"sessionID": sessionID,
			"role":      roleAssistant,
			"summary":   true,
			"time":      map[string]any{"created": created, "completed": created},
		}
		copyV2Usage(summary, msg)
		text := v2String(msg, "summary")
		return []map[string]any{marker, {"info": summary, "parts": []map[string]any{
			v2Part(id+"-summary", 0, map[string]any{"type": partText, "text": text}),
		}}}
	case roleAssistant:
		info["role"] = roleAssistant
		timeInfo := map[string]any{"created": created}
		if completed := v2Time(msg, "completed"); completed != 0 {
			timeInfo["completed"] = completed
		}
		info["time"] = timeInfo
		if agent, ok := msg["agent"].(string); ok {
			info["agent"] = agent
			info["mode"] = agent
		}
		if finish, ok := msg["finish"]; ok {
			info["finish"] = finish
		}
		copyV2Usage(info, msg)
		var parts []map[string]any
		content, ok := msg["content"].([]any)
		if !ok {
			content = nil
		}
		for i, c := range content {
			if part, ok := c.(map[string]any); ok {
				parts = append(parts, v2Part(id, i, convertOpenCodeV2Content(part, dir)))
			}
		}
		if parts == nil {
			parts = []map[string]any{}
		}
		return []map[string]any{{"info": info, "parts": parts}}
	default:
		// "idle" and any marker this version does not know carry no
		// conversation content.
		return nil
	}
}

// copyV2Usage copies an OpenCode 2 message's model, cost and tokens into a
// 1.x message info.
func copyV2Usage(info, msg map[string]any) {
	if model, ok := msg["model"].(map[string]any); ok {
		if id, ok := model["id"].(string); ok {
			info["modelID"] = id
		}
		if provider, ok := model["providerID"].(string); ok {
			info["providerID"] = provider
		}
	}
	if cost, ok := msg["cost"]; ok {
		info["cost"] = cost
	}
	if tokens, ok := msg["tokens"]; ok {
		info["tokens"] = tokens
	}
}

// convertOpenCodeV2Content converts one assistant content entry. Text and
// reasoning keep their shape; a tool entry becomes a 1.x tool part.
func convertOpenCodeV2Content(part map[string]any, dir string) map[string]any {
	if part["type"] != partTool {
		return part
	}
	name := v2String(part, "name")
	if legacy, ok := openCodeV2ToolNames[name]; ok {
		name = legacy
	}
	out := map[string]any{"type": partTool, "tool": name}
	if id, ok := part["id"].(string); ok {
		out["callID"] = id
	}
	state, ok := part["state"].(map[string]any)
	if !ok {
		return out
	}
	newState := map[string]any{}
	for k, v := range state {
		switch k {
		case "content", "error", "input", "metadata":
		default:
			newState[k] = v
		}
	}
	if input, ok := state["input"].(map[string]any); ok {
		newState["input"] = convertOpenCodeV2ToolInput(name, input, dir)
	}
	if text := v2ContentText(state["content"]); text != "" {
		newState["output"] = text
	}
	if e, ok := state["error"].(map[string]any); ok {
		if message, ok := e["message"].(string); ok {
			newState["error"] = message
		}
	}
	if meta, ok := state["metadata"].(map[string]any); ok {
		newState["metadata"] = convertOpenCodeV2ToolMetadata(meta, dir)
	}
	if t, ok := part["time"].(map[string]any); ok {
		start := t["ran"]
		if start == nil {
			start = t["created"]
		}
		newState["time"] = map[string]any{"start": start, "end": t["completed"]}
	}
	out["state"] = newState
	return out
}

// convertOpenCodeV2ToolInput maps OpenCode 2 input keys to their 1.x names:
// edit and write take `path` (1.x `filePath`), resolved against the session
// directory like the paths in the tool's metadata; the subagent tool takes
// `agent` and `sessionID` (1.x task's `subagent_type` and `task_id`).
func convertOpenCodeV2ToolInput(tool string, input map[string]any, dir string) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	rename := func(from, to string) {
		if v, ok := out[from]; ok {
			if _, exists := out[to]; !exists {
				out[to] = v
			}
			delete(out, from)
		}
	}
	switch tool {
	case "edit", "write":
		if p, ok := out["path"].(string); ok && p != "" {
			out["path"] = v2AbsPath(dir, p)
		}
		rename("path", "filePath")
	case toolTask:
		rename("agent", "subagent_type")
		rename("sessionID", "task_id")
	}
	return out
}

// convertOpenCodeV2ToolMetadata maps a patch's `files[].file`, relative to the
// session directory in OpenCode 2, to the 1.x absolute `filePath`, and the
// subagent's child `sessionID` to the 1.x `sessionId`.
func convertOpenCodeV2ToolMetadata(meta map[string]any, dir string) map[string]any {
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	if child, ok := out["sessionID"]; ok {
		if _, exists := out["sessionId"]; !exists {
			out["sessionId"] = child
		}
		delete(out, "sessionID")
	}
	files, ok := meta["files"].([]any)
	if !ok {
		return out
	}
	converted := make([]any, 0, len(files))
	for _, f := range files {
		file, ok := f.(map[string]any)
		if !ok {
			converted = append(converted, f)
			continue
		}
		entry := make(map[string]any, len(file))
		for k, v := range file {
			entry[k] = v
		}
		if rel, ok := file["file"].(string); ok && rel != "" {
			if _, exists := entry["filePath"]; !exists {
				entry["filePath"] = v2AbsPath(dir, rel)
				entry["relativePath"] = rel
			}
		}
		converted = append(converted, entry)
	}
	out["files"] = converted
	return out
}

func v2AbsPath(dir, p string) string {
	if dir == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, filepath.FromSlash(p))
}

// v2ContentText joins the text entries of a tool result's content.
func v2ContentText(content any) string {
	entries, ok := content.([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, e := range entries {
		if entry, ok := e.(map[string]any); ok {
			if text, ok := entry["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// v2Part stamps a converted part with a stable ID; 1.x parts always carry one.
func v2Part(messageID string, index int, part map[string]any) map[string]any {
	if _, ok := part["id"]; !ok {
		part["id"] = messageID + "-" + strconv.Itoa(index)
	}
	return part
}

// v2Time reads msg.time[key] as Unix ms, 0 when absent.
func v2Time(msg map[string]any, key string) int64 {
	t, ok := msg["time"].(map[string]any)
	if !ok {
		return 0
	}
	if v, ok := t[key].(float64); ok {
		return int64(v)
	}
	return 0
}

// v2String reads m[key] as a string, "" when absent or not a string.
func v2String(m map[string]any, key string) string {
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return s
}
