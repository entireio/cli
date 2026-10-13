//go:build integration

package integration

import (
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
	id := fmt.Sprintf("cursor-tok-%d", h.nextSessionNumber())
	s := &cursorTokenSession{unsupportedSession: unsupportedSession{"cursor"}, h: h, id: id,
		path: filepath.Join(h.agentDir, id, id+".jsonl")}
	s.hook("session-start", map[string]any{"model": "cursor-default"})
	return s
}

type cursorTokenSession struct {
	unsupportedSession

	h       *tokenHarness
	id      string
	path    string
	pending tokenCounts
	turn    int
}

func (s *cursorTokenSession) ID() string             { return s.id }
func (s *cursorTokenSession) TranscriptPath() string { return s.path }

func (s *cursorTokenSession) hook(name string, extra map[string]any) {
	in := map[string]any{"conversation_id": s.id, "transcript_path": s.path}
	for k, v := range extra {
		in[k] = v
	}
	hookCall(s.h, s.h.sessionDir(s.id), "cursor", name, in)
}

func (s *cursorTokenSession) StartTurn(prompt string) {
	s.turn++
	s.pending = tokenCounts{}
	appendJSONLines(s.h, s.path, map[string]any{"type": roleUser, "text": prompt})
	s.hook("before-submit-prompt", map[string]any{"prompt": prompt})
}

func (s *cursorTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	appendJSONLines(s.h, s.path, map[string]any{"type": roleAssistant, "text": "wrote " + path})
}

func (s *cursorTokenSession) Spend(u tokenCounts) {
	s.pending = s.pending.plus(u)
	appendJSONLines(s.h, s.path, map[string]any{"type": roleAssistant, "text": "ok"})
}

// Stop reports the turn's tokens; Cursor reports input including cache reads
// and writes. A repeated Stop reports the turn's running total again.
func (s *cursorTokenSession) Stop() {
	u := s.pending
	s.hook("stop", map[string]any{
		"model": "cursor-default", "loop_count": 1,
		"input_tokens":      u.In + u.CacheRead + u.CacheWrite,
		"output_tokens":     u.Out,
		"cache_read_tokens": u.CacheRead, "cache_write_tokens": u.CacheWrite,
	})
}

func (s *cursorTokenSession) SessionEnd() { s.hook("session-end", nil) }

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

// Codex rollouts have no cache-write counter.
func (codexTokenDriver) Expressible(u tokenCounts) tokenCounts {
	u.CacheWrite = 0
	return u
}

func (codexTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("019a0000-0000-7000-8000-%012d", h.nextSessionNumber())
	s := &codexTokenSession{unsupportedSession: unsupportedSession{"codex"}, h: h, id: id,
		path: filepath.Join(h.agentDir, "rollout-2026-01-01T00-00-00-"+id+".jsonl")}
	appendJSONLines(h, s.path, map[string]any{"type": "session_meta", "payload": map[string]any{
		"id": id, "thread_source": "user", "timestamp": "2026-01-01T00:00:00Z", "cwd": h.sessionDir(id),
	}})
	return s
}

type codexTokenSession struct {
	unsupportedSession

	h                     *tokenHarness
	id, path              string
	turn                  int
	input, cached, output int
}

func (s *codexTokenSession) ID() string             { return s.id }
func (s *codexTokenSession) TranscriptPath() string { return s.path }

func (s *codexTokenSession) hook(name, event string, extra map[string]any) {
	in := map[string]any{
		"session_id": s.id, "transcript_path": s.path, "cwd": s.h.sessionDir(s.id), "model": "gpt-5",
		"permission_mode": "default", "hook_event_name": event, "turn_id": fmt.Sprintf("turn-%d", s.turn),
	}
	for k, v := range extra {
		in[k] = v
	}
	hookCall(s.h, s.h.sessionDir(s.id), "codex", name, in)
}

func (s *codexTokenSession) StartTurn(prompt string) {
	s.turn++
	s.hook("user-prompt-submit", "UserPromptSubmit", map[string]any{"prompt": prompt})
	appendJSONLines(s.h, s.path, map[string]any{"type": "event_msg", "payload": map[string]any{
		"type": "user_message", "message": prompt}})
}

func (s *codexTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	appendJSONLines(s.h, s.path, map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "custom_tool_call", "status": "completed", "name": "apply_patch",
		"input": "*** Begin Patch\n*** Add File: " + path + "\n+x\n*** End Patch",
	}})
}

