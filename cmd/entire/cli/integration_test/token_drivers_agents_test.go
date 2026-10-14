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
	"strings"
	"time"

	"github.com/stretchr/testify/require"
)

// Token drivers for the agents other than Claude Code. Each writes the
// agent's own transcript (or hook payload) format and fires its real hooks.
// Cursor, Codex, Copilot CLI, Antigravity, Factory Droid, OpenCode and Pi
// follow real captures; the external agent and Vogon have no real
// counterpart and keep the shapes their CLI parsers document.

// tokenDriverBase holds what every simple driver shares.
type tokenDriverBase struct {
	name, agent string
	caps        []tokenCap
	// headless marks a variant whose agent runs one process per prompt.
	headless bool
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
func (b tokenDriverBase) Runs(scenario string) bool {
	return !b.headless || headlessScenarios[scenario]
}

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
// cache reads and the cache writes (a capture's first turn reported 47,165 =
// 6 + 31,480 read + 15,679 written), keyed by the turn's generation_id, which
// beforeSubmitPrompt announced. Every call here spends cache writes.
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

// newCodexExecTokenDriver is `codex exec`: one process per prompt, so
// SessionStart (startup, then resume) and SessionEnd fire around every turn.
func newCodexExecTokenDriver() codexTokenDriver {
	d := newCodexTokenDriver()
	d.name, d.headless = "codex-exec", true
	return d
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

func (d codexTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("codex", h.t.Name(), h.nextSessionNumber())
	s := &codexTokenSession{unsupportedSession: unsupportedSession{"codex"}, h: h, id: id, headless: d.headless,
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
	headless bool
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
	if s.headless && s.turn > 1 {
		s.hook("session-start", "SessionStart", map[string]any{"source": "resume"})
	}
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
// next, then the Stop hook. `codex exec` then exits (SessionEnd).
func (s *codexTokenSession) Stop() {
	s.event(map[string]any{"type": "task_complete", "turn_id": s.turnID, "last_agent_message": "done"})
	s.turnHook("stop", "Stop", map[string]any{"stop_hook_active": false, "last_assistant_message": "done"})
	if s.headless {
		s.SessionEnd()
	}
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

// newCopilotHeadlessTokenDriver is `copilot -p`: one process per prompt.
// userPromptSubmitted fires before sessionStart, and session.shutdown is
// appended after the agent-stop and session-end hooks of every process.
func newCopilotHeadlessTokenDriver() copilotTokenDriver {
	d := newCopilotTokenDriver()
	d.name, d.headless = "copilot-cli-p", true
	return d
}

func (copilotTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_COPILOT_SESSION_DIR="+h.agentDir)
}

func (d copilotTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("copilot", h.t.Name(), h.nextSessionNumber())
	s := &copilotTokenSession{unsupportedSession: unsupportedSession{"copilot-cli"}, h: h, id: id, headless: d.headless,
		path: filepath.Join(h.agentDir, id, "events.jsonl")}
	if !d.headless {
		s.startProcess("")
	}
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
	headless   bool
	started    bool
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

// startProcess writes session.start (or session.resume) and fires
// sessionStart, as a new copilot process does.
func (s *copilotTokenSession) startProcess(prompt string) {
	source := "new"
	if s.started {
		source = "resume"
		s.event("session.resume", map[string]any{"eventCount": s.seq, "selectedModel": "claude-test",
			"context": map[string]any{"cwd": s.h.sessionDir(s.id)}})
	} else {
		s.event("session.start", map[string]any{"sessionId": s.id, "version": 1, "producer": "copilot-agent",
			"copilotVersion": "1.0.97", "selectedModel": "claude-test", "context": map[string]any{"cwd": s.h.sessionDir(s.id)}})
	}
	s.started = true
	extra := map[string]any{"source": source}
	if prompt != "" {
		extra["initialPrompt"] = prompt
	}
	s.hook("session-start", extra)
}

func (s *copilotTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submitted", map[string]any{"prompt": prompt})
	if s.headless {
		// copilot -p: the prompt hook fires before the process's sessionStart.
		s.startProcess(prompt)
	}
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

// Stop fires agent-stop; a headless process then exits.
func (s *copilotTokenSession) Stop() {
	s.hook("agent-stop", map[string]any{"transcriptPath": s.path, "stopReason": "end_turn", "stop_hook_active": false})
	if s.headless {
		s.SessionEnd()
	}
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

// --- Factory AI Droid: no usage in the transcript, a cumulative sidecar ---

type droidTokenDriver struct{ tokenDriverBase }

// newDroidTokenDriver lacks capTranscriptTokens: Droid's tokens are in its
// settings.json, not its transcript.
func newDroidTokenDriver() droidTokenDriver {
	return droidTokenDriver{tokenDriverBase{name: "factoryai-droid", agent: "factoryai-droid",
		caps: []tokenCap{capAttach, capSessionEnd, capSubagent, capMidTurnTokens}}}
}

// newDroidExecTokenDriver is `droid exec`: one process per prompt, each firing
// SessionStart (startup, then resume), the user-prompt-submit alias, Stop and
// SessionEnd. The real UserPromptSubmit never fires.
func newDroidExecTokenDriver() droidTokenDriver {
	d := newDroidTokenDriver()
	d.name, d.headless = "factoryai-droid-exec", true
	return d
}

// Setup lays out ~/.factory/sessions: a session's transcript is in its
// project dir, a Worker's at the sessions root.
func (droidTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = filepath.Join(realDir(h), "sessions", "-tmp-repo")
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_DROID_PROJECT_DIR="+h.agentDir)
}

func (d droidTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("droid", h.t.Name(), h.nextSessionNumber())
	s := &droidTokenSession{unsupportedSession: unsupportedSession{"factoryai-droid"}, h: h, id: id, headless: d.headless,
		path: filepath.Join(h.agentDir, id+".jsonl")}
	appendJSONLines(h, s.path, map[string]any{"type": "session_start", "id": id, "title": "task", "sessionTitle": "task",
		"owner": "tester", "version": 2, "cwd": h.sessionDir(id)})
	if !d.headless {
		s.startProcess()
	}
	return s
}

// droidTokenSession fakes Factory AI Droid 0.82.0. Transcript lines are
// {"type":"message","id","timestamp","message":{role,content},"parentId"}
// after a session_start line; assistant messages carry openaiMessageId and no
// usage at all. The binary keeps the session's cumulative tokenUsage
// (inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens,
// thinkingTokens) in <session>.settings.json next to the transcript; the
// BYOK capture had none there, so whether outputTokens includes
// thinkingTokens is unverified and the driver spends no thinking.
//
// Hook payloads carry snake_case and camelCase copies of every field and no
// model. Each process fires SessionStart and, because Entire also installs
// user-prompt-submit on SessionStart, the same payload again as
// user-prompt-submit. The interactive driver's real UserPromptSubmit (with
// the prompt) was not captured.
type droidTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	headless bool
	running  bool
	started  bool
	seq      int
	parent   string
	// spent is the session's cumulative usage, which settings.json reports.
	spent tokenCounts
}

func (s *droidTokenSession) ID() string             { return s.id }
func (s *droidTokenSession) TranscriptPath() string { return s.path }

// droidPayload is a Droid hook payload: each field in snake_case and again in
// camelCase.
func droidPayload(fields map[string]any) map[string]any {
	out := make(map[string]any, 2*len(fields))
	for k, v := range fields {
		out[k] = v
		parts := strings.Split(k, "_")
		for i := 1; i < len(parts); i++ {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
		out[strings.Join(parts, "")] = v
	}
	return out
}

func droidHook(h *tokenHarness, sessionID, transcript, name, event string, extra map[string]any) {
	in := droidPayload(mergeMaps(map[string]any{"session_id": sessionID, "transcript_path": transcript,
		"cwd": h.sessionDir(sessionID), "permission_mode": "off", "hook_event_name": event}, extra))
	hookCall(h, h.sessionDir(sessionID), "factoryai-droid", name, in)
}

func (s *droidTokenSession) hook(name, event string, extra map[string]any) {
	droidHook(s.h, s.id, s.path, name, event, extra)
}

// startProcess is a droid process starting on the session: SessionStart and
// its user-prompt-submit alias.
func (s *droidTokenSession) startProcess() {
	source := "startup"
	if s.started {
		source = "resume"
	}
	s.started, s.running = true, true
	extra := map[string]any{"source": source}
	s.hook("session-start", "SessionStart", extra)
	s.hook("user-prompt-submit", "SessionStart", extra)
}

func (s *droidTokenSession) message(role string, content []any, extra map[string]any) {
	s.seq++
	id := fakeUUID(s.id, "message", s.seq)
	line := map[string]any{"type": entryTypeMessage, "id": id, "timestamp": nowMillis(),
		"message": mergeMaps(map[string]any{"role": role, "content": content}, extra)}
	if s.parent != "" {
		line["parentId"] = s.parent
	}
	appendJSONLines(s.h, s.path, line)
	s.parent = id
}

// assistant writes an API call's message and adds its usage to the
// settings.json total.
func (s *droidTokenSession) assistant(u tokenCounts, content ...any) {
	s.message(roleAssistant, content, map[string]any{"openaiMessageId": fmt.Sprintf("oai%s%d", s.id[:8], s.seq)})
	s.spent = s.spent.plus(u)
	writeDroidSettings(s.h, s.path, s.headless, s.spent)
}

// writeDroidSettings rewrites <session>.settings.json with the cumulative usage.
func writeDroidSettings(h *tokenHarness, transcript string, exec bool, u tokenCounts) {
	tags := []any{}
	if exec {
		tags = []any{map[string]any{"name": "exec"}}
	}
	writeJSONFile(h, strings.TrimSuffix(transcript, ".jsonl")+".settings.json", map[string]any{
		"assistantActiveTimeMs": 0, "model": "droid-test", "reasoningEffort": "none", "interactionMode": "auto",
		"autonomyLevel": "off", "autonomyMode": "normal", "tags": tags, "providerLock": "generic-chat-completion-api",
		"tokenUsage": map[string]any{"inputTokens": u.In, "outputTokens": u.Out, "cacheCreationTokens": u.CacheWrite,
			"cacheReadTokens": u.CacheRead, "thinkingTokens": 0},
	})
}

func (s *droidTokenSession) StartTurn(prompt string) {
	if !s.running {
		s.startProcess()
	}
	if !s.headless {
		s.hook("user-prompt-submit", "UserPromptSubmit", map[string]any{"prompt": prompt})
	}
	s.message(roleUser, []any{
		map[string]any{"type": blockTypeText, "text": "<system-reminder>\nUser system info\n</system-reminder>"},
		map[string]any{"type": blockTypeText, "text": prompt},
	}, nil)
}

func (s *droidTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	toolID := fmt.Sprintf("function-call-%s%d", s.id[:8], s.seq)
	name, input := "Execute", map[string]any{"command": tool.command, "riskLevel": "medium", "riskLevelReason": "commit"}
	if tool.path != "" {
		name, input = "Create", map[string]any{"file_path": tool.path, "content": "x"}
	}
	s.assistant(u, map[string]any{"type": blockTypeToolUse, "id": toolID, "name": name, "input": input})
	run()
	s.message(roleUser, []any{map[string]any{"type": blockTypeToolResult, "tool_use_id": toolID, "content": "ok"}}, nil)
}

func (s *droidTokenSession) Spend(u tokenCounts) {
	s.assistant(u, map[string]any{"type": blockTypeText, "text": "working"})
}

// SpendSubagent runs a Task Worker as Droid does: a session of its own whose
// transcript sits at the sessions root, its session_start naming the calling
// session and tool call (and no cwd), with its own hooks (SessionStart
// carrying calling_session_id) and settings.json usage. The project dir gets
// only a session_end stub for it. The parent's Pre/PostToolUse carry no
// tool_use_id, and the Task result is a plain string with no agent ID.
func (s *droidTokenSession) SpendSubagent(agentID string, u tokenCounts) {
	toolID := fmt.Sprintf("function-call-%s%d", s.id[:8], s.seq)
	input := map[string]any{"subagent_type": "worker", "description": "help", "prompt": "help"}
	s.message(roleAssistant, []any{map[string]any{"type": blockTypeToolUse, "id": toolID, "name": "Task", "input": input}},
		map[string]any{"openaiMessageId": "oai-" + agentID})
	s.hook("pre-tool-use", "PreToolUse", map[string]any{"tool_name": "Task", "tool_input": input})

	worker := s.workerID(agentID)
	s.h.subagentSessions[worker] = true
	path := s.workerPath(agentID)
	appendJSONLines(s.h, path, map[string]any{"type": "session_start", "id": worker, "title": "Task Tool Invocation",
		"sessionTitle": "worker: help", "isSessionTitleManuallySet": true, "owner": "tester",
		"callingSessionId": s.id, "callingToolUseId": toolID, "version": 2})
	start := map[string]any{"source": "startup", "calling_session_id": s.id}
	droidHook(s.h, worker, path, "session-start", "SessionStart", start)
	droidHook(s.h, worker, path, "user-prompt-submit", "SessionStart", start)
	appendJSONLines(s.h, path,
		map[string]any{"type": entryTypeMessage, "id": worker + "-u", "timestamp": nowMillis(),
			"message": map[string]any{"role": roleUser, "content": []any{map[string]any{"type": blockTypeText, "text": "help"}}}},
		map[string]any{"type": entryTypeMessage, "id": worker + "-a", "timestamp": nowMillis(), "parentId": worker + "-u",
			"message": map[string]any{"role": roleAssistant, "openaiMessageId": "oai-" + worker,
				"content": []any{map[string]any{"type": blockTypeText, "text": "sub work"}}}})
	writeDroidSettings(s.h, path, true, u)
	droidHook(s.h, worker, path, "stop", "Stop", map[string]any{"stop_hook_active": false, "tool_execution_count": 0})
	droidHook(s.h, worker, path, "session-end", "SessionEnd", map[string]any{"reason": "other"})
	appendJSONLines(s.h, filepath.Join(s.h.agentDir, worker+".jsonl"), map[string]any{"type": "session_end",
		"timestamp": nowMillis(), "toolCount": 0, "finalText": "sub work"})

	s.message(roleUser, []any{map[string]any{"type": blockTypeToolResult, "tool_use_id": toolID, "content": "sub work"}}, nil)
	s.hook("post-tool-use", "PostToolUse", map[string]any{"tool_name": "Task", "tool_input": input,
		"tool_response": "sub work"})
}

func (s *droidTokenSession) workerID(agentID string) string { return fakeUUID(s.id, "worker", agentID) }

func (s *droidTokenSession) workerPath(agentID string) string {
	return filepath.Join(filepath.Dir(s.h.agentDir), s.workerID(agentID)+".jsonl")
}

func (s *droidTokenSession) DeleteSubagentTranscript(agentID string) {
	require.NoError(s.h.t, os.Remove(s.workerPath(agentID)))
}

// Stop fires Stop; a `droid exec` process then exits.
func (s *droidTokenSession) Stop() {
	s.hook("stop", "Stop", map[string]any{"stop_hook_active": false, "tool_execution_count": 0})
	if s.headless {
		s.SessionEnd()
	}
}

func (s *droidTokenSession) SessionEnd() {
	s.hook("session-end", "SessionEnd", map[string]any{"reason": "other", "message_count": s.seq})
	s.running = false
}

// --- OpenCode: the export, with each API call's tokens filled in at step end ---

type openCodeTokenDriver struct{ tokenDriverBase }

func newOpenCodeTokenDriver() openCodeTokenDriver {
	return openCodeTokenDriver{tokenDriverBase{name: "opencode", agent: "opencode",
		caps: []tokenCap{capAttach, capSessionEnd, capMidTurnTokens}}}
}

// newOpenCodeRunTokenDriver is `opencode run`: one process per prompt, each
// firing session-start, turn-start and turn-end, and never session-end.
func newOpenCodeRunTokenDriver() openCodeTokenDriver {
	d := newOpenCodeTokenDriver()
	d.name, d.headless = "opencode-run", true
	d.caps = []tokenCap{capAttach, capMidTurnTokens}
	return d
}

func (openCodeTokenDriver) Setup(h *tokenHarness) {
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_OPENCODE_MOCK_EXPORT=1")
}

func (d openCodeTokenDriver) NewSession(h *tokenHarness) tokenSession {
	sum := sha256.Sum256([]byte(fmt.Sprint("opencode", h.t.Name(), h.nextSessionNumber())))
	id := "ses_" + hex.EncodeToString(sum[:13])
	s := &openCodeTokenSession{unsupportedSession: unsupportedSession{"opencode"}, h: h, id: id, headless: d.headless,
		path: filepath.Join(h.env.OpenCodeProjectDir, id+".json")}
	s.write()
	if !d.headless {
		s.startProcess()
	}
	return s
}

// openCodeTokenSession fakes OpenCode 1.18.33. Every API call is one
// assistant message whose info.tokens {total, input, output, reasoning,
// cache{read, write}} is repeated in its step-finish part. Input excludes
// cache reads, reasoning is separate from output, and total is their sum.
// The message exists with zero tokens while its tool runs: the tokens are
// filled in at step end, after the tool finishes. The plugin fires
// turn-start with an empty prompt and model, and turn-end with the model;
// `opencode run` never fires session-end, and a turn whose API call fails
// fires turn-end twice (not modelled). The interactive (TUI) driver was not
// captured and keeps the plugin's session-end.
type openCodeTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	headless bool
	running  bool
	messages []*openCodeMessage
	userID   string
	parts    int
	// spent is the session's cumulative usage, for info.tokens.
	spent *openCodeUsage
}

// openCodeMessage is one export message: info and parts.
type openCodeMessage struct {
	Info    map[string]any `json:"info"`
	Parts   []any          `json:"parts"`
	created int
}

// openCodeUsage is usage in OpenCode's terms: reasoning apart from output.
type openCodeUsage struct{ input, output, reasoning, read, write int }

func (u openCodeUsage) plus(o openCodeUsage) openCodeUsage {
	return openCodeUsage{u.input + o.input, u.output + o.output, u.reasoning + o.reasoning, u.read + o.read, u.write + o.write}
}

// render is the tokens object; total is the sum of every field.
func (u openCodeUsage) render() map[string]any {
	return map[string]any{"total": u.input + u.output + u.reasoning + u.read + u.write, "input": u.input,
		"output": u.output, "reasoning": u.reasoning, "cache": map[string]any{"read": u.read, "write": u.write}}
}

func (s *openCodeTokenSession) ID() string             { return s.id }
func (s *openCodeTokenSession) TranscriptPath() string { return s.path }

func (s *openCodeTokenSession) hook(name string, extra map[string]any) {
	hookCall(s.h, s.h.sessionDir(s.id), "opencode", name, mergeMaps(map[string]any{"session_id": s.id}, extra))
}

func (s *openCodeTokenSession) startProcess() {
	s.running = true
	s.hook("session-start", nil)
}

// write stores the export where `opencode export` would produce it, and
// the copy the CLI reads in mock-export mode: an export at any moment shows
// the session as it stands, including a call still waiting on its tool.
func (s *openCodeTokenSession) write() {
	info := map[string]any{"id": s.id, "slug": "test-session", "directory": s.h.sessionDir(s.id), "path": "",
		"title": "test", "agent": "build", "version": "1.18.33", "cost": 0,
		"model": map[string]any{"id": "opencode-test", "providerID": "openai", "variant": "default"}}
	if s.spent != nil {
		info["tokens"] = s.spent.render()
	}
	export := map[string]any{"info": info, "messages": s.messages}
	writeJSONFile(s.h, s.path, export)
	writeJSONFile(s.h, filepath.Join(s.h.sessionDir(s.id), ".entire", "tmp", s.id+".json"), export)
}

func (s *openCodeTokenSession) part(msgID string, fields map[string]any) map[string]any {
	s.parts++
	return mergeMaps(map[string]any{"id": fmt.Sprintf("prt_%s%04d", s.id[4:12], s.parts), "sessionID": s.id,
		"messageID": msgID}, fields)
}

// openCodeTokens renders usage as OpenCode does. With h.reasoning, a third
// of output is reasoning, reported apart from output.
func (s *openCodeTokenSession) openCodeTokens(u tokenCounts) openCodeUsage {
	reasoning := 0
	if s.h.reasoning {
		reasoning = u.Out / 3
	}
	return openCodeUsage{input: u.In, output: u.Out - reasoning, reasoning: reasoning, read: u.CacheRead, write: u.CacheWrite}
}

func (s *openCodeTokenSession) StartTurn(prompt string) {
	if !s.running {
		s.startProcess()
	}
	n := len(s.messages) + 1
	s.userID = fmt.Sprintf("msg_%s%04d", s.id[4:12], n)
	s.messages = append(s.messages, &openCodeMessage{
		Info: map[string]any{"id": s.userID, "sessionID": s.id, "role": roleUser, "agent": "build",
			"time":  map[string]any{"created": 1792000000000 + n},
			"model": map[string]any{"providerID": "openai", "modelID": "opencode-test"}},
		Parts: []any{s.part(s.userID, map[string]any{"type": blockTypeText, "text": prompt})},
	})
	s.write()
	s.hook("turn-start", map[string]any{"prompt": "", "model": ""})
}

// call appends one API call's message with zero tokens; finish fills in its
// tokens and step-finish part.
func (s *openCodeTokenSession) call(parts func(msgID string) []any) (*openCodeMessage, string) {
	n := len(s.messages) + 1
	msgID := fmt.Sprintf("msg_%s%04d", s.id[4:12], n)
	all := []any{s.part(msgID, map[string]any{"type": "step-start", "snapshot": "0000000"})}
	if s.h.reasoning {
		all = append(all, s.part(msgID, map[string]any{"type": "reasoning", "text": "thinking"}))
	}
	msg := &openCodeMessage{created: 1792000000000 + n, Parts: append(all, parts(msgID)...)}
	msg.Info = map[string]any{"id": msgID, "sessionID": s.id, "parentID": s.userID, "role": roleAssistant,
		"mode": "build", "agent": "build", "cost": 0, "tokens": openCodeUsage{}.render(), "modelID": "opencode-test",
		"providerID": "openai", "path": map[string]any{"cwd": s.h.sessionDir(s.id), "root": s.h.sessionDir(s.id)},
		"time": map[string]any{"created": msg.created}}
	s.messages = append(s.messages, msg)
	s.write()
	return msg, msgID
}

func (s *openCodeTokenSession) finish(msg *openCodeMessage, msgID, reason string, u tokenCounts, extra ...any) {
	usage := s.openCodeTokens(u)
	spent := usage
	if s.spent != nil {
		spent = s.spent.plus(usage)
	}
	s.spent = &spent
	tokens := usage.render()
	msg.Info["tokens"], msg.Info["finish"] = tokens, reason
	msg.Info["time"] = map[string]any{"created": msg.created, "completed": msg.created + 1}
	msg.Parts = append(append(msg.Parts, s.part(msgID, map[string]any{"type": "step-finish",
		"reason": reason, "snapshot": "0000000", "tokens": tokens, "cost": 0})), extra...)
	s.write()
}

// ToolCall writes the call's message with zero tokens, runs the tool, and only
// then fills in the tokens, as OpenCode does at step end.
func (s *openCodeTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	callID := fmt.Sprintf("call_%s%04d", s.id[4:12], len(s.messages)+1)
	name, state := "bash", map[string]any{"status": "running", "input": map[string]any{"command": tool.command}}
	if tool.path != "" {
		name, state = "apply_patch", map[string]any{"status": "running",
			"input": map[string]any{"patchText": "*** Begin Patch\n*** Add File: " + tool.path + "\n+x\n*** End Patch"}}
	}
	msg, msgID := s.call(func(msgID string) []any {
		return []any{s.part(msgID, map[string]any{"type": "tool", "tool": name, "callID": callID, "state": state})}
	})
	run()
	state["status"], state["output"], state["title"] = "completed", "ok", "ok"
	var extra []any
	if tool.path != "" {
		abs := filepath.Join(s.h.sessionDir(s.id), tool.path)
		state["metadata"] = map[string]any{"diff": "", "files": []any{map[string]any{"filePath": abs, "relativePath": tool.path,
			"type": "add", "additions": 1, "deletions": 0}}, "diagnostics": map[string]any{}, "truncated": false}
		extra = append(extra, s.part(msgID, map[string]any{"type": "patch", "hash": "0000000", "files": []string{abs}}))
	}
	s.finish(msg, msgID, "tool-calls", u, extra...)
}

func (s *openCodeTokenSession) Spend(u tokenCounts) {
	msg, msgID := s.call(func(msgID string) []any {
		return []any{s.part(msgID, map[string]any{"type": blockTypeText, "text": "working"})}
	})
	s.finish(msg, msgID, "stop", u)
}

// Stop fires turn-end; an `opencode run` process then exits without
// session-end.
func (s *openCodeTokenSession) Stop() {
	s.hook("turn-end", map[string]any{"model": "opencode-test"})
	if s.headless {
		s.running = false
	}
}

func (s *openCodeTokenSession) SessionEnd() {
	s.hook("session-end", nil)
	s.running = false
}

// --- Pi: session JSONL with a parentId chain ---

type piTokenDriver struct{ tokenDriverBase }

func newPiTokenDriver() piTokenDriver {
	return piTokenDriver{tokenDriverBase{name: "pi", agent: "pi",
		caps: []tokenCap{capAttach, capSessionEnd, capTranscriptTokens, capMidTurnTokens}}}
}

// newPiHeadlessTokenDriver is `pi -p`: one process per prompt, each firing
// session_start, before_agent_start, agent_end and session_shutdown.
func newPiHeadlessTokenDriver() piTokenDriver {
	d := newPiTokenDriver()
	d.name, d.headless = "pi-p", true
	return d
}

func (piTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_PI_SESSION_DIR="+h.agentDir)
}

func (d piTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("pi", h.t.Name(), h.nextSessionNumber())
	s := &piTokenSession{unsupportedSession: unsupportedSession{"pi"}, h: h, id: id, headless: d.headless,
		path: filepath.Join(h.agentDir, "2026-01-01T00-00-00-000Z_"+id+".jsonl")}
	appendJSONLines(h, s.path, map[string]any{"type": "session", "version": 3, "id": id,
		"timestamp": "2026-01-01T00:00:00.000Z", "cwd": h.sessionDir(id)})
	s.entry("model_change", map[string]any{"provider": "google", "modelId": "pi-test"})
	s.entry("thinking_level_change", map[string]any{"thinkingLevel": "medium"})
	if !d.headless {
		s.startProcess()
	}
	return s
}

// piTokenSession fakes Pi 0.70.2. After the session header come
// model_change and thinking_level_change entries, then message entries
// (user, assistant, toolResult) chained by parentId. Each assistant message
// carries usage {input, output, cacheRead, cacheWrite, totalTokens, cost},
// written when the response arrives, before its tool runs. Hooks:
// session_start, before_agent_start (prompt, skill_events), agent_end, and
// session_shutdown with only {type}, which Entire ignores (it can race ahead
// of agent_end; the driver sends it after).
type piTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	headless bool
	running  bool
	seq      int
	parent   any
}

func (s *piTokenSession) ID() string             { return s.id }
func (s *piTokenSession) TranscriptPath() string { return s.path }

func (s *piTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"type": name, "cwd": s.h.sessionDir(s.id), "session_file": s.path}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "pi", name, in)
}

func (s *piTokenSession) startProcess() {
	s.running = true
	s.hook("session_start", nil)
}

func (s *piTokenSession) entry(typ string, fields map[string]any) {
	s.seq++
	id := fakeUUID(s.id, "entry", s.seq)[:8]
	appendJSONLines(s.h, s.path, mergeMaps(map[string]any{"type": typ, "id": id, "parentId": s.parent,
		"timestamp": nowMillis()}, fields))
	s.parent = id
}

func (s *piTokenSession) message(m map[string]any) {
	s.entry(entryTypeMessage, map[string]any{"message": mergeMaps(map[string]any{"timestamp": time.Now().UnixMilli()}, m)})
}

func piUsage(u tokenCounts) map[string]any {
	return map[string]any{"input": u.In, "output": u.Out, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite,
		"totalTokens": u.total(), "cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}
}

func (s *piTokenSession) assistant(u tokenCounts, stopReason string, content ...any) {
	s.message(map[string]any{"role": roleAssistant, "content": content, "api": "google-generative-ai",
		"provider": "google", "model": "pi-test", "usage": piUsage(u), "stopReason": stopReason,
		"responseId": fmt.Sprintf("resp%d", s.seq)})
}

func (s *piTokenSession) StartTurn(prompt string) {
	if !s.running {
		s.startProcess()
	}
	s.hook("before_agent_start", map[string]any{"prompt": prompt, "skill_events": []any{}})
	s.message(map[string]any{"role": roleUser, "content": []any{map[string]any{"type": blockTypeText, "text": prompt}}})
}

func (s *piTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	callID := fmt.Sprintf("bash_%d", s.seq)
	name, args := "bash", map[string]any{"command": tool.command}
	if tool.path != "" {
		callID, name, args = fmt.Sprintf("write_%d", s.seq), "write", map[string]any{"path": tool.path, "content": "x"}
	}
	s.assistant(u, "toolUse", map[string]any{"type": "thinking", "thinking": "plan"},
		map[string]any{"type": "toolCall", "id": callID, "name": name, "arguments": args})
	run()
	s.message(map[string]any{"role": "toolResult", "toolCallId": callID, "toolName": name,
		"content": []any{map[string]any{"type": blockTypeText, "text": "ok"}}, "isError": false})
}

func (s *piTokenSession) Spend(u tokenCounts) {
	s.assistant(u, "stop", map[string]any{"type": blockTypeText, "text": "working"})
}

// Stop fires agent_end; a `pi -p` process then shuts down.
func (s *piTokenSession) Stop() {
	s.hook("agent_end", nil)
	if s.headless {
		s.SessionEnd()
	}
}

// SessionEnd is the process exiting: session_shutdown carries only its type.
func (s *piTokenSession) SessionEnd() {
	hookCall(s.h, s.h.sessionDir(s.id), "pi", "session_shutdown", map[string]any{"type": "session_shutdown"})
	s.running = false
}

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
