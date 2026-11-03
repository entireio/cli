package codexitem

import (
	"encoding/json"
	"net/url"
	"strings"
)

// rolloutLine is a rollout line as far as an item_completed event needs it.
type rolloutLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type          string          `json:"type"`
		TurnID        string          `json:"turn_id"`
		Item          json.RawMessage `json:"item"`
		StartedAtMs   int64           `json:"started_at_ms"`
		CompletedAtMs int64           `json:"completed_at_ms"`
	} `json:"payload"`
}

// rolloutItem is the fields of payload.item that every item type has, plus
// "kind", which tells Extension items apart.
type rolloutItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type rolloutMessage struct {
	Content []contentPart `json:"content"`
}

type rolloutCommand struct {
	Command          []string `json:"command"`
	CWD              string   `json:"cwd"`
	AggregatedOutput string   `json:"aggregated_output"`
	ExitCode         *int     `json:"exit_code"`
	Status           string   `json:"status"`
}

type rolloutFileChange struct {
	Changes map[string]struct {
		Type        string `json:"type"`
		Content     string `json:"content"`
		UnifiedDiff string `json:"unified_diff"`
		MovePath    string `json:"move_path"`
	} `json:"changes"`
	Status string `json:"status"`
}

type rolloutMCPToolCall struct {
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    *mcpResult      `json:"result"`
	Error     *errorMessage   `json:"error"`
	Status    string          `json:"status"`
}

type rolloutWebSearch struct {
	Query string `json:"query"`
}

type rolloutImageView struct {
	Path string `json:"path"`
}

type rolloutCollabToolCall struct {
	Tool              string   `json:"tool"`
	Prompt            string   `json:"prompt"`
	ReceiverThreadIDs []string `json:"receiver_thread_ids"`
	Status            string   `json:"status"`
}

type rolloutSubAgentActivity struct {
	AgentThreadID string `json:"agent_thread_id"`
	AgentPath     string `json:"agent_path"`
}

// DecodeRollout decodes one rollout line; ok is true for an event_msg
// item_completed line whose item it can read (Kind other included) and that
// has an ID.
func DecodeRollout(line []byte) (Item, bool) {
	var l rolloutLine
	if err := json.Unmarshal(line, &l); err != nil || l.Type != "event_msg" || l.Payload.Type != "item_completed" {
		return Item{}, false
	}
	var head rolloutItem
	if err := json.Unmarshal(l.Payload.Item, &head); err != nil || head.Type == "" || head.ID == "" {
		return Item{}, false
	}
	it := Item{
		ID:            head.ID,
		TurnID:        l.Payload.TurnID,
		Kind:          KindOther,
		Status:        StatusCompleted,
		StartedAtMs:   l.Payload.StartedAtMs,
		CompletedAtMs: l.Payload.CompletedAtMs,
		LineTS:        l.Timestamp,
	}
	if !decodeRolloutBody(&it, head, l.Payload.Item) {
		return Item{}, false
	}
	return it, true
}

// decodeRolloutBody sets its kind-specific fields from the item's raw JSON;
// false when the item has its type's fields in a shape it can't read.
func decodeRolloutBody(it *Item, head rolloutItem, raw json.RawMessage) bool {
	switch head.Type {
	case "UserMessage":
		var m rolloutMessage
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		it.Kind, it.Parts = KindUser, userTexts(m.Content)
	case "AgentMessage":
		var m rolloutMessage
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		it.Kind, it.Text = KindAgent, strings.Join(texts(m.Content), "")
	case "CommandExecution":
		var c rolloutCommand
		if json.Unmarshal(raw, &c) != nil {
			return false
		}
		it.Kind, it.Status = KindCommand, status(c.Status, StatusCompleted)
		it.Command, it.CWD = scriptOfArgv(c.Command), pathOfFileURI(c.CWD)
		it.Output, it.ExitCode = c.AggregatedOutput, c.ExitCode
	case "FileChange":
		var f rolloutFileChange
		if json.Unmarshal(raw, &f) != nil {
			return false
		}
		it.Kind, it.Status = KindFileChange, status(f.Status, StatusCompleted)
		changes := make([]Change, 0, len(f.Changes))
		for path, c := range f.Changes {
			// An added or deleted file's text is in content, as the
			// app-server's diff has it; an update has a unified diff.
			diff := c.UnifiedDiff
			if c.Type == "add" || c.Type == "delete" {
				diff = c.Content
			}
			changes = append(changes, Change{Path: path, Kind: c.Type, MovePath: c.MovePath, Diff: diff})
		}
		it.Changes = sortChanges(changes)
	case "McpToolCall":
		var m rolloutMCPToolCall
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		it.Kind, it.Status = KindMCPTool, status(m.Status, StatusCompleted)
		setMCP(it, m.Server, m.Tool, m.Arguments, m.Result, m.Error)
	case "Extension":
		if head.Kind != "web.search" {
			return true
		}
		var w rolloutWebSearch
		if json.Unmarshal(raw, &w) != nil {
			return false
		}
		it.Kind, it.Query = KindWebSearch, w.Query
	case "ImageView":
		var v rolloutImageView
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		it.Kind, it.Path = KindImageView, v.Path
	case "CollabAgentToolCall":
		var c rolloutCollabToolCall
		if json.Unmarshal(raw, &c) != nil {
			return false
		}
		it.Kind, it.Status = KindCollabTool, status(c.Status, StatusCompleted)
		it.Tool, it.Prompt, it.ReceiverThreadIDs = c.Tool, c.Prompt, nonEmpty(c.ReceiverThreadIDs)
	case "SubAgentActivity":
		var s rolloutSubAgentActivity
		if json.Unmarshal(raw, &s) != nil {
			return false
		}
		it.Kind = KindSubAgent
		it.AgentThreadID, it.AgentPath, it.ActivityKind = s.AgentThreadID, s.AgentPath, head.Kind
	}
	return true
}

// pathOfFileURI is the path of a file:// URI (the rollout's cwd), or s itself
// when it isn't one.
func pathOfFileURI(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" {
		return s
	}
	p := u.Path
	// file:///C:/dir is the Windows path C:/dir.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p
}
