package list

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// SetStrategy sets the strategy to use for actions.
// This must be called before performing actions.
var currentStrategy strategy.Strategy

// SetStrategy sets the current strategy for actions.
func SetStrategy(s strategy.Strategy) {
	currentStrategy = s
}

// PerformOpen opens a session or checkpoint without changing branches.
// It copies session logs to the agent's session directory and returns the resume command.
func PerformOpen(node *Node) *ActionResult {
	result := &ActionResult{
		Action: ActionOpen,
	}

	// Determine session ID and checkpoint ID based on node type
	var sessionID, checkpointID string
	switch node.Type {
	case NodeTypeBranch:
		result.Error = errors.New("open is only available for sessions and checkpoints")
		return result
	case NodeTypeSession:
		sessionID = node.SessionID
		// Find most recent checkpoint
		if len(node.Children) > 0 {
			checkpointID = node.Children[0].CheckpointID
		}
	case NodeTypeCheckpoint:
		checkpointID = node.CheckpointID
		// Get session ID from parent
		if node.Parent != nil && node.Parent.Type == NodeTypeSession {
			sessionID = node.Parent.SessionID
		}
	}

	if sessionID == "" {
		result.Error = errors.New("could not determine session ID")
		return result
	}

	result.SessionID = sessionID
	result.CheckpointID = checkpointID

	// Get agent
	ag, err := agent.Detect()
	if err != nil {
		ag = agent.Default()
		if ag == nil {
			result.Error = fmt.Errorf("no agent available: %w", err)
			return result
		}
	}

	// Restore session logs
	if checkpointID != "" {
		if err := restoreSessionLogs(ag, sessionID, checkpointID); err != nil {
			result.Error = err
			return result
		}
		result.Message = "Session logs restored for: " + sessionID
	}

	// Generate resume command
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	result.ResumeCommand = ag.FormatResumeCommand(agentSessionID)

	return result
}

// PerformResume switches to the appropriate branch and restores the session.
func PerformResume(node *Node) *ActionResult {
	result := &ActionResult{
		Action: ActionResume,
	}

	// Determine branch and session based on node type
	var branchName, sessionID, checkpointID string
	switch node.Type {
	case NodeTypeBranch:
		branchName = node.ID
		// Find most recent session
		if len(node.Children) > 0 {
			sessionNode := node.Children[0]
			sessionID = sessionNode.SessionID
			if len(sessionNode.Children) > 0 {
				checkpointID = sessionNode.Children[0].CheckpointID
			}
		}
	case NodeTypeSession:
		sessionID = node.SessionID
		if len(node.Children) > 0 {
			checkpointID = node.Children[0].CheckpointID
		}
		// Get branch from parent
		if node.Parent != nil && node.Parent.Type == NodeTypeBranch {
			branchName = node.Parent.ID
		}
	case NodeTypeCheckpoint:
		checkpointID = node.CheckpointID
		// Get session from parent
		if node.Parent != nil && node.Parent.Type == NodeTypeSession {
			sessionID = node.Parent.SessionID
			// Get branch from grandparent
			if node.Parent.Parent != nil && node.Parent.Parent.Type == NodeTypeBranch {
				branchName = node.Parent.Parent.ID
			}
		}
	}

	result.SessionID = sessionID
	result.CheckpointID = checkpointID

	// Get agent
	ag, err := agent.Detect()
	if err != nil {
		ag = agent.Default()
		if ag == nil {
			result.Error = fmt.Errorf("no agent available: %w", err)
			return result
		}
	}

	// If we have a branch and it's not the current one, we need to checkout
	// Note: This doesn't actually checkout here - we just prepare the message
	// The actual checkout would be done by the caller using the resume command
	if branchName != "" {
		result.Message = fmt.Sprintf("Branch: %s\nSession: %s", branchName, sessionID)
	} else if sessionID != "" {
		result.Message = "Session: " + sessionID
	}

	// Restore session logs if we have them
	if sessionID != "" && checkpointID != "" {
		if err := restoreSessionLogs(ag, sessionID, checkpointID); err != nil {
			// Non-fatal, just warn
			result.Message += fmt.Sprintf("\nWarning: could not restore session logs: %v", err)
		}
	}

	// Set current session
	if sessionID != "" {
		if err := paths.WriteCurrentSession(sessionID); err != nil {
			result.Message += fmt.Sprintf("\nWarning: could not set current session: %v", err)
		}
	}

	// Generate resume command
	if sessionID != "" {
		agentSessionID := ag.ExtractAgentSessionID(sessionID)
		result.ResumeCommand = ag.FormatResumeCommand(agentSessionID)
	}

	return result
}

