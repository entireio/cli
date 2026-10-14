//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// Token drivers for the agents other than Claude Code. Each writes the
// agent's own transcript (or hook payload) format and fires its real hooks.
// Cursor, Codex, Copilot CLI and Antigravity follow real captures; Factory
// Droid, OpenCode, Pi, the external agent and Vogon were not captured and
// keep the shapes their CLI parsers document.

// tokenDriverBase holds what every simple driver shares.
type tokenDriverBase struct {
	name, agent string
	caps        []tokenCap
}

func (b tokenDriverBase) Name() string      { return b.name }
func (b tokenDriverBase) AgentName() string { return b.agent }
func (b tokenDriverBase) FullMatrix() bool  { return false }
func (b tokenDriverBase) Caps() map[tokenCap]bool {
	m := map[tokenCap]bool{}
	for _, c := range b.caps {
		m[c] = true
	}
	return m
}
func (b tokenDriverBase) Expressible(u tokenCounts) tokenCounts { return u }
func (b tokenDriverBase) Runs(string) bool                      { return true }

// realDir resolves symlinks so paths match what the CLI computes on macOS.
func realDir(h *tokenHarness) string {
	d := h.t.TempDir()
	if resolved, err := filepath.EvalSymlinks(d); err == nil {
		return resolved
	}
	return d
}

func writeJSONFile(h *tokenHarness, path string, v any) {
	h.t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	require.NoError(h.t, err)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(h.t, os.WriteFile(path, data, 0o644))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- Cursor: tokens arrive only in the Stop hook payload ---

type cursorTokenDriver struct{ tokenDriverBase }

func newCursorTokenDriver() cursorTokenDriver {
	return cursorTokenDriver{tokenDriverBase{name: "cursor", agent: "cursor",
		caps: []tokenCap{capAttach, capSessionEnd}}}
}

func (cursorTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_CURSOR_PROJECT_DIR="+h.agentDir)
}

func (cursorTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("cursor", h.t.Name(), h.nextSessionNumber())
	s := &cursorTokenSession{unsupportedSession: unsupportedSession{"cursor"}, h: h, id: id,
		path: filepath.Join(h.agentDir, id, id+".jsonl")}
	s.hook("session-start", "sessionStart", map[string]any{"generation_id": id, "is_background_agent": false})
	return s
}

// cursorTokenSession fakes Cursor 2026.10.01. Its transcript is
// {"role":..,"message":{"content":[text|tool_use]}} lines with no usage; the
// tokens come only in the stop payload, per turn: input_tokens includes the
// cache reads (and, the CLI assumes, cache writes; the capture had none),
// keyed by the turn's generation_id, which beforeSubmitPrompt announced.
// sessionStart and the first beforeSubmitPrompt carry transcript_path null:
// the transcript does not exist yet.
type cursorTokenSession struct {
	unsupportedSession

	h          *tokenHarness
	id, path   string
	generation string
	pending    tokenCounts
	turn       int
}

func (s *cursorTokenSession) ID() string             { return s.id }
func (s *cursorTokenSession) TranscriptPath() string { return s.path }

func (s *cursorTokenSession) hook(name, event string, extra map[string]any) {
	var transcript any
	if fileExists(s.path) {
		transcript = s.path
	}
	in := mergeMaps(map[string]any{"conversation_id": s.id, "session_id": s.id, "model": "cursor-test",
		"hook_event_name": event, "cursor_version": "2026.10.01-test", "workspace_roots": []string{s.h.sessionDir(s.id)},
		"transcript_path": transcript}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "cursor", name, in)
}

func (s *cursorTokenSession) line(role string, content ...any) {
	appendJSONLines(s.h, s.path, map[string]any{"role": role, "message": map[string]any{"content": content}})
}

func (s *cursorTokenSession) StartTurn(prompt string) {
	s.turn++
	s.pending = tokenCounts{}
	s.generation = fakeUUID(s.id, "generation", s.turn)
	s.hook("before-submit-prompt", "beforeSubmitPrompt", map[string]any{"generation_id": s.generation,
		"prompt": prompt, "attachments": []any{}})
	s.line(roleUser, map[string]any{"type": blockTypeText, "text": "<user_query>\n" + prompt + "\n</user_query>"})
}

func (s *cursorTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	s.pending = s.pending.plus(u)
	call := map[string]any{"type": blockTypeToolUse, "name": "Shell", "input": map[string]any{
		"command": tool.command, "working_directory": s.h.sessionDir(s.id), "description": "commit"}}
	if tool.path != "" {
		abs := filepath.Join(s.h.sessionDir(s.id), tool.path)
		call = map[string]any{"type": blockTypeToolUse, "name": "ApplyPatch",
			"input": "*** Begin Patch\n*** Add File: " + abs + "\n+x\n*** End Patch\n"}
	}
	s.line(roleAssistant, map[string]any{"type": blockTypeText, "text": "working"}, call)
	run()
}

func (s *cursorTokenSession) Spend(u tokenCounts) {
	s.pending = s.pending.plus(u)
	s.line(roleAssistant, map[string]any{"type": blockTypeText, "text": "ok"})
}

// Stop reports the turn's tokens. A repeated Stop for the same turn re-sends
// its generation_id with the turn's running total.
func (s *cursorTokenSession) Stop() {
	u := s.pending
	s.hook("stop", "stop", map[string]any{
		"generation_id": s.generation, "status": "completed", "loop_count": 0,
		"input_tokens":      u.In + u.CacheRead + u.CacheWrite,
		"output_tokens":     u.Out,
		"cache_read_tokens": u.CacheRead, "cache_write_tokens": u.CacheWrite,
	})
}

func (s *cursorTokenSession) SessionEnd() {
	appendJSONLines(s.h, s.path, map[string]any{"type": "turn_ended", "status": "success"})
	s.hook("session-end", "sessionEnd", map[string]any{"generation_id": s.id, "reason": "completed",
		"duration_ms": 1000, "is_background_agent": false, "final_status": "completed"})
}

// --- Codex: cumulative token_count snapshots in the rollout ---

type codexTokenDriver struct{ tokenDriverBase }

func newCodexTokenDriver() codexTokenDriver {
	return codexTokenDriver{tokenDriverBase{name: "codex", agent: "codex",
		caps: []tokenCap{capAttach, capSessionEnd, capTranscriptTokens, capMidTurnTokens, capSubagent}}}
}

func (codexTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = filepath.Join(h.env.RepoDir, ".entire", "tmp")
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_CODEX_SESSION_DIR="+h.agentDir)
}

// Expressible drops cache writes: 0.156.1 reports cache_write_input_tokens,
// but it was 0 in every capture and the CLI does not read it.
func (codexTokenDriver) Expressible(u tokenCounts) tokenCounts {
	u.CacheWrite = 0
	return u
}

func (codexTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("codex", h.t.Name(), h.nextSessionNumber())
	s := &codexTokenSession{unsupportedSession: unsupportedSession{"codex"}, h: h, id: id,
		path: filepath.Join(h.agentDir, "rollout-2026-01-01T00-00-00-"+id+".jsonl")}
	s.line("session_meta", map[string]any{"session_id": id, "id": id, "timestamp": "2026-01-01T00:00:00.000Z",
		"cwd": h.sessionDir(id), "originator": "codex_exec", "cli_version": "0.156.1", "source": "exec",
		"thread_source": "user", "model_provider": "openai"})
	s.hook("session-start", "SessionStart", map[string]any{"source": "startup"})
	return s
}

// codexTokenSession fakes Codex 0.156.1. Every API call writes a
// token_usage_record (usage, turn_token_usage, thread_token_usage) when the
// response arrives and an event_msg token_count (cumulative
// total_token_usage, last_token_usage) only after the tool's output: the
// call that runs `git commit` has its token_count land after the commit's
// hooks ran. Input includes cached input; reasoning is inside output. Tools
// run through custom_tool_call "exec"; a file write shows as an
// item_completed FileChange with an absolute path.
type codexTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	ordinal  int
	turn     int
	turnID   string
	calls    int
	// thread and turnUsage are the cumulative and per-turn usage in Codex's
	// shape (input includes cached input).
	thread, turnUsage tokenCounts
}

