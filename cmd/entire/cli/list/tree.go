package list

import (
	"fmt"
	"strings"
)

// BuildTree converts TreeData into a hierarchical Node tree.
func BuildTree(data *TreeData) []*Node {
	var nodes []*Node

	for _, branch := range data.Branches {
		branchNode := &Node{
			Type:      NodeTypeBranch,
			ID:        branch.Name,
			Label:     formatBranchLabel(branch, data.CurrentBranch),
			IsCurrent: branch.IsCurrent,
			IsMerged:  branch.IsMerged,
			Expanded:  branch.IsCurrent, // Current branch starts expanded
		}

		// Add sessions as children
		for _, sess := range branch.Sessions {
			sessionNode := &Node{
				Type:        NodeTypeSession,
				ID:          sess.Session.ID,
				Label:       formatSessionLabel(sess),
				SessionID:   sess.Session.ID,
				Description: sess.Session.Description,
				Strategy:    sess.Session.Strategy,
				StartTime:   sess.Session.StartTime,
				IsActive:    sess.IsActive,
				Parent:      branchNode,
				Expanded:    false,
			}

			// Add checkpoints as children
			for _, cp := range sess.Session.Checkpoints {
				checkpointNode := &Node{
					Type:             NodeTypeCheckpoint,
					ID:               cp.CheckpointID,
					Label:            cp.Message, // Will be formatted in view
					CheckpointID:     cp.CheckpointID,
					Timestamp:        cp.Timestamp,
					Message:          cp.Message,
					IsTaskCheckpoint: cp.IsTaskCheckpoint,
					ToolUseID:        cp.ToolUseID,
					Parent:           sessionNode,
				}
				sessionNode.Children = append(sessionNode.Children, checkpointNode)
			}

			branchNode.Children = append(branchNode.Children, sessionNode)
		}

		nodes = append(nodes, branchNode)
	}

	return nodes
}

// formatBranchLabel creates the display label for a branch.
func formatBranchLabel(branch BranchInfo, _ string) string {
	label := branch.Name

	if branch.IsCurrent {
		label += " *"
	}

	if branch.IsMerged {
		label += " (merged)"
	}

	sessionCount := len(branch.Sessions)
	if sessionCount > 0 {
		label += fmt.Sprintf("  (%d session", sessionCount)
		if sessionCount > 1 {
			label += "s"
		}
		label += ")"
	}

	return label
}

// formatSessionLabel creates the display label for a session.
func formatSessionLabel(sess SessionInfo) string {
	// Truncate session ID for display (e.g., "2025-01-10-abc12345")
	displayID := sess.Session.ID
	if len(displayID) > 19 {
		displayID = displayID[:19]
	}

	parts := []string{displayID}

	// Add checkpoint count
	if len(sess.Session.Checkpoints) > 0 {
		parts = append(parts, fmt.Sprintf("(%d checkpoints)", len(sess.Session.Checkpoints)))
	}

	// Add description (truncated)
	if sess.Session.Description != "" && sess.Session.Description != "No description" {
		desc := sess.Session.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		parts = append(parts, "- "+desc)
	}

	label := strings.Join(parts, " ")

	if sess.IsActive {
		label += " (active)"
	}

	return label
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
