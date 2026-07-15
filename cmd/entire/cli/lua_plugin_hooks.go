package cli

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/plugins"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// lifecycleHookName maps an internal agent lifecycle event to the stable,
// plugin-facing hook name. The mapping is deliberately explicit (not a direct
// enum-to-string) so the internal EventType can evolve without breaking the
// documented plugin surface. Events with no plugin hook (SubagentStart,
// ToolUse) return ok=false.
func lifecycleHookName(t agent.EventType) (string, bool) {
	switch t {
	case agent.SessionStart:
		return plugins.HookSessionStart, true
	case agent.TurnStart:
		return plugins.HookTurnStart, true
	case agent.TurnEnd:
		return plugins.HookTurnEnd, true
	case agent.Compaction:
		return plugins.HookCompaction, true
	case agent.SessionEnd:
		return plugins.HookSessionEnd, true
	case agent.SubagentEnd:
		return plugins.HookSubagentEnd, true
	case agent.ModelUpdate:
		return plugins.HookModelUpdate, true
	case agent.SubagentStart, agent.ToolUse:
		return "", false
	default:
		return "", false
	}
}

// firePluginLifecycleHook dispatches the observer hook for a successfully
// handled lifecycle event. Best-effort: FireHook is a no-op when no plugin is
// enabled and never propagates plugin failures.
func firePluginLifecycleHook(ctx context.Context, ag agent.Agent, event *agent.Event) {
	hook, ok := lifecycleHookName(event.Type)
	if !ok {
		return
	}
	plugins.FireHook(ctx, hook, lifecycleHookPayload(ag, event))
}

// lifecycleHookPayload builds the Lua-table payload for a lifecycle hook. It
// exposes operational metadata (ids, agent, model, file paths) but not prompt
// text or file contents — richer, sensitive data is reserved for
// capability-gated APIs.
func lifecycleHookPayload(ag agent.Agent, event *agent.Event) map[string]any {
	payload := map[string]any{
		"event": event.Type.String(),
		"agent": string(ag.Name()),
	}
	if event.SessionID != "" {
		payload["session_id"] = event.SessionID
	}
	if event.SessionRef != "" {
		payload["session_ref"] = event.SessionRef
	}
	if event.Model != "" {
		payload["model"] = event.Model
	}
	if len(event.ModifiedFiles) > 0 {
		payload["modified_files"] = event.ModifiedFiles
	}
	if len(event.NewFiles) > 0 {
		payload["new_files"] = event.NewFiles
	}
	if len(event.DeletedFiles) > 0 {
		payload["deleted_files"] = event.DeletedFiles
	}
	if event.SubagentType != "" {
		payload["subagent_type"] = event.SubagentType
	}
	return payload
}

// firePluginCheckpointSaved dispatches the checkpoint_saved observer hook after
// a session step checkpoint is written.
func firePluginCheckpointSaved(ctx context.Context, stepCtx strategy.StepContext) {
	payload := map[string]any{
		"agent": string(stepCtx.AgentType),
	}
	if stepCtx.SessionID != "" {
		payload["session_id"] = stepCtx.SessionID
	}
	if len(stepCtx.ModifiedFiles) > 0 {
		payload["modified_files"] = stepCtx.ModifiedFiles
	}
	if len(stepCtx.NewFiles) > 0 {
		payload["new_files"] = stepCtx.NewFiles
	}
	if len(stepCtx.DeletedFiles) > 0 {
		payload["deleted_files"] = stepCtx.DeletedFiles
	}
	plugins.FireHook(ctx, plugins.HookCheckpointSaved, payload)
}