func (s *codexTokenSession) ID() string             { return s.id }
func (s *codexTokenSession) TranscriptPath() string { return s.path }

func (s *codexTokenSession) line(typ string, payload map[string]any) {
	appendJSONLines(s.h, s.path, map[string]any{"timestamp": nowMillis(), "ordinal": s.ordinal, "type": typ, "payload": payload})
	s.ordinal++
}

func (s *codexTokenSession) event(payload map[string]any) { s.line("event_msg", payload) }

func (s *codexTokenSession) hook(name, event string, extra map[string]any) {
	in := mergeMaps(map[string]any{
		"session_id": s.id, "transcript_path": s.path, "cwd": s.h.sessionDir(s.id), "model": "gpt-test",
		"permission_mode": "bypassPermissions", "hook_event_name": event,
	}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "codex", name, in)
}

func (s *codexTokenSession) turnHook(name, event string, extra map[string]any) {
	s.hook(name, event, mergeMaps(map[string]any{"turn_id": s.turnID}, extra))
}

// codexUsage renders usage the way Codex counts it: cached input inside input.
func codexUsage(u tokenCounts) map[string]any {
	in := u.In + u.CacheRead
	return map[string]any{"input_tokens": in, "cached_input_tokens": u.CacheRead, "cache_write_input_tokens": 0,
		"output_tokens": u.Out, "reasoning_output_tokens": u.Out / 3, "total_tokens": in + u.Out}
}

func (s *codexTokenSession) StartTurn(prompt string) {
	s.turn++
	s.turnID = fakeUUID(s.id, "turn", s.turn)
	s.turnUsage = tokenCounts{}
	s.event(map[string]any{"type": "task_started", "turn_id": s.turnID, "root_turn_id": s.turnID,
		"started_at": time.Now().Unix(), "model_context_window": 258400, "collaboration_mode_kind": "default"})
	s.turnHook("user-prompt-submit", "UserPromptSubmit", map[string]any{"prompt": prompt})
	s.line("turn_context", map[string]any{"turn_id": s.turnID, "root_turn_id": s.turnID, "cwd": s.h.sessionDir(s.id),
		"approval_policy": "never"})
	s.line("response_item", map[string]any{"type": "message", "id": "msg_" + fakeUUID(s.id, "prompt", s.turn), "role": roleUser,
		"content": []any{map[string]any{"type": "input_text", "text": prompt}}})
	s.event(map[string]any{"type": "item_completed", "thread_id": s.id, "turn_id": s.turnID, "item": map[string]any{
		"type": "UserMessage", "content": []any{map[string]any{"type": blockTypeText, "text": prompt}}}})
}

