package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// sessionInfoRaw matches the JSON payload piped from the OpenCode plugin for session events.
// The plugin sends only session_id; Go calls `opencode export` to get the transcript.
type sessionInfoRaw struct {
	SessionID string `json:"session_id"`
}

// turnStartRaw matches the JSON payload for turn-start (user prompt submission).
type turnStartRaw struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
}

// turnEndRaw matches the JSON payload for turn-end (session idle).
// Extends sessionInfoRaw with model info captured during the turn.
type turnEndRaw struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
}

// --- Export JSON types (from `opencode export`) ---

// ExportSession represents the top-level structure of `opencode export` output.
// This is OpenCode's native format for session data.
type ExportSession struct {
	Info     SessionInfo     `json:"info"`
	Messages []ExportMessage `json:"messages"`
}

// SessionInfo contains session metadata from the export.
type SessionInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

// ExportMessage represents a single message in the export format.
// Each message contains info (metadata) and parts (content).
type ExportMessage struct {
	Info  MessageInfo `json:"info"`
	Parts []Part      `json:"parts"`
}

// MessageInfo contains message metadata.
type MessageInfo struct {
	ID        string  `json:"id"`
	SessionID string  `json:"sessionID,omitempty"`
	Role      string  `json:"role"` // "user" or "assistant"
	Time      Time    `json:"time"`
	Tokens    *Tokens `json:"tokens,omitempty"`
	Cost      float64 `json:"cost,omitempty"`
}

// Message role constants.
const (
	roleAssistant = "assistant"
	roleUser      = "user"
)

// Time holds message timestamps.
type Time struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

// Tokens holds token usage from assistant messages.
type Tokens struct {
	Input     int   `json:"input"`
	Output    int   `json:"output"`
	Reasoning int   `json:"reasoning"`
	Cache     Cache `json:"cache"`
}

