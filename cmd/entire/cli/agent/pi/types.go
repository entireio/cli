package pi

// piHookInput represents the JSON input from pi extension hooks.
type piHookInput struct {
	SessionID      string   `json:"session_id"`
	TranscriptPath string   `json:"transcript_path"`
	Prompt         string   `json:"prompt,omitempty"`
	ModifiedFiles  []string `json:"modified_files,omitempty"`
}

// piSessionEntry represents a line in pi's JSONL session file.
type piSessionEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	ParentID  string          `json:"parentId,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	Message   *piMessage      `json:"message,omitempty"`
	Version   int             `json:"version,omitempty"`
	CWD       string          `json:"cwd,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	Data      interface{}     `json:"data,omitempty"`
}

// piMessage represents a message in pi's session format.
type piMessage struct {
	Role      string           `json:"role"`
	Content   []piContentBlock `json:"content,omitempty"`
	ToolName  string           `json:"toolName,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Details   interface{}      `json:"details,omitempty"`
	IsError   bool             `json:"isError,omitempty"`
	Timestamp int64            `json:"timestamp,omitempty"`
	Provider  string           `json:"provider,omitempty"`
	Model     string           `json:"model,omitempty"`
}

// piContentBlock represents a content block in pi messages.
type piContentBlock struct {
	Type      string      `json:"type"`
	Text      string      `json:"text,omitempty"`
	ID        string      `json:"id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Arguments interface{} `json:"arguments,omitempty"`
}

// PiSettings represents pi's .pi/settings.json format.
type PiSettings struct {
	Packages   []string `json:"packages,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
}

// PiSessionHeader represents the first line of a pi session file.
type PiSessionHeader struct {
	Type          string `json:"type"` // "session"
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}
