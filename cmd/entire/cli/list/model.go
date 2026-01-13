// Package list provides a hierarchical interactive view of branches, checkpoints, and sessions.
package list

import (
	"time"
)

// NodeType identifies the type of item in the tree.
type NodeType int

const (
	NodeTypeBranch NodeType = iota
	NodeTypeCheckpoint
	NodeTypeSession
)

// Node represents an item in the hierarchical tree view.
type Node struct {
	Type     NodeType
	ID       string // Branch name, checkpoint ID, or session ID
	Label    string // Display label
	Children []*Node

	// Branch-specific fields
	IsCurrent bool
	IsMerged  bool

	// Checkpoint-specific fields
	CheckpointID     string
	CommitHash       string
	CommitMsg        string
	Timestamp        time.Time
	StepsCount       int
	IsTaskCheckpoint bool
	ToolUseID        string

	// Session-specific fields (when showing session under checkpoint)
	SessionID   string
	Description string
	IsActive    bool // Currently running session

	// Parent reference for navigation
	Parent *Node

	// UI state
	Expanded bool
	Selected bool
}

// BranchInfo contains information about a branch and its checkpoints.
type BranchInfo struct {
	Name        string
	IsCurrent   bool
	IsMerged    bool
	Checkpoints []CheckpointInfo
}

// CheckpointInfo contains information about a checkpoint on a branch.
type CheckpointInfo struct {
	CheckpointID string
	CommitHash   string // Commit with Entire-Checkpoint trailer
	CommitMsg    string // Commit message (for display)
	CreatedAt    time.Time
	StepsCount   int // Steps that led to this checkpoint
	IsTask       bool
	ToolUseID    string
	// Sessions associated with this checkpoint (can be multiple from concurrent sessions)
	Sessions []SessionInfo
}

// SessionInfo contains session details shown under a checkpoint.
type SessionInfo struct {
	SessionID   string
	Description string
	IsActive    bool
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