// Spend appends a cumulative snapshot: Codex input includes cached input.
func (s *codexTokenSession) Spend(u tokenCounts) {
	s.input += u.In + u.CacheRead
	s.cached += u.CacheRead
	s.output += u.Out
	appendJSONLines(s.h, s.path, map[string]any{"type": "event_msg", "payload": map[string]any{
		"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{
			"input_tokens": s.input, "cached_input_tokens": s.cached, "output_tokens": s.output,
			"total_tokens": s.input + s.output,
		}},
	}})
}

// SpendSubagent runs a spawned Codex thread: its own rollout (session_meta id
// = agent id) with one terminal turn, announced by subagent-start/stop.
func (s *codexTokenSession) SpendSubagent(agentID string, u tokenCounts) {
	childID := codexChildID(agentID)
	turn := fmt.Sprintf("turn-%d", s.turn)
	s.hook("subagent-start", "SubagentStart", map[string]any{"agent_id": childID, "agent_type": "worker"})
	child := s.childPath(agentID)
	appendJSONLines(s.h, child,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": childID, "thread_source": "subagent",
			"timestamp": "2026-01-01T00:00:01Z", "cwd": s.h.sessionDir(s.id)}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": turn}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{
			"total_token_usage": map[string]any{"input_tokens": u.In + u.CacheRead, "cached_input_tokens": u.CacheRead,
				"output_tokens": u.Out, "total_tokens": u.In + u.CacheRead + u.Out}}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": turn}},
	)
	s.hook("subagent-stop", "SubagentStop", map[string]any{"agent_id": childID, "agent_type": "worker",
		"agent_transcript_path": child})
}

func (s *codexTokenSession) childPath(agentID string) string {
	return filepath.Join(s.h.agentDir, "rollout-2026-01-01T00-00-01-"+codexChildID(agentID)+".jsonl")
}

func (s *codexTokenSession) DeleteSubagentTranscript(agentID string) {
	require.NoError(s.h.t, os.Remove(s.childPath(agentID)))
}

// codexChildID turns the harness's agent ID into a UUID-shaped thread ID.
func codexChildID(agentID string) string {
	return "019b0000-0000-7000-8000-" + fmt.Sprintf("%012x", len(agentID)*7919+int(agentID[len(agentID)-1]))
}

func (s *codexTokenSession) Stop() {
	s.hook("stop", "Stop", map[string]any{"stop_hook_active": false, "last_assistant_message": "done"})
}

func (s *codexTokenSession) SessionEnd() { s.hook("session-end", "SessionEnd", nil) }

// --- Copilot CLI: per-message output tokens in events.jsonl ---

type copilotTokenDriver struct{ tokenDriverBase }

func newCopilotTokenDriver() copilotTokenDriver {
	return copilotTokenDriver{tokenDriverBase{name: "copilot-cli", agent: "copilot-cli",
		caps: []tokenCap{capAttach, capSessionEnd, capTranscriptTokens, capMidTurnTokens}}}
}

func (copilotTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv, "ENTIRE_TEST_COPILOT_SESSION_DIR="+h.agentDir)
}

// While the session runs, Copilot's events carry only output tokens per
// message; input and cache counts appear only in the session.shutdown
// aggregate. The driver spends what the live transcript can show.
func (copilotTokenDriver) Expressible(u tokenCounts) tokenCounts {
	return tokenCounts{Out: u.Out}
}

func (copilotTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("copilot-tok-%d", h.nextSessionNumber())
	s := &copilotTokenSession{unsupportedSession: unsupportedSession{"copilot-cli"}, h: h, id: id,
		path: filepath.Join(h.agentDir, id, "events.jsonl")}
	s.event("session.start", map[string]any{"sessionId": id})
	return s
}

type copilotTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
	seq      int
	output   int
	calls    int
}

func (s *copilotTokenSession) ID() string             { return s.id }
func (s *copilotTokenSession) TranscriptPath() string { return s.path }

func (s *copilotTokenSession) event(typ string, data map[string]any) {
	s.seq++
	appendJSONLines(s.h, s.path, map[string]any{"type": typ, "data": data, "id": strconv.Itoa(s.seq),
		"timestamp": time.Now().UTC().Format(time.RFC3339), "parentId": strconv.Itoa(s.seq - 1)})
}

