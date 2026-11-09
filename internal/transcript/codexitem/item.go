// Package codexitem decodes Codex items, the units of a Codex session
// (a user message, an agent message, a command, a file change, a tool call),
// from both of Codex's encodings — the rollout file's item_completed events
// and the app-server's item notifications — into one normalized Item, so the
// stored transcript and a live stream map them identically.
package codexitem

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// Kind is what an item is.
type Kind string

const (
	KindUser       Kind = "user"
	KindAgent      Kind = "agent"
	KindCommand    Kind = "command"
	KindFileChange Kind = "fileChange"
	KindMCPTool    Kind = "mcpTool"
	KindWebSearch  Kind = "webSearch"
	KindImageView  Kind = "imageView"
	KindCollabTool Kind = "collabTool"
	KindSubAgent   Kind = "subAgentActivity"
	KindOther      Kind = "other" // reasoning, compaction, plans, dynamic tools, other extensions
)

// Status is an item's state; Codex's own values, empty for items without one.
type Status string

const (
	StatusInProgress Status = "inProgress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusDeclined   Status = "declined"
)

// Change is one file of a file change.
type Change struct {
	Path     string
	Kind     string // add, delete, update
	MovePath string // update with a move
	Diff     string // the unified diff; for add and delete, the file's content
}

// Item is a decoded Codex item. Only the fields of its Kind are set.
type Item struct {
	ID            string
	TurnID        string
	Kind          Kind
	Status        Status
	StartedAtMs   int64
	CompletedAtMs int64
	LineTS        string // rollout: the line's timestamp, as written (fallback for a missing completed_at_ms)

	Parts []string // user: the text of each content part, in order
	Text  string   // agent: the message text (rollout: the parts' text joined with no separator)

	Command  string // command: the script (see scriptOfArgv / scriptOfString)
	CWD      string
	Output   string // command: aggregated output; mcpTool: result text
	ExitCode *int

	Changes []Change // fileChange, sorted by Path

	Server    string          // mcpTool
	Tool      string          // mcpTool, collabTool (e.g. spawnAgent)
	Arguments json.RawMessage // mcpTool
	Error     string          // mcpTool: the error message, if any

	Query string // webSearch
	Path  string // imageView

	Prompt            string   // collabTool
	ReceiverThreadIDs []string // collabTool
	AgentThreadID     string   // subAgentActivity
	AgentPath         string   // subAgentActivity
	ActivityKind      string   // subAgentActivity: started, interacted, interrupted, completed
}

// Failed reports whether the item ended in failure: a failed or declined
// status, a non-zero exit code, or an MCP error.
func (it Item) Failed() bool {
	if it.Status == StatusFailed || it.Status == StatusDeclined || it.Error != "" {
		return true
	}
	return it.ExitCode != nil && *it.ExitCode != 0
}

// The wire pieces both encodings share, and the normalization both apply.

// contentPart is a part of a message's or an MCP result's content.
type contentPart struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

// mcpResult is an MCP tool call's result. Only the rollout's results have
// been seen to carry isError; both decoders honour it when present.
type mcpResult struct {
	Content []contentPart `json:"content"`
	IsError bool          `json:"isError"`
}

// errorMessage is an item's error object.
type errorMessage struct {
	Message string `json:"message"`
}

// userTexts is the text of a user message's text parts, in order; nil for
// none. Other parts (images, skills, mentions) are not the user's words, even
// should one carry a text field.
func userTexts(parts []contentPart) []string {
	var out []string
	for _, p := range parts {
		if (p.Type == "text" || p.Type == "input_text") && p.Text != nil {
			out = append(out, *p.Text)
		}
	}
	return out
}

// texts is the text of every part that has one, in order; nil for none.
func texts(parts []contentPart) []string {
	var out []string
	for _, p := range parts {
		if p.Text != nil {
			out = append(out, *p.Text)
		}
	}
	return out
}

// setMCP sets its MCP call fields, decoded the same way in both encodings.
func setMCP(it *Item, server, tool string, arguments json.RawMessage, result *mcpResult, callErr *errorMessage) {
	it.Server, it.Tool = server, tool
	it.Arguments = compactJSON(arguments)
	if result != nil {
		var out []byte
		for _, p := range result.Content {
			if p.Type == "text" && p.Text != nil {
				out = append(out, *p.Text...)
			}
		}
		it.Output = string(out)
	}
	switch {
	case callErr != nil && callErr.Message != "":
		it.Error = callErr.Message
	case result != nil && result.IsError:
		it.Error = "tool reported an error"
	}
}

// compactJSON is raw compacted; nil for an absent or null value (or one that
// isn't JSON, which encoding/json has already ruled out).
func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil
	}
	return buf.Bytes()
}

// status is s as a Status, or def when s is empty.
func status(s string, def Status) Status {
	if s == "" {
		return def
	}
	return Status(s)
}

// sortChanges sorts a file change's changes by path; nil for none.
func sortChanges(changes []Change) []Change {
	if len(changes) == 0 {
		return nil
	}
	slices.SortStableFunc(changes, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return changes
}

// nonEmpty is s, or nil when it has no elements.
func nonEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