// PerformRewind restores the code state to a checkpoint.
func PerformRewind(node *Node) *ActionResult {
	result := &ActionResult{
		Action: ActionRewind,
	}

	if node.Type != NodeTypeCheckpoint {
		result.Error = errors.New("rewind is only available for checkpoints")
		return result
	}

	checkpointID := node.CheckpointID
	result.CheckpointID = checkpointID

	// Get session ID from parent
	var sessionID string
	if node.Parent != nil && node.Parent.Type == NodeTypeSession {
		sessionID = node.Parent.SessionID
	}
	result.SessionID = sessionID

	// Get agent
	ag, err := agent.Detect()
	if err != nil {
		ag = agent.Default()
		if ag == nil {
			result.Error = fmt.Errorf("no agent available: %w", err)
			return result
		}
	}

	// Get strategy
	strat := currentStrategy
	if strat == nil {
		result.Error = errors.New("strategy not set")
		return result
	}

	// Get rewind points
	rewindPoints, err := strat.GetRewindPoints(100)
	if err != nil {
		result.Error = fmt.Errorf("failed to get rewind points: %w", err)
		return result
	}

	// Find matching rewind point
	var targetPoint *strategy.RewindPoint
	for i, point := range rewindPoints {
		if point.CheckpointID == checkpointID {
			targetPoint = &rewindPoints[i]
			break
		}
	}

	if targetPoint == nil {
		// Checkpoint might be logs-only
		result.Message = fmt.Sprintf("Checkpoint %s is logs-only (code state not available for full rewind)", checkpointID[:8])
		if sessionID != "" {
			// Still restore logs
			if err := restoreSessionLogs(ag, sessionID, checkpointID); err == nil {
				result.Message += "\nSession logs restored."
			}
			agentSessionID := ag.ExtractAgentSessionID(sessionID)
			result.ResumeCommand = ag.FormatResumeCommand(agentSessionID)
		}
		return result
	}

	// Check if rewind is possible
	canRewind, reason, err := strat.CanRewind()
	if err != nil {
		result.Error = fmt.Errorf("failed to check rewind status: %w", err)
		return result
	}
	if !canRewind {
		result.Error = fmt.Errorf("cannot rewind: %s", reason)
		return result
	}

	// Perform the rewind
	if err := strat.Rewind(*targetPoint); err != nil {
		result.Error = fmt.Errorf("rewind failed: %w", err)
		return result
	}

	result.Message = "Rewound to checkpoint: " + checkpointID[:8]

	// Generate resume command
	if sessionID != "" {
		agentSessionID := ag.ExtractAgentSessionID(sessionID)
		result.ResumeCommand = ag.FormatResumeCommand(agentSessionID)
	}

	return result
}

// restoreSessionLogs copies session logs from entire/sessions to the agent's session directory.
func restoreSessionLogs(ag agent.Agent, sessionID, checkpointID string) error {
	// Get session directory for this agent
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	sessionDir, err := ag.GetSessionDir(cwd)
	if err != nil {
		return fmt.Errorf("failed to determine session directory: %w", err)
	}

	// Extract agent-specific session ID
	agentSessionID := ag.ExtractAgentSessionID(sessionID)
	sessionLogPath := filepath.Join(sessionDir, agentSessionID+".jsonl")

	// Check if already exists
	if fileExists(sessionLogPath) {
		return nil // Already restored
	}

	// Get strategy
	strat := currentStrategy
	if strat == nil {
		return errors.New("strategy not set")
	}

	// Get session log content
	logContent, _, err := strat.GetSessionLog(checkpointID)
	if err != nil {
		return fmt.Errorf("failed to get session log: %w", err)
	}

	// Create an AgentSession with the native data
	agentSession := &agent.AgentSession{
		SessionID:  agentSessionID,
		AgentName:  ag.Name(),
		RepoPath:   cwd,
		SessionRef: sessionLogPath,
		NativeData: logContent,
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Write the session using the agent's WriteSession method
	if err := ag.WriteSession(agentSession); err != nil {
		return fmt.Errorf("failed to write session: %w", err)
	}

	return nil
}

// fileExists checks if a file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