func (s *copilotTokenSession) hook(name string, extra map[string]any) {
	in := map[string]any{"sessionId": s.id, "transcriptPath": s.path, "cwd": s.h.sessionDir(s.id),
		"timestamp": time.Now().UnixMilli()}
	for k, v := range extra {
		in[k] = v
	}
	hookCall(s.h, s.h.sessionDir(s.id), "copilot-cli", name, in)
}

func (s *copilotTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submitted", map[string]any{"prompt": prompt})
	s.event("user.message", map[string]any{"content": prompt})
}

func (s *copilotTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	files, err := json.Marshal([]string{path})
	require.NoError(s.h.t, err)
	s.event("tool.execution_complete", map[string]any{"toolCallId": fmt.Sprintf("tc%d", s.seq),
		"model": "claude-sonnet-4.6", "toolTelemetry": map[string]any{"properties": map[string]any{"filePaths": string(files)}}})
}

func (s *copilotTokenSession) Spend(u tokenCounts) {
	s.output += u.Out
	s.calls++
	s.event("assistant.message", map[string]any{"content": "ok", "outputTokens": u.Out})
}

func (s *copilotTokenSession) Stop() { s.hook("agent-stop", nil) }

// SessionEnd writes the shutdown aggregate Copilot appends on exit, holding
// the same output total the per-message events already show.
func (s *copilotTokenSession) SessionEnd() {
	s.event("session.shutdown", map[string]any{"modelMetrics": map[string]any{"claude-sonnet-4.6": map[string]any{
		"requests": map[string]any{"count": s.calls},
		"usage":    map[string]any{"inputTokens": 0, "outputTokens": s.output, "cacheReadTokens": 0, "cacheWriteTokens": 0},
	}}})
	s.hook("session-end", nil)
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
	in := map[string]any{"session_id": s.id, "transcript_path": s.path, "model": "droid-test"}
	for k, v := range extra {
		in[k] = v
	}
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

func (s *droidTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	toolID := fmt.Sprintf("toolu_%s_%d", s.id, s.seq)
	s.message(roleAssistant, "", []any{map[string]any{"type": blockTypeToolUse, "id": toolID, "name": "Write",
		"input": map[string]any{"file_path": path, "content": "x"}}}, nil)
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
	in := map[string]any{"session_id": s.id, "model": "opencode-test"}
	for k, v := range extra {
		in[k] = v
	}
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

func (s *openCodeTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	s.add(roleAssistant, []any{map[string]any{"type": "tool", "tool": "write", "callID": "call-" + path,
		"state": map[string]any{"status": "completed", "input": map[string]any{"filePath": path}, "output": "ok"}}}, nil)
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
	in := map[string]any{"type": name, "cwd": s.h.sessionDir(s.id), "session_file": s.path}
	for k, v := range extra {
		in[k] = v
	}
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

func (s *piTokenSession) StartTurn(prompt string) {
	s.hook("before_agent_start", map[string]any{"prompt": prompt})
	s.message(map[string]any{"role": roleUser, "content": []any{map[string]any{"type": blockTypeText, "text": prompt}}})
}

func (s *piTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	s.message(map[string]any{"role": roleAssistant, "model": "pi-test", "content": []any{map[string]any{
		"type": "toolCall", "id": "tc-" + path, "name": "write", "arguments": map[string]any{"path": path, "content": "x"}}}})
}

func (s *piTokenSession) Spend(u tokenCounts) {
	s.message(map[string]any{"role": roleAssistant, "model": "pi-test",
		"content": []any{map[string]any{"type": blockTypeText, "text": "working"}},
		"usage":   map[string]any{"input": u.In, "output": u.Out, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite}})
}

func (s *piTokenSession) Stop() { s.hook("agent_end", nil) }

// --- Antigravity: cumulative statusline snapshots outside the transcript ---

type antigravityTokenDriver struct{ tokenDriverBase }

func newAntigravityTokenDriver() antigravityTokenDriver {
	return antigravityTokenDriver{tokenDriverBase{name: "antigravity", agent: "antigravity"}}
}

func (antigravityTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	h.env.ExtraEnv = append(h.env.ExtraEnv,
		antigravityBrainDirEnv(h.env.RepoDir),
		"ENTIRE_ANTIGRAVITY_STATUS_DIR="+h.agentDir,
		"ENTIRE_ANTIGRAVITY_CONFIG_DIR="+realDir(h))
}

func (antigravityTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("antigravity-tok-%d", h.nextSessionNumber())
	s := &antigravityTokenSession{unsupportedSession: unsupportedSession{"antigravity"}, h: h, id: id,
		path:     filepath.Join(antigravityBrainDir(h.env.RepoDir), id, ".system_generated", "logs", "transcript.jsonl"),
		snapshot: filepath.Join(h.agentDir, id+".jsonl"),
		clock:    time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)}
	return s
}

type antigravityTokenSession struct {
	unsupportedSession

	h                  *tokenHarness
	id, path, snapshot string
	step, invocation   int
	totalIn, totalOut  int
	clock              time.Time
}

func (s *antigravityTokenSession) ID() string             { return s.id }
func (s *antigravityTokenSession) TranscriptPath() string { return s.path }

func (s *antigravityTokenSession) hook(name string, extra map[string]any) {
	in := map[string]any{"conversationId": s.id, "workspacePaths": []string{s.h.sessionDir(s.id)},
		"transcriptPath": s.path, "artifactDirectoryPath": filepath.Join(s.h.env.RepoDir, ".gemini", "antigravity-cli", "artifacts")}
	for k, v := range extra {
		in[k] = v
	}
	hookCall(s.h, s.h.sessionDir(s.id), "antigravity", name, in)
}

func (s *antigravityTokenSession) transcriptLine(typ, content string) {
	appendJSONLines(s.h, s.path, map[string]any{"step_index": s.step, "source": "MODEL", "type": typ,
		"status": "DONE", "content": content})
	s.step++
}

func (s *antigravityTokenSession) StartTurn(prompt string) {
	s.transcriptLine("USER_INPUT", prompt)
	s.hook("pre-invocation", map[string]any{"invocationNum": s.invocation, "initialNumSteps": s.step})
	s.invocation++
}

func (s *antigravityTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	s.hook("pre-tool-use", map[string]any{"stepIdx": s.step, "toolCall": map[string]any{
		"name": "write_to_file", "args": map[string]any{"TargetFile": path, "Overwrite": false}}})
	s.transcriptLine("CODE_ACTION", "wrote "+path)
}

// Spend appends a snapshot: input/output totals are cumulative, cache counts
// are the call's own (current_usage).
func (s *antigravityTokenSession) Spend(u tokenCounts) {
	s.totalIn += u.In
	s.totalOut += u.Out
	s.clock = s.clock.Add(time.Minute)
	appendJSONLines(s.h, s.snapshot, map[string]any{"ts": s.clock.Format(time.RFC3339), "conversation_id": s.id,
		"context_window": map[string]any{"total_input_tokens": s.totalIn, "total_output_tokens": s.totalOut,
			"current_usage": map[string]any{"input_tokens": u.In, "output_tokens": u.Out,
				"cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite}}})
	s.transcriptLine("PLANNER_RESPONSE", "working")
}

func (s *antigravityTokenSession) Stop() {
	s.hook("stop", map[string]any{"executionNum": s.invocation, "terminationReason": "model_stop",
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
	in := map[string]any{"session_id": s.id, "transcript_path": s.path, "model": "vogon-test"}
	for k, v := range extra {
		in[k] = v
	}
	hookCall(s.h, s.h.sessionDir(s.id), "vogon", name, in)
}

func (s *vogonTokenSession) line(m map[string]any) {
	m["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	appendJSONLines(s.h, s.path, m)
}

func (s *vogonTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submit", map[string]any{"prompt": prompt})
	s.line(map[string]any{"type": roleUser, "message": prompt})
}

func (s *vogonTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	s.line(map[string]any{"type": "tool_use", "message": "wrote " + path, "files": []string{path}})
}

func (s *vogonTokenSession) Spend(u tokenCounts) {
	s.line(map[string]any{"type": roleAssistant, "message": "working", "usage": map[string]any{
		"input_tokens": u.In, "output_tokens": u.Out, "cache_read_tokens": u.CacheRead, "cache_creation_tokens": u.CacheWrite}})
}

func (s *vogonTokenSession) Stop()       { s.hook("stop", nil) }
func (s *vogonTokenSession) SessionEnd() { s.hook("session-end", nil) }
