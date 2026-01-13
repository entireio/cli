package list

import (
	"fmt"
	"strings"
)

// BuildTree converts TreeData into a hierarchical Node tree.
// Hierarchy: Branch → Checkpoint → Session (shown as nested info)
func BuildTree(data *TreeData) []*Node {
	var nodes []*Node

	for _, branch := range data.Branches {
		branchNode := &Node{
			Type:      NodeTypeBranch,
			ID:        branch.Name,
			Label:     formatBranchLabel(branch),
			IsCurrent: branch.IsCurrent,
			IsMerged:  branch.IsMerged,
			Expanded:  branch.IsCurrent, // Current branch starts expanded
		}

		// Add checkpoints as children of branch
		for _, cp := range branch.Checkpoints {
			// Determine if any session in this checkpoint is active
			hasActiveSession := false
			for _, sess := range cp.Sessions {
				if sess.IsActive {
					hasActiveSession = true
					break
				}
			}

			// Use first session's info for the checkpoint node (for backwards compat with actions)
			var primarySessionID, primaryDescription string
			if len(cp.Sessions) > 0 {
				primarySessionID = cp.Sessions[0].SessionID
				primaryDescription = cp.Sessions[0].Description
			}

			checkpointNode := &Node{
				Type:             NodeTypeCheckpoint,
				ID:               cp.CheckpointID,
				Label:            formatCheckpointLabel(cp),
				CheckpointID:     cp.CheckpointID,
				CommitHash:       cp.CommitHash,
				CommitMsg:        cp.CommitMsg,
				Timestamp:        cp.CreatedAt,
				StepsCount:       cp.StepsCount,
				IsTaskCheckpoint: cp.IsTask,
				ToolUseID:        cp.ToolUseID,
				// Primary session info (for actions that need a session ID)
				SessionID:   primarySessionID,
				Description: primaryDescription,
				IsActive:    hasActiveSession,
				Parent:      branchNode,
				Expanded:    false,
			}

			// Add all sessions as child nodes
			for _, sess := range cp.Sessions {
				sessionNode := &Node{
					Type:        NodeTypeSession,
					ID:          sess.SessionID + "-" + cp.CheckpointID, // Unique ID for this session instance
					Label:       formatSessionInfoLabel(sess),
					SessionID:   sess.SessionID,
					Description: sess.Description,
					IsActive:    sess.IsActive,
					Parent:      checkpointNode,
				}
				checkpointNode.Children = append(checkpointNode.Children, sessionNode)
			}

			branchNode.Children = append(branchNode.Children, checkpointNode)
		}

		nodes = append(nodes, branchNode)
	}

	return nodes
}

// formatBranchLabel creates the display label for a branch.
func formatBranchLabel(branch BranchInfo) string {
	label := branch.Name

	if branch.IsCurrent {
		label += " *"
	}

	if branch.IsMerged {
		label += " (merged)"
	}

	checkpointCount := len(branch.Checkpoints)
	if checkpointCount > 0 {
		label += fmt.Sprintf("  (%d checkpoint", checkpointCount)
		if checkpointCount > 1 {
			label += "s"
		}
		label += ")"
	}

	return label
}

// formatCheckpointLabel creates the display label for a checkpoint.
func formatCheckpointLabel(cp CheckpointInfo) string {
	parts := []string{}

	// Show commit hash and message
	if cp.CommitHash != "" {
		parts = append(parts, cp.CommitHash)
	}

	// Show commit message (truncated)
	if cp.CommitMsg != "" {
		msg := cp.CommitMsg
		if len(msg) > 50 {
			msg = msg[:47] + "..."
		}
		parts = append(parts, msg)
	}

	// Show steps count
	if cp.StepsCount > 0 {
		parts = append(parts, fmt.Sprintf("(%d steps)", cp.StepsCount))
	}

	// Show session count if multiple sessions
	if len(cp.Sessions) > 1 {
		parts = append(parts, fmt.Sprintf("[%d sessions]", len(cp.Sessions)))
	}

	label := strings.Join(parts, " ")

	// Check if any session is active
	hasActive := false
	for _, sess := range cp.Sessions {
		if sess.IsActive {
			hasActive = true
			break
		}
	}
	if hasActive {
		label += " (active)"
	}

	if cp.IsTask {
		label += " [task]"
	}

	return label
}

// formatSessionInfoLabel creates the display label for a session under a checkpoint.
func formatSessionInfoLabel(sess SessionInfo) string {
	// Truncate session ID for display (e.g., "2025-01-10-abc12345")
	displayID := sess.SessionID
	if len(displayID) > 19 {
		displayID = displayID[:19]
	}

	parts := []string{"Session: " + displayID}

	// Add description (truncated)
	if sess.Description != "" && sess.Description != "No description" {
		desc := sess.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		parts = append(parts, "- "+desc)
	}

	if sess.IsActive {
		parts = append(parts, "(active)")
	}

	return strings.Join(parts, " ")
}

// FlattenTree returns a flat list of visible nodes for navigation.
// Only includes nodes that should be visible based on expansion state.
func FlattenTree(nodes []*Node) []*Node {
	var flat []*Node
	for _, node := range nodes {
		flat = append(flat, node)
		if node.Expanded {
			flat = append(flat, flattenChildren(node)...)
		}
	}
	return flat
}

// flattenChildren recursively flattens children of an expanded node.
func flattenChildren(node *Node) []*Node {
	var flat []*Node
	for _, child := range node.Children {
		flat = append(flat, child)
		if child.Expanded {
			flat = append(flat, flattenChildren(child)...)
		}
	}
	return flat
}

// GetNodeDepth returns the depth of a node in the tree (0 for root).
func GetNodeDepth(node *Node) int {
	depth := 0
	current := node.Parent
	for current != nil {
		depth++
		current = current.Parent
	}
	return depth
}

// FindNodeByID searches the tree for a node with the given ID.
func FindNodeByID(nodes []*Node, id string) *Node {
	for _, node := range nodes {
		if node.ID == id {
			return node
		}
		if found := FindNodeByID(node.Children, id); found != nil {
			return found
		}
	}
	return nil
}
