//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// The drivers write what each agent writes, in the order it writes it. The
// shapes follow real captures (2026-10-14) of Claude Code 2.1.299, Codex
// 0.156.1, Cursor 2026.10.01, Copilot CLI 1.0.97 and Antigravity (agy) 1.3.4;
// each driver's comment says what was observed. The values are synthetic:
// the harness picks every count, so the truth is known exactly.

// tokenCap names an optional behaviour a token driver can fake. Scenarios
// that need a capability the driver lacks report "not supported".
type tokenCap string

const (
	// capSubagent: a subagent runs in the foreground of a turn and its tokens
	// are visible to the CLI.
	capSubagent tokenCap = "subagent"
	// capBackgroundSubagent: a subagent launched in one turn finishes later.
	capBackgroundSubagent tokenCap = "background-subagent"
	// capSessionEnd: the agent fires a session-end hook.
	capSessionEnd tokenCap = "session-end"
	// capCompact: the agent compacts by appending to its transcript.
	capCompact tokenCap = "compact"
	// capFork: the agent can start a new session from a copy of another's history.
	capFork tokenCap = "fork"
	// capSplitMessage: one API call is written as several transcript rows
	// with the same message id.
	capSplitMessage tokenCap = "split-message"
	// capAttach: `entire session attach` can find the session's transcript.
	capAttach tokenCap = "attach"
	// capResumeRestore: `entire session resume --force` restores the stored
	// transcript over the local one.
	capResumeRestore tokenCap = "resume-restore"
	// capImport: `entire import <agent>` can import the session's history.
	capImport tokenCap = "import"
	// capTranscriptTokens: tokens are read from a transcript file (not from a
	// hook payload), so the transcript can be made unreadable.
	capTranscriptTokens tokenCap = "transcript-tokens"
	// capWorktreeHooks: hooks can run from a linked worktree (adopt).
	capWorktreeHooks tokenCap = "worktree-hooks"
	// capMidTurnTokens: a turn's tokens are visible before its Stop, so a turn
	// that never stops (stuck, dead agent) still has knowable tokens. Agents
	// that report tokens only in the Stop payload (Cursor) lack it.
	capMidTurnTokens tokenCap = "mid-turn-tokens"
)

// tokenAgentDriver fakes one agent through the real `entire hooks` entry
// points, so the invariants see exactly what a live agent would produce.
type tokenAgentDriver interface {
	Name() string
	// AgentName is the registry name passed to `session attach -a`.
	AgentName() string
	Caps() map[tokenCap]bool
	// FullMatrix reports whether every scenario runs by default; otherwise
	// only the smoke subset runs unless ENTIRE_TOKEN_MATRIX=full.
	FullMatrix() bool
	// Setup configures the environment (env vars on env.ExtraEnv, settings)
	// before any session starts.
	Setup(h *tokenHarness)
	NewSession(h *tokenHarness) tokenSession
	// Expressible trims a usage to the fields the agent's own format can
	// carry (Codex has no cache writes); the harness spends and records that.
	Expressible(u tokenCounts) tokenCounts
	// Runs reports whether the driver takes part in a scenario at all; a
	// variant driver that pins one agent mode runs only the rows it needs.
	Runs(scenario string) bool
}

// toolUse is the tool an API call asks for: a file write (path, relative to
// the session's worktree) or a shell command.
type toolUse struct {
	path    string
	command string
}

func writeTool(path string) toolUse    { return toolUse{path: path} }
func shellTool(command string) toolUse { return toolUse{command: command} }

