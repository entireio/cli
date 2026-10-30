package transcript

import (
	"encoding/json"
	"time"

	"github.com/entireio/cli/internal/transcript/codexitem"
)

// codexTextBlock is an assistant text block.
type codexTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexItemToolUse is a tool_use block; its fields in the order they print.
type codexItemToolUse struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type codexCommandInput struct {
	Command string `json:"command"`
}

type codexFileChangeInput struct {
	Changes []codexFileChangeInputChange `json:"changes"`
}

type codexFileChangeInputChange struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	MovePath string `json:"move_path,omitempty"`
	Diff     string `json:"diff"`
}

type codexQueryInput struct {
	Query string `json:"query"`
}

type codexPathInput struct {
	Path string `json:"path"`
}

type codexPromptInput struct {
	Prompt string `json:"prompt,omitempty"`
}

type codexAgentPathInput struct {
	AgentPath string `json:"agent_path"`
}

// codexSubAgentTools names the tool use a sub-agent activity shows as: the
// call that caused it. A spawn and an interrupt are spawn_agent and
// interrupt_agent, as the response_item path names those calls; an
// interaction is either send_message or followup_task, which the activity
// doesn't tell apart, so it gets a name of its own. A completed sub-agent
// shows nothing: its result reaches the parent as a later tool output.
var codexSubAgentTools = map[string]string{
	"started":     "spawn_agent",
	"interacted":  "interact_agent",
	"interrupted": "interrupt_agent",
}

// codexSpawnAgentTool is the collab tool that starts a sub-agent.
const codexSpawnAgentTool = "spawnAgent"

// codexItemRecords maps one Codex item to transcript records, keyed by the
// item ID. Convert's Codex item path and CodexNotificationRecords both use it,
// so a completed item looks the same stored and live. A running command shows
// its output so far, when it has any. Items that aren't shown (reasoning,
// compaction, …) give no records.
func codexItemRecords(it codexitem.Item) []Record {
	done := it.Status != codexitem.StatusInProgress
	status := ToolStatusSuccess
	if it.Failed() {
		status = ToolStatusError
	}
	var result *ToolResult
	if done {
		result = &ToolResult{Status: status}
	}

	var name string
	var input any
	switch it.Kind {
	case codexitem.KindUser:
		text := codexUserPartsText(it.Parts)
		if text == "" {
			return nil
		}
		return codexMessageRecords(it, TypeUser, []userTextBlock{{Text: text}})
	case codexitem.KindAgent:
		if it.Text == "" {
			return nil
		}
		return codexMessageRecords(it, TypeAssistant, []codexTextBlock{{Type: BlockText, Text: it.Text}})
	case codexitem.KindCommand:
		name, input = "Bash", codexCommandInput{Command: it.Command}
		switch {
		case done:
			result.Output = it.Output
		case it.Output != "":
			result = &ToolResult{Output: it.Output, Status: ToolStatusSuccess}
		}
	case codexitem.KindFileChange:
		changes := make([]codexFileChangeInputChange, 0, len(it.Changes))
		for _, c := range it.Changes {
			changes = append(changes, codexFileChangeInputChange(c))
		}
		name, input = "FileChange", codexFileChangeInput{Changes: changes}
	case codexitem.KindMCPTool:
		name, input = it.Server+"."+it.Tool, it.Arguments
		if len(it.Arguments) == 0 {
			input = json.RawMessage(`{}`)
		}
		if result != nil {
			result.Output = it.Output
			if result.Output == "" {
				result.Output = it.Error
			}
		}
	case codexitem.KindWebSearch:
		name, input = "WebSearch", codexQueryInput{Query: it.Query}
	case codexitem.KindImageView:
		name, input = "ImageView", codexPathInput{Path: it.Path}
	case codexitem.KindCollabTool:
		name, input = it.Tool, codexPromptInput{Prompt: it.Prompt}
		if result != nil && it.Tool == codexSpawnAgentTool && len(it.ReceiverThreadIDs) > 0 {
			result.Agent = &ToolResultAgent{ID: it.ReceiverThreadIDs[0]}
		}
	case codexitem.KindSubAgent:
		name = codexSubAgentTools[it.ActivityKind]
		if name == "" {
			return nil
		}
		input = codexAgentPathInput{AgentPath: it.AgentPath}
		result = &ToolResult{Status: ToolStatusSuccess}
		if it.AgentThreadID != "" {
			result.Agent = &ToolResultAgent{ID: it.AgentThreadID}
		}
	case codexitem.KindOther:
		return nil
	default:
		return nil
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	records := codexMessageRecords(it, TypeAssistant, []codexItemToolUse{{Type: BlockToolUse, ID: it.ID, Name: name, Input: inputJSON}})
	if records == nil || result == nil {
		return records
	}
	return append(records, Record{
		Op:        OpResult,
		ID:        it.ID,
		SourceID:  it.ID,
		TS:        records[0].TS,
		ToolUseID: it.ID,
		Result:    result,
	})
}

// codexMessageRecords is the message record of item it with role and content.
func codexMessageRecords(it codexitem.Item, role string, content any) []Record {
	contentJSON, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	return []Record{{
		Op:       OpMessage,
		ID:       it.ID,
		SourceID: it.ID,
		Role:     role,
		TS:       codexItemTS(it),
		Content:  contentJSON,
	}}
}

// codexItemTS is an item's timestamp as a JSON string, nil when it has none.
// A finished item: when it completed, else its rollout line's timestamp
// (written at completion), else when it started. A running item: when it
// started, else its line's timestamp.
func codexItemTS(it codexitem.Item) json.RawMessage {
	first, last := it.CompletedAtMs, it.StartedAtMs
	if it.Status == codexitem.StatusInProgress {
		first, last = it.StartedAtMs, 0
	}
	var s string
	switch {
	case first != 0:
		s = codexMsTS(first)
	case it.LineTS != "":
		s = it.LineTS
	case last != 0:
		s = codexMsTS(last)
	default:
		return nil
	}
	ts, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return ts
}

// codexMsTS formats Unix milliseconds as RFC 3339 UTC with milliseconds.
func codexMsTS(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// CodexLive is what a live stream knows about an item beyond the item itself.
type CodexLive struct {
	Text        *string // agent text so far (from deltas); nil: the item's own text
	Output      *string // command output so far; nil: the item's own output
	Interrupted bool    // the item's turn ended while it was in progress: show it failed
	TS          string  // fallback time (RFC 3339), used when the item has none
}

// CodexNotificationRecords maps one Codex app-server item notification
// (method item/started or item/completed, its params) to records, with the
// same mapping as Convert's Codex item path. ok is false for anything else,
// including an item it can't read or one without an ID. An item that isn't
// shown (reasoning, …) is ok with no records.
func CodexNotificationRecords(method string, params []byte, live CodexLive) (recs []Record, ok bool) {
	it, ok := codexitem.DecodeDaemon(method, params)
	if !ok {
		return nil, false
	}
	if live.Text != nil && it.Kind == codexitem.KindAgent {
		it.Text = *live.Text
	}
	if live.Output != nil && it.Kind == codexitem.KindCommand {
		it.Output = *live.Output
	}
	if live.Interrupted && (it.Status == codexitem.StatusInProgress || it.Status == "") {
		it.Status = codexitem.StatusFailed
	}
	it.LineTS = live.TS
	return codexItemRecords(it), true
}
