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
				// Session info for this checkpoint
				SessionID:   cp.SessionID,
				Description: cp.Description,
				IsActive:    cp.IsActive,
				Parent:      branchNode,
				Expanded:    false,
			}

			// Add session as child node for display purposes
			sessionNode := &Node{
				Type:        NodeTypeSession,
				ID:          cp.SessionID + "-" + cp.CheckpointID, // Unique ID for this session instance
				Label:       formatSessionLabel(cp),
				SessionID:   cp.SessionID,
				Description: cp.Description,
				IsActive:    cp.IsActive,
				Parent:      checkpointNode,
			}
			checkpointNode.Children = append(checkpointNode.Children, sessionNode)

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

	label := strings.Join(parts, " ")

	if cp.IsActive {
		label += " (active)"
	}

	if cp.IsTask {
		label += " [task]"
	}

	return label
}

// formatSessionLabel creates the display label for a session under a checkpoint.
func formatSessionLabel(cp CheckpointInfo) string {
	// Truncate session ID for display (e.g., "2025-01-10-abc12345")
	displayID := cp.SessionID
	if len(displayID) > 19 {
		displayID = displayID[:19]
	}

	parts := []string{"Session: " + displayID}

	// Add description (truncated)
	if cp.Description != "" && cp.Description != "No description" {
		desc := cp.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		parts = append(parts, "- "+desc)
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