// tokenSession is one fake agent session. Spend-type methods only write what
// the agent would write; the harness records the truth.
//
// Every API call either asks for a tool (ToolCall) or answers in text
// (Spend). An agent reads every tool result with another API call, so a turn
// always ends with a Spend, and a commit the agent makes (with a tool) is
// always followed by usage.
type tokenSession interface {
	ID() string
	StartTurn(prompt string)
	// ToolCall makes one API call that asks for tool, runs it (run has the
	// tool's effect: the file write or the commit) and records its result.
	// Drivers write the call's usage where and when the agent does relative
	// to the tool running.
	ToolCall(u tokenCounts, tool toolUse, run func())
	// Spend makes one API call that answers in text.
	Spend(u tokenCounts)
	// SpendSubagent runs a foreground subagent that makes one API call.
	SpendSubagent(agentID string, u tokenCounts)
	StartBackgroundSubagent(agentID string)
	FinishSubagent(agentID string, u tokenCounts)
	DeleteSubagentTranscript(agentID string)
	// SpendPartial / SpendFinal write one API call as two rows with the same
	// message id, both carrying the call's final usage (as Claude Code does).
	SpendPartial(msgID string, u tokenCounts)
	SpendFinal(msgID string, u tokenCounts)
	Stop()
	SessionEnd()
	Compact()
	Fork() tokenSession
	TranscriptPath() string
}

// hookCall runs `entire hooks <agent> <hook>` with payload on stdin from dir.
func hookCall(h *tokenHarness, dir, agentNS, hook string, payload any) {
	h.t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(h.t, err)
	cmd := exec.CommandContext(h.t.Context(), getTestBinary(), "hooks", agentNS, hook)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = h.env.cliEnv()
	out, err := cmd.CombinedOutput()
	require.NoErrorf(h.t, err, "hooks %s %s: %s\ninput: %s", agentNS, hook, out, data)
}

// appendJSONLines appends JSON values as lines to path, creating it.
func appendJSONLines(h *tokenHarness, path string, lines ...any) {
	h.t.Helper()
	require.NoError(h.t, os.MkdirAll(filepath.Dir(path), 0o750))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(h.t, err)
	for _, l := range lines {
		data, err := json.Marshal(l)
		require.NoError(h.t, err)
		_, err = f.Write(append(data, '\n'))
		require.NoError(h.t, err)
	}
	require.NoError(h.t, f.Close())
}