// Cache holds cache-related token counts.
type Cache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// Part represents a message part (text, tool, etc.).
type Part struct {
	ID     string     `json:"id,omitempty"` // Part ID (e.g., "prt_..."), added in OpenCode 1.2.x
	Type   string     `json:"type"`         // "text", "tool", etc.
	Text   string     `json:"text,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	CallID string     `json:"callID,omitempty"`
	State  *ToolState `json:"state,omitempty"`
}

// ToolState represents tool execution state.
type ToolState struct {
	Status   string             `json:"status"` // "pending", "running", "completed", "error"
	Input    map[string]any     `json:"input,omitempty"`
	Output   string             `json:"output,omitempty"`
	Metadata *ToolStateMetadata `json:"metadata,omitempty"`
}

// ToolStateMetadata holds metadata from tool execution results.
type ToolStateMetadata struct {
	Files []ToolFileInfo `json:"files,omitempty"`
}

// ToolFileInfo represents a file affected by a tool operation.
type ToolFileInfo struct {
	FilePath     string `json:"filePath"`
	RelativePath string `json:"relativePath,omitempty"`
}

// FileModificationTools are tools in OpenCode that modify files on disk.
// These match the actual tool names from OpenCode's source (packages/opencode/src/tool/):
//   - edit:        edit.ts  — exact string replacement in existing files
//   - write:       write.ts — create or overwrite files
//   - apply_patch: apply_patch.ts — unified diff patches (used by gpt-* models except gpt-4)
//
// Tool selection is mutually exclusive: apply_patch is enabled for gpt-* (non-gpt-4, non-oss)
// models; edit+write are enabled for all other models (Claude, Gemini, gpt-4, etc.).
// The batch tool (experimental) creates separate transcript parts per sub-call,
// so its children are already captured by this list.
var FileModificationTools = []string{
	"edit",
	"write",
	"apply_patch",
}

// --- OpenCode 2 export shape ---
//
// OpenCode 2 moved the export to `opencode session export` and changed the
// message schema: each message carries a "type" plus a typed "content" array
// instead of v1's "info"/"parts". NormalizeExportSession maps either shape onto
// the v1 ExportSession the rest of the package reads, so only this file and the
// compact package need to know the difference.

// exportEnvelope is the shape shared by both versions: a session object with an
// "info" object and a "messages" array.
type exportEnvelope struct {
	Info     json.RawMessage   `json:"info"`
	Messages []json.RawMessage `json:"messages"`
}

// sessionInfoV2 is the OpenCode 2 session info object.
type sessionInfoV2 struct {
	ID    string  `json:"id"`
	Title string  `json:"title"`
	Time  *timeV2 `json:"time"`
}

type timeV2 struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

// messageV2 is a single OpenCode 2 message. User and system messages carry text
// at the message level; assistant messages carry typed content parts.
type messageV2 struct {
	ID      string      `json:"id"`
	Type    string      `json:"type"` // "user", "assistant", "system", "synthetic", ...
	Text    string      `json:"text,omitempty"`
	Time    Time        `json:"time"`
	Tokens  *Tokens     `json:"tokens"`
	Cost    float64     `json:"cost"`
	Content []contentV2 `json:"content"`
}

// contentV2 is one entry of an OpenCode 2 message's typed content array.
type contentV2 struct {
	Type  string       `json:"type"` // "text", "reasoning", "tool"
	Text  string       `json:"text,omitempty"`
	ID    string       `json:"id,omitempty"`
	Name  string       `json:"name,omitempty"`
	State *toolStateV2 `json:"state,omitempty"`
}

// toolStateV2 is the execution state of an OpenCode 2 tool call.
type toolStateV2 struct {
	Status   string             `json:"status"`
	Output   string             `json:"output,omitempty"`
	Input    map[string]any     `json:"input,omitempty"`
	Content  []toolContentV2    `json:"content,omitempty"`
	Metadata *ToolStateMetadata `json:"metadata,omitempty"`
}

type toolContentV2 struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// NormalizeExportSession parses either the v1 or v2 OpenCode export shape and
// returns the v1-normalized ExportSession the rest of the package consumes.
func NormalizeExportSession(data []byte) (*ExportSession, error) {
	if len(data) == 0 {
		return nil, nil //nolint:nilnil // nil for empty data is expected
	}

	var env exportEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse export session: %w", err)
	}

	if !isV2Export(env.Messages) {
		var v1 ExportSession
		if err := json.Unmarshal(data, &v1); err != nil {
			return nil, fmt.Errorf("failed to parse export session: %w", err)
		}
		return &v1, nil
	}

	return normalizeV2Export(env)
}

// isV2Export reports whether the message array uses the OpenCode 2 shape. v1
// messages nest metadata under "info"; v2 messages carry "type"/"content".
// An empty array reads as v1, which normalizes identically (no messages).
func isV2Export(messages []json.RawMessage) bool {
	if len(messages) == 0 {
		return false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(messages[0], &probe) != nil {
		return false
	}
	_, hasInfo := probe["info"]
	return !hasInfo
}

func normalizeV2Export(env exportEnvelope) (*ExportSession, error) {
	var info sessionInfoV2
	if len(env.Info) > 0 {
		if err := json.Unmarshal(env.Info, &info); err != nil {
			return nil, fmt.Errorf("failed to parse v2 session info: %w", err)
		}
	}

	session := &ExportSession{
		Info: SessionInfo{
			ID:    info.ID,
			Title: info.Title,
		},
	}
	if info.Time != nil {
		session.Info.CreatedAt = info.Time.Created
		session.Info.UpdatedAt = info.Time.Updated
	}

	for _, raw := range env.Messages {
		var msg messageV2
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse v2 message: %w", err)
		}
		out := ExportMessage{
			Info: MessageInfo{
				ID:     msg.ID,
				Role:   normalizeRoleV2(msg.Type),
				Time:   msg.Time,
				Tokens: msg.Tokens,
				Cost:   msg.Cost,
			},
		}
		for _, content := range msg.Content {
			out.Parts = append(out.Parts, partFromV2(content))
		}
		// User and system messages carry their text at the message level.
		if msg.Text != "" {
			out.Parts = append(out.Parts, Part{Type: "text", Text: msg.Text})
		}
		session.Messages = append(session.Messages, out)
	}

	return session, nil
}

// normalizeRoleV2 maps an OpenCode 2 message type onto the v1 role vocabulary.
// Unknown types are preserved so role checks simply skip them.
func normalizeRoleV2(messageType string) string {
	switch messageType {
	case roleUser:
		return roleUser
	case roleAssistant:
		return roleAssistant
	default:
		return messageType
	}
}

func partFromV2(content contentV2) Part {
	switch content.Type {
	case "text":
		return Part{Type: "text", Text: content.Text}
	case "reasoning":
		return Part{Type: "reasoning", Text: content.Text}
	case "tool":
		part := Part{Type: "tool", Tool: content.Name, CallID: content.ID}
		if content.State != nil {
			part.State = &ToolState{
				Status:   content.State.Status,
				Input:    content.State.Input,
				Output:   toolOutputFromV2(content.State),
				Metadata: content.State.Metadata,
			}
		}
		return part
	default:
		return Part{Type: content.Type, Text: content.Text}
	}
}

// toolOutputFromV2 flattens an OpenCode 2 tool result into the single output
// string the v1 model stores. v2 carries the result as typed content parts.
func toolOutputFromV2(state *toolStateV2) string {
	if state.Output != "" {
		return state.Output
	}
	var out strings.Builder
	for _, content := range state.Content {
		if content.Type != "text" || content.Text == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(content.Text)
	}
	return out.String()
}