// usageRecord writes the per-call record Codex appends when a response arrives.
func (s *codexTokenSession) usageRecord(u tokenCounts) {
	s.calls++
	s.thread = s.thread.plus(u)
	s.turnUsage = s.turnUsage.plus(u)
	s.line("token_usage_record", map[string]any{"thread_id": s.id, "turn_id": s.turnID, "session_id": s.id,
		"root_turn_id": s.turnID, "response_id": fmt.Sprintf("resp_%s_%d", s.id[:8], s.calls),
		"usage": codexUsage(u), "turn_token_usage": codexUsage(s.turnUsage), "thread_token_usage": codexUsage(s.thread)})
}

// tokenCount writes the cumulative snapshot the CLI reads.
func (s *codexTokenSession) tokenCount(u tokenCounts) {
	s.event(map[string]any{"type": "token_count", "info": map[string]any{
		"total_token_usage": codexUsage(s.thread), "last_token_usage": codexUsage(u), "model_context_window": 258400,
	}, "rate_limits": nil})
}

func (s *codexTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	callID := fmt.Sprintf("call_%s_%d", s.id[:8], s.calls+1)
	input := fmt.Sprintf("const r = await tools.exec_command({cmd:%q}); text(r.output);", tool.command)
	if tool.path != "" {
		input = fmt.Sprintf("const patch = %q;\nconst a = await tools.apply_patch(patch);\ntext(a);",
			"*** Begin Patch\n*** Add File: "+tool.path+"\n+x\n*** End Patch")
	}
	s.line("response_item", map[string]any{"type": "custom_tool_call", "status": "completed", "call_id": callID,
		"name": "exec", "input": input})
	s.usageRecord(u)
	run()
	if tool.path != "" {
		abs := filepath.Join(s.h.sessionDir(s.id), tool.path)
		s.event(map[string]any{"type": "item_completed", "thread_id": s.id, "turn_id": s.turnID, "item": map[string]any{
			"type": "FileChange", "changes": map[string]any{abs: map[string]any{"type": "add", "content": "x\n"}},
			"status": "completed"}})
		s.turnHook("post-tool-use", "PostToolUse", map[string]any{"tool_name": "apply_patch", "tool_use_id": callID,
			"tool_input":    map[string]any{"command": "*** Begin Patch\n*** Add File: " + tool.path + "\n+x\n*** End Patch"},
			"tool_response": "Success. Updated the following files:\nA " + tool.path + "\n"})
	} else {
		s.event(map[string]any{"type": "item_completed", "thread_id": s.id, "turn_id": s.turnID, "item": map[string]any{
			"type": "CommandExecution", "command": []string{"/bin/zsh", "-lc", tool.command}, "status": "completed", "exit_code": 0}})
		s.turnHook("post-tool-use", "PostToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": callID,
			"tool_input": map[string]any{"command": tool.command}, "tool_response": "ok"})
	}
	s.line("response_item", map[string]any{"type": "custom_tool_call_output", "call_id": callID,
		"output": []any{map[string]any{"type": "input_text", "text": "Script completed"}}})
	s.tokenCount(u)
}

func (s *codexTokenSession) Spend(u tokenCounts) {
	s.line("response_item", map[string]any{"type": "message", "role": roleAssistant, "phase": "final_answer",
		"content": []any{map[string]any{"type": "output_text", "text": "done"}}})
	s.usageRecord(u)
	s.tokenCount(u)
}

// SpendSubagent runs a spawned Codex thread: its own rollout (session_meta id
// = agent id) with one terminal turn, announced by subagent-start/stop.
func (s *codexTokenSession) SpendSubagent(agentID string, u tokenCounts) {
	childID := codexChildID(agentID)
	s.turnHook("subagent-start", "SubagentStart", map[string]any{"agent_id": childID, "agent_type": "worker"})
	child := s.childPath(agentID)
	appendJSONLines(s.h, child,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": childID, "thread_source": "subagent",
			"timestamp": "2026-01-01T00:00:01Z", "cwd": s.h.sessionDir(s.id)}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": s.turnID}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{
			"total_token_usage": codexUsage(u), "last_token_usage": codexUsage(u), "model_context_window": 258400}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": s.turnID}},
	)
	s.turnHook("subagent-stop", "SubagentStop", map[string]any{"agent_id": childID, "agent_type": "worker",
		"agent_transcript_path": child})
}

func (s *codexTokenSession) childPath(agentID string) string {
	return filepath.Join(s.h.agentDir, "rollout-2026-01-01T00-00-01-"+codexChildID(agentID)+".jsonl")
}

func (s *codexTokenSession) DeleteSubagentTranscript(agentID string) {
	require.NoError(s.h.t, os.Remove(s.childPath(agentID)))
}

// codexChildID derives a UUID-shaped thread ID from the harness's agent ID.
func codexChildID(agentID string) string {
	sum := sha256.Sum256([]byte(agentID))
	return "019b0000-0000-7000-8000-" + hex.EncodeToString(sum[:6])
}

// Stop: the turn's last token_count is already written; task_complete comes
// next, then the Stop hook.
func (s *codexTokenSession) Stop() {
	s.event(map[string]any{"type": "task_complete", "turn_id": s.turnID, "last_agent_message": "done"})
	s.turnHook("stop", "Stop", map[string]any{"stop_hook_active": false, "last_assistant_message": "done"})
}