// fakeUUID derives a stable UUID-shaped ID from its parts.
func fakeUUID(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	x := hex.EncodeToString(sum[:16])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

func nowMillis() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// unsupportedSession panics on every optional operation; drivers embed it and
// override what they support. Scenarios gate on Caps, so reaching one of
// these is a harness bug.
type unsupportedSession struct{ name string }

func (u unsupportedSession) fail(op string) {
	panic(fmt.Sprintf("token driver %s does not support %s; gate the scenario on Caps", u.name, op))
}
func (u unsupportedSession) SpendSubagent(string, tokenCounts)  { u.fail("SpendSubagent") }
func (u unsupportedSession) StartBackgroundSubagent(string)     { u.fail("StartBackgroundSubagent") }
func (u unsupportedSession) FinishSubagent(string, tokenCounts) { u.fail("FinishSubagent") }
func (u unsupportedSession) DeleteSubagentTranscript(string)    { u.fail("DeleteSubagentTranscript") }
func (u unsupportedSession) SpendPartial(string, tokenCounts)   { u.fail("SpendPartial") }
func (u unsupportedSession) SpendFinal(string, tokenCounts)     { u.fail("SpendFinal") }
func (u unsupportedSession) SessionEnd()                        { u.fail("SessionEnd") }
func (u unsupportedSession) Compact()                           { u.fail("Compact") }
func (u unsupportedSession) Fork() tokenSession                 { u.fail("Fork"); return nil }

// headlessScenarios is what a headless variant driver (one agent process per
// prompt, so a session end and a session start around every turn) runs: the
// rows where that can change the outcome. Scenarios that end the session
// themselves stay with the interactive driver.
var headlessScenarios = map[string]bool{
	"plain turns":                         true,
	"no-file turn then commit":            true,
	"mid-turn agent commit":               true,
	"mid-turn agent commit of every file": true,
	"real-run replay":                     true,
}

// --- Claude Code ---

// claudeTokenDriver drives Claude Code. headless is `claude -p`: every prompt
// is its own process, so SessionStart (source startup, then resume) and
// SessionEnd fire around every turn.
type claudeTokenDriver struct{ headless bool }

func (d claudeTokenDriver) Name() string {
	if d.headless {
		return "claude-code-p"
	}
	return "claude-code"
}
func (claudeTokenDriver) AgentName() string  { return agentClaudeCode }
func (d claudeTokenDriver) FullMatrix() bool { return !d.headless }
func (d claudeTokenDriver) Caps() map[tokenCap]bool {
	return map[tokenCap]bool{
		capSubagent: true, capBackgroundSubagent: true, capSessionEnd: true, capCompact: true,
		capFork: true, capSplitMessage: true, capAttach: true, capResumeRestore: true,
		capImport: true, capTranscriptTokens: true, capWorktreeHooks: true, capMidTurnTokens: true,
	}
}
func (claudeTokenDriver) Setup(*tokenHarness)                   {}
func (claudeTokenDriver) Expressible(u tokenCounts) tokenCounts { return u }
func (d claudeTokenDriver) Runs(scenario string) bool {
	return !d.headless || headlessScenarios[scenario]
}

func (d claudeTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fakeUUID("claude", h.t.Name(), h.nextSessionNumber())
	s := &claudeTokenSession{
		h: h, id: id, headless: d.headless,
		path: filepath.Join(h.env.ClaudeProjectDir, id+".jsonl"),
	}
	s.hook("session-start", "SessionStart", map[string]any{"source": "startup"})
	return s
}

// claudeTokenSession writes a Claude Code transcript in the agent's project
// dir (where attach, resume and import look) by appending, as Claude does.
//
// Observed in 2.1.299: an API call's content blocks are separate rows with
// the same message.id, and every row repeats the call's identical final
// usage (thinking + tool_use, or thinking + text). A tool call's usage is on
// its tool_use rows, written before the tool runs; there is no separate text
// row for it. Between messages sit rows without usage: queue-operation,
// attachment, last-prompt, system stop_hook_summary and cost-state.
type claudeTokenSession struct {
	h        *tokenHarness
	id, path string
	headless bool
	seq      int
	turns    int
	promptID string
	parent   any
	// spent is the process's running usage, for the cost-state row.
	spent tokenCounts
}

func (s *claudeTokenSession) ID() string             { return s.id }
func (s *claudeTokenSession) TranscriptPath() string { return s.path }

func (s *claudeTokenSession) hook(name, event string, extra map[string]any) {
	payload := mergeMaps(map[string]any{"session_id": s.id, "transcript_path": s.path,
		"cwd": s.h.sessionDir(s.id), "hook_event_name": event}, extra)
	if s.promptID != "" && event != "SessionStart" {
		payload["prompt_id"] = s.promptID
	}
	hookCall(s.h, s.h.sessionDir(s.id), agentClaudeCode, name, payload)
}

// record is a conversation row: it gets a uuid and chains to the previous one.
func (s *claudeTokenSession) record(typ string, fields map[string]any) map[string]any {
	s.seq++
	uuid := fakeUUID(s.id, "row", s.seq)
	row := mergeMaps(map[string]any{
		"parentUuid": s.parent, "isSidechain": false, "type": typ, "uuid": uuid,
		"timestamp": nowMillis(), "userType": "external", "entrypoint": "cli",
		"cwd": s.h.sessionDir(s.id), "sessionId": s.id, "version": "2.1.299", "gitBranch": "feature/test-branch",
	}, fields)
	s.parent = uuid
	return row
}

func (s *claudeTokenSession) appendLines(lines ...any) { appendJSONLines(s.h, s.path, lines...) }

// claudeUsage is a usage object as Claude Code 2.1.299 writes it.
func claudeUsage(u tokenCounts) map[string]any {
	return map[string]any{
		"input_tokens": u.In, "cache_creation_input_tokens": u.CacheWrite,
		"cache_read_input_tokens": u.CacheRead, "output_tokens": u.Out,
		"output_tokens_details": map[string]any{"thinking_tokens": u.Out / 3},
		"server_tool_use":       map[string]any{"web_search_requests": 0, "web_fetch_requests": 0},
		"service_tier":          "standard",
		"cache_creation":        map[string]any{"ephemeral_1h_input_tokens": u.CacheWrite, "ephemeral_5m_input_tokens": 0},
		"inference_geo":         "global",
		"iterations": []any{map[string]any{"input_tokens": u.In, "output_tokens": u.Out,
			"cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite,
			"cache_creation": map[string]any{"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": u.CacheWrite},
			"type":           "message"}},
		"speed": "standard",
	}
}

func claudeAssistant(msgID string, content []any, stopReason string, u *tokenCounts) map[string]any {
	m := map[string]any{"model": "claude-test", "id": msgID, "type": "message", "role": roleAssistant,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil}
	if u != nil {
		m["usage"] = claudeUsage(*u)
	}
	return m
}

// assistantRow is one content block of an API call.
func (s *claudeTokenSession) assistantRow(msgID string, block map[string]any, stopReason string, u *tokenCounts) map[string]any {
	return s.record(roleAssistant, map[string]any{
		"message":   claudeAssistant(msgID, []any{block}, stopReason, u),
		"requestId": "req_" + msgID, "apiBlockIndex": 0,
	})
}

func thinkingBlock() map[string]any {
	return map[string]any{"type": "thinking", "thinking": "", "signature": "c2lnbmF0dXJl"}
}

func (s *claudeTokenSession) StartTurn(prompt string) {
	s.turns++
	if s.headless && s.turns > 1 {
		s.hook("session-start", "SessionStart", map[string]any{"source": "resume"})
	}
	s.promptID = fakeUUID(s.id, "prompt", s.turns)
	ts := nowMillis()
	s.appendLines(
		map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": ts, "sessionId": s.id, "content": prompt},
		map[string]any{"type": "queue-operation", "operation": "dequeue", "timestamp": ts, "sessionId": s.id},
	)
	s.hook("user-prompt-submit", "UserPromptSubmit", map[string]any{"prompt": prompt, "permission_mode": "default"})
	s.appendLines(
		s.record(roleUser, map[string]any{"promptId": s.promptID, "message": map[string]any{"role": roleUser, "content": prompt},
			"permissionMode": "default"}),
		s.record("attachment", map[string]any{"attachment": map[string]any{"type": "hook_success",
			"hookName": "UserPromptSubmit", "hookEvent": "UserPromptSubmit", "exitCode": 0}}),
	)
	s.appendLines(map[string]any{"type": "last-prompt", "lastPrompt": prompt, "leafUuid": s.parent, "sessionId": s.id})
}

func (s *claudeTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	s.seq++
	msgID := fmt.Sprintf("msg_%s_%d", s.id[:8], s.seq)
	toolID := fmt.Sprintf("toolu_%s_%d", s.id[:8], s.seq)
	block := map[string]any{"type": blockTypeToolUse, "id": toolID, "caller": map[string]any{"type": "direct"}}
	result := map[string]any{"stdout": "ok", "stderr": "", "interrupted": false}
	if tool.path != "" {
		abs := filepath.Join(s.h.sessionDir(s.id), tool.path)
		block["name"] = "Write"
		block["input"] = map[string]any{"file_path": abs, "content": "x\n"}
		result = map[string]any{"type": "create", "filePath": abs, "content": "x\n", "structuredPatch": []any{}}
	} else {
		block["name"] = "Bash"
		block["input"] = map[string]any{"command": tool.command, "description": "Commit"}
	}
	s.spent = s.spent.plus(u)
	s.appendLines(s.assistantRow(msgID, thinkingBlock(), "tool_use", &u))
	toolRow := s.assistantRow(msgID, block, "tool_use", &u)
	s.appendLines(toolRow)
	run()
	s.appendLines(s.record(roleUser, map[string]any{"promptId": s.promptID,
		"message": map[string]any{"role": roleUser, "content": []any{map[string]any{
			"tool_use_id": toolID, "type": blockTypeToolResult, "content": "ok"}}},
		"toolUseResult": result, "sourceToolAssistantUUID": toolRow["uuid"]}))
}

func (s *claudeTokenSession) Spend(u tokenCounts) {
	s.seq++
	msgID := fmt.Sprintf("msg_%s_%d", s.id[:8], s.seq)
	s.SpendPartial(msgID, u)
	s.SpendFinal(msgID, u)
}

// SpendPartial writes an API call's thinking row and SpendFinal its text
// row. Both carry the call's final usage, as Claude Code writes them.
func (s *claudeTokenSession) SpendPartial(msgID string, u tokenCounts) {
	s.spent = s.spent.plus(u)
	s.appendLines(s.assistantRow(msgID, thinkingBlock(), "end_turn", &u))
}

func (s *claudeTokenSession) SpendFinal(msgID string, u tokenCounts) {
	s.appendLines(s.assistantRow(msgID, map[string]any{"type": blockTypeText, "text": "working"}, "end_turn", &u))
}

func (s *claudeTokenSession) subagentPath(agentID string) string {
	return filepath.Join(paths.SubagentsDir(filepath.Dir(s.path), s.id), paths.AgentTranscriptFileName(agentID))
}

// launchSubagent writes the Agent call, its launch hooks and the result
// naming the agent, which is how the CLI finds the agent's transcript.
func (s *claudeTokenSession) launchSubagent(agentID string, background bool) string {
	toolID := "toolu_" + agentID
	s.appendLines(s.assistantRow("msg-launch-"+agentID, map[string]any{
		"type": blockTypeToolUse, "id": toolID, "name": "Agent",
		"input": map[string]any{"prompt": "help", "subagent_type": "general-purpose"},
	}, "tool_use", nil))
	s.hook("pre-task", "PreToolUse", map[string]any{"tool_use_id": toolID, "tool_name": "Agent",
		"tool_input": map[string]any{"subagent_type": "general-purpose"}})
	s.hook("subagent-start", "SubagentStart", map[string]any{"agent_id": agentID, "agent_type": "general-purpose"})
	appendJSONLines(s.h, s.subagentPath(agentID), map[string]any{
		"type": roleUser, "isSidechain": true, "agentId": agentID, "uuid": agentID + "-u",
		"message": map[string]any{"role": roleUser, "content": "help"},
	})
	if background {
		s.appendLines(s.record(roleUser, map[string]any{"message": map[string]any{"role": roleUser, "content": []any{map[string]any{
			"type": blockTypeToolResult, "tool_use_id": toolID,
			"content": "Async agent launched successfully.\nagentId: " + agentID,
		}}}}))
		s.hook("post-task", "PostToolUse", map[string]any{"tool_use_id": toolID, "tool_name": "Agent", "tool_input": map[string]any{},
			"tool_response": map[string]any{"status": "async_launched", "isAsync": true, "agentId": agentID}})
	}
	return toolID
}

// subagentCall writes one subagent API call (thinking + text rows with the
// same usage) and fires SubagentStop.
func (s *claudeTokenSession) subagentCall(agentID string, u tokenCounts) {
	msgID := fmt.Sprintf("msg-%s-%d", agentID, u.total())
	for i, block := range []map[string]any{thinkingBlock(), {"type": blockTypeText, "text": "sub work"}} {
		appendJSONLines(s.h, s.subagentPath(agentID), map[string]any{
			"type": roleAssistant, "isSidechain": true, "agentId": agentID, "uuid": fmt.Sprintf("%s-%d-%d", agentID, u.total(), i),
			"requestId": "req_" + msgID, "message": claudeAssistant(msgID, []any{block}, "end_turn", &u),
		})
	}
	s.hook("subagent-stop", "SubagentStop", map[string]any{"agent_id": agentID,
		"agent_type": "general-purpose", "agent_transcript_path": s.subagentPath(agentID)})
}

func (s *claudeTokenSession) SpendSubagent(agentID string, u tokenCounts) {
	toolID := s.launchSubagent(agentID, false)
	s.subagentCall(agentID, u)
	s.appendLines(s.record(roleUser, map[string]any{"message": map[string]any{"role": roleUser, "content": []any{map[string]any{
		"type": blockTypeToolResult, "tool_use_id": toolID, "content": "Done.\nagentId: " + agentID,
	}}}}))
	s.hook("post-task", "PostToolUse", map[string]any{"tool_use_id": toolID, "tool_name": "Agent", "tool_input": map[string]any{},
		"tool_response": map[string]any{"status": "completed", "agentId": agentID}})
}

func (s *claudeTokenSession) StartBackgroundSubagent(agentID string) { s.launchSubagent(agentID, true) }

func (s *claudeTokenSession) FinishSubagent(agentID string, u tokenCounts) {
	s.subagentCall(agentID, u)
}

func (s *claudeTokenSession) DeleteSubagentTranscript(agentID string) {
	require.NoError(s.h.t, os.Remove(s.subagentPath(agentID)))
}

// Stop fires the Stop hook, then appends what Claude writes after it: the
// hook summary, the prompt pointer and the running cost state. A headless
// (-p) process then exits, firing SessionEnd.
func (s *claudeTokenSession) Stop() {
	s.hook("stop", "Stop", map[string]any{"stop_hook_active": false, "last_assistant_message": "done",
		"permission_mode": "default", "background_tasks": []any{}, "session_crons": []any{}})
	s.appendLines(s.record("system", map[string]any{"subtype": "stop_hook_summary", "hookCount": 1, "hookErrors": []any{},
		"preventedContinuation": false, "stopReason": "", "hasOutput": false, "level": "suggestion"}))
	s.appendLines(
		map[string]any{"type": "last-prompt", "leafUuid": s.parent, "sessionId": s.id},
		map[string]any{"type": "cost-state", "sessionId": s.id, "modelUsage": map[string]any{"claude-test": map[string]any{
			"inputTokens": s.spent.In, "outputTokens": s.spent.Out,
			"cacheReadInputTokens": s.spent.CacheRead, "cacheCreationInputTokens": s.spent.CacheWrite}}},
	)
	if s.headless {
		s.SessionEnd()
	}
}

func (s *claudeTokenSession) SessionEnd() {
	s.hook("session-end", "SessionEnd", map[string]any{"reason": "other"})
}

// Compact appends what /compact writes: a boundary and a summary. The
// compaction call's own usage is not in the transcript, so it is not truth.
func (s *claudeTokenSession) Compact() {
	s.appendLines(
		s.record("system", map[string]any{"subtype": "compact_boundary", "content": "Conversation compacted",
			"compactMetadata": map[string]any{"trigger": "manual", "preTokens": 1000}}),
		s.record(roleUser, map[string]any{"message": map[string]any{"role": roleUser, "content": "Summary of the conversation so far."},
			"isCompactSummary": true}),
	)
}

// Fork starts a new session whose transcript begins with a copy of this
// one's history, as `claude --fork-session` does: the copied lines keep the
// original sessionId.
func (s *claudeTokenSession) Fork() tokenSession {
	f, ok := claudeTokenDriver{}.NewSession(s.h).(*claudeTokenSession)
	require.True(s.h.t, ok)
	s.h.dirs[f.id] = s.h.sessionDir(s.id)
	data, err := os.ReadFile(s.path)
	require.NoError(s.h.t, err)
	require.NoError(s.h.t, os.WriteFile(f.path, data, 0o600))
	f.seq = s.seq
	f.parent = s.parent
	return f
}
