package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Antigravity hook name constants — these become subcommands under `entire hooks antigravity`.
const (
	HookNamePreToolUse     = "pre-tool-use"
	HookNamePostToolUse    = "post-tool-use"
	HookNamePreInvocation  = "pre-invocation"
	HookNamePostInvocation = "post-invocation"
	HookNameStop           = "stop"
)

// HookNames returns the hook verbs Antigravity supports.
// These become subcommands: entire hooks antigravity <verb>
func (a *AntigravityAgent) HookNames() []string {
	return []string{
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNamePreInvocation,
		HookNamePostInvocation,
		HookNameStop,
	}
}

// ParseHookEvent translates an Antigravity hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance (e.g., post-tool-use).
func (a *AntigravityAgent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNamePreInvocation:
		return parsePreInvocation(stdin)
	case HookNamePostInvocation:
		return parsePostInvocation(stdin)
	case HookNameStop:
		return parseStop(stdin)
	case HookNamePreToolUse:
		return parsePreToolUse(stdin)
	case HookNamePostToolUse:
		// PostToolUse has no lifecycle significance in v1
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// parsePreInvocation handles the PreInvocation hook → TurnStart.
// PreInvocation fires before each agent invocation. We always emit TurnStart;
// the framework's strategy.InitializeSession is idempotent on first arrival.
func parsePreInvocation(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[InvocationPayload](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.ConversationID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

// parsePostInvocation handles the PostInvocation hook → TurnEnd.
func parsePostInvocation(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[InvocationPayload](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.ConversationID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

// parseStop handles the Stop hook.
// Returns SessionEnd when fullyIdle=true; returns nil when background tasks
// are still running (fullyIdle=false) to avoid finalizing prematurely.
func parseStop(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[StopPayload](stdin)
	if err != nil {
		return nil, err
	}
	if !raw.FullyIdle {
		return nil, nil //nolint:nilnil // Background tasks running — do not end session yet
	}
	return &agent.Event{
		Type:       agent.SessionEnd,
		SessionID:  raw.ConversationID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

// parsePreToolUse handles the PreToolUse hook → ToolUse for mutating tools.
// Returns nil for non-mutating tools (no lifecycle action needed).
func parsePreToolUse(stdin io.Reader) (*agent.Event, error) {
	raw, err := agent.ReadAndParseHookInput[PreToolUsePayload](stdin)
	if err != nil {
		return nil, err
	}
	modifiedFiles, newFiles := extractFilesFromToolCall(&raw.ToolCall)
	if modifiedFiles == nil && newFiles == nil {
		return nil, nil //nolint:nilnil // Non-mutating tool — no lifecycle action
	}
	return &agent.Event{
		Type:          agent.ToolUse,
		SessionID:     raw.ConversationID,
		SessionRef:    raw.TranscriptPath,
		ModifiedFiles: modifiedFiles,
		NewFiles:      newFiles,
		Timestamp:     time.Now(),
	}, nil
}

// writeToFileArgs captures the relevant fields from write_to_file tool arguments.
type writeToFileArgs struct {
	TargetFile string `json:"TargetFile"`
	Overwrite  bool   `json:"Overwrite"`
}

// targetFileArgs captures the TargetFile field for replace_file_content /
// multi_replace_file_content.
type targetFileArgs struct {
	TargetFile string `json:"TargetFile"`
}

// extractFilesFromToolCall inspects the tool call and returns the files it
// will modify or create. Both slices are nil for non-mutating tools.
func extractFilesFromToolCall(tc *ToolCall) (modifiedFiles, newFiles []string) {
	switch tc.Name {
	case "write_to_file":
		var args writeToFileArgs
		if err := json.Unmarshal(tc.Args, &args); err != nil || args.TargetFile == "" {
			return nil, nil
		}
		if args.Overwrite {
			return []string{args.TargetFile}, nil
		}
		return nil, []string{args.TargetFile}

	case "replace_file_content", "multi_replace_file_content":
		var args targetFileArgs
		if err := json.Unmarshal(tc.Args, &args); err != nil || args.TargetFile == "" {
			return nil, nil
		}
		return []string{args.TargetFile}, nil

	default:
		return nil, nil
	}
}