func (s *codexTokenSession) SessionEnd() {
	s.hook("session-end", "SessionEnd", map[string]any{"reason": "other"})
}

// --- Copilot CLI: per-call usage records, a cumulative shutdown aggregate ---

type copilotTokenDriver struct{ tokenDriverBase }

func newCopilotTokenDriver() copilotTokenDriver {
	return copilotTokenDriver{tokenDriverBase{name: "copilot-cli", agent: "copilot-cli",
		caps: []tokenCap{capAttach, capSessionEnd, capTranscriptTokens, capMidTurnTokens}}}
}

func (copilotTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_COPILOT_SESSION_DIR="+h.agentDir)
}

func (copilotTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("copilot", h.t.Name(), h.nextSessionNumber())
	s := &copilotTokenSession{unsupportedSession: unsupportedSession{"copilot-cli"}, h: h, id: id,
		path: filepath.Join(h.agentDir, id, "events.jsonl")}
	s.event("session.start", map[string]any{"sessionId": s.id, "version": 1, "producer": "copilot-agent",
		"copilotVersion": "1.0.97", "selectedModel": "claude-test", "context": map[string]any{"cwd": h.sessionDir(id)}})
	s.hook("session-start", map[string]any{"source": "new"})
	return s
}

// copilotTokenSession fakes Copilot CLI 1.0.97 events.jsonl. Each API call
// writes a session.usage_record (inputTokens includes cache reads and cache
// writes; cacheReadTokens, cacheWriteTokens, outputTokens, reasoningTokens,
// apiCallId); assistant.message events carry no token fields. When the
// process exits, after the agent-stop and session-end hooks have run, it
// appends session.shutdown, whose modelMetrics usage is cumulative across
// every process of the session and also cache-inclusive. Only agent-stop's
// payload names the transcript.
type copilotTokenSession struct {
	unsupportedSession

	h          *tokenHarness
	id, path   string
	prev       string
	seq, calls int
	turnID     int
	// spent is the session's cumulative usage, which session.shutdown reports.
	spent tokenCounts
}

func (s *copilotTokenSession) ID() string             { return s.id }
func (s *copilotTokenSession) TranscriptPath() string { return s.path }

func (s *copilotTokenSession) event(typ string, data map[string]any) {
	s.seq++
	id := fakeUUID(s.id, "event", s.seq)
	var parent any
	if s.prev != "" {
		parent = s.prev
	}
	appendJSONLines(s.h, s.path, map[string]any{"type": typ, "data": data, "id": id,
		"timestamp": nowMillis(), "parentId": parent})
	s.prev = id
}

func (s *copilotTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"sessionId": s.id, "cwd": s.h.sessionDir(s.id), "timestamp": time.Now().UnixMilli()}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "copilot-cli", name, in)
}

func (s *copilotTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submitted", map[string]any{"prompt": prompt})
	s.turnID = 0
	s.event("user.message", map[string]any{"content": prompt, "messageId": fakeUUID(s.id, "prompt", s.seq)})
}

// call wraps one API call in assistant.turn_start/turn_end around its usage
// record and message.
func (s *copilotTokenSession) call(u tokenCounts, toolRequests []any, content string) {
	s.calls++
	s.spent = s.spent.plus(u)
	apiCallID := fmt.Sprintf("msg_%s_%d", s.id[:8], s.calls)
	s.event("assistant.turn_start", map[string]any{"turnId": strconv.Itoa(s.turnID)})
	s.event("session.usage_record", map[string]any{"usage": map[string]any{
		"model": "claude-test", "inputTokens": u.In + u.CacheRead + u.CacheWrite, "outputTokens": u.Out,
		"cacheReadTokens": u.CacheRead, "cacheWriteTokens": u.CacheWrite, "reasoningTokens": u.Out / 3,
		"apiCallId": apiCallID, "initiator": "agent",
		"accounting": map[string]any{"sourceSessionId": s.id, "sequence": s.calls, "usageId": apiCallID},
	}})
	s.event("assistant.message", map[string]any{"messageId": fakeUUID(s.id, "message", s.calls), "model": "claude-test",
		"content": content, "toolRequests": toolRequests, "turnId": strconv.Itoa(s.turnID), "apiCallId": apiCallID})
}

func (s *copilotTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	toolCallID := fmt.Sprintf("toolu_%s_%d", s.id[:8], s.calls+1)
	name, args := "bash", map[string]any{"command": tool.command}
	abs := filepath.Join(s.h.sessionDir(s.id), tool.path)
	if tool.path != "" {
		name, args = "create", map[string]any{"path": abs, "file_text": "x"}
	}
	s.call(u, []any{map[string]any{"toolCallId": toolCallID, "name": name, "arguments": args, "type": "function"}}, "")
	s.event("tool.execution_start", map[string]any{"toolCallId": toolCallID, "toolName": name, "arguments": args})
	run()
	done := map[string]any{"toolCallId": toolCallID, "model": "claude-test", "turnId": strconv.Itoa(s.turnID),
		"success": true, "result": map[string]any{"content": "ok"}}
	if tool.path != "" {
		files, err := json.Marshal([]string{abs})
		require.NoError(s.h.t, err)
		done["fileEdits"] = []any{map[string]any{"path": abs, "kind": "create"}}
		done["toolTelemetry"] = map[string]any{"properties": map[string]any{"command": "create"},
			"restrictedProperties": map[string]any{"filePaths": string(files)}}
	}
	s.event("tool.execution_complete", done)
	s.event("assistant.turn_end", map[string]any{"turnId": strconv.Itoa(s.turnID)})
	s.turnID++
}

