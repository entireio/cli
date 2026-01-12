// Package list provides a hierarchical interactive view of branches, sessions, and checkpoints.
package list

import (
	"time"

	"entire.io/cli/cmd/entire/cli/strategy"
)

// NodeType identifies the type of item in the tree.
type NodeType int

const (
	NodeTypeBranch NodeType = iota
	NodeTypeSession
	NodeTypeCheckpoint
)

// Node represents an item in the hierarchical tree view.
type Node struct {
	Type     NodeType
	ID       string // Branch name, session ID, or checkpoint ID
	Label    string // Display label
	Children []*Node

	// Branch-specific fields
	IsCurrent bool
	IsMerged  bool

	// Session-specific fields
	SessionID   string
	Description string
	Strategy    string
	StartTime   time.Time
	IsActive    bool // Currently running session

	// Checkpoint-specific fields
	CheckpointID     string
	Timestamp        time.Time
	Message          string
	IsTaskCheckpoint bool
	ToolUseID        string

	// Parent reference for navigation
	Parent *Node

	// UI state
	Expanded bool
	Selected bool
}

// BranchInfo contains information about a branch and its associated sessions.
type BranchInfo struct {
	Name      string
	IsCurrent bool
	IsMerged  bool
	Sessions  []SessionInfo
}

// SessionInfo contains information about a session within a branch.
type SessionInfo struct {
	Session     strategy.Session
	IsActive    bool   // Currently running in an agent
	BranchName  string // Branch this session is associated with
	BaseCommit  string // Commit this session started from
	Checkpoints []CheckpointInfo
}

// CheckpointInfo contains information about a checkpoint.
type CheckpointInfo struct {
	Checkpoint strategy.Checkpoint
	CommitHash string // Code commit hash if committed
}

// TreeData holds the complete data for the hierarchical view.
type TreeData struct {
	Branches      []BranchInfo
	CurrentBranch string
	MainBranch    string
}

// Action represents an action that can be performed on a node.
type Action string

const (
	ActionOpen   Action = "open"   // Open session logs without changing branch
	ActionResume Action = "resume" // Switch to branch and resume session
	ActionRewind Action = "rewind" // Restore to checkpoint state
)

// ActionResult contains the result of performing an action.
type ActionResult struct {
	Action        Action
	SessionID     string
	CheckpointID  string
	ResumeCommand string
	Message       string
	Error         error
}
