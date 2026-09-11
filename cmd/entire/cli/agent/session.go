package agent

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// AgentSession represents a coding session's data.
// Each agent stores data in its native format (JSONL, SQLite, Markdown, etc.)
// and only the originating agent can read/write it.
//
// Design: Sessions are NOT interoperable between agents. A session created by
// Claude Code can only be read/written by Claude Code. This simplifies the
// implementation as we don't need format conversion.
//
//nolint:revive // AgentSession is clearer than Session in context of the package
type AgentSession struct {
	SessionID  string
	AgentName  types.AgentName
	RepoPath   string
	SessionRef string // Path/reference to session in agent's storage
	StartTime  time.Time

	// NativeData holds the session content in the agent's native format.
	// Only the originating agent can interpret this data.
	// Examples:
	//   - Claude Code: raw JSONL bytes
	//   - Cursor: serialized SQLite rows
	//   - Aider: Markdown content
	NativeData []byte

	// Computed fields - populated by the agent when reading
	ModifiedFiles []string
	NewFiles      []string
	DeletedFiles  []string
}