func (s *copilotTokenSession) Spend(u tokenCounts) {
	s.call(u, []any{}, "ok")
	s.event("assistant.turn_end", map[string]any{"turnId": strconv.Itoa(s.turnID)})
	s.turnID++
}

func (s *copilotTokenSession) Stop() {
	s.hook("agent-stop", map[string]any{"transcriptPath": s.path, "stopReason": "end_turn", "stop_hook_active": false})
}

// SessionEnd is the process exiting: the session-end hook runs, then the
// process appends its cumulative session.shutdown.
func (s *copilotTokenSession) SessionEnd() {
	s.hook("session-end", map[string]any{"reason": "complete"})
	u := s.spent
	details := map[string]any{"input": map[string]any{"tokenCount": u.In},
		"cache_read": map[string]any{"tokenCount": u.CacheRead}, "cache_write": map[string]any{"tokenCount": u.CacheWrite},
		"output": map[string]any{"tokenCount": u.Out}}
	s.event("session.usage_checkpoint", map[string]any{})
	s.event("session.shutdown", map[string]any{
		"accountingSnapshot": map[string]any{"tokenDetails": details},
		"modelMetrics": map[string]any{"claude-test": map[string]any{
			"requests": map[string]any{"count": s.calls},
			"usage": map[string]any{"inputTokens": u.In + u.CacheRead + u.CacheWrite, "outputTokens": u.Out,
				"cacheReadTokens": u.CacheRead, "cacheWriteTokens": u.CacheWrite, "reasoningTokens": u.Out / 3},
			"tokenDetails": details,
		}},
	})
}

// --- Factory AI Droid: Claude-like usage inside Droid's message envelope ---

type droidTokenDriver struct{ tokenDriverBase }

func newDroidTokenDriver() droidTokenDriver {
	return droidTokenDriver{tokenDriverBase{name: "factoryai-droid", agent: "factoryai-droid",
		caps: []tokenCap{capAttach, capSessionEnd, capTranscriptTokens, capSubagent, capSplitMessage, capMidTurnTokens}}}
}

func (droidTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_DROID_PROJECT_DIR="+h.agentDir)
}

func (droidTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("droid-tok-%d", h.nextSessionNumber())
	return &droidTokenSession{unsupportedSession: unsupportedSession{"factoryai-droid"}, h: h, id: id,
		path: filepath.Join(h.agentDir, id+".jsonl")}
}

type droidTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	seq      int
}

func (s *droidTokenSession) ID() string             { return s.id }
func (s *droidTokenSession) TranscriptPath() string { return s.path }

func (s *droidTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"session_id": s.id, "transcript_path": s.path, "model": "droid-test"}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "factoryai-droid", name, in)
}

func (s *droidTokenSession) message(role, msgID string, content []any, u *tokenCounts) {
	s.seq++
	inner := map[string]any{"role": role, "content": content}
	if msgID != "" {
		inner["id"] = msgID
	}
	if u != nil {
		inner["usage"] = claudeUsage(*u)
	}
	appendJSONLines(s.h, s.path, map[string]any{"type": entryTypeMessage, "id": fmt.Sprintf("m%d", s.seq), "message": inner})
}

func (s *droidTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submit", map[string]any{"prompt": prompt})
	s.message(roleUser, "", []any{map[string]any{"type": blockTypeText, "text": prompt}}, nil)
}

func (s *droidTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	toolID := fmt.Sprintf("toolu_%s_%d", s.id, s.seq)
	name, input := "Execute", map[string]any{"command": tool.command}
	if tool.path != "" {
		name, input = "Write", map[string]any{"file_path": tool.path, "content": "x"}
	}
	s.message(roleAssistant, fmt.Sprintf("msg_%s_%d", s.id, s.seq+1),
		[]any{map[string]any{"type": blockTypeToolUse, "id": toolID, "name": name, "input": input}}, &u)
	run()
	s.message(roleUser, "", []any{map[string]any{"type": blockTypeToolResult, "tool_use_id": toolID, "content": "Success"}}, nil)
}

func (s *droidTokenSession) Spend(u tokenCounts) {
	s.SpendFinal(fmt.Sprintf("msg_%s_%d", s.id, s.seq+1), u)
}

func (s *droidTokenSession) SpendPartial(msgID string, u tokenCounts) { s.SpendFinal(msgID, u) }

func (s *droidTokenSession) SpendFinal(msgID string, u tokenCounts) {
	s.message(roleAssistant, msgID, []any{map[string]any{"type": blockTypeText, "text": "working"}}, &u)
}

// SpendSubagent runs a Task subagent Claude-style: its transcript sits under
// <dir>/<session>/subagents and the tool result names its agent ID.
func (s *droidTokenSession) SpendSubagent(agentID string, u tokenCounts) {
	toolID := "toolu_" + agentID
	s.message(roleAssistant, "", []any{map[string]any{"type": blockTypeToolUse, "id": toolID, "name": "Task",
		"input": map[string]any{"prompt": "help", "subagent_type": "worker"}}}, nil)
	s.hook("pre-tool-use", map[string]any{"tool_use_id": toolID, "tool_name": "Task",
		"tool_input": map[string]any{"prompt": "help", "subagent_type": "worker"}})
	appendJSONLines(s.h, s.subagentPath(agentID), map[string]any{"type": entryTypeMessage, "id": "sm1", "message": map[string]any{
		"role": roleAssistant, "id": "msg_" + agentID, "content": []any{map[string]any{"type": blockTypeText, "text": "sub"}},
		"usage": claudeUsage(u)}})
	s.message(roleUser, "", []any{map[string]any{"type": blockTypeToolResult, "tool_use_id": toolID,
		"content": "Done.\nagentId: " + agentID}}, nil)
	s.hook("post-tool-use", map[string]any{"tool_use_id": toolID, "tool_name": "Task",
		"tool_input": map[string]any{"prompt": "help"}, "tool_response": map[string]any{"agentId": agentID}})
}

