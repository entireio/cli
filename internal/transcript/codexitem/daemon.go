package codexitem

import "encoding/json"

// daemonParams is an item/started or item/completed notification's params.
type daemonParams struct {
	Item          json.RawMessage `json:"item"`
	TurnID        string          `json:"turnId"`
	StartedAtMs   int64           `json:"startedAtMs"`
	CompletedAtMs int64           `json:"completedAtMs"`
}

// daemonItem is the fields of params.item that every item type has.
type daemonItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type daemonUserMessage struct {
	Content []contentPart `json:"content"`
}

type daemonAgentMessage struct {
	Text string `json:"text"`
}

type daemonCommand struct {
	Command          string `json:"command"`
	CWD              string `json:"cwd"`
	AggregatedOutput string `json:"aggregatedOutput"`
	ExitCode         *int   `json:"exitCode"`
	Status           string `json:"status"`
}

type daemonFileChange struct {
	Changes []struct {
		Path string `json:"path"`
		Kind struct {
			Type     string `json:"type"`
			MovePath string `json:"move_path"`
		} `json:"kind"`
		Diff string `json:"diff"`
	} `json:"changes"`
	Status string `json:"status"`
}

type daemonMCPToolCall struct {
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    *mcpResult      `json:"result"`
	Error     *errorMessage   `json:"error"`
	Status    string          `json:"status"`
}

type daemonWebSearch struct {
	Query string `json:"query"`
}

type daemonImageView struct {
	Path string `json:"path"`
}

type daemonCollabToolCall struct {
	Tool              string   `json:"tool"`
	Prompt            string   `json:"prompt"`
	ReceiverThreadIDs []string `json:"receiverThreadIds"`
	Status            string   `json:"status"`
}

type daemonSubAgentActivity struct {
	Kind          string `json:"kind"`
	AgentThreadID string `json:"agentThreadId"`
	AgentPath     string `json:"agentPath"`
}

// DecodeDaemon decodes an app-server item/started or item/completed
// notification's params; ok is false for other methods and for an item it
// can't read or that has no ID.
func DecodeDaemon(method string, params []byte) (Item, bool) {
	var def Status
	switch method {
	case "item/started":
		def = StatusInProgress
	case "item/completed":
		def = StatusCompleted
	default:
		return Item{}, false
	}
	var p daemonParams
	if err := json.Unmarshal(params, &p); err != nil {
		return Item{}, false
	}
	var head daemonItem
	if err := json.Unmarshal(p.Item, &head); err != nil || head.Type == "" || head.ID == "" {
		return Item{}, false
	}
	it := Item{
		ID:            head.ID,
		TurnID:        p.TurnID,
		Kind:          KindOther,
		Status:        def,
		StartedAtMs:   p.StartedAtMs,
		CompletedAtMs: p.CompletedAtMs,
	}
	if !decodeDaemonBody(&it, head.Type, p.Item, def) {
		return Item{}, false
	}
	return it, true
}

// decodeDaemonBody sets its kind-specific fields from the item's raw JSON;
// false when the item has its type's fields in a shape it can't read.
func decodeDaemonBody(it *Item, typ string, raw json.RawMessage, def Status) bool {
	switch typ {
	case "userMessage":
		var m daemonUserMessage
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		it.Kind, it.Parts = KindUser, userTexts(m.Content)
	case "agentMessage":
		var m daemonAgentMessage
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		it.Kind, it.Text = KindAgent, m.Text
	case "commandExecution":
		var c daemonCommand
		if json.Unmarshal(raw, &c) != nil {
			return false
		}
		it.Kind, it.Status = KindCommand, status(c.Status, def)
		it.Command, it.CWD = scriptOfString(c.Command), c.CWD
		it.Output, it.ExitCode = c.AggregatedOutput, c.ExitCode
	case "fileChange":
		var f daemonFileChange
		if json.Unmarshal(raw, &f) != nil {
			return false
		}
		it.Kind, it.Status = KindFileChange, status(f.Status, def)
		changes := make([]Change, 0, len(f.Changes))
		for _, c := range f.Changes {
			changes = append(changes, Change{Path: c.Path, Kind: c.Kind.Type, MovePath: c.Kind.MovePath, Diff: c.Diff})
		}
		it.Changes = sortChanges(changes)
	case "mcpToolCall":
		var m daemonMCPToolCall
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		it.Kind, it.Status = KindMCPTool, status(m.Status, def)
		setMCP(it, m.Server, m.Tool, m.Arguments, m.Result, m.Error)
	case "webSearch":
		var w daemonWebSearch
		if json.Unmarshal(raw, &w) != nil {
			return false
		}
		it.Kind, it.Query = KindWebSearch, w.Query
	case "imageView":
		var v daemonImageView
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		it.Kind, it.Path = KindImageView, v.Path
	case "collabAgentToolCall":
		var c daemonCollabToolCall
		if json.Unmarshal(raw, &c) != nil {
			return false
		}
		it.Kind, it.Status = KindCollabTool, status(c.Status, def)
		it.Tool, it.Prompt, it.ReceiverThreadIDs = c.Tool, c.Prompt, nonEmpty(c.ReceiverThreadIDs)
	case "subAgentActivity":
		var s daemonSubAgentActivity
		if json.Unmarshal(raw, &s) != nil {
			return false
		}
		it.Kind = KindSubAgent
		it.AgentThreadID, it.AgentPath, it.ActivityKind = s.AgentThreadID, s.AgentPath, s.Kind
	}
	return true
}
