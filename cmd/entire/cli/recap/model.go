package recap

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// DataSource tags where a projected value came from.
type DataSource int

const (
	SourceLocal DataSource = iota
	SourceServer
	SourceMixed
)

// dataSourceUnknown is the string form used when a DataSource value is
// outside the enumerated set. Distinct from repoUnknown (aggregate.go) —
// same literal, different concept.
const dataSourceUnknown = "unknown"

// String returns the lower-case name used in chrome footers and JSON output.
func (d DataSource) String() string {
	switch d {
	case SourceServer:
		return "server"
	case SourceMixed:
		return "mixed"
	case SourceLocal:
		return "local"
	default:
		return dataSourceUnknown
	}
}

// TokenProvider is injected by callers for testing; production uses
// auth.LookupCurrentToken via defaultTokenProvider (see enrich.go).
// Declared here so LoadOpts can reference it without a cross-file cycle.
type TokenProvider func() (string, error)

// RecapSession is a read-only projection of a session for recap views.
// Built by load.go from session.State + per-session checkpoint rollups,
// optionally enriched with api-fetched labels. Never persisted.
//
//nolint:revive // RecapSession is clearer than Session in the recap package context
type RecapSession struct {
	Repo         string
	SessionID    string
	Branch       string
	WorktreeID   string
	WorktreePath string
	BaseCommit   string

	StartedAt       time.Time
	EndedAt         *time.Time
	LastInteraction time.Time
	Phase           session.Phase

	Checkpoints   []RecapCheckpoint
	FilesTouched  []string
	LinkedCommits []string // SHAs whose message contains this session's Entire-Checkpoint trailers

	AgentsUsed []string
	ModelsUsed []string

	Labels     []string // tier 3; empty when offline or not yet analyzed
	Badges     []string // tier 2; deterministic local facts
	TokenUsage *agent.TokenUsage

	IsActive    bool
	IsResumable bool

	Source DataSource
}

// SpanMinutes returns the honest first-to-last checkpoint timestamp span
// in minutes. Returns 0 when fewer than two checkpoints exist.
func (a RecapSession) SpanMinutes() float64 {
	if len(a.Checkpoints) < 2 {
		return 0
	}
	first := a.Checkpoints[0].CreatedAt
	last := a.Checkpoints[0].CreatedAt
	for _, cp := range a.Checkpoints[1:] {
		if cp.CreatedAt.Before(first) {
			first = cp.CreatedAt
		}
		if cp.CreatedAt.After(last) {
			last = cp.CreatedAt
		}
	}
	return last.Sub(first).Minutes()
}

// RecapCheckpoint is a projection of one committed checkpoint.
//
//nolint:revive // RecapCheckpoint is clearer than Checkpoint in the recap package context
type RecapCheckpoint struct {
	Repo         string
	SessionID    string
	ID           id.CheckpointID
	CreatedAt    time.Time
	IsTask       bool
	ToolUseID    string
	Agent        string
	Model        string
	FilesTouched []string
	LinkedCommit string
	TokenUsage   *agent.TokenUsage
	TurnID       string
	Labels       []string // tier 3
	ToolProfile  *ToolProfile
	SkillsUsed   []string // tier 3 — from server analysis
	MCPServers   []string // tier 3 — from server analysis, flattened names
	Badges       []string
	Source       DataSource
}

// ToolProfile mirrors api/src/types.ts ToolProfile but with optional
// per-category durations. DurationMs is 0 when the server hasn't
// provided it yet.
type ToolProfile struct {
	Categories map[string]ToolCategoryMetrics `json:"categories"`
	Total      int                            `json:"total"`
}

// ToolCategoryMetrics holds count and optional duration for a tool category.
type ToolCategoryMetrics struct {
	Count      int   `json:"count"`
	DurationMs int64 `json:"durationMs,omitempty"`
}