func (s *droidTokenSession) subagentPath(agentID string) string {
	return filepath.Join(paths.SubagentsDir(filepath.Dir(s.path), s.id), paths.AgentTranscriptFileName(agentID))
}

func (s *droidTokenSession) DeleteSubagentTranscript(agentID string) {
	require.NoError(s.h.t, os.Remove(s.subagentPath(agentID)))
}

func (s *droidTokenSession) Stop()       { s.hook("stop", nil) }
func (s *droidTokenSession) SessionEnd() { s.hook("session-end", nil) }

// --- OpenCode: the whole export is rewritten each time ---

type openCodeTokenDriver struct{ tokenDriverBase }

func newOpenCodeTokenDriver() openCodeTokenDriver {
	return openCodeTokenDriver{tokenDriverBase{name: "opencode", agent: "opencode",
		caps: []tokenCap{capAttach, capSessionEnd, capMidTurnTokens}}}
}

func (openCodeTokenDriver) Setup(h *tokenHarness) {
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_OPENCODE_MOCK_EXPORT=1")
}

func (openCodeTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("opencode-tok-%d", h.nextSessionNumber())
	s := &openCodeTokenSession{unsupportedSession: unsupportedSession{"opencode"}, h: h, id: id,
		path: filepath.Join(h.env.OpenCodeProjectDir, id+".json")}
	s.write()
	s.hook("session-start", nil)
	return s
}

type openCodeTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	messages []map[string]any
}

func (s *openCodeTokenSession) ID() string             { return s.id }
func (s *openCodeTokenSession) TranscriptPath() string { return s.path }

func (s *openCodeTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"session_id": s.id, "model": "opencode-test"}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "opencode", name, in)
}

// write stores the export where `opencode export` would produce it, and
// the copy the CLI reads in mock-export mode.
func (s *openCodeTokenSession) write() {
	export := map[string]any{"info": map[string]any{"id": s.id}, "messages": s.messages}
	writeJSONFile(s.h, s.path, export)
	writeJSONFile(s.h, filepath.Join(s.h.sessionDir(s.id), ".entire", "tmp", s.id+".json"), export)
}

func (s *openCodeTokenSession) add(role string, parts []any, u *tokenCounts) {
	n := len(s.messages) + 1
	info := map[string]any{"id": fmt.Sprintf("msg-%d", n), "role": role,
		"time": map[string]any{"created": 1708300000 + n, "completed": 1708300000 + n + 1}}
	if u != nil {
		info["tokens"] = map[string]any{"input": u.In, "output": u.Out, "reasoning": 0,
			"cache": map[string]any{"read": u.CacheRead, "write": u.CacheWrite}}
	}
	s.messages = append(s.messages, map[string]any{"info": info, "parts": parts})
	s.write()
}

func (s *openCodeTokenSession) StartTurn(prompt string) {
	s.hook("turn-start", map[string]any{"prompt": prompt})
	s.add(roleUser, []any{map[string]any{"type": blockTypeText, "text": prompt}}, nil)
}

func (s *openCodeTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	part := map[string]any{"type": "tool", "tool": "bash", "callID": fmt.Sprintf("call-%d", len(s.messages)),
		"state": map[string]any{"status": "completed", "input": map[string]any{"command": tool.command}, "output": "ok"}}
	if tool.path != "" {
		part = map[string]any{"type": "tool", "tool": "write", "callID": "call-" + tool.path,
			"state": map[string]any{"status": "completed", "input": map[string]any{"filePath": tool.path}, "output": "ok"}}
	}
	s.add(roleAssistant, []any{part}, &u)
	run()
}

func (s *openCodeTokenSession) Spend(u tokenCounts) {
	s.add(roleAssistant, []any{map[string]any{"type": blockTypeText, "text": "working"}}, &u)
}

func (s *openCodeTokenSession) Stop()       { s.hook("turn-end", nil) }
func (s *openCodeTokenSession) SessionEnd() { s.hook("session-end", nil) }

// --- Pi: session JSONL with a linear parentId chain ---

type piTokenDriver struct{ tokenDriverBase }

func newPiTokenDriver() piTokenDriver {
	return piTokenDriver{tokenDriverBase{name: "pi", agent: "pi", caps: []tokenCap{capAttach, capTranscriptTokens, capMidTurnTokens}}}
}

func (piTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_PI_SESSION_DIR="+h.agentDir)
}

func (piTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("0199a000-0000-7000-8000-%012d", h.nextSessionNumber())
	s := &piTokenSession{unsupportedSession: unsupportedSession{"pi"}, h: h, id: id,
		path: filepath.Join(h.agentDir, "2026-01-01T00-00-00-000Z_"+id+".jsonl")}
	appendJSONLines(h, s.path, map[string]any{"type": "session", "version": 3, "id": id,
		"timestamp": "2026-01-01T00:00:00.000Z", "cwd": h.sessionDir(id)})
	s.hook("session_start", nil)
	return s
}

type piTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	seq      int
}

func (s *piTokenSession) ID() string             { return s.id }
func (s *piTokenSession) TranscriptPath() string { return s.path }

func (s *piTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"type": name, "cwd": s.h.sessionDir(s.id), "session_file": s.path}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "pi", name, in)
}

func (s *piTokenSession) message(m map[string]any) {
	s.seq++
	var parent any
	if s.seq > 1 {
		parent = fmt.Sprintf("m%d", s.seq-1)
	}
	appendJSONLines(s.h, s.path, map[string]any{"type": entryTypeMessage, "id": fmt.Sprintf("m%d", s.seq),
		"parentId": parent, "message": m})
}

func piUsage(u tokenCounts) map[string]any {
	return map[string]any{"input": u.In, "output": u.Out, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite}
}

func (s *piTokenSession) StartTurn(prompt string) {
	s.hook("before_agent_start", map[string]any{"prompt": prompt})
	s.message(map[string]any{"role": roleUser, "content": []any{map[string]any{"type": blockTypeText, "text": prompt}}})
}

func (s *piTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	call := map[string]any{"type": "toolCall", "id": fmt.Sprintf("tc-%d", s.seq), "name": "bash",
		"arguments": map[string]any{"command": tool.command}}
	if tool.path != "" {
		call = map[string]any{"type": "toolCall", "id": "tc-" + tool.path, "name": "write",
			"arguments": map[string]any{"path": tool.path, "content": "x"}}
	}
	s.message(map[string]any{"role": roleAssistant, "model": "pi-test", "content": []any{call}, "usage": piUsage(u)})
	run()
}

func (s *piTokenSession) Spend(u tokenCounts) {
	s.message(map[string]any{"role": roleAssistant, "model": "pi-test",
		"content": []any{map[string]any{"type": blockTypeText, "text": "working"}}, "usage": piUsage(u)})
}

func (s *piTokenSession) Stop() { s.hook("agent_end", nil) }

// --- Antigravity: per-call usage in the transcript, a lagging statusline ---

type antigravityTokenDriver struct{ tokenDriverBase }

func newAntigravityTokenDriver() antigravityTokenDriver {
	return antigravityTokenDriver{tokenDriverBase: tokenDriverBase{name: "antigravity", agent: "antigravity"}}
}

func (antigravityTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv,
		antigravityBrainDirEnv(h.env.RepoDir),
		"ENTIRE_ANTIGRAVITY_STATUS_DIR="+h.agentDir,
		"ENTIRE_ANTIGRAVITY_CONFIG_DIR="+realDir(h))
}

// Expressible bills the whole prompt as input: agy 1.3.4 reported no cache
// reads or writes in any capture (cache_read_tokens and the statusline's
// cache fields were always 0), and its transcript has no cache-write field.
// Folding cache reads into input also keeps each call's input the size of
// its context, as in the capture, which is what the context gauge tracks.
func (antigravityTokenDriver) Expressible(u tokenCounts) tokenCounts {
	u.In += u.CacheRead
	u.CacheRead, u.CacheWrite = 0, 0
	return u
}

func (antigravityTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("antigravity", h.t.Name(), h.nextSessionNumber())
	brain := filepath.Join(antigravityBrainDir(h.env.RepoDir), id)
	return &antigravityTokenSession{unsupportedSession: unsupportedSession{"antigravity"}, h: h, id: id, brain: brain,
		path:     filepath.Join(brain, ".system_generated", "logs", "transcript_full.jsonl"),
		snapshot: filepath.Join(h.agentDir, id+".jsonl"),
		clock:    time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)}
}

// antigravityTokenSession fakes agy 1.3.4. transcript_full.jsonl has one
// step per line; PLANNER_RESPONSE steps carry the call's input_tokens,
// cache_read_tokens and output_tokens. pre-invocation fires before every
// model call (invocationNum counts from 0 within one run), and Stop fires
// once per run (executionNum 0, terminationReason NO_TOOL_CALL).
//
// The statusline snapshot (what the CLI reads) is written just before each
// pre-invocation and lags one call: it shows every call finished so far, so
// a run's last call first appears in the next run's first snapshot.
// total_output_tokens is cumulative, but total_input_tokens is not: it is a
// context-size gauge that tracks the latest reported call's prompt (plus a
// near-constant ~5k in the capture, left out here), not a sum.
type antigravityTokenSession struct {
	unsupportedSession

	h                         *tokenHarness
	id, brain, path, snapshot string
	step, runCalls            int
	turns                     int
	// done is every finished call, in order.
	done  []tokenCounts
	clock time.Time
}

func (s *antigravityTokenSession) ID() string             { return s.id }
func (s *antigravityTokenSession) TranscriptPath() string { return s.path }

func (s *antigravityTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"conversationId": s.id, "workspacePaths": []string{s.h.sessionDir(s.id)},
		"transcriptPath": s.path, "artifactDirectoryPath": s.brain, "modelName": "gemini-test"}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "antigravity", name, in)
}

func (s *antigravityTokenSession) stepLine(source, typ string, extra map[string]any) {
	appendJSONLines(s.h, s.path, mergeMaps(map[string]any{"step_index": s.step, "source": source, "type": typ,
		"status": "DONE", "created_at": s.clock.Format(time.RFC3339)}, extra))
	s.step++
}

