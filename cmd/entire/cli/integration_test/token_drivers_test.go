//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

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
	// capSplitMessage: one API call is streamed as several transcript rows
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
}

// tokenSession is one fake agent session. Spend-type methods only write what
// the agent would write; the harness records the truth.
type tokenSession interface {
	ID() string
	StartTurn(prompt string)
	// WriteFile creates path in the session's worktree and records the tool
	// call that wrote it.
	WriteFile(path string)
	// Spend records one API call of the main agent.
	Spend(u tokenCounts)
	// SpendSubagent runs a foreground subagent that makes one API call.
	SpendSubagent(agentID string, u tokenCounts)
	StartBackgroundSubagent(agentID string)
	FinishSubagent(agentID string, u tokenCounts)
	DeleteSubagentTranscript(agentID string)
	// SpendPartial / SpendFinal stream one API call as two rows with the same
	// message id: the second row carries the final usage.
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
	cmd := exec.CommandContext(context.Background(), getTestBinary(), "hooks", agentNS, hook)
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

// --- Claude Code ---

type claudeTokenDriver struct{}

func (claudeTokenDriver) Name() string      { return "claude-code" }
func (claudeTokenDriver) AgentName() string { return agentClaudeCode }
func (claudeTokenDriver) FullMatrix() bool  { return true }
func (claudeTokenDriver) Caps() map[tokenCap]bool {
	return map[tokenCap]bool{
		capSubagent: true, capBackgroundSubagent: true, capSessionEnd: true, capCompact: true,
		capFork: true, capSplitMessage: true, capAttach: true, capResumeRestore: true,
		capImport: true, capTranscriptTokens: true, capWorktreeHooks: true, capMidTurnTokens: true,
	}
}
func (claudeTokenDriver) Setup(*tokenHarness)                   {}
func (claudeTokenDriver) Expressible(u tokenCounts) tokenCounts { return u }

func (claudeTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("claude-tok-%d", h.nextSessionNumber())
	return &claudeTokenSession{
		h: h, id: id,
		path: filepath.Join(h.env.ClaudeProjectDir, id+".jsonl"),
	}
}

// claudeTokenSession writes a Claude Code transcript in the agent's project
// dir (where attach, resume and import look) by appending, as Claude does.
type claudeTokenSession struct {
	h    *tokenHarness
	id   string
	path string
	seq  int
}

func (s *claudeTokenSession) ID() string             { return s.id }
func (s *claudeTokenSession) TranscriptPath() string { return s.path }

func (s *claudeTokenSession) hook(name string, extra map[string]any) {
	payload := map[string]any{"session_id": s.id, "transcript_path": s.path}
	for k, v := range extra {
		payload[k] = v
	}
	hookCall(s.h, s.h.sessionDir(s.id), agentClaudeCode, name, payload)
}

func (s *claudeTokenSession) line(typ string, message any, extra map[string]any) map[string]any {
	s.seq++
	l := map[string]any{
		"type": typ, "uuid": fmt.Sprintf("%s-%d", s.id, s.seq), "sessionId": s.id, "cwd": s.h.sessionDir(s.id),
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": message,
	}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func (s *claudeTokenSession) appendLines(lines ...any) { appendJSONLines(s.h, s.path, lines...) }

func claudeUsage(u tokenCounts) map[string]any {
	return map[string]any{
		"input_tokens": u.In, "output_tokens": u.Out,
		"cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite,
	}
}

func claudeAssistant(msgID string, content []any, u *tokenCounts) map[string]any {
	m := map[string]any{"id": msgID, "role": roleAssistant, "model": "claude-test", "content": content}
	if u != nil {
		m["usage"] = claudeUsage(*u)
	}
	return m
}

func (s *claudeTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submit", map[string]any{"prompt": prompt})
	s.appendLines(s.line(roleUser, map[string]any{"role": roleUser, "content": prompt}, nil))
}

func (s *claudeTokenSession) WriteFile(path string) {
	s.h.writeWorktreeFile(s.h.sessionDir(s.id), path)
	s.seq++
	toolID := fmt.Sprintf("toolu_%s_%d", s.id, s.seq)
	s.appendLines(
		s.line(roleAssistant, claudeAssistant(fmt.Sprintf("msg-tool-%s-%d", s.id, s.seq), []any{map[string]any{
			"type": blockTypeToolUse, "id": toolID, "name": "Write",
			"input": map[string]any{"file_path": path, "content": "x"},
		}}, nil), nil),
		s.line(roleUser, map[string]any{"role": roleUser, "content": []any{map[string]any{
			"type": blockTypeToolResult, "tool_use_id": toolID, "content": "Success",
		}}}, nil),
	)
}

func (s *claudeTokenSession) Spend(u tokenCounts) {
	s.seq++
	s.SpendFinal(fmt.Sprintf("msg-%s-%d", s.id, s.seq), u)
}

func (s *claudeTokenSession) SpendPartial(msgID string, u tokenCounts) { s.SpendFinal(msgID, u) }

func (s *claudeTokenSession) SpendFinal(msgID string, u tokenCounts) {
	s.appendLines(s.line(roleAssistant, claudeAssistant(msgID,
		[]any{map[string]any{"type": blockTypeText, "text": "working"}}, &u), nil))
}

func (s *claudeTokenSession) subagentPath(agentID string) string {
	return filepath.Join(paths.SubagentsDir(filepath.Dir(s.path), s.id), paths.AgentTranscriptFileName(agentID))
}

// launchSubagent writes the Agent call, its launch hooks and the result
// naming the agent, which is how the CLI finds the agent's transcript.
func (s *claudeTokenSession) launchSubagent(agentID string, background bool) string {
	toolID := "toolu_" + agentID
	s.appendLines(s.line(roleAssistant, claudeAssistant("msg-launch-"+agentID, []any{map[string]any{
		"type": blockTypeToolUse, "id": toolID, "name": "Agent",
		"input": map[string]any{"prompt": "help", "subagent_type": "general-purpose"},
	}}, nil), nil))
	s.hook("pre-task", map[string]any{"tool_use_id": toolID, "tool_input": map[string]any{"subagent_type": "general-purpose"}})
	s.hook("subagent-start", map[string]any{"hook_event_name": "SubagentStart", "agent_id": agentID, "agent_type": "general-purpose"})
	appendJSONLines(s.h, s.subagentPath(agentID), map[string]any{
		"type": roleUser, "isSidechain": true, "agentId": agentID, "uuid": agentID + "-u",
		"message": map[string]any{"role": roleUser, "content": "help"},
	})
	if background {
		s.appendLines(s.line(roleUser, map[string]any{"role": roleUser, "content": []any{map[string]any{
			"type": blockTypeToolResult, "tool_use_id": toolID,
			"content": "Async agent launched successfully.\nagentId: " + agentID,
		}}}, nil))
		s.hook("post-task", map[string]any{"tool_use_id": toolID, "tool_name": "Agent", "tool_input": map[string]any{},
			"tool_response": map[string]any{"status": "async_launched", "isAsync": true, "agentId": agentID}})
	}
	return toolID
}

func (s *claudeTokenSession) subagentCall(agentID string, u tokenCounts) {
	appendJSONLines(s.h, s.subagentPath(agentID), map[string]any{
		"type": roleAssistant, "isSidechain": true, "agentId": agentID, "uuid": fmt.Sprintf("%s-%d", agentID, u.total()),
		"message": claudeAssistant(fmt.Sprintf("msg-%s-%d", agentID, u.total()),
			[]any{map[string]any{"type": blockTypeText, "text": "sub work"}}, &u),
	})
	s.hook("subagent-stop", map[string]any{"hook_event_name": "SubagentStop", "agent_id": agentID,
		"agent_type": "general-purpose", "agent_transcript_path": s.subagentPath(agentID)})
}

func (s *claudeTokenSession) SpendSubagent(agentID string, u tokenCounts) {
	toolID := s.launchSubagent(agentID, false)
	s.subagentCall(agentID, u)
	s.appendLines(s.line(roleUser, map[string]any{"role": roleUser, "content": []any{map[string]any{
		"type": blockTypeToolResult, "tool_use_id": toolID, "content": "Done.\nagentId: " + agentID,
	}}}, nil))
	s.hook("post-task", map[string]any{"tool_use_id": toolID, "tool_name": "Agent", "tool_input": map[string]any{},
		"tool_response": map[string]any{"status": "completed", "agentId": agentID}})
}

func (s *claudeTokenSession) StartBackgroundSubagent(agentID string) { s.launchSubagent(agentID, true) }

func (s *claudeTokenSession) FinishSubagent(agentID string, u tokenCounts) {
	s.subagentCall(agentID, u)
}

func (s *claudeTokenSession) DeleteSubagentTranscript(agentID string) {
	require.NoError(s.h.t, os.Remove(s.subagentPath(agentID)))
}

func (s *claudeTokenSession) Stop()       { s.hook("stop", nil) }
func (s *claudeTokenSession) SessionEnd() { s.hook("session-end", nil) }

// Compact appends what /compact writes: a boundary and a summary. The
// compaction call's own usage is not in the transcript, so it is not truth.
func (s *claudeTokenSession) Compact() {
	s.appendLines(
		s.line("system", nil, map[string]any{"subtype": "compact_boundary", "content": "Conversation compacted",
			"compactMetadata": map[string]any{"trigger": "manual", "preTokens": 1000}}),
		s.line(roleUser, map[string]any{"role": roleUser, "content": "Summary of the conversation so far."},
			map[string]any{"isCompactSummary": true}),
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
	return f
}
