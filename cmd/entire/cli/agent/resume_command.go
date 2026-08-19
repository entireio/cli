package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// ForegroundCommandSpec describes a command Entire can launch in the caller's
// terminal without going through a shell.
type ForegroundCommandSpec struct {
	Binary string
	Args   []string
}

// ResumeCommandSpecFor returns the foreground command shape for agents whose
// resume command is safe for Entire to launch directly. Agents not listed here
// still expose FormatResumeCommand for print-only resume instructions.
func ResumeCommandSpecFor(name types.AgentName, sessionID string) (ForegroundCommandSpec, bool) {
	sessionID = strings.TrimSpace(sessionID)
	switch name {
	case AgentNameClaudeCode:
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "claude", Args: []string{"-r", sessionID}}, true
	case AgentNameCodex:
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "codex", Args: []string{"resume", sessionID}}, true
	case AgentNameCopilotCLI:
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "copilot", Args: []string{"--resume", sessionID}}, true
	case AgentNameFactoryAIDroid:
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "droid", Args: []string{"--session-id", sessionID}}, true
	case AgentNameGemini:
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "gemini", Args: []string{"--resume", sessionID}}, true
	case AgentNameGoose:
		// --session-id requires --resume, so the flags are always paired.
		if sessionID == "" {
			return ForegroundCommandSpec{Binary: "goose", Args: []string{"session", "--resume"}}, true
		}
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{
			Binary: "goose",
			Args:   []string{"session", "--resume", "--session-id", sessionID},
		}, true
	case AgentNameQwenCode:
		// Bare `qwen` starts a fresh session, so the no-ID case uses --continue.
		if sessionID == "" {
			return ForegroundCommandSpec{Binary: "qwen", Args: []string{"--continue"}}, true
		}
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "qwen", Args: []string{"--resume", sessionID}}, true
	case AgentNameOpenHands:
		if sessionID == "" {
			return ForegroundCommandSpec{Binary: "openhands"}, true
		}
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{
			Binary: "openhands",
			Args:   []string{"--resume", dashedConversationID(sessionID)},
		}, true
	case AgentNameOpenCode:
		if sessionID == "" {
			return ForegroundCommandSpec{Binary: "opencode"}, true
		}
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "opencode", Args: []string{"-s", sessionID}}, true
	case AgentNamePi:
		if sessionID == "" {
			return ForegroundCommandSpec{Binary: "pi", Args: []string{"--continue"}}, true
		}
		if !isLaunchableResumeSessionID(sessionID) {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "pi", Args: []string{"--session", sessionID}}, true
	default:
		return ForegroundCommandSpec{}, false
	}
}

func isLaunchableResumeSessionID(sessionID string) bool {
	return sessionID != "" && validation.ValidateSessionID(sessionID) == nil
}

// NewResumeForegroundCommand builds a foreground command for resuming a session,
// when the agent has a launchable resume command. ok=false means callers should
// print FormatResumeCommand for the user instead.
func NewResumeForegroundCommand(ctx context.Context, name types.AgentName, sessionID string) (*exec.Cmd, bool, error) {
	spec, ok := ResumeCommandSpecFor(name, sessionID)
	if !ok {
		return nil, false, nil
	}
	cmd, err := NewForegroundCommand(ctx, spec.Binary, spec.Args...)
	if err != nil {
		return nil, true, fmt.Errorf("build %s resume command: %w", spec.Binary, err)
	}
	return cmd, true, nil
}

// dashedConversationID converts OpenHands' undashed hex32 conversation id into
// the dashed UUID form `openhands --resume` expects.
//
// OpenHands names the on-disk conversation directory with the undashed form but
// prints and accepts the dashed one, so the id Entire stores can be either.
// Anything that is not 32 hex characters passes through untouched.
//
// This duplicates resumeID in agent/openhands rather than calling it: that
// package imports agent, so importing it back would be a cycle. The two are
// pinned to agree by TestResumeCommandSpecMatchesFormattedResumeCommand.
func dashedConversationID(id string) string {
	if strings.Contains(id, "-") || len(id) != 32 {
		return id
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return id
		}
	}
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:32]
}