func (s *antigravityTokenSession) StartTurn(prompt string) {
	s.turns++
	s.runCalls = 0
	s.stepLine("USER_EXPLICIT", "USER_INPUT", map[string]any{"content": "<USER_REQUEST>\n" + prompt + "\n</USER_REQUEST>"})
	if s.turns > 1 {
		s.stepLine("SYSTEM", "SYSTEM_MESSAGE", map[string]any{"content": "system"})
	}
}

// writeSnapshot appends the statusline the tee writes before a model call.
func (s *antigravityTokenSession) writeSnapshot() {
	s.clock = s.clock.Add(time.Minute)
	cw := map[string]any{"total_input_tokens": 0, "total_output_tokens": 0, "context_window_size": 1048576}
	if n := len(s.done); n > 0 {
		out := 0
		for _, c := range s.done {
			out += c.Out
		}
		last := s.done[n-1]
		cw["total_input_tokens"] = last.In
		cw["total_output_tokens"] = out
		cw["current_usage"] = map[string]any{"input_tokens": last.In, "output_tokens": last.Out,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": last.CacheRead}
	}
	appendJSONLines(s.h, s.snapshot, map[string]any{"ts": s.clock.Format(time.RFC3339Nano), "conversation_id": s.id,
		"context_window": cw})
}

// modelCall is one API call: snapshot, pre-invocation, then the planner step.
func (s *antigravityTokenSession) modelCall(u tokenCounts, extra map[string]any) {
	s.writeSnapshot()
	s.hook("pre-invocation", map[string]any{"invocationNum": s.runCalls, "initialNumSteps": s.step})
	s.runCalls++
	s.stepLine("MODEL", "PLANNER_RESPONSE", mergeMaps(map[string]any{"input_tokens": u.In,
		"cache_read_tokens": u.CacheRead, "output_tokens": u.Out}, extra))
	s.done = append(s.done, u)
}

func (s *antigravityTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	name, args := "run_command", map[string]any{"CommandLine": tool.command, "Cwd": s.h.sessionDir(s.id)}
	if tool.path != "" {
		name, args = "write_to_file", map[string]any{"TargetFile": filepath.Join(s.h.sessionDir(s.id), tool.path),
			"CodeContent": "x\n", "Overwrite": true, "Description": "write"}
	}
	call := map[string]any{"name": name, "args": args}
	s.modelCall(u, map[string]any{"tool_calls": []any{call}})
	s.hook("pre-tool-use", map[string]any{"stepIdx": s.step, "toolCall": call})
	run()
	s.stepLine("MODEL", "GENERIC", map[string]any{"content": "done"})
}

func (s *antigravityTokenSession) Spend(u tokenCounts) {
	s.modelCall(u, map[string]any{"content": "working"})
}

func (s *antigravityTokenSession) Stop() {
	s.hook("stop", map[string]any{"executionNum": 0, "terminationReason": "NO_TOOL_CALL",
		"error": "", "fullyIdle": true})
}

// --- Vogon: the deterministic fake agent's own JSONL ---

type vogonTokenDriver struct{ tokenDriverBase }

func newVogonTokenDriver() vogonTokenDriver {
	return vogonTokenDriver{tokenDriverBase{name: "vogon", agent: "vogon",
		caps: []tokenCap{capAttach, capSessionEnd, capTranscriptTokens, capMidTurnTokens}}}
}

// Vogon is the cheapest driver, so it runs the whole table by default.
func (vogonTokenDriver) FullMatrix() bool { return true }

func (vogonTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_VOGON_PROJECT_DIR="+h.agentDir)
}

func (vogonTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("vogon-tok-%d", h.nextSessionNumber())
	s := &vogonTokenSession{unsupportedSession: unsupportedSession{"vogon"}, h: h, id: id,
		path: filepath.Join(h.agentDir, id+".jsonl")}
	s.hook("session-start", nil)
	return s
}

type vogonTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
}

func (s *vogonTokenSession) ID() string             { return s.id }
func (s *vogonTokenSession) TranscriptPath() string { return s.path }

func (s *vogonTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"session_id": s.id, "transcript_path": s.path, "model": "vogon-test"}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "vogon", name, in)
}

func (s *vogonTokenSession) line(m map[string]any) {
	m["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	appendJSONLines(s.h, s.path, m)
}

func vogonUsage(u tokenCounts) map[string]any {
	return map[string]any{"input_tokens": u.In, "output_tokens": u.Out, "cache_read_tokens": u.CacheRead,
		"cache_creation_tokens": u.CacheWrite}
}

func (s *vogonTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submit", map[string]any{"prompt": prompt})
	s.line(map[string]any{"type": roleUser, "message": prompt})
}

func (s *vogonTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	l := map[string]any{"type": "tool_use", "message": tool.command, "usage": vogonUsage(u)}
	if tool.path != "" {
		l = map[string]any{"type": "tool_use", "message": "wrote " + tool.path, "files": []string{tool.path}, "usage": vogonUsage(u)}
	}
	s.line(l)
	run()
}

func (s *vogonTokenSession) Spend(u tokenCounts) {
	s.line(map[string]any{"type": roleAssistant, "message": "working", "usage": vogonUsage(u)})
}

func (s *vogonTokenSession) Stop()       { s.hook("stop", nil) }
func (s *vogonTokenSession) SessionEnd() { s.hook("session-end", nil) }
