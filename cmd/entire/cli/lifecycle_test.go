package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockLifecycleAgent is a minimal Agent implementation for lifecycle tests.
type mockLifecycleAgent struct {
	name           types.AgentName
	agentType      types.AgentType
	transcriptData []byte
	transcriptErr  error
}

var _ agent.Agent = (*mockLifecycleAgent)(nil)

func (m *mockLifecycleAgent) Name() types.AgentName                          { return m.name }
func (m *mockLifecycleAgent) Type() types.AgentType                          { return m.agentType }
func (m *mockLifecycleAgent) Description() string                            { return "Mock agent for lifecycle tests" }
func (m *mockLifecycleAgent) IsPreview() bool                                { return false }
func (m *mockLifecycleAgent) DetectPresence(_ context.Context) (bool, error) { return false, nil }
func (m *mockLifecycleAgent) ProtectedDirs() []string                        { return nil }
func (m *mockLifecycleAgent) GetSessionID(_ *agent.HookInput) string         { return "" }

func (m *mockLifecycleAgent) ReadTranscript(_ string) ([]byte, error) {
	if m.transcriptErr != nil {
		return nil, m.transcriptErr
	}
	return m.transcriptData, nil
}

func (m *mockLifecycleAgent) ChunkTranscript(_ context.Context, content []byte, _ int) ([][]byte, error) {
	return [][]byte{content}, nil
}

func (m *mockLifecycleAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	var result []byte
	for _, c := range chunks {
		result = append(result, c...)
	}
	return result, nil
}

func (m *mockLifecycleAgent) GetSessionDir(_ string) (string, error) {
	return "", nil
}

func (m *mockLifecycleAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

//nolint:nilnil // Mock implementation
func (m *mockLifecycleAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil
}

func (m *mockLifecycleAgent) WriteSession(_ context.Context, _ *agent.AgentSession) error {
	return nil
}

func (m *mockLifecycleAgent) FormatResumeCommand(_ string) string {
	return ""
}

func newMockAgent() *mockLifecycleAgent {
	return &mockLifecycleAgent{
		name:           "mock-lifecycle",
		agentType:      "Mock Lifecycle Agent",
		transcriptData: []byte(`{"type":"user","message":"test"}`),
	}
}

// mockAnalyzerAgent extends mockLifecycleAgent with a fake TranscriptAnalyzer
// implementation. The background Final capture path reads modified files from
// the subagent's own transcript via this interface, not from git status, so
// tests exercising that path need an agent that implements it.
type mockAnalyzerAgent struct {
	*mockLifecycleAgent

	analyzerFiles []string
	analyzerErr   error

	// onExtract, when set, is invoked from inside ExtractModifiedFilesFromOffset
	// — i.e. mid-capture, after handleSubagentStopFinal has loaded its initial
	// (pre-capture) session state snapshot but before completeSubagentTaskRecord
	// returns. Tests use it to simulate a racing SessionEnd landing exactly in
	// that window.
	onExtract func()
}

var _ agent.TranscriptAnalyzer = (*mockAnalyzerAgent)(nil)

func (m *mockAnalyzerAgent) GetTranscriptPosition(_ string) (int, error) { return 0, nil }

func (m *mockAnalyzerAgent) ExtractModifiedFilesFromOffset(_ string, _ int) ([]string, int, error) {
	if m.onExtract != nil {
		m.onExtract()
	}
	if m.analyzerErr != nil {
		return nil, 0, m.analyzerErr
	}
	return m.analyzerFiles, 0, nil
}

// --- DispatchLifecycleEvent tests ---

func TestDispatchLifecycleEvent_NilAgent(t *testing.T) {
	t.Parallel()

	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: "test-session",
	}

	err := DispatchLifecycleEvent(context.Background(), nil, event)
	if err == nil {
		t.Error("expected error for nil agent, got nil")
	}
	if !strings.Contains(err.Error(), "agent cannot be nil") {
		t.Errorf("expected error message about nil agent, got: %v", err)
	}
}

func TestDispatchLifecycleEvent_NilEvent(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()

	err := DispatchLifecycleEvent(context.Background(), ag, nil)
	if err == nil {
		t.Error("expected error for nil event, got nil")
	}
	if !strings.Contains(err.Error(), "event cannot be nil") {
		t.Errorf("expected error message about nil event, got: %v", err)
	}
}

// TestDispatchLifecycleEvent_SkipsForwardedHookFromNonOwningAgent verifies the
// dispatcher-level dedup: when SessionState records a different owning agent,
// non-SessionStart / non-TurnStart events from forwarded hooks no-op. This
// covers the Cursor IDE → .claude/settings.json forwarding scenario for Stop,
// SubagentStart/End, Compaction, SessionEnd, and ModelUpdate events.
func TestDispatchLifecycleEvent_SkipsForwardedHookFromNonOwningAgent(t *testing.T) {
	setupStopTestRepo(t)

	sessionID := "test-skip-nonowning"
	require.NoError(t, strategy.SaveSessionState(context.Background(), &strategy.SessionState{
		SessionID:  sessionID,
		AgentType:  agent.AgentTypeCursor,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}))

	// Claude Code fires SessionEnd for Cursor's session (Cursor IDE forwarded hook).
	claudeAgent := newMockAgent()
	claudeAgent.agentType = agent.AgentTypeClaudeCode

	require.NoError(t, DispatchLifecycleEvent(context.Background(), claudeAgent, &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: sessionID,
		Timestamp: time.Now(),
	}))

	// If the dispatcher had let the event through, markSessionEnded would have
	// transitioned to ENDED and set EndedAt.
	state, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Nil(t, state.EndedAt, "non-owning agent's SessionEnd must not transition the session")
}

// TestDispatchLifecycleEvent_AllowsTurnStartFromMismatchedAgent verifies that
// TurnStart bypasses the dispatcher-level skip so InitializeSession runs (and
// can repair a wrongly-set AgentType via transcript-path resolution).
func TestDispatchLifecycleEvent_AllowsTurnStartFromMismatchedAgent(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	repo, err := strategy.OpenRepository(ctx)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	sessionID := "test-turnstart-mismatch"
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:  sessionID,
		AgentType:  agent.AgentTypeClaudeCode,
		BaseCommit: head.Hash().String(),
		StartedAt:  time.Now(),
	}))

	cursorAgent := newMockAgent()
	cursorAgent.agentType = agent.AgentTypeCursor

	require.NoError(t, DispatchLifecycleEvent(ctx, cursorAgent, &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Timestamp: time.Now(),
	}))

	// InitializeSession generates a fresh TurnID on every dispatch. If the
	// dispatcher had skipped, TurnID would still be empty.
	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.NotEmpty(t, state.TurnID, "TurnStart must dispatch (and generate a TurnID) even when the firing agent disagrees with the recorded owner")
}

// TestDispatchLifecycleEvent_SkipsAllNonBypassEventsFromNonOwner verifies the
// skip applies uniformly to every non-bypass event type. If the dispatcher
// had let any of these through, downstream handlers would either error
// (transcript file not found, etc.) or mutate state — both are detectable.
func TestDispatchLifecycleEvent_SkipsAllNonBypassEventsFromNonOwner(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	sessionID := "test-skip-all-events"
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:  sessionID,
		AgentType:  agent.AgentTypeCursor,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		ModelName:  "initial-model",
	}))

	nonOwner := newMockAgent()
	nonOwner.agentType = agent.AgentTypeClaudeCode

	skipEligible := []agent.EventType{
		agent.TurnEnd,
		agent.Compaction,
		agent.SubagentStart,
		agent.SubagentEnd,
		agent.ModelUpdate,
		agent.SessionEnd,
	}

	for _, et := range skipEligible {
		t.Run(et.String(), func(t *testing.T) {
			err := DispatchLifecycleEvent(ctx, nonOwner, &agent.Event{
				Type:       et,
				SessionID:  sessionID,
				SessionRef: "/nonexistent/transcript.jsonl", // would fail in handler
				Model:      "would-overwrite-on-modelupdate",
				Timestamp:  time.Now(),
			})
			require.NoError(t, err, "skip must return nil; downstream handler would have errored on missing transcript")
		})
	}

	// Side-effect assertions: the handlers most likely to mutate state never ran.
	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Nil(t, state.EndedAt, "SessionEnd skipped: EndedAt should remain nil")
	require.Equal(t, "initial-model", state.ModelName, "ModelUpdate skipped: ModelName should not have been overwritten")
}

// TestDispatchLifecycleEvent_DoesNotSkipWhenOwnerMatches verifies that when
// the firing agent IS the recorded owner, the event runs normally.
func TestDispatchLifecycleEvent_DoesNotSkipWhenOwnerMatches(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	sessionID := "test-owner-match"
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:  sessionID,
		AgentType:  agent.AgentTypeCursor,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}))

	owner := newMockAgent()
	owner.agentType = agent.AgentTypeCursor

	require.NoError(t, DispatchLifecycleEvent(ctx, owner, &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: sessionID,
		Timestamp: time.Now(),
	}))

	// Owner's SessionEnd must run markSessionEnded → EndedAt is set.
	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.NotNil(t, state.EndedAt, "SessionEnd from the owning agent must transition the session")
}

// TestDispatchLifecycleEvent_DoesNotSkipWhenAgentTypeUnset verifies the early
// bootstrap window: SessionStart fired but TurnStart hasn't yet, so
// state.AgentType is empty. The skip must NOT engage in this state.
func TestDispatchLifecycleEvent_DoesNotSkipWhenAgentTypeUnset(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	sessionID := "test-agenttype-unset"
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:  sessionID,
		AgentType:  "", // unset
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}))

	ag := newMockAgent()
	ag.agentType = agent.AgentTypeClaudeCode

	require.NoError(t, DispatchLifecycleEvent(ctx, ag, &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: sessionID,
		Timestamp: time.Now(),
	}))

	// Without a recorded owner, the dispatcher cannot tell who is forwarded;
	// the event must reach the handler.
	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.NotNil(t, state.EndedAt, "with no recorded owner, SessionEnd must run regardless of firing agent")
}

func TestEventBypassesAgentOwnershipCheck(t *testing.T) {
	t.Parallel()

	bypassed := []agent.EventType{agent.SessionStart, agent.TurnStart}
	for _, et := range bypassed {
		if !eventBypassesAgentOwnershipCheck(et) {
			t.Errorf("%s must bypass the ownership check", et)
		}
	}

	notBypassed := []agent.EventType{
		agent.TurnEnd,
		agent.Compaction,
		agent.SubagentStart,
		agent.SubagentEnd,
		agent.ModelUpdate,
		agent.SessionEnd,
	}
	for _, et := range notBypassed {
		if eventBypassesAgentOwnershipCheck(et) {
			t.Errorf("%s must be subject to the ownership check", et)
		}
	}
}

func TestDispatchLifecycleEvent_UnknownEventType(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.EventType(999), // Unknown type
		SessionID: "test-session",
	}

	err := DispatchLifecycleEvent(context.Background(), ag, event)
	if err == nil {
		t.Error("expected error for unknown event type, got nil")
	}
	if !strings.Contains(err.Error(), "unknown lifecycle event type") {
		t.Errorf("expected error message about unknown event type, got: %v", err)
	}
}

// TestDispatchLifecycleEvent_RejectsTraversalSessionID verifies the dispatcher
// rejects a path-unsafe session ID for every event type, before routing to a
// handler. This guards handlers that build filesystem paths from the ID without
// their own check (notably handleLifecycleTurnEnd's .entire/metadata/<id>/
// MkdirAll + WriteFile). The guard runs before any repo/FS access, so no repo
// setup is needed.
func TestDispatchLifecycleEvent_RejectsTraversalSessionID(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	for _, evType := range []agent.EventType{
		agent.TurnEnd, agent.ModelUpdate, agent.Compaction, agent.SubagentEnd, agent.SessionEnd,
	} {
		err := DispatchLifecycleEvent(context.Background(), ag, &agent.Event{
			Type:       evType,
			SessionID:  "../../etc/evil",
			SessionRef: "/dev/null",
			Model:      "x",
		})
		if err == nil {
			t.Fatalf("%v event with traversal session ID: got nil error, want rejection", evType)
		}
		if !strings.Contains(err.Error(), "invalid session ID") {
			t.Errorf("%v event: error = %q, want \"invalid session ID\"", evType, err)
		}
	}

	// ToolUseID and SubagentID also build filesystem paths (task metadata dir,
	// subagent transcript path) and must be rejected too.
	if err := DispatchLifecycleEvent(context.Background(), ag, &agent.Event{
		Type: agent.SubagentEnd, SessionID: "ok-session", ToolUseID: "../../evil", SessionRef: "/dev/null",
	}); err == nil || !strings.Contains(err.Error(), "invalid tool use ID") {
		t.Errorf("traversal tool use ID: error = %v, want \"invalid tool use ID\"", err)
	}
	if err := DispatchLifecycleEvent(context.Background(), ag, &agent.Event{
		Type: agent.SubagentEnd, SessionID: "ok-session", SubagentID: "../../evil", SessionRef: "/dev/null",
	}); err == nil || !strings.Contains(err.Error(), "invalid subagent ID") {
		t.Errorf("traversal subagent ID: error = %v, want \"invalid subagent ID\"", err)
	}
}

// --- handleLifecycleSessionStart tests ---

func TestHandleLifecycleSessionStart_EmptySessionID(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "", // Empty
	}

	err := handleLifecycleSessionStart(context.Background(), ag, event)
	if err == nil {
		t.Error("expected error for empty session ID, got nil")
	}
	if !strings.Contains(err.Error(), "no session_id") {
		t.Errorf("expected error message about missing session_id, got: %v", err)
	}
}

// mockHookResponseAgent extends mockLifecycleAgent with HookResponseWriter.
type mockHookResponseAgent struct {
	mockLifecycleAgent

	lastMessage string
}

var _ agent.HookResponseWriter = (*mockHookResponseAgent)(nil)

func (m *mockHookResponseAgent) WriteHookResponse(message string) error {
	m.lastMessage = message
	return nil
}

func newMockHookResponseAgent() *mockHookResponseAgent {
	return &mockHookResponseAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:      "mock-hrw",
			agentType: "Mock HRW Agent",
		},
	}
}

// TestHandleLifecycleSessionStart_StoresAgentTypeHint verifies the
// SessionStart hook claims the session for its agent so a wrapper agent's
// later TurnStart hook (e.g., Cursor IDE forwarding to Claude Code's hook
// system) cannot re-label the session.
func TestHandleLifecycleSessionStart_StoresAgentTypeHint(t *testing.T) {
	setupStopTestRepo(t)

	ag := newMockHookResponseAgent()
	ag.agentType = agent.AgentTypeCursor
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "test-agent-hint",
		Timestamp: time.Now(),
	}
	require.NoError(t, handleLifecycleSessionStart(context.Background(), ag, event))

	got := strategy.LoadAgentTypeHint(context.Background(), "test-agent-hint")
	require.Equal(t, agent.AgentTypeCursor, got)
}

// TestHandleLifecycleSessionStart_AgentTypeHintFirstWriterWins verifies that
// when multiple agents fire SessionStart for the same session ID, only the
// first agent's claim is recorded AND only the first emits the banner. This
// matches both the Cursor cross-agent and the Gemini repeat-source
// (startup → resume) cases — the user must see the banner only once.
func TestHandleLifecycleSessionStart_AgentTypeHintFirstWriterWins(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	sessionID := "test-agent-hint-race"

	first := newMockHookResponseAgent()
	first.agentType = agent.AgentTypeCursor
	require.NoError(t, handleLifecycleSessionStart(ctx, first, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))
	require.NotEmpty(t, first.lastMessage, "first SessionStart must emit the banner")

	second := newMockHookResponseAgent()
	second.agentType = agent.AgentTypeClaudeCode
	require.NoError(t, handleLifecycleSessionStart(ctx, second, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))
	require.Empty(t, second.lastMessage, "subsequent SessionStarts for the same session must not emit the banner again")

	got := strategy.LoadAgentTypeHint(ctx, sessionID)
	require.Equal(t, agent.AgentTypeCursor, got, "first SessionStart caller must own the session")
}

// TestHandleLifecycleSessionStart_NonWriterClaimDoesNotSuppressBanner covers
// the Cursor + Claude Code forwarding race: Cursor IDE forwards SessionStart
// to both .cursor/hooks.json (Cursor agent — no HookResponseWriter) and
// .claude/settings.json (Claude Code — has HookResponseWriter). When Cursor
// wins the ownership claim, Claude Code must still emit the banner; otherwise
// the user sees nothing ~50% of the time (the original Bugbot finding).
func TestHandleLifecycleSessionStart_NonWriterClaimDoesNotSuppressBanner(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	sessionID := "test-non-writer-claim"

	// Non-writer agent (Cursor) wins the ownership race.
	nonWriter := newMockAgent()
	nonWriter.agentType = agent.AgentTypeCursor
	require.NoError(t, handleLifecycleSessionStart(ctx, nonWriter, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))

	// Writer-capable agent (Claude Code) fires SessionStart for the same session.
	writer := newMockHookResponseAgent()
	writer.agentType = agent.AgentTypeClaudeCode
	require.NoError(t, handleLifecycleSessionStart(ctx, writer, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))
	require.NotEmpty(t, writer.lastMessage,
		"banner-capable agent must emit the banner even after a non-writer claimed ownership")

	// Ownership still belongs to whoever called StoreAgentTypeHint first.
	require.Equal(t, agent.AgentTypeCursor, strategy.LoadAgentTypeHint(ctx, sessionID),
		"first SessionStart caller still owns the session")
}

// TestHandleLifecycleSessionStart_BannerClaimedOnce verifies that once a
// banner-capable agent has shown the banner, a subsequent banner-capable
// agent firing SessionStart for the same session ID does not duplicate it.
func TestHandleLifecycleSessionStart_BannerClaimedOnce(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	sessionID := "test-banner-claimed-once"

	first := newMockHookResponseAgent()
	first.agentType = agent.AgentTypeClaudeCode
	require.NoError(t, handleLifecycleSessionStart(ctx, first, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))
	require.NotEmpty(t, first.lastMessage)

	second := newMockHookResponseAgent()
	second.agentType = agent.AgentTypeGemini
	require.NoError(t, handleLifecycleSessionStart(ctx, second, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))
	require.Empty(t, second.lastMessage,
		"banner must not be re-emitted once a writer agent has shown it")
}

// TestHandleLifecycleSessionStart_GeminiRepeatSourceDoesNotDuplicate covers
// the specific case the user reported: Gemini fires SessionStart twice for
// the same session (e.g., source=startup followed by source=resume) and we
// were emitting the banner both times.
func TestHandleLifecycleSessionStart_GeminiRepeatSourceDoesNotDuplicate(t *testing.T) {
	setupStopTestRepo(t)

	ctx := context.Background()
	sessionID := "test-gemini-repeat"

	ag := newMockHookResponseAgent()
	ag.agentType = agent.AgentTypeGemini

	require.NoError(t, handleLifecycleSessionStart(ctx, ag, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))
	first := ag.lastMessage
	require.NotEmpty(t, first)

	ag.lastMessage = ""
	require.NoError(t, handleLifecycleSessionStart(ctx, ag, &agent.Event{
		Type: agent.SessionStart, SessionID: sessionID, Timestamp: time.Now(),
	}))
	require.Empty(t, ag.lastMessage, "second SessionStart from the same agent must not re-emit the banner")
}

func TestHandleLifecycleSessionStart_EmptyRepoWarning(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir) // no commits — empty repo
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ag := newMockHookResponseAgent()
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "test-empty-repo-warning",
		Timestamp: time.Now(),
	}

	err := handleLifecycleSessionStart(context.Background(), ag, event)
	require.NoError(t, err)

	if !strings.Contains(ag.lastMessage, "no commits yet") {
		t.Errorf("expected message containing 'no commits yet', got: %q", ag.lastMessage)
	}
}

func TestHandleLifecycleSessionStart_DefaultMessageWithCommits(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ag := newMockHookResponseAgent()
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "test-default-message",
		Timestamp: time.Now(),
	}

	err := handleLifecycleSessionStart(context.Background(), ag, event)
	require.NoError(t, err)

	if !strings.Contains(ag.lastMessage, "link this conversation to your next commit") {
		t.Errorf("expected message containing 'link this conversation to your next commit', got: %q", ag.lastMessage)
	}
	if strings.Contains(ag.lastMessage, "no commits yet") {
		t.Errorf("did not expect empty-repo warning, got: %q", ag.lastMessage)
	}
	if !strings.HasPrefix(ag.lastMessage, "\n\nEntire CLI ") {
		t.Errorf("expected multiline session-start banner, got %q", ag.lastMessage)
	}
	if !strings.Contains(ag.lastMessage, "\n\n") {
		t.Errorf("expected default agent banner to remain multiline, got %q", ag.lastMessage)
	}
}

func TestSessionStartMessage_CodexUsesSingleLineBanner(t *testing.T) {
	t.Parallel()

	msg := sessionStartMessage(agent.AgentNameCodex, false)
	require.Equal(t, "Entire CLI will link this conversation to your next commit.", msg)
	if strings.Contains(msg, "\n") {
		t.Fatalf("expected single-line Codex message, got %q", msg)
	}
}

func TestSessionStartMessage_CodexUsesSingleLineBannerForEmptyRepo(t *testing.T) {
	t.Parallel()

	msg := sessionStartMessage(agent.AgentNameCodex, true)
	require.Equal(t, "Entire CLI found no commits yet — checkpoints will activate after your first commit.", msg)
	if strings.Contains(msg, "\n") {
		t.Fatalf("expected single-line Codex empty-repo message, got %q", msg)
	}
}

func TestHandleLifecycleSessionStart_CodexConcurrentSessionsStaySingleLine(t *testing.T) {
	t.Parallel()

	msg := sessionStartMessage(agent.AgentNameCodex, false)
	msg += " 1 other active conversation(s) in this workspace will also be included. Use 'entire status' for more information."

	if strings.Contains(msg, "\n") {
		t.Fatalf("expected Codex concurrent-session message to stay single-line, got %q", msg)
	}
	if strings.Contains(msg, "  ") {
		t.Fatalf("expected Codex concurrent-session message to avoid repeated spaces, got %q", msg)
	}
}

// --- handleLifecycleTurnStart tests ---

func TestHandleLifecycleTurnStart_EmptySessionID(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: "", // Empty
	}

	err := handleLifecycleTurnStart(context.Background(), ag, event)
	if err == nil {
		t.Error("expected error for empty session ID, got nil")
	}
	if !strings.Contains(err.Error(), "no session_id") {
		t.Errorf("expected error message about missing session_id, got: %v", err)
	}
}

// --- handleLifecycleTurnEnd tests ---

func TestHandleLifecycleTurnEnd_EmptyTranscriptRef(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "test-session",
		SessionRef: "", // Empty transcript path
	}

	err := handleLifecycleTurnEnd(context.Background(), ag, event)
	if err == nil {
		t.Error("expected error for empty transcript ref, got nil")
	}
	if !strings.Contains(err.Error(), "transcript file not specified") {
		t.Errorf("expected error about transcript file, got: %v", err)
	}
}

func TestHandleLifecycleTurnEnd_NonexistentTranscript(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "test-session",
		SessionRef: "/nonexistent/path/to/transcript.jsonl",
	}

	err := handleLifecycleTurnEnd(context.Background(), ag, event)
	if err == nil {
		t.Error("expected error for nonexistent transcript, got nil")
	}
	if !strings.Contains(err.Error(), "transcript file not found") {
		t.Errorf("expected error about transcript file, got: %v", err)
	}
}

// mockPreparerAgent is a mock that implements TranscriptPreparer.
// It creates the transcript file when PrepareTranscript is called,
// simulating OpenCode's lazy-fetch behavior.
type mockPreparerAgent struct {
	mockLifecycleAgent

	prepareTranscriptCalled bool
}

var _ agent.TranscriptPreparer = (*mockPreparerAgent)(nil)

func (m *mockPreparerAgent) PrepareTranscript(_ context.Context, sessionRef string) error {
	m.prepareTranscriptCalled = true
	// Create the file (simulating opencode export writing to disk)
	if err := os.MkdirAll(filepath.Dir(sessionRef), 0o750); err != nil {
		return err
	}
	return os.WriteFile(sessionRef, m.transcriptData, 0o600)
}

func TestHandleLifecycleTurnEnd_PreparerCreatesFile(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupGitRepoWithCommit(t, tmpDir)
	paths.ClearWorktreeRootCache()

	// Transcript file does NOT exist yet — PrepareTranscript should create it
	transcriptPath := filepath.Join(tmpDir, ".entire", "tmp", "sess-lazy.json")

	ag := &mockPreparerAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-preparer",
			agentType:      "Mock Preparer Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}`),
		},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "sess-lazy",
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	err := handleLifecycleTurnEnd(context.Background(), ag, event)

	// PrepareTranscript should have been called
	if !ag.prepareTranscriptCalled {
		t.Error("expected PrepareTranscript to be called")
	}

	// The handler may fail later (no strategy state, etc), but it should NOT
	// fail with "transcript file not found" — that was the bug.
	if err != nil && strings.Contains(err.Error(), "transcript file not found") {
		t.Errorf("handler failed with 'transcript file not found' — PrepareTranscript was not called before fileExists check: %v", err)
	}
}

func TestHandleLifecycleTurnEnd_EmptyRepository(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize an empty git repo (no commits)
	if err := os.MkdirAll(".git/objects", 0o755); err != nil {
		t.Fatalf("Failed to create .git: %v", err)
	}
	if err := os.WriteFile(".git/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}
	paths.ClearWorktreeRootCache()

	// Create a transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o644); err != nil {
		t.Fatalf("Failed to create transcript: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "test-session",
		SessionRef: transcriptPath,
	}

	err := handleLifecycleTurnEnd(context.Background(), ag, event)

	// Should return nil so the hook exits 0 — agents treat non-zero as failure.
	// The user was already warned at session start.
	if err != nil {
		t.Errorf("expected nil for empty repository (graceful no-op), got: %v", err)
	}
}

// TestTurnFlow_StatusBudgetBreachDegradesGracefully pins the zombie-hook
// incident regression (stray `git init` in $HOME): when the worktree status
// walk breaches its wall-clock budget, both turn hooks must still succeed —
// the agent treats a non-zero hook exit as failure — and turn-end must
// checkpoint the transcript-derived files while skipping new-file detection,
// so pre-existing untracked files are not misattributed to the agent. Before
// the budget existed the failure mode was worse (hook ground for hours), but
// the degrade path itself is what this test locks in.
func TestTurnFlow_StatusBudgetBreachDegradesGracefully(t *testing.T) {
	// Not parallel: t.Chdir plus the process-global status budget latch.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "README.md", "# test\n")
	testutil.GitAdd(t, tmpDir, "README.md")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	gitrepo.SetStatusBudgetBreachedForTesting(true)
	t.Cleanup(func() { gitrepo.SetStatusBudgetBreachedForTesting(false) })

	ctx := context.Background()
	sessionID := "sess-budget-breach"

	// Pre-existing untracked file that must not be claimed by the checkpoint.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "preexisting.txt"), []byte("x"), 0o600))

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	// Agents report absolute paths; resolve symlinks (macOS /var → /private/var)
	// so the path normalizes against the repo root the handler resolves.
	resolvedDir, err := filepath.EvalSymlinks(tmpDir)
	require.NoError(t, err)
	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: &mockLifecycleAgent{
			name:           "mock-analyzer",
			agentType:      "Mock Analyzer Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}`),
		},
		analyzerFiles: []string{filepath.Join(resolvedDir, "agent.txt")},
	}

	// Turn start: the untracked scan is degraded, not fatal.
	startEvent := &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Prompt:     "write agent.txt",
		Timestamp:  time.Now(),
	}
	require.NoError(t, handleLifecycleTurnStart(ctx, ag, startEvent))

	preState, err := LoadPrePromptState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, preState)
	require.True(t, preState.UntrackedScanSkipped, "breached scan must be recorded so turn-end skips new-file detection")

	// The agent writes its file during the turn.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "agent.txt"), []byte("agent"), 0o600))

	endEvent := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}
	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, endEvent), "turn-end must exit 0 when status is unavailable")

	// Capture proceeded from transcript-derived data: the agent's file is in
	// the session state, the pre-existing untracked file is not.
	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "agent.txt")
	require.NotContains(t, state.FilesTouched, "preexisting.txt")
	require.NotNil(t, state.CaptureDegradedAt, "the breach must be persisted so `entire status` can surface it")
}

// mockPromptAgent adds PromptExtractor so turn-end's LastPrompt backfill runs.
type mockPromptAgent struct {
	mockAnalyzerAgent

	prompts []string
}

var _ agent.PromptExtractor = (*mockPromptAgent)(nil)

func (m *mockPromptAgent) ExtractPrompts(string, int) ([]string, error) { return m.prompts, nil }

// TestHandleLifecycleTurnEnd_SaveBreachStillBackfillsLastPrompt pins the
// degrade-path bookkeeping: when the first-checkpoint status read inside
// SaveStep breaches its budget, the checkpoint is skipped and the hook exits
// 0 — but the turn-end tail must still run, backfilling LastPrompt (SaveStep
// initialized session state before failing, wiping any earlier value) and
// persisting the capture-degraded marker.
func TestHandleLifecycleTurnEnd_SaveBreachStillBackfillsLastPrompt(t *testing.T) {
	// Not parallel: t.Chdir plus the package-global git-status budget override.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "README.md", "# test\n")
	testutil.GitAdd(t, tmpDir, "README.md")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	t.Cleanup(checkpoint.SetGitStatusBudgetForTesting(time.Nanosecond))

	ctx := context.Background()
	sessionID := "sess-save-breach-backfill"

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	resolvedDir, err := filepath.EvalSymlinks(tmpDir)
	require.NoError(t, err)
	ag := &mockPromptAgent{
		mockAnalyzerAgent: mockAnalyzerAgent{
			mockLifecycleAgent: &mockLifecycleAgent{
				name:           "mock-prompt",
				agentType:      "Mock Prompt Agent",
				transcriptData: []byte(`{"type":"user","message":"test"}`),
			},
			analyzerFiles: []string{filepath.Join(resolvedDir, "agent.txt")},
		},
		prompts: []string{"write agent.txt"},
	}

	// Turn start without a prompt (exec-mode shape, e.g. Factory Droid):
	// prompt.txt stays empty so turn-end must backfill from the transcript.
	startEvent := &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}
	require.NoError(t, handleLifecycleTurnStart(ctx, ag, startEvent))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "agent.txt"), []byte("agent"), 0o600))

	endEvent := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}
	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, endEvent), "save breach must degrade, not fail the hook")

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 0, state.StepCount, "the breached save must not have produced a checkpoint")
	require.Equal(t, "write agent.txt", state.LastPrompt, "the backfill must run on the degrade path too")
	require.NotNil(t, state.CaptureDegradedAt, "the save breach must be persisted so `entire status` can surface it")
}

// TestHandleLifecycleTurnEnd_ScanSkippedMarkerSkipsNewFileDetection pins the
// cross-process degrade shape the same-process test above cannot reach: turn
// start runs in one hook process whose status walk breaches (writing the
// UntrackedScanSkipped marker), while turn end runs in a fresh process whose
// walk SUCCEEDS. PreUntrackedFiles() converts the marker state's nil
// UntrackedFiles into a non-nil empty baseline, so without the changes.New
// guard in handleLifecycleTurnEnd every pre-existing untracked file in the
// worktree would be classified New and claimed by the checkpoint. Deleting
// that guard must fail this test.
func TestHandleLifecycleTurnEnd_ScanSkippedMarkerSkipsNewFileDetection(t *testing.T) {
	// Not parallel: t.Chdir plus the process-global status budget latch.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "README.md", "# test\n")
	testutil.GitAdd(t, tmpDir, "README.md")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ctx := context.Background()
	sessionID := "sess-marker-cross-process"

	// Pre-existing untracked file the checkpoint must not claim.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "preexisting.txt"), []byte("x"), 0o600))

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	resolvedDir, err := filepath.EvalSymlinks(tmpDir)
	require.NoError(t, err)
	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: &mockLifecycleAgent{
			name:           "mock-analyzer",
			agentType:      "Mock Analyzer Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}`),
		},
		analyzerFiles: []string{filepath.Join(resolvedDir, "agent.txt")},
	}

	// Process A: turn start under a breached budget writes the marker.
	gitrepo.SetStatusBudgetBreachedForTesting(true)
	startEvent := &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Prompt:     "write agent.txt",
		Timestamp:  time.Now(),
	}
	require.NoError(t, handleLifecycleTurnStart(ctx, ag, startEvent))
	// Process B: turn end runs with a fresh latch and a healthy walk.
	gitrepo.SetStatusBudgetBreachedForTesting(false)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "agent.txt"), []byte("agent"), 0o600))

	endEvent := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}
	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, endEvent))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "agent.txt")
	require.NotContains(t, state.FilesTouched, "preexisting.txt",
		"marker must disable new-file detection: with no baseline, pre-existing untracked files would be misattributed to the agent")
	require.NotNil(t, state.CaptureDegradedAt,
		"a marker-degraded turn must be persisted even when the end-hook walk succeeds")

	// A following turn whose scans stay within budget clears the marker: the
	// warning means "the LAST turn degraded", not "some turn once did".
	require.NoError(t, handleLifecycleTurnStart(ctx, ag, startEvent))
	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, endEvent))
	state, err = strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, state.CaptureDegradedAt, "a healthy turn must clear the degradation marker")
}

// TestHandleLifecycleSubagentEnd_ScanSkippedMarkerSkipsNewFileDetection is the
// subagent-path twin of the turn-end marker test: the pre-task scan breaches
// in one process, subagent-end's walk succeeds in another, and without the
// changes.New guard in handleLifecycleSubagentEnd the task checkpoint would
// claim every pre-existing untracked file. Deleting that guard must fail this
// test.
func TestHandleLifecycleSubagentEnd_ScanSkippedMarkerSkipsNewFileDetection(t *testing.T) {
	// Not parallel: t.Chdir plus the process-global status budget latch.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "README.md", "# test\n")
	testutil.GitAdd(t, tmpDir, "README.md")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ctx := context.Background()
	sessionID := "sess-task-marker"
	toolUseID := "toolu-cross-process-01"

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "preexisting.txt"), []byte("x"), 0o600))

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	resolvedDir, err := filepath.EvalSymlinks(tmpDir)
	require.NoError(t, err)
	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: &mockLifecycleAgent{
			name:           "mock-analyzer",
			agentType:      "Mock Analyzer Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}`),
		},
		analyzerFiles: []string{filepath.Join(resolvedDir, "task.txt")},
	}

	// Process A: pre-task capture under a breached budget writes the marker.
	gitrepo.SetStatusBudgetBreachedForTesting(true)
	require.NoError(t, CapturePreTaskState(ctx, toolUseID))
	gitrepo.SetStatusBudgetBreachedForTesting(false)

	preState, err := LoadPreTaskState(ctx, toolUseID)
	require.NoError(t, err)
	require.NotNil(t, preState)
	require.True(t, preState.UntrackedScanSkipped)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "task.txt"), []byte("task"), 0o600))

	endEvent := &agent.Event{
		Type:       agent.SubagentEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		ToolUseID:  toolUseID,
		Timestamp:  time.Now(),
	}
	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, endEvent))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "task.txt")
	require.NotContains(t, state.FilesTouched, "preexisting.txt",
		"marker must disable new-file detection on the subagent path")
}

// TestHandleClaudeCodePostTodo_ScanSkippedMarkerSkipsNewFileDetection is the
// PostTodo twin of the turn-end/subagent-end marker tests: PostTodo detects
// changes with a nil baseline (all untracked files classified New), so after
// a pre-task scan breach (marker written in one process) a PostTodo whose own
// walk succeeds would claim every pre-existing untracked file in the
// incremental checkpoint. Deleting the PostTodo marker guard must fail this
// test.
func TestHandleClaudeCodePostTodo_ScanSkippedMarkerSkipsNewFileDetection(t *testing.T) {
	// Not parallel: t.Chdir, the process-global status budget latch, and the
	// currentHookAgentName hook-context global.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "README.md", "# test\n")
	testutil.GitAdd(t, tmpDir, "README.md")
	testutil.GitCommit(t, tmpDir, "init")
	// PostTodo skips on the default branch; incremental checkpoints only run
	// on feature branches.
	testutil.GitCheckoutNewBranch(t, tmpDir, "feature/posttodo")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	currentHookAgentName = agent.AgentNameClaudeCode
	t.Cleanup(func() { currentHookAgentName = "" })

	ctx := context.Background()
	toolUseID := "toolu-posttodo-01"

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "preexisting.txt"), []byte("x"), 0o600))

	// Process A: pre-task capture under a breached budget writes the marker.
	gitrepo.SetStatusBudgetBreachedForTesting(true)
	require.NoError(t, CapturePreTaskState(ctx, toolUseID))
	gitrepo.SetStatusBudgetBreachedForTesting(false)

	// The subagent modifies a tracked file during the task, so the incremental
	// checkpoint still has real content to save.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# test\nmodified\n"), 0o600))

	input := `{"session_id":"sess-posttodo","transcript_path":"","tool_name":"TodoWrite",` +
		`"tool_use_id":"toolu-todowrite-01","tool_input":{"todos":[{"content":"do work","status":"completed"}]}}`
	require.NoError(t, handleClaudeCodePostTodoFromReader(ctx, strings.NewReader(input)))

	state, err := strategy.LoadSessionState(ctx, "sess-posttodo")
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "README.md")
	require.NotContains(t, state.FilesTouched, "preexisting.txt",
		"marker must disable new-file detection on the PostTodo path")
}

// --- handleLifecycleCompaction tests ---

func TestHandleLifecycleCompaction_PreservesTranscriptOffset(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with a commit (not empty)
	setupGitRepoWithCommit(t, tmpDir)
	paths.ClearWorktreeRootCache()

	// Create .entire directory structure
	if err := os.MkdirAll(paths.EntireDir, 0o755); err != nil {
		t.Fatalf("Failed to create .entire: %v", err)
	}

	// Create a transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	transcriptContent := `{"type":"user","message":{"role":"user","content":"test prompt"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
		t.Fatalf("Failed to create transcript: %v", err)
	}

	sessionID := "compaction-test-session"

	// Create session state with non-zero transcript offset (set by prior condensation)
	sessionState := &strategy.SessionState{
		SessionID:                 sessionID,
		StartedAt:                 time.Now(),
		CheckpointTranscriptStart: 50,
	}
	if err := strategy.SaveSessionState(context.Background(), sessionState); err != nil {
		t.Fatalf("Failed to save session state: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.Compaction,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
	}

	// Compaction should NOT reset the transcript offset.
	// Many agents (e.g., Gemini) fire pre-compress as a no-op after every tool call;
	// resetting the offset causes stale files to re-appear in carry-forward.
	err := handleLifecycleCompaction(context.Background(), ag, event)
	if err != nil {
		t.Logf("handleLifecycleCompaction returned error (expected in minimal test): %v", err)
	}

	// Verify CheckpointTranscriptStart was preserved (not reset to 0)
	loadedState, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("Failed to load session state after compaction: %v", loadErr)
	}
	require.NotNil(t, loadedState, "Session state is nil after compaction")
	if loadedState.CheckpointTranscriptStart != 50 {
		t.Errorf("CheckpointTranscriptStart = %d, want 50 (compaction should preserve offset)",
			loadedState.CheckpointTranscriptStart)
	}
}

// --- handleLifecycleSessionEnd tests ---

func TestHandleLifecycleSessionEnd_EmptySessionID(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: "", // Empty
	}

	// Empty session ID should return nil (no error, just no-op)
	err := handleLifecycleSessionEnd(context.Background(), ag, event)
	if err != nil {
		t.Errorf("expected no error for empty session ID on SessionEnd, got: %v", err)
	}
}

// --- resolveTranscriptOffset tests ---

func TestResolveTranscriptOffset_PrefersPrePromptState(t *testing.T) {
	t.Parallel()

	preState := &PrePromptState{
		TranscriptOffset: 42,
	}

	offset := resolveTranscriptOffset(context.Background(), preState, "test-session")
	if offset != 42 {
		t.Errorf("expected offset 42 from pre-prompt state, got %d", offset)
	}
}

func TestResolveTranscriptOffset_NilPrePromptState(t *testing.T) {
	t.Parallel()

	// With nil pre-prompt state and no session state, should return 0
	offset := resolveTranscriptOffset(context.Background(), nil, "nonexistent-session")
	if offset != 0 {
		t.Errorf("expected offset 0 for nil pre-prompt state, got %d", offset)
	}
}

func TestResolveTranscriptOffset_ZeroOffsetInPrePromptState(t *testing.T) {
	t.Parallel()

	preState := &PrePromptState{
		TranscriptOffset: 0, // Zero should fall through to session state
	}

	// With zero in pre-prompt state and no session state, should return 0
	offset := resolveTranscriptOffset(context.Background(), preState, "nonexistent-session")
	if offset != 0 {
		t.Errorf("expected offset 0, got %d", offset)
	}
}

// --- Event type routing tests ---

func TestDispatchLifecycleEvent_RoutesToCorrectHandler(t *testing.T) {
	// NOT parallel: uses t.Chdir to isolate from real repo state.
	// Without this, the SubagentEnd case creates .git/entire-sessions/test.json
	// in the real repo whenever untracked files exist, because DetectFileChanges
	// reports them as new files and SaveTaskStep falls back to initializeSession.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	// Test that each event type is routed (we can't easily verify which handler
	// was called without dependency injection, but we can verify no panic and
	// expected error types for each event type with minimal required data)

	testCases := []struct {
		name        string
		eventType   agent.EventType
		sessionID   string
		expectError bool
		errorSubstr string
	}{
		{
			name:        "SessionStart with empty session ID",
			eventType:   agent.SessionStart,
			sessionID:   "",
			expectError: true,
			errorSubstr: "no session_id",
		},
		{
			name:        "TurnStart with empty session ID",
			eventType:   agent.TurnStart,
			sessionID:   "",
			expectError: true,
			errorSubstr: "no session_id",
		},
		{
			name:        "TurnEnd with empty transcript",
			eventType:   agent.TurnEnd,
			sessionID:   "test",
			expectError: true,
			errorSubstr: "transcript file not specified",
		},
		{
			name:        "Compaction with empty transcript is no-op",
			eventType:   agent.Compaction,
			sessionID:   "test",
			expectError: false, // Compaction just resets offset; doesn't read transcript
		},
		{
			name:        "SessionEnd with empty session ID is no-op",
			eventType:   agent.SessionEnd,
			sessionID:   "",
			expectError: false,
		},
		{
			name:        "SubagentStart with valid data",
			eventType:   agent.SubagentStart,
			sessionID:   "test",
			expectError: true, // Will fail due to CapturePreTaskState needing git repo
			errorSubstr: "failed to capture pre-task state",
		},
		{
			name:        "SubagentEnd with valid data",
			eventType:   agent.SubagentEnd,
			sessionID:   "test",
			expectError: false, // Succeeds when run from a valid git repo
		},
		{
			name:        "ModelUpdate with empty model is no-op",
			eventType:   agent.ModelUpdate,
			sessionID:   "test",
			expectError: false,
		},
		{
			name:        "ModelUpdate with empty session ID is no-op",
			eventType:   agent.ModelUpdate,
			sessionID:   "",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ag := newMockAgent()
			event := &agent.Event{
				Type:      tc.eventType,
				SessionID: tc.sessionID,
				Timestamp: time.Now(),
			}

			err := DispatchLifecycleEvent(context.Background(), ag, event)

			if tc.expectError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errorSubstr)
				} else if !strings.Contains(err.Error(), tc.errorSubstr) {
					t.Errorf("expected error containing %q, got: %v", tc.errorSubstr, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			}
		})
	}
}

// --- Helper functions for test setup ---

// setupGitRepoWithCommit initializes a git repo with an initial commit.
func setupGitRepoWithCommit(t *testing.T, dir string) {
	t.Helper()

	// Initialize git repo
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("Failed to create .git/objects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatalf("Failed to create .git/refs/heads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}

	// Create a dummy file to commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("Failed to create README.md: %v", err)
	}

	// Use go-git to create an initial commit
	repo, err := strategy.OpenRepository(context.Background())
	if err != nil {
		// If we can't open with go-git, the empty repo check will work differently
		t.Logf("Note: Could not open repository with go-git: %v", err)
		return
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Logf("Note: Could not get worktree: %v", err)
		return
	}

	if _, err := wt.Add("README.md"); err != nil {
		t.Logf("Note: Could not add file: %v", err)
		return
	}

	if _, err := wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	}); err != nil {
		t.Logf("Note: Could not create commit: %v", err)
	}
}

// --- Prompt backfill tests ---

// mockPromptExtractorAgent implements PromptExtractor for lifecycle tests.
type mockPromptExtractorAgent struct {
	mockLifecycleAgent

	prompts    []string
	extractErr error
}

var _ agent.PromptExtractor = (*mockPromptExtractorAgent)(nil)

func (m *mockPromptExtractorAgent) ExtractPrompts(string, int) ([]string, error) {
	return m.prompts, m.extractErr
}

func TestHandleLifecycleTurnStart_WritesPromptContent(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	sessionID := "test-prompt-content"
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "create a file called hello.txt",
		Timestamp: time.Now(),
	}

	require.NoError(t, handleLifecycleTurnStart(context.Background(), ag, event))

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr)

	if string(data) != "create a file called hello.txt" {
		t.Errorf("expected prompt content 'create a file called hello.txt', got %q", string(data))
	}
}

// TestHandleLifecycleTurnEnd_PrefersEventTokenUsage verifies that when the
// hook payload reports per-turn token usage (e.g., Cursor's stop hook),
// the lifecycle handler uses those numbers verbatim instead of falling back
// to transcript-based computation. This is the only way Cursor sessions get
// non-zero token data, since Cursor's JSONL transcript has no usage fields.
func TestHandleLifecycleTurnEnd_PrefersEventTokenUsage(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	// Modify a file so SaveStep actually runs.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "init.txt"), []byte("changed"), 0o600))

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	sessionID := "test-prefer-event-tokens"
	ag := newMockAgent()
	ag.transcriptData = []byte(`{"type":"user","message":"test"}` + "\n")

	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
		TokenUsage: &agent.TokenUsage{
			InputTokens:         200,
			CacheReadTokens:     4000,
			CacheCreationTokens: 800,
			OutputTokens:        50,
			APICallCount:        1,
		},
	}

	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	state, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.NotNil(t, state.TokenUsage, "session state TokenUsage must be populated from event.TokenUsage")
	require.Equal(t, 200, state.TokenUsage.InputTokens, "InputTokens must match event-provided value, not transcript-derived")
	require.Equal(t, 4000, state.TokenUsage.CacheReadTokens)
	require.Equal(t, 800, state.TokenUsage.CacheCreationTokens)
	require.Equal(t, 50, state.TokenUsage.OutputTokens)
	require.Equal(t, 1, state.TokenUsage.APICallCount)
}

type mockContextInjectorAgent struct {
	mockLifecycleAgent
}

var _ agent.ContextInjector = (*mockContextInjectorAgent)(nil)

func (m *mockContextInjectorAgent) InjectionEvent() agent.EventType { return agent.TurnStart }

func (m *mockContextInjectorAgent) RenderContextInjection(agent.ContextInjection) ([]byte, error) {
	return nil, nil
}

func addGitHubOriginForLifecycleTest(t *testing.T, repoDir string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@github.com:acme/repo.git")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}

func TestHandleLifecycleTurnStart_ContextInjectionUnknownCacheDoesNotMarkDecided(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir().
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	addGitHubOriginForLifecycleTest(t, tmpDir)
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	ag := &mockContextInjectorAgent{mockLifecycleAgent: *newMockAgent()}
	sessionID := "test-trail-inject-unknown"
	event := &agent.Event{Type: agent.TurnStart, SessionID: sessionID, Prompt: "hello", Timestamp: time.Now()}

	require.NoError(t, handleLifecycleTurnStart(context.Background(), ag, event))

	state, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.False(t, state.ContextInjectionDecided, "unknown/missing cache should not permanently suppress later injection")
}

func TestHandleLifecycleTurnStart_ContextInjectionFreshTrueMarksDecided(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir().
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	addGitHubOriginForLifecycleTest(t, tmpDir)
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	require.NoError(t, saveTrailsEnabledForRepo(context.Background(), true))

	ag := &mockContextInjectorAgent{mockLifecycleAgent: *newMockAgent()}
	sessionID := "test-trail-inject-true"
	scope, err := currentTrailEnablementScope(context.Background())
	require.NoError(t, err)
	require.NoError(t, saveTrailEnablementScopeHint(context.Background(), sessionID, scope))
	event := &agent.Event{Type: agent.TurnStart, SessionID: sessionID, Prompt: "hello", Timestamp: time.Now()}

	require.NoError(t, handleLifecycleTurnStart(context.Background(), ag, event))

	state, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.True(t, state.ContextInjectionDecided, "fresh true cache should make a final injection decision")
}

func TestHandleLifecycleTurnStart_RecordsGenericSkillSlashEvent(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	sessionID := "test-generic-skill-slash"
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "/skill:trigger-analysis inspect the implementation",
		Timestamp: time.Date(2026, 5, 25, 12, 34, 56, 0, time.UTC),
	}

	require.NoError(t, handleLifecycleTurnStart(context.Background(), ag, event))

	state, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Len(t, state.SkillEvents, 1)

	skillEvent := state.SkillEvents[0]
	require.Equal(t, agent.SkillEventTypePromptInvocation, skillEvent.EventType)
	require.Equal(t, "trigger-analysis", skillEvent.Skill.Name)
	require.Equal(t, string(ag.Name()), skillEvent.Source.Agent)
	require.Equal(t, agent.SkillSignalPromptSlashCommand, skillEvent.Source.Signal)
	require.Equal(t, agent.SkillConfidenceExplicit, skillEvent.Source.Confidence)
	require.Equal(t, state.TurnID, skillEvent.TurnID)
	require.Equal(t, "2026-05-25T12:34:56Z", skillEvent.Timestamp)
	require.Equal(t, "/skill:trigger-analysis", skillEvent.Native["command"])
	require.Equal(t, agent.SkillCollapseTargetUserMessage, skillEvent.Collapse.Target)
	require.True(t, skillEvent.Collapse.DefaultCollapsed)
}

func TestHandleLifecycleTurnStart_DoesNotDuplicateGenericSkillSlashEventFromForwardedHook(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	sessionID := "test-generic-skill-forwarded"
	ownerAgent := newMockAgent()
	forwardedAgent := &mockLifecycleAgent{
		name:           "forwarded-agent",
		agentType:      "Forwarded Agent",
		transcriptData: []byte(`{"type":"user","message":"test"}`),
	}
	prompt := "/skill:trigger-analysis inspect the implementation"

	require.NoError(t, handleLifecycleTurnStart(context.Background(), ownerAgent, &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    prompt,
		Timestamp: time.Date(2026, 5, 25, 12, 34, 56, 0, time.UTC),
	}))
	require.NoError(t, handleLifecycleTurnStart(context.Background(), forwardedAgent, &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    prompt,
		Timestamp: time.Date(2026, 5, 25, 12, 34, 57, 0, time.UTC),
	}))

	state, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, ownerAgent.Type(), state.AgentType)
	require.Len(t, state.SkillEvents, 1)
	require.Equal(t, string(ownerAgent.Name()), state.SkillEvents[0].Source.Agent)
}

func TestHandleLifecycleTurnEnd_BackfillsPromptFromTranscript(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	// Create a transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	sessionID := "test-backfill"
	ag := &mockPromptExtractorAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-prompt",
			agentType:      "Mock Prompt Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}` + "\n"),
		},
		prompts: []string{"create a file called notes/deep.md"},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	// Do NOT create prompt.txt — simulating hooks never firing.
	// TurnEnd should backfill from transcript via PromptExtractor.
	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr, "prompt.txt should have been created by backfill")

	if string(data) != "create a file called notes/deep.md" {
		t.Errorf("expected backfilled prompt, got %q", string(data))
	}
}

func TestHandleLifecycleTurnEnd_NoBackfillWhenPromptFileHasContent(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	sessionID := "test-no-backfill"
	ag := &mockPromptExtractorAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-prompt",
			agentType:      "Mock Prompt Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}` + "\n"),
		},
		prompts: []string{"should NOT appear"},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	// Pre-create prompt.txt with content — simulating hooks that captured the prompt.
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(sessionDirAbs, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDirAbs, paths.PromptFileName), []byte("original prompt"), 0o600))

	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr)

	if string(data) != "original prompt" {
		t.Errorf("expected original prompt preserved, got %q", string(data))
	}
}

func TestHandleLifecycleTurnEnd_BackfillUpdatesSessionState(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	sessionID := "test-backfill-state"
	ag := &mockPromptExtractorAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-prompt",
			agentType:      "Mock Prompt Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}` + "\n"),
		},
		prompts: []string{"first prompt", "second prompt"},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	// Pre-create session state with BaseCommit set (simulating InitializeSession
	// that ran during TurnStart but with empty prompt due to exec mode).
	// BaseCommit must be set so SaveStep doesn't reinitialize the state.
	repo, err := strategy.OpenRepository(context.Background())
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	state := &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: head.Hash().String(),
		LastPrompt: "",
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	// Verify session state was updated with the last prompt
	updated, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)

	if updated.LastPrompt != "second prompt" {
		t.Errorf("expected LastPrompt 'second prompt', got %q", updated.LastPrompt)
	}
}

func TestHandleLifecycleTurnEnd_BackfillsPromptFromOpenCodeTranscript(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	transcript := `{"info":{"id":"ses_test"},"messages":[{"info":{"id":"msg-1","role":"user","time":{"created":1708300000}},"parts":[{"type":"text","text":"create a file called notes/deep.md with a paragraph about deep validation. Do not ask for confirmation or approval, just make the change."}]},{"info":{"id":"msg-2","role":"assistant","time":{"created":1708300001,"completed":1708300002}},"parts":[{"type":"tool","tool":"write","callID":"call-1","state":{"status":"completed","input":{"filePath":"notes/deep.md"},"output":"ok"}}]}]}`
	transcriptPath := filepath.Join(tmpDir, "transcript.json")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o600))

	sessionID := "test-opencode-backfill"
	ag := &opencode.OpenCodeAgent{}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	repo, err := strategy.OpenRepository(context.Background())
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	state := &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: head.Hash().String(),
		LastPrompt: "",
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr)
	require.Contains(t, string(data), "create a file called notes/deep.md")

	updated, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)
	require.Contains(t, updated.LastPrompt, "create a file called notes/deep.md")
}

// TestAdoptReviewEnv_TagsSession verifies that when ENTIRE_REVIEW_* env vars
// are set on the process (as `entire review` sets them on the spawned agent),
// handleLifecycleTurnStart tags the session state with Kind=agent_review,
// ReviewSkills, and ReviewPrompt.
func TestAdoptReviewEnv_TagsSession(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	t.Setenv(review.EnvSession, "1")
	t.Setenv(review.EnvAgent, string(ag.Name()))
	t.Setenv(review.EnvStartingSHA, testutil.GetHeadHash(t, tmp))
	skillsJSON, encErr := review.EncodeSkills([]string{"/pr-review-toolkit:review-pr"})
	if encErr != nil {
		t.Fatalf("encode skills: %v", encErr)
	}
	t.Setenv(review.EnvSkills, skillsJSON)
	t.Setenv(review.EnvPrompt, "Review this branch.")

	sessionID := "test-review-env-001"
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "Review this branch.",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, event); err != nil {
		t.Fatalf("handleLifecycleTurnStart: %v", err)
	}

	state, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if state == nil {
		t.Fatal("state is nil after turn start")
	}
	if state.Kind != session.KindAgentReview {
		t.Errorf("Kind: got %q, want agent_review", state.Kind)
	}
	if len(state.ReviewSkills) != 1 || state.ReviewSkills[0] != "/pr-review-toolkit:review-pr" {
		t.Errorf("ReviewSkills: got %v", state.ReviewSkills)
	}
	if state.ReviewPrompt != "Review this branch." {
		t.Errorf("ReviewPrompt: got %q", state.ReviewPrompt)
	}
}

// TestAdoptReviewEnv_NormalSession verifies that when ENTIRE_REVIEW_SESSION is
// not set, handleLifecycleTurnStart leaves Kind empty (normal coding session).
func TestAdoptReviewEnv_NormalSession(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	// Explicitly ensure the review env vars are absent.
	t.Setenv(review.EnvSession, "")

	sessionID := "test-review-env-002"
	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "Hello.",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, event); err != nil {
		t.Fatalf("handleLifecycleTurnStart: %v", err)
	}

	state, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if state == nil {
		t.Fatal("state is nil after turn start")
	}
	if state.Kind != "" {
		t.Errorf("Kind: got %q, want empty (normal session)", state.Kind)
	}
}

func TestAdoptReviewEnv_WrongAgentLeavesUntagged(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	t.Setenv(review.EnvSession, "1")
	t.Setenv(review.EnvAgent, "other-agent")
	t.Setenv(review.EnvStartingSHA, testutil.GetHeadHash(t, tmp))
	t.Setenv(review.EnvSkills, "[]")
	t.Setenv(review.EnvPrompt, "Review this branch.")

	sessionID := "test-review-env-wrong-agent"
	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "Review this branch.",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, event); err != nil {
		t.Fatalf("handleLifecycleTurnStart: %v", err)
	}

	state, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if state == nil {
		t.Fatal("state is nil after turn start")
	}
	if state.Kind != "" {
		t.Errorf("Kind: got %q, want empty for wrong agent", state.Kind)
	}
}

func TestAdoptReviewEnv_StaleStartingSHALeavesUntagged(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	t.Setenv(review.EnvSession, "1")
	t.Setenv(review.EnvAgent, string(ag.Name()))
	t.Setenv(review.EnvStartingSHA, strings.Repeat("0", 40))
	t.Setenv(review.EnvSkills, "[]")
	t.Setenv(review.EnvPrompt, "Review this branch.")

	sessionID := "test-review-env-stale-sha"
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "Review this branch.",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, event); err != nil {
		t.Fatalf("handleLifecycleTurnStart: %v", err)
	}

	state, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if state == nil {
		t.Fatal("state is nil after turn start")
	}
	if state.Kind != "" {
		t.Errorf("Kind: got %q, want empty for stale starting SHA", state.Kind)
	}
}

// TestAdoptReviewEnv_MalformedSkillsLeavesUntagged verifies that when
// ENTIRE_REVIEW_SKILLS contains malformed JSON, adoptReviewEnv logs a warning
// and leaves the session untagged rather than corrupting metadata.
func TestAdoptReviewEnv_MalformedSkillsLeavesUntagged(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	t.Setenv(review.EnvSession, "1")
	t.Setenv(review.EnvSkills, "not json {[") // malformed JSON
	t.Setenv(review.EnvAgent, string(ag.Name()))
	t.Setenv(review.EnvStartingSHA, testutil.GetHeadHash(t, tmp))
	t.Setenv(review.EnvPrompt, "anything")

	sessionID := "test-review-env-malformed"
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "anything",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, event); err != nil {
		t.Fatalf("handleLifecycleTurnStart: %v", err)
	}

	state, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if state == nil {
		t.Fatal("state is nil after turn start")
	}
	if state.Kind != "" {
		t.Errorf("Kind: got %q, want empty (malformed skills must not tag session)", state.Kind)
	}
	if len(state.ReviewSkills) != 0 {
		t.Errorf("ReviewSkills: got %v, want empty", state.ReviewSkills)
	}
	if state.ReviewPrompt != "" {
		t.Errorf("ReviewPrompt: got %q, want empty", state.ReviewPrompt)
	}
}

// TestAdoptReviewEnv_AlreadyTaggedNotOverwritten verifies that adoptReviewEnv
// is idempotent: when state.Kind is already set (e.g. on a subsequent turn of
// a review session), the function returns without modifying state.
func TestAdoptReviewEnv_AlreadyTaggedNotOverwritten(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	sessionID := "test-review-env-already-tagged"
	ag := newMockAgent()

	// Run a full first turn with ENTIRE_REVIEW_* set so the session is tagged.
	t.Setenv(review.EnvSession, "1")
	oldSkillsJSON, encErr := review.EncodeSkills([]string{"/old-skill"})
	if encErr != nil {
		t.Fatalf("encode old skills: %v", encErr)
	}
	t.Setenv(review.EnvSkills, oldSkillsJSON)
	t.Setenv(review.EnvAgent, string(ag.Name()))
	t.Setenv(review.EnvStartingSHA, testutil.GetHeadHash(t, tmp))
	t.Setenv(review.EnvPrompt, "old prompt")

	firstTurn := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "old prompt",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, firstTurn); err != nil {
		t.Fatalf("first handleLifecycleTurnStart: %v", err)
	}

	// Verify the first turn tagged the session correctly.
	stateAfterFirst, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("load state after first turn: %v", loadErr)
	}
	if stateAfterFirst == nil || stateAfterFirst.Kind != session.KindAgentReview {
		t.Fatalf("first turn did not tag session; Kind=%q", stateAfterFirst.Kind)
	}

	// Now change env vars to DIFFERENT values and run a second turn.
	// adoptReviewEnv must short-circuit because Kind is already set.
	newSkillsJSON, encErr2 := review.EncodeSkills([]string{"/new-skill"})
	if encErr2 != nil {
		t.Fatalf("encode new skills: %v", encErr2)
	}
	t.Setenv(review.EnvSkills, newSkillsJSON)
	t.Setenv(review.EnvPrompt, "new prompt")

	secondTurn := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "new prompt",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, secondTurn); err != nil {
		t.Fatalf("second handleLifecycleTurnStart: %v", err)
	}

	state, loadErr2 := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr2 != nil {
		t.Fatalf("load state after second turn: %v", loadErr2)
	}
	if state == nil {
		t.Fatal("state is nil after second turn")
	}
	if state.Kind != session.KindAgentReview {
		t.Errorf("Kind: got %q, want agent_review", state.Kind)
	}
	if len(state.ReviewSkills) != 1 || state.ReviewSkills[0] != "/old-skill" {
		t.Errorf("ReviewSkills: got %v, want [/old-skill] (must not be overwritten on second turn)", state.ReviewSkills)
	}
	if state.ReviewPrompt != "old prompt" {
		t.Errorf("ReviewPrompt: got %q, want %q (must not be overwritten on second turn)", state.ReviewPrompt, "old prompt")
	}
}

// testInvestigateRunID is the placeholder run ID used by the
// adoptInvestigateEnv tests below. Production run IDs are 12 hex chars; the
// adopter does not enforce the format itself, so a fixed test value is fine.
const testInvestigateRunID = "abcdef012345"

// setInvestigateEnv populates all ENTIRE_INVESTIGATE_* env vars for a test
// using t.Setenv (so they are restored at test end). agentName must match
// the hook's agent for adoption to succeed.
func setInvestigateEnv(t *testing.T, agentName, startingSHA, topic string) {
	t.Helper()
	t.Setenv(investigate.EnvSession, "1")
	t.Setenv(investigate.EnvAgent, agentName)
	t.Setenv(investigate.EnvStartingSHA, startingSHA)
	t.Setenv(investigate.EnvRunID, testInvestigateRunID)
	t.Setenv(investigate.EnvTopic, topic)
}

// TestAdoptInvestigateEnv_Success verifies that adoptInvestigateEnv tags the
// session state with Kind=agent_investigate and populates the investigate
// fields when all ENTIRE_INVESTIGATE_* env vars are valid.
func TestAdoptInvestigateEnv_Success(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	headSHA := testutil.GetHeadHash(t, tmp)
	setInvestigateEnv(t, string(ag.Name()), headSHA, "Why is checkout flaky?")

	sessionID := "test-investigate-env-success"
	state := &session.State{
		SessionID:  sessionID,
		BaseCommit: headSHA,
	}
	adoptInvestigateEnv(context.Background(), state, string(ag.Name()))

	if state.Kind != session.KindAgentInvestigate {
		t.Errorf("Kind: got %q, want agent_investigate", state.Kind)
	}
	if state.InvestigateRunID != testInvestigateRunID {
		t.Errorf("InvestigateRunID: got %q", state.InvestigateRunID)
	}
	if state.InvestigateTopic != "Why is checkout flaky?" {
		t.Errorf("InvestigateTopic: got %q", state.InvestigateTopic)
	}
}

// TestAdoptInvestigateEnv_AgentMismatch verifies that adoption is skipped
// (and state is left untouched) when the env's agent does not match the
// expected hook agent.
func TestAdoptInvestigateEnv_AgentMismatch(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	headSHA := testutil.GetHeadHash(t, tmp)
	// Env says claude-code; the hook is "codex" — mismatch must skip adoption.
	setInvestigateEnv(t, "claude-code", headSHA, "topic")

	state := &session.State{
		SessionID:  "test-investigate-env-agent-mismatch",
		BaseCommit: headSHA,
	}
	adoptInvestigateEnv(context.Background(), state, "codex")

	if state.Kind != "" {
		t.Errorf("Kind: got %q, want empty for agent mismatch", state.Kind)
	}
	if state.InvestigateRunID != "" {
		t.Errorf("InvestigateRunID: got %q, want empty", state.InvestigateRunID)
	}
}

// TestAdoptInvestigateEnv_StaleStartingSHA verifies that adoption is skipped
// when the env's starting SHA does not match the session's base commit
// (stale env from an earlier HEAD).
func TestAdoptInvestigateEnv_StaleStartingSHA(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	// "deadbeef" vs state.BaseCommit "cafebabe" — different SHAs.
	setInvestigateEnv(t, string(ag.Name()), "deadbeef", "topic")

	state := &session.State{
		SessionID:  "test-investigate-env-stale-sha",
		BaseCommit: "cafebabe",
	}
	adoptInvestigateEnv(context.Background(), state, string(ag.Name()))

	if state.Kind != "" {
		t.Errorf("Kind: got %q, want empty for stale starting SHA", state.Kind)
	}
}

// TestAdoptInvestigateEnv_AlreadyTaggedNotOverwritten verifies that when a
// session is already tagged (e.g. as a review session by an outer adoption),
// adoptInvestigateEnv short-circuits and does not modify state.
func TestAdoptInvestigateEnv_AlreadyTaggedNotOverwritten(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	headSHA := testutil.GetHeadHash(t, tmp)
	setInvestigateEnv(t, string(ag.Name()), headSHA, "topic")

	// Pre-tag the state as a review session.
	state := &session.State{
		SessionID:    "test-investigate-env-already-tagged",
		BaseCommit:   headSHA,
		Kind:         session.KindAgentReview,
		ReviewPrompt: "review prompt",
		ReviewSkills: []string{"/skill"},
	}
	adoptInvestigateEnv(context.Background(), state, string(ag.Name()))

	if state.Kind != session.KindAgentReview {
		t.Errorf("Kind: got %q, want agent_review (must not be overwritten)", state.Kind)
	}
	if state.InvestigateRunID != "" {
		t.Errorf("InvestigateRunID: got %q, want empty (must not be set)", state.InvestigateRunID)
	}
	if state.InvestigateTopic != "" {
		t.Errorf("InvestigateTopic: got %q, want empty (must not be set)", state.InvestigateTopic)
	}
}

// TestAdoptInvestigateEnv_SessionEnvNotOne verifies that adoption is skipped
// when ENTIRE_INVESTIGATE_SESSION is set to anything other than "1".
func TestAdoptInvestigateEnv_SessionEnvNotOne(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	headSHA := testutil.GetHeadHash(t, tmp)
	t.Setenv(investigate.EnvSession, "0")
	t.Setenv(investigate.EnvAgent, string(ag.Name()))
	t.Setenv(investigate.EnvStartingSHA, headSHA)
	t.Setenv(investigate.EnvRunID, testInvestigateRunID)
	t.Setenv(investigate.EnvTopic, "topic")

	state := &session.State{
		SessionID:  "test-investigate-env-session-not-one",
		BaseCommit: headSHA,
	}
	adoptInvestigateEnv(context.Background(), state, string(ag.Name()))

	if state.Kind != "" {
		t.Errorf("Kind: got %q, want empty when SESSION!=\"1\"", state.Kind)
	}
}

// TestAdoptInvestigateEnv_RejectsBadRunID verifies that an env var
// handshake with a malformed (non-12-hex) or empty RunID does not tag the
// session. This protects downstream condensation from joining on junk run
// IDs leaked via stale shell env or hand-set vars.
// TestAdoptInvestigateEnv_TagsSessionViaHandleLifecycleTurnStart is the
// investigate twin of TestAdoptReviewEnv_TagsSession: it drives
// handleLifecycleTurnStart end-to-end and asserts the persisted session
// state carries Kind=agent_investigate plus the run id/topic decoded from
// the env vars. Distinct from the more focused TestAdoptInvestigateEnv_*
// cases above, which call adoptInvestigateEnv directly.
func TestAdoptInvestigateEnv_TagsSessionViaHandleLifecycleTurnStart(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir() and t.Setenv()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	headSHA := testutil.GetHeadHash(t, tmp)
	setInvestigateEnv(t, string(ag.Name()), headSHA, "Why is checkout flaky?")

	sessionID := "test-investigate-env-via-handle-001"
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "Investigate this.",
		Timestamp: time.Now(),
	}
	if err := handleLifecycleTurnStart(context.Background(), ag, event); err != nil {
		t.Fatalf("handleLifecycleTurnStart: %v", err)
	}

	state, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if state == nil {
		t.Fatal("state is nil after turn start")
	}
	if state.Kind != session.KindAgentInvestigate {
		t.Errorf("Kind: got %q, want agent_investigate", state.Kind)
	}
	if state.InvestigateRunID != testInvestigateRunID {
		t.Errorf("InvestigateRunID: got %q, want %q", state.InvestigateRunID, testInvestigateRunID)
	}
	if state.InvestigateTopic != "Why is checkout flaky?" {
		t.Errorf("InvestigateTopic: got %q", state.InvestigateTopic)
	}
}

func TestAdoptInvestigateEnv_RejectsBadRunID(t *testing.T) {
	cases := []struct {
		name  string
		runID string
	}{
		{"empty", ""},
		{"too short", "abcdef0"},
		{"too long", "abcdef0123456789"},
		{"uppercase", "ABCDEF012345"},
		{"non-hex", "notatallhex!"},
		{"path-traversal attempt", "../../../etc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Cannot use t.Parallel(): t.Chdir + t.Setenv.
			tmp := t.TempDir()
			testutil.InitRepo(t, tmp)
			testutil.WriteFile(t, tmp, "f.txt", "x")
			testutil.GitAdd(t, tmp, "f.txt")
			testutil.GitCommit(t, tmp, "init")
			t.Chdir(tmp)
			paths.ClearWorktreeRootCache()

			ag := newMockAgent()
			headSHA := testutil.GetHeadHash(t, tmp)
			t.Setenv(investigate.EnvSession, "1")
			t.Setenv(investigate.EnvAgent, string(ag.Name()))
			t.Setenv(investigate.EnvStartingSHA, headSHA)
			t.Setenv(investigate.EnvRunID, tc.runID)
			t.Setenv(investigate.EnvTopic, "topic")

			state := &session.State{
				SessionID:  "test-investigate-env-bad-run-id-" + tc.name,
				BaseCommit: headSHA,
			}
			adoptInvestigateEnv(context.Background(), state, string(ag.Name()))

			if state.Kind != "" {
				t.Errorf("Kind: got %q, want empty for bad run ID %q", state.Kind, tc.runID)
			}
			if state.InvestigateRunID != "" {
				t.Errorf("InvestigateRunID: got %q, want empty (must not be set)", state.InvestigateRunID)
			}
		})
	}
}

// promptWindow mirrors strategy.checkpointStepCount (unexported there): the
// displayed step count = SessionTurnCount - PromptWindowBase, floored at 1.
func promptWindow(s *strategy.SessionState) int {
	if w := s.SessionTurnCount - s.PromptWindowBase; w >= 1 {
		return w
	}
	return 1
}

// writeCheckpoint simulates what CondenseSession does to the window state: read
// the count, then set the deferred-reset flag (without zeroing the window).
func writeCheckpoint(s *strategy.SessionState) int {
	n := promptWindow(s)
	s.PromptWindowResetPending = true
	return n
}

// TestPromptWindowDeferredReset exercises the two product-required examples:
// (1) p1,p2,p3 -> A=3 then p4,p5 -> C=2, and (2) two checkpoints with no prompt
// in between report the same count (deferred reset).
func TestPromptWindowDeferredReset(t *testing.T) {
	turn := func(s *strategy.SessionState) {
		persistEventMetadataToState(&agent.Event{Type: agent.TurnEnd}, s)
	}

	s := &strategy.SessionState{}

	// p1,p2,p3 -> checkpoint A => 3
	turn(s)
	turn(s)
	turn(s)
	if got := writeCheckpoint(s); got != 3 {
		t.Fatalf("checkpoint A = %d, want 3", got)
	}

	// Back-to-back: checkpoint B with no prompt in between => same as A (3), not 0.
	if got := writeCheckpoint(s); got != 3 {
		t.Fatalf("back-to-back checkpoint B = %d, want 3", got)
	}

	// The next prompt re-anchors the window to start fresh.
	turn(s) // p4: first prompt of the new window
	if s.PromptWindowResetPending {
		t.Fatalf("ResetPending should be cleared after the first post-checkpoint turn")
	}
	if s.PromptWindowBase != 3 {
		t.Fatalf("PromptWindowBase = %d, want 3 (re-anchored to pre-turn count)", s.PromptWindowBase)
	}
	turn(s) // p5
	if got := writeCheckpoint(s); got != 2 {
		t.Fatalf("checkpoint C = %d, want 2", got)
	}
}

// TestPromptWindowExecModeCumulativeTurnCount verifies the window derives
// correctly when turns arrive as a cumulative hook-reported TurnCount (exec-mode
// agents that never fire UserPromptSubmit/TurnStart), rather than as self-counted
// TurnEnd increments.
func TestPromptWindowExecModeCumulativeTurnCount(t *testing.T) {
	exec := func(s *strategy.SessionState, cumulative int) {
		persistEventMetadataToState(&agent.Event{Type: agent.TurnEnd, TurnCount: cumulative}, s)
	}

	s := &strategy.SessionState{}

	exec(s, 1)
	exec(s, 2)
	exec(s, 3)
	if got := writeCheckpoint(s); got != 3 {
		t.Fatalf("exec checkpoint A = %d, want 3", got)
	}

	exec(s, 4) // re-anchors base to 3
	exec(s, 5)
	if got := writeCheckpoint(s); got != 2 {
		t.Fatalf("exec checkpoint B = %d, want 2", got)
	}
}

// TestPromptWindowStaleHookDoesNotResetEarly guards against a repeated/stale hook
// (same cumulative TurnCount, so the count doesn't actually advance) clearing the
// deferred reset early. If it did, a later back-to-back checkpoint would report 1
// instead of matching the prior checkpoint's count.
func TestPromptWindowStaleHookDoesNotResetEarly(t *testing.T) {
	exec := func(s *strategy.SessionState, cumulative int) {
		persistEventMetadataToState(&agent.Event{Type: agent.TurnEnd, TurnCount: cumulative}, s)
	}

	s := &strategy.SessionState{}
	exec(s, 1)
	exec(s, 2)
	exec(s, 3)
	if got := writeCheckpoint(s); got != 3 {
		t.Fatalf("checkpoint A = %d, want 3", got)
	}

	// Stale hook: same cumulative count, no real advance — must not re-anchor.
	exec(s, 3)
	if !s.PromptWindowResetPending {
		t.Fatalf("stale hook should not clear ResetPending")
	}
	if got := writeCheckpoint(s); got != 3 {
		t.Fatalf("back-to-back checkpoint B after stale hook = %d, want 3", got)
	}
}

// TestHandleLifecycleSessionStart_NoSynchronousNetworkForTrailEnablement
// guards against SessionStart hooks stalling agent startup: the
// trails-enablement cache refresh must be handed off to a detached subprocess,
// never performed inline on the SessionStart hook path. A slow/unreachable API
// host previously added up to trailEnablementSessionStartRefreshTimeout (1s) of
// synchronous latency to every session start once the hourly cache went stale.
//
// The deterministic guarantee is the spawn seam: SessionStart must invoke the
// detached-refresh spawn exactly once and return without doing the network work
// itself. As a production-shaped backstop the API base points at a blackholed
// https host that accepts the TCP connection but never answers — so a
// regression that dials inline both contacts that host (dialed > 0) and burns
// the ~1s session-start budget instead of returning immediately. (Plain http
// would be rejected by api.RequireSecureURL before any dial, so the host must
// be https to actually exercise the synchronous-dial path.)
func TestHandleLifecycleSessionStart_NoSynchronousNetworkForTrailEnablement(t *testing.T) {
	setupStopTestRepo(t)
	runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/entirehq/example.git")

	// Blackhole https host: accept connections but never complete the TLS
	// handshake or respond, so an inline dial stalls until a timeout fires
	// (mirrors the unreachable-host case that motivated the detached refresh)
	// rather than failing fast.
	var dialed int32
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			atomic.AddInt32(&dialed, 1)
			_ = conn // hold open; never respond
		}
	}()
	t.Setenv("ENTIRE_API_BASE_URL", "https://"+ln.Addr().String())

	var spawnCount int32
	prevSpawn := trailRefreshSpawn
	trailRefreshSpawn = func(worktreeRoot string) {
		atomic.AddInt32(&spawnCount, 1)
		if worktreeRoot == "" {
			t.Error("expected non-empty worktree root passed to trail refresh spawn")
		}
	}
	t.Cleanup(func() { trailRefreshSpawn = prevSpawn })

	ag := newMockHookResponseAgent()
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "test-no-sync-trail-dial",
		Timestamp: time.Now(),
	}

	start := time.Now()
	err = handleLifecycleSessionStart(context.Background(), ag, event)
	elapsed := time.Since(start)

	require.NoError(t, err)
	// Deterministic guarantee: the network-capable refresh is delegated to the
	// detached spawn exactly once, never run inline.
	if got := atomic.LoadInt32(&spawnCount); got != 1 {
		t.Fatalf("expected exactly one detached trail-enablement refresh spawn, got %d", got)
	}
	// Backstops: SessionStart neither contacted the API host nor blocked.
	if got := atomic.LoadInt32(&dialed); got != 0 {
		t.Fatalf("SessionStart dialed the trails-enablement API synchronously; the refresh must run out of process")
	}
	if elapsed > time.Second {
		t.Fatalf("handleLifecycleSessionStart took %v; trails-enablement refresh must be detached, not synchronous", elapsed)
	}
}

// blockingCellCore is a cellCoreClient that hangs every call until its
// caller's context is done, standing in for a reachable-but-unresponsive
// control plane.
type blockingCellCore struct{}

func (blockingCellCore) GetRepo(ctx context.Context, _ coreapi.GetRepoParams) (*coreapi.Repo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingCellCore) ListClusters(ctx context.Context) (*coreapi.ListClustersOutputBody, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingCellCore) ListRepos(ctx context.Context, _ coreapi.ListReposParams) (*coreapi.ListReposOutputBody, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRunTrailEnablementRefresh_BoundedByTimeoutAgainstUnresponsiveHost
// verifies the deferred refresh work still completes (or at least
// gives up) within its own bounded timeout when the control plane never
// responds — the network work that used to block SessionStart must still
// happen, just out of the hook's critical path, and it must not hang forever.
//
// The hang is injected at the cell-target resolver (newCellCoreClient) rather
// than at a blackholed ENTIRE_API_BASE_URL listener as this test used to:
// resolveRepoCellTarget (cell_target.go) no longer falls back to a live
// network dial when it can't resolve the repo's processing placement — a
// resolution failure is now a fast, direct error — so a blackholed data-API
// host would never be dialed at all and this test would pass for the wrong
// reason (an early return, not a bounded wait).
func TestRunTrailEnablementRefresh_BoundedByTimeoutAgainstUnresponsiveHost(t *testing.T) {
	setupStopTestRepo(t)
	runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/entirehq/example.git")

	var attempted int32
	prevCellCore := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) {
		atomic.AddInt32(&attempted, 1)
		return blockingCellCore{}, nil
	}
	t.Cleanup(func() { newCellCoreClient = prevCellCore })

	start := time.Now()
	refreshErr := runTrailEnablementRefresh(context.Background())
	elapsed := time.Since(start)

	// Best-effort: control-plane failure must not surface as a hard error.
	require.NoError(t, refreshErr)
	if elapsed > trailEnablementRefreshTimeout+2*time.Second {
		t.Fatalf("runTrailEnablementRefresh took %v, expected to give up within roughly %v", elapsed, trailEnablementRefreshTimeout)
	}
	// Prove the test actually exercised the blocking resolver rather than
	// passing via an early return (e.g. scope resolution failing first).
	if got := atomic.LoadInt32(&attempted); got == 0 {
		t.Fatalf("expected the cell-target resolver to have been invoked at least once")
	}
}

// TestRunTrailEnablementRefresh_NotOnboardedSavesDisabledCache guards a
// regression: before the fail-loud cell-resolution rewrite, a not-onboarded
// repo still got a client (the old home-jurisdiction fallback), and the
// subsequent TrailsEnabled API call's 403/404 was what cached enabled=false.
// Now trailRefreshAPIClient fails before a client exists for the exact same
// repos, landing in the generic "authenticated client unavailable" branch,
// which never writes the cache — cachedTrailsEnablementForScope stays
// unknown forever and trailRefreshSpawnThrottle can't stop SessionStart from
// re-forking a refresh child on every invocation. errRepoNotOnboarded is the
// one error this refresh must recognize as a permanent negative and persist.
func TestRunTrailEnablementRefresh_NotOnboardedSavesDisabledCache(t *testing.T) {
	setupStopTestRepo(t)
	runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/entirehq/example.git")

	prevClient := trailRefreshAPIClient
	trailRefreshAPIClient = func(context.Context, bool, string) (*api.Client, error) {
		return nil, fmt.Errorf("resolve the Entire cell for entirehq/example: %w", errRepoNotOnboarded)
	}
	t.Cleanup(func() { trailRefreshAPIClient = prevClient })

	require.NoError(t, runTrailEnablementRefresh(context.Background()))

	scope, err := currentTrailEnablementScope(context.Background())
	require.NoError(t, err)
	if got := cachedTrailsEnablementForScope(context.Background(), scope, time.Now()); got != trailEnablementCacheDisabled {
		t.Fatalf("cached enablement = %v, want trailEnablementCacheDisabled (saved, not left unknown)", got)
	}
}

// TestRunTrailEnablementRefresh_CandidateRowSavesDisabledCache is the
// candidate-row counterpart to TestRunTrailEnablementRefresh_NotOnboardedSavesDisabledCache
// above, and exercises the real resolution chain (rather than mocking
// trailRefreshAPIClient directly) so it also proves resolveProcessingPlacement
// itself classifies a Candidate row as errRepoNotOnboarded.
//
// It does NOT claim the Candidate row is the common real-world trigger — it
// isn't. Verified against prod 2026-08-19: today's control plane only matches
// onboarded repos in a Filter lookup, so a public non-onboarded repo comes back
// as ZERO rows, and this branch is currently unreachable in production. The
// coverage stays because the OpenAPI text on ListReposParams.Filter promises
// the opposite, so the branch is one server change away from live. See the
// comment on the Candidate branch in cell_target.go.
func TestRunTrailEnablementRefresh_CandidateRowSavesDisabledCache(t *testing.T) {
	setupStopTestRepo(t)
	runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/entirehq/example.git")

	withFakeCellCore(t, &fakeCellCore{
		repos:    reposOutput(candidateRepoIndexFixture("entirehq/example")),
		clusters: clustersWithSlugs(),
	})

	require.NoError(t, runTrailEnablementRefresh(context.Background()))

	scope, err := currentTrailEnablementScope(context.Background())
	require.NoError(t, err)
	if got := cachedTrailsEnablementForScope(context.Background(), scope, time.Now()); got != trailEnablementCacheDisabled {
		t.Fatalf("cached enablement = %v, want trailEnablementCacheDisabled (saved, not left unknown)", got)
	}
}

// TestNewRefreshTrailEnablementCmd_APIFailureExitsZero guards against the
// detached __refresh_trail_enablement subprocess exiting non-zero on a
// transient network/API failure. The refresh is best-effort cache warming
// with stdout/stderr discarded (see newRefreshTrailEnablementCmd) — there is
// no one watching the exit code, so a failing TrailsEnabled call must be
// logged (already covered by TestRefreshTrailEnablementCmd_LogsBackgroundFailureToFile-
// style tests) and swallowed, never propagated as a command error, mirroring
// __send_analytics.
func TestNewRefreshTrailEnablementCmd_APIFailureExitsZero(t *testing.T) {
	setupStopTestRepo(t)
	runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/entirehq/example.git")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	prevClient := trailRefreshAPIClient
	trailRefreshAPIClient = func(context.Context, bool, string) (*api.Client, error) {
		return api.NewClientWithBaseURL("test-token", srv.URL), nil
	}
	t.Cleanup(func() { trailRefreshAPIClient = prevClient })

	cmd := newRefreshTrailEnablementCmd()
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.ExecuteContext(context.Background()),
		"detached refresh command must exit 0 even when the API call fails (best-effort cache warming)")
}

// TestRefreshTrailEnablementCmd_LogsBackgroundFailureToFile guards
// diagnosability: the detached __refresh_trail_enablement child runs with
// stdout/stderr discarded, so a failing background refresh must still leave a
// trail in .entire/logs/entire.log instead of vanishing. The command runs in a
// repo with no origin remote, so the scope resolves-and-fails locally (no
// network) and that failure has to be logged to the repo's log file.
//
// Executed through the real root command, because the root PersistentPreRunE is
// what opens the log file — the same path the detached child takes through
// main.go. Constructing the subcommand alone would exercise a wiring production
// never uses.
func TestRefreshTrailEnablementCmd_LogsBackgroundFailureToFile(t *testing.T) {
	setupStopTestRepo(t)
	markRepoSetUpForLogging(t)
	t.Setenv("ENTIRE_LOG_LEVEL", "debug")

	require.NoError(t, executeThroughRoot(t, "__refresh_trail_enablement"))

	root, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	logData, err := os.ReadFile(filepath.Join(root, ".entire", "logs", "entire.log"))
	require.NoError(t, err)
	require.Contains(t, string(logData), "trails enablement refresh skipped: scope unresolved",
		"background refresh failure must be diagnosable in .entire/logs/entire.log")
}

// TestRefreshTrailEnablementCmd_NoStrayLogsOutsideWorktree guards log-file
// creation against running outside a resolvable worktree. The root
// PersistentPreRun — the single logger-construction site — must gate on
// WorktreeRoot, or a child whose worktree was removed/relocated between spawn
// and exec would MkdirAll a stray .entire/logs/ wherever it happens to be
// running.
//
// Run through the real root: the gate lives there now, so constructing the
// subcommand alone would pass without ever reaching the code under test.
func TestRefreshTrailEnablementCmd_NoStrayLogsOutsideWorktree(t *testing.T) {
	dir := t.TempDir() // a plain temp dir, not a git worktree
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Setenv("ENTIRE_LOG_LEVEL", "debug")

	require.NoError(t, executeThroughRoot(t, "__refresh_trail_enablement"))

	_, statErr := os.Stat(filepath.Join(dir, ".entire", "logs"))
	require.True(t, os.IsNotExist(statErr),
		"must not create a stray .entire/logs outside a resolvable worktree")
}

// TestTrailRefreshRecentlySpawned_ThrottlesWithinWindow verifies the spawn-side
// guard: within trailRefreshSpawnThrottle of a recorded spawn,
// further spawns are suppressed; once the window passes a fresh spawn is allowed
// and re-recorded. Without this, an unreachable host — which never writes the
// cache, so the hourly TTL never starts — would fork a refresh child on every
// SessionStart.
func TestTrailRefreshRecentlySpawned_ThrottlesWithinWindow(t *testing.T) {
	commonDir := t.TempDir()
	now := time.Now()

	require.False(t, trailRefreshRecentlySpawned(commonDir, now),
		"first call records the spawn and is not throttled")
	require.True(t, trailRefreshRecentlySpawned(commonDir, now.Add(time.Second)),
		"a second attempt within the window is throttled")
	require.False(t, trailRefreshRecentlySpawned(commonDir, now.Add(trailRefreshSpawnThrottle)),
		"at the window boundary the spawn is allowed and re-recorded")
	require.True(t, trailRefreshRecentlySpawned(commonDir, now.Add(trailRefreshSpawnThrottle+time.Second)),
		"an attempt within the window of the re-recorded spawn is throttled")
}

// TestSpawnDetachedTrailEnablementRefresh_CollapsesBurst verifies the throttle is
// actually wired into the spawn path: a burst of SessionStart-driven attempts for
// the same repo forks a single child, not one per hook.
func TestSpawnDetachedTrailEnablementRefresh_CollapsesBurst(t *testing.T) {
	setupStopTestRepo(t)

	var spawnCount int32
	prevSpawn := trailRefreshSpawn
	trailRefreshSpawn = func(string) { atomic.AddInt32(&spawnCount, 1) }
	t.Cleanup(func() { trailRefreshSpawn = prevSpawn })

	spawnDetachedTrailEnablementRefresh(context.Background())
	spawnDetachedTrailEnablementRefresh(context.Background())
	spawnDetachedTrailEnablementRefresh(context.Background())

	if got := atomic.LoadInt32(&spawnCount); got != 1 {
		t.Fatalf("expected the burst to collapse to a single detached spawn, got %d", got)
	}
}

// resolvingSubagentAgent is a mockLifecycleAgent that also reports a subagent
// session link, standing in for an agent whose subagents run as their own
// sessions (Factory AI Droid's Workers).
type resolvingSubagentAgent struct {
	mockLifecycleAgent

	link agent.SubagentSessionLink
	ok   bool
}

var _ agent.SubagentSessionResolver = (*resolvingSubagentAgent)(nil)

func (r *resolvingSubagentAgent) ResolveSubagentSession(_ string) (agent.SubagentSessionLink, bool) {
	return r.link, r.ok
}

// TestResolveSubagentSessionLink_RejectsPathUnsafeIDs pins the choke point every
// SubagentSessionResolver passes through. The IDs it returns are interpolated
// into metadata paths by SessionMetadataDirFromSessionID and TaskMetadataDir,
// neither of which sanitizes, so validation cannot be left to implementations.
func TestResolveSubagentSessionLink_RejectsPathUnsafeIDs(t *testing.T) {
	t.Parallel()

	tests := map[string]agent.SubagentSessionLink{
		"parent traverses":   {ParentSessionID: "../../etc", ToolUseID: "toolu_1"},
		"parent separator":   {ParentSessionID: "a/b", ToolUseID: "toolu_1"},
		"tool use traverses": {ParentSessionID: "parent-1", ToolUseID: "../../etc"},
		"tool use separator": {ParentSessionID: "parent-1", ToolUseID: "a/b"},
		"empty parent":       {ParentSessionID: "", ToolUseID: "toolu_1"},
		"empty tool use":     {ParentSessionID: "parent-1", ToolUseID: ""},
	}

	for name, link := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ag := &resolvingSubagentAgent{link: link, ok: true}

			_, ok := resolveSubagentSessionLink(context.Background(), ag, "transcript.jsonl")

			if ok {
				t.Errorf("link %+v must be rejected, not used to build a metadata path", link)
			}
		})
	}
}

// TestResolveSubagentSessionLink_AcceptsValidLink guards against the validation
// above rejecting everything, which would silently restore the original bug.
func TestResolveSubagentSessionLink_AcceptsValidLink(t *testing.T) {
	t.Parallel()

	want := agent.SubagentSessionLink{
		ParentSessionID: "0b34cbcb-108c-4800-b68e-af7093c8cae9",
		ToolUseID:       "toolu_01SC9sRHSef1vtNFtMrX9w6T",
		SubagentType:    "worker",
	}
	ag := &resolvingSubagentAgent{link: want, ok: true}

	got, ok := resolveSubagentSessionLink(context.Background(), ag, "transcript.jsonl")

	if !ok {
		t.Fatal("a well-formed link must be accepted")
	}
	if got.ParentSessionID != want.ParentSessionID || got.ToolUseID != want.ToolUseID {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestResolveSubagentSessionLink_AgentWithoutCapability confirms the default
// path: an agent whose subagents block the parent turn (Claude Code) is left on
// the ordinary session-checkpoint path.
func TestResolveSubagentSessionLink_AgentWithoutCapability(t *testing.T) {
	t.Parallel()

	ag := &mockLifecycleAgent{name: "mock", agentType: "mock"}

	if _, ok := resolveSubagentSessionLink(context.Background(), ag, "transcript.jsonl"); ok {
		t.Error("an agent without the capability must never resolve a subagent link")
	}
}

// TestSaveSubagentSessionTaskStep_SecondTurn_MergesAndKeepsDeclaredPath pins
// the upsert contract a hook-driven Worker turn cannot reach: a second turn
// whose transcript ref is EMPTY must still merge its files with turn 1's and
// must not erase turn 1's declared transcript path.
func TestSaveSubagentSessionTaskStep_SecondTurn_MergesAndKeepsDeclaredPath(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	setupSubagentEndTestRepo(t)
	ctx := context.Background()

	step := subagentSessionStep{
		link:          agent.SubagentSessionLink{ParentSessionID: "droid-parent", ToolUseID: "toolu_worker1", SubagentType: "worker"},
		sessionID:     "droid-worker",
		event:         &agent.Event{Type: agent.TurnEnd, SessionID: "droid-worker", Timestamp: time.Now()},
		transcriptRef: "/tmp/worker.jsonl",
		modifiedFiles: []string{"a.txt"},
		agentType:     agent.AgentTypeClaudeCode,
		strat:         GetStrategy(ctx),
	}
	require.NoError(t, saveSubagentSessionTaskStep(ctx, step))

	step.transcriptRef = ""
	step.modifiedFiles = []string{"b.txt"}
	require.NoError(t, saveSubagentSessionTaskStep(ctx, step))

	state, err := strategy.LoadSessionState(ctx, "droid-parent")
	require.NoError(t, err)
	require.NotNil(t, state)
	rec := state.FindTaskRecord("toolu_worker1")
	require.NotNil(t, rec)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt"}, rec.Files, "the second turn must merge, not overwrite")
	assert.Equal(t, "/tmp/worker.jsonl", rec.DeclaredTranscriptPath,
		"an empty second-turn transcript ref must not erase the declared path")
	assert.False(t, rec.CompletedAt.IsZero())
}

// --- handleLifecycleSubagentEnd: background launch marker + SubagentStop dispatch ---

// setupSubagentEndTestRepo initializes a git repo with one commit and chdirs
// into it, returning the repo dir and HEAD hash for building session state and
// shadow-branch assertions.
func setupSubagentEndTestRepo(t *testing.T) (repoDir, headHash string) {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	return tmpDir, testutil.GetHeadHash(t, tmpDir)
}

// TestHandleLifecycleSubagentEnd_ResolvesMissingToolUseIDViaTaskDescription
// reproduces Cursor's real subagentStop payload, which carries no subagent_id
// (confirmed against Cursor's hooks documentation — only subagentStart has
// one). Without ResolvePreTaskToolUseID, event.ToolUseID stays "", so
// LoadPreTaskState/SaveTaskStep/CleanupPreTaskState all key off an empty
// string: the real pre-task-<id>.json file is orphaned, and a second parallel
// subagent's file would collide on the same empty key.
//
// This drives the full path through DispatchLifecycleEvent, not just the
// resolver in isolation, so it also pins that the resolved ID actually reaches
// event.ToolUseID/SubagentID before the pre-task-state lookup.
func TestHandleLifecycleSubagentEnd_ResolvesMissingToolUseIDViaTaskDescription(t *testing.T) {
	// NOT parallel: uses t.Chdir, like TestDispatchLifecycleEvent_RoutesToCorrectHandler.
	tmpDir, _ := setupSubagentEndTestRepo(t)

	ctx := context.Background()

	// Two subagents were started in parallel, each capturing its own pre-task
	// file at SubagentStart (which, unlike Cursor's subagentStop, does carry an
	// ID) with a distinct task description.
	const otherToolUseID = "toolu_other"
	if err := CapturePreTaskStateWithMeta(ctx, otherToolUseID, "unrelated task"); err != nil {
		t.Fatalf("CapturePreTaskStateWithMeta(other) error = %v", err)
	}
	const wantToolUseID = "toolu_target"
	const wantDescription = "write release notes"
	if err := CapturePreTaskStateWithMeta(ctx, wantToolUseID, wantDescription); err != nil {
		t.Fatalf("CapturePreTaskStateWithMeta(target) error = %v", err)
	}

	// The target subagent's file change, detected via git status since the
	// mock agent has no transcript analyzer.
	testutil.WriteFile(t, tmpDir, "docs/release.md", "release notes")

	transcriptPath := filepath.Join(tmpDir, "main.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:            agent.SubagentEnd,
		SessionID:       "test-session",
		SessionRef:      transcriptPath,
		ToolUseID:       "", // Cursor's real subagentStop payload: no subagent_id
		TaskDescription: wantDescription,
		Timestamp:       time.Now(),
	}

	if err := DispatchLifecycleEvent(ctx, ag, event); err != nil {
		t.Fatalf("DispatchLifecycleEvent(SubagentEnd) error = %v", err)
	}

	if event.ToolUseID != wantToolUseID {
		t.Errorf("event.ToolUseID = %q, want %q (resolved via task description)", event.ToolUseID, wantToolUseID)
	}
	if event.SubagentID != wantToolUseID {
		t.Errorf("event.SubagentID = %q, want %q (backfilled from resolved tool_use_id)", event.SubagentID, wantToolUseID)
	}

	// The resolved subagent's own pre-task file must be cleaned up by
	// CleanupPreTaskState, keyed by the resolved (not empty) ID.
	if _, err := os.Stat(preTaskStateFile(ctx, wantToolUseID)); !os.IsNotExist(err) {
		t.Errorf("pre-task file for resolved tool_use_id %q should be cleaned up, stat err = %v", wantToolUseID, err)
	}
	// The unrelated parallel subagent's pre-task file must survive untouched —
	// this is the "orphans real pre-task files" / "parallel Stops collide on
	// empty key" failure mode the fix prevents.
	if _, err := os.Stat(preTaskStateFile(ctx, otherToolUseID)); err != nil {
		t.Errorf("unrelated pre-task file for %q must NOT be removed, stat err = %v", otherToolUseID, err)
	}
}

// TestHandleLifecycleSubagentEnd_ConcurrentCleanupBetweenResolveAndLoadSkipsCheckpoint
// simulates a second, concurrent SubagentEnd (e.g. a sibling subagent racing
// through the same ambiguous "single active pre-task file" fallback, or a
// duplicate hook delivery for the same subagent) whose CleanupPreTaskState
// deletes the just-resolved pre-task file in the window between this
// handler's own ResolvePreTaskToolUseID call and its LoadPreTaskState call.
//
// Without the resolvedAmbiguously guard, this falls through to
// DetectFileChanges(ctx, nil), which treats every untracked file as new and
// mints a spurious checkpoint out of files this subagent never touched. The
// fix must instead skip the checkpoint entirely: there is no reliable
// baseline once the pre-task state has vanished after an ambiguous resolve.
func TestHandleLifecycleSubagentEnd_ConcurrentCleanupBetweenResolveAndLoadSkipsCheckpoint(t *testing.T) {
	// NOT parallel: uses t.Chdir and a package-level test seam.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()

	const toolUseID = "toolu_target"
	if err := CapturePreTaskStateWithMeta(ctx, toolUseID, ""); err != nil {
		t.Fatalf("CapturePreTaskStateWithMeta() error = %v", err)
	}

	// A file that pre-existed as untracked before this subagent ran. If the
	// bug reappears, DetectFileChanges(ctx, nil) would wrongly report this as
	// a new file and the checkpoint would go through.
	testutil.WriteFile(t, tmpDir, "unrelated-untracked.txt", "pre-existing, not this subagent's work")

	prevHook := afterAmbiguousSubagentEndResolve
	t.Cleanup(func() { afterAmbiguousSubagentEndResolve = prevHook })
	var hookCalled bool
	afterAmbiguousSubagentEndResolve = func(resolvedAmbiguously bool, resolvedID string) {
		hookCalled = true
		if !resolvedAmbiguously || resolvedID != toolUseID {
			t.Errorf("hook saw (resolvedAmbiguously=%v, id=%q), want (true, %q)", resolvedAmbiguously, resolvedID, toolUseID)
		}
		// Simulate the concurrent sibling's CleanupPreTaskState winning the race.
		if err := CleanupPreTaskState(ctx, resolvedID); err != nil {
			t.Fatalf("simulated concurrent cleanup failed: %v", err)
		}
	}

	transcriptPath := filepath.Join(tmpDir, "main.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.SubagentEnd,
		SessionID:  "test-session",
		SessionRef: transcriptPath,
		ToolUseID:  "", // ambiguous resolve path, like Cursor's real subagentStop payload
		Timestamp:  time.Now(),
	}

	if err := DispatchLifecycleEvent(ctx, ag, event); err != nil {
		t.Fatalf("DispatchLifecycleEvent(SubagentEnd) error = %v", err)
	}
	if !hookCalled {
		t.Fatal("test seam hook was never invoked; resolvedAmbiguously path did not run")
	}

	// No checkpoint (shadow branch) should have been created for this task: the
	// handler must have skipped rather than minting a spurious one out of a
	// pre-existing untracked file it never touched.
	out, err := exec.CommandContext(ctx, "git", "branch", "--list", "entire/*").Output()
	if err != nil {
		t.Fatalf("git branch --list failed: %v", err)
	}
	if branches := strings.TrimSpace(string(out)); branches != "" {
		t.Errorf("expected no shadow checkpoint branch after a vanished pre-task state, got: %s", branches)
	}
}

// TestHandleLifecycleSubagentEnd_NoChangesKeepsUncorroboratedPreTaskState pins
// the other half of the ambiguous-resolve contract. The vanished-state guard
// covers preState == nil; this covers preState != nil with no file changes,
// where the handler used to delete the pre-task file it had only guessed at.
//
// The single-active-file fallback (no ID and no description) names the one
// active file, which here belongs to a sibling that is still running and has
// not written anything yet. Deleting it would make the sibling's own
// SubagentEnd hit the vanished-state guard and drop a real checkpoint, so the
// file must survive.
func TestHandleLifecycleSubagentEnd_NoChangesKeepsUncorroboratedPreTaskState(t *testing.T) {
	// NOT parallel: uses t.Chdir.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()

	// The still-running sibling's baseline, captured at its SubagentStart. It
	// is the only active pre-task file, so the fallback will name it.
	const siblingToolUseID = "toolu_sibling"
	if err := CapturePreTaskStateWithMeta(ctx, siblingToolUseID, ""); err != nil {
		t.Fatalf("CapturePreTaskStateWithMeta() error = %v", err)
	}

	// The transcript lives outside the repo: inside it, it would itself be an
	// untracked new file and the handler would take the checkpoint path instead
	// of the "no file changes detected" path this test is about.
	transcriptPath := filepath.Join(t.TempDir(), "main.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.SubagentEnd,
		SessionID:  "test-session",
		SessionRef: transcriptPath,
		ToolUseID:  "", // no ID and no description: single-active-file fallback
		Timestamp:  time.Now(),
	}

	if err := DispatchLifecycleEvent(ctx, ag, event); err != nil {
		t.Fatalf("DispatchLifecycleEvent(SubagentEnd) error = %v", err)
	}

	// Nothing changed, so no checkpoint should exist — this is the no-changes
	// path, not the post-SaveTaskStep cleanup.
	out, err := exec.CommandContext(ctx, "git", "branch", "--list", "entire/*").Output()
	if err != nil {
		t.Fatalf("git branch --list failed: %v", err)
	}
	if branches := strings.TrimSpace(string(out)); branches != "" {
		t.Fatalf("expected no shadow checkpoint branch with no file changes, got: %s", branches)
	}
	if _, err := os.Stat(preTaskStateFile(ctx, siblingToolUseID)); err != nil {
		t.Errorf("pre-task file for %q must survive an uncorroborated no-changes resolve, stat err = %v", siblingToolUseID, err)
	}
}

// TestHandleLifecycleSubagentEnd_SubagentStop_KeepsUncorroboratedPreTaskState
// pins the Final-path twin of TestHandleLifecycleSubagentEnd_NoChangesKeepsUncorroboratedPreTaskState.
// When SubagentStop arrives with an empty tool_use_id (Claude Code's anticipated
// payload shape) and no task description, the single-active-file fallback names
// the one pre-task file — which may belong to a still-running sibling. The
// ambiguousWithoutDescription early exit must not delete that baseline.
func TestHandleLifecycleSubagentEnd_SubagentStop_KeepsUncorroboratedPreTaskState(t *testing.T) {
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "subagent-stop-ambiguous-session"

	const siblingToolUseID = "toolu_sibling"
	if err := CapturePreTaskStateWithMeta(ctx, siblingToolUseID, ""); err != nil {
		t.Fatalf("CapturePreTaskStateWithMeta() error = %v", err)
	}

	saveInFlightSession(ctx, t, sessionID, headHash, session.TaskRecord{
		ToolUseID: siblingToolUseID,
		AgentID:   "agent-sibling",
		StartedAt: time.Now(),
	})

	mainTranscriptPath, _ := writeSubagentTranscripts(t, "agent-sibling")
	ag := newMockAgent()
	event := finalSubagentEvent(sessionID, "", "") // empty ID: single-active-file fallback
	event.SessionRef = mainTranscriptPath

	if err := handleLifecycleSubagentEnd(ctx, ag, event); err != nil {
		t.Fatalf("handleLifecycleSubagentEnd(SubagentStop) error = %v", err)
	}

	if _, err := os.Stat(preTaskStateFile(ctx, siblingToolUseID)); err != nil {
		t.Errorf("pre-task file for %q must survive an uncorroborated SubagentStop resolve, stat err = %v", siblingToolUseID, err)
	}

	// No checkpoint should have been written from this misattributed stop.
	shadowBranch := checkpoint.ShadowBranchNameForCommit(headHash, "")
	if testutil.BranchExists(t, repoDir, shadowBranch) {
		t.Error("expected no shadow checkpoint branch after ambiguous SubagentStop skip")
	}
}

// TestHandleLifecycleSubagentEnd_NoChangesCleansDescriptionMatchedPreTaskState
// is the counterpart: a description-corroborated resolve is evidence that the
// pre-task file really is this subagent's, so the no-changes path must still
// clean it up. Skipping cleanup here would leak a file on Cursor's designed
// path and break the single-active-file fallback for every later subagent.
func TestHandleLifecycleSubagentEnd_NoChangesCleansDescriptionMatchedPreTaskState(t *testing.T) {
	// NOT parallel: uses t.Chdir.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	ctx := context.Background()

	const toolUseID = "toolu_target"
	const description = "review the changelog"
	if err := CapturePreTaskStateWithMeta(ctx, toolUseID, description); err != nil {
		t.Fatalf("CapturePreTaskStateWithMeta() error = %v", err)
	}

	// Outside the repo, so it is not itself an untracked change — see the
	// sibling test above.
	transcriptPath := filepath.Join(t.TempDir(), "main.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:            agent.SubagentEnd,
		SessionID:       "test-session",
		SessionRef:      transcriptPath,
		ToolUseID:       "", // Cursor's real subagentStop payload: no subagent_id
		TaskDescription: description,
		Timestamp:       time.Now(),
	}

	if err := DispatchLifecycleEvent(ctx, ag, event); err != nil {
		t.Fatalf("DispatchLifecycleEvent(SubagentEnd) error = %v", err)
	}

	// No checkpoint was minted (nothing changed), so this must be the no-changes
	// cleanup rather than the post-SaveTaskStep one.
	out, err := exec.CommandContext(ctx, "git", "branch", "--list", "entire/*").Output()
	if err != nil {
		t.Fatalf("git branch --list failed: %v", err)
	}
	if branches := strings.TrimSpace(string(out)); branches != "" {
		t.Fatalf("expected no shadow checkpoint branch with no file changes, got: %s", branches)
	}
	if _, err := os.Stat(preTaskStateFile(ctx, toolUseID)); !os.IsNotExist(err) {
		t.Errorf("description-matched pre-task file for %q should still be cleaned up with no changes, stat err = %v", toolUseID, err)
	}
}

// readShadowBranchFile reads the file at treePath from the tree at the tip of
// branchName, returning (content, true) if found or ("", false) otherwise
// (branch missing or file missing at that path).
func readShadowBranchFile(t *testing.T, repoDir, branchName, treePath string) (string, bool) {
	t.Helper()
	repo, err := gitrepo.OpenPath(repoDir)
	require.NoError(t, err)
	defer repo.Close()

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return "", false
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return "", false
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}
	file, err := tree.File(treePath)
	if err != nil {
		return "", false
	}
	content, err := file.Contents()
	if err != nil {
		return "", false
	}
	return content, true
}

// saveInFlightSession persists an active session state for sessionID based at
// headHash with the given (live, in-flight) task records.
func saveInFlightSession(ctx context.Context, t *testing.T, sessionID, headHash string, tasks ...session.TaskRecord) {
	t.Helper()
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:   sessionID,
		BaseCommit:  headHash,
		StartedAt:   time.Now(),
		Phase:       session.PhaseActive,
		TaskRecords: tasks,
	}))
}

// saveInFlightTranscriptSession is saveInFlightSession plus a registered agent
// type and transcript path, for tests whose condensation needs real content.
func saveInFlightTranscriptSession(ctx context.Context, t *testing.T, sessionID, headHash string, phase session.Phase, transcriptPath string, tasks ...session.TaskRecord) {
	t.Helper()
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:      sessionID,
		BaseCommit:     headHash,
		StartedAt:      time.Now(),
		Phase:          phase,
		AgentType:      agent.AgentTypeClaudeCode,
		TranscriptPath: transcriptPath,
		TaskRecords:    tasks,
	}))
}

// finalSubagentEvent builds a Final SubagentEnd (SubagentStop) event; callers
// needing SessionRef or SubagentTranscriptPath set them on the returned event.
func finalSubagentEvent(sessionID, toolUseID, subagentID string) *agent.Event {
	return &agent.Event{
		Type:       agent.SubagentEnd,
		SessionID:  sessionID,
		ToolUseID:  toolUseID,
		SubagentID: subagentID,
		Final:      true,
		Timestamp:  time.Now(),
	}
}

// writeSubagentTranscripts writes a one-line main transcript plus a sibling
// subagent transcript for agentID (the legacy layout ResolveAgentTranscriptPath
// falls back to) in a fresh temp dir, outside the repo so their presence never
// shows up as a git-status change.
func writeSubagentTranscripts(t *testing.T, agentID string) (mainTranscriptPath, subagentTranscriptPath string) {
	t.Helper()
	dir := t.TempDir()
	mainTranscriptPath = filepath.Join(dir, "main.jsonl")
	require.NoError(t, os.WriteFile(mainTranscriptPath, []byte(`{"type":"human","message":{"content":"do something"}}`+"\n"), 0o600))
	subagentTranscriptPath = filepath.Join(dir, "agent-"+agentID+".jsonl")
	require.NoError(t, os.WriteFile(subagentTranscriptPath, []byte(`{"type":"assistant"}`+"\n"), 0o600))
	return mainTranscriptPath, subagentTranscriptPath
}

// findSessionCheckpoint returns the permanent checkpoint written for sessionID,
// or false when none exists.
func findSessionCheckpoint(ctx context.Context, t *testing.T, sessionID string) (strategy.CheckpointInfo, bool) {
	t.Helper()
	checkpoints, err := strategy.ListCheckpoints(ctx)
	require.NoError(t, err)
	for _, cp := range checkpoints {
		if cp.SessionID == sessionID {
			return cp, true
		}
	}
	return strategy.CheckpointInfo{}, false
}

// readCheckpointTaskFile reads relPath under sessionID's permanent checkpoint
// tree (metadata branch), returning ("", false) when absent.
func readCheckpointTaskFile(ctx context.Context, t *testing.T, repoDir, sessionID, relPath string) (string, bool) {
	t.Helper()
	cp, found := findSessionCheckpoint(ctx, t, sessionID)
	if !found {
		return "", false
	}
	return readShadowBranchFile(t, repoDir, paths.MetadataBranchName, cp.CheckpointID.Path()+"/"+relPath)
}

// makeNInFlightTasks returns n distinct live (uncompleted) task records.
func makeNInFlightTasks(n int) []session.TaskRecord {
	tasks := make([]session.TaskRecord, 0, n)
	for i := range n {
		tasks = append(tasks, session.TaskRecord{
			ToolUseID: fmt.Sprintf("toolu_%d", i),
			AgentID:   fmt.Sprintf("agent-%d", i),
			StartedAt: time.Now(),
		})
	}
	return tasks
}

// TestHandleLifecycleSubagentEnd_LaunchDispatch covers the launch-time
// (non-Final) post-task branch's background/foreground split.
func TestHandleLifecycleSubagentEnd_LaunchDispatch(t *testing.T) {
	// NOT parallel: subtests use t.Chdir via setupSubagentEndTestRepo.

	// background is the regression this PR fixes: Claude Code's launch-time
	// post-task hook fires seconds after a background subagent starts, before
	// any real work happens. Capturing at that point would save an empty stub
	// task step and never revisit it. The launch event must instead record an
	// in-flight marker (carrying the launch-time subagent_type/description,
	// since SubagentStop payloads have no tool_input to derive them from) and
	// save nothing yet.
	t.Run("background records marker without task step", func(t *testing.T) {
		repoDir, headHash := setupSubagentEndTestRepo(t)
		ctx := context.Background()
		sessionID := "bg-launch-session"

		saveInFlightSession(ctx, t, sessionID, headHash)

		ag := newMockAgent()
		event := &agent.Event{
			Type:       agent.SubagentEnd,
			SessionID:  sessionID,
			ToolUseID:  "toolu_bg1",
			SubagentID: "agent-bg1",
			ToolInput:  json.RawMessage(`{"subagent_type":"reviewer","description":"Review the PR","run_in_background":true}`),
			Final:      false,
			Timestamp:  time.Now(),
		}

		err := handleLifecycleSubagentEnd(ctx, ag, event)
		require.NoError(t, err)

		state, loadErr := strategy.LoadSessionState(ctx, sessionID)
		require.NoError(t, loadErr)
		require.NotNil(t, state)
		require.Len(t, state.TaskRecords, 1, "background launch must record an in-flight marker")
		marker := state.TaskRecords[0]
		assert.Equal(t, "toolu_bg1", marker.ToolUseID)
		assert.Equal(t, "agent-bg1", marker.AgentID)
		assert.Equal(t, "reviewer", marker.SubagentType)
		assert.Equal(t, "Review the PR", marker.TaskDescription)

		shadowBranch := checkpoint.ShadowBranchNameForCommit(headHash, "")
		if testutil.BranchExists(t, repoDir, shadowBranch) {
			t.Error("background launch must defer capture to subagent-stop, not save a task step immediately")
		}
	})

	// foreground guards invariant 2: a foreground Task invocation (no
	// run_in_background) completes at post-task time — its record is created
	// on completion (no launch stub exists for foreground) with the task's
	// files, which must also merge into the session's FilesTouched.
	t.Run("foreground completes record at post-task", func(t *testing.T) {
		repoDir, headHash := setupSubagentEndTestRepo(t)
		ctx := context.Background()
		sessionID := "fg-launch-session"

		saveInFlightSession(ctx, t, sessionID, headHash)

		testutil.WriteFile(t, repoDir, "foreground.txt", "written by foreground subagent")

		ag := newMockAgent()
		event := &agent.Event{
			Type:       agent.SubagentEnd,
			SessionID:  sessionID,
			ToolUseID:  "toolu_fg1",
			SubagentID: "agent-fg1",
			ToolInput:  json.RawMessage(`{"subagent_type":"dev","description":"Implement X"}`),
			Final:      false,
			Timestamp:  time.Now(),
		}

		err := handleLifecycleSubagentEnd(ctx, ag, event)
		require.NoError(t, err)

		shadowBranch := checkpoint.ShadowBranchNameForCommit(headHash, "")
		if testutil.BranchExists(t, repoDir, shadowBranch) {
			t.Error("foreground completion must write a task record, not a shadow task step")
		}

		state, loadErr := strategy.LoadSessionState(ctx, sessionID)
		require.NoError(t, loadErr)
		require.NotNil(t, state)
		require.Len(t, state.TaskRecords, 1, "foreground completion must create its record (no launch stub exists)")
		rec := state.TaskRecords[0]
		assert.Equal(t, "toolu_fg1", rec.ToolUseID)
		assert.Equal(t, "agent-fg1", rec.AgentID)
		assert.Equal(t, "dev", rec.SubagentType)
		assert.Equal(t, "Implement X", rec.TaskDescription)
		assert.False(t, rec.CompletedAt.IsZero(), "the record must be completed, not left in flight")
		assert.Contains(t, rec.Files, "foreground.txt")
		assert.Contains(t, state.FilesTouched, "foreground.txt",
			"task files must merge into FilesTouched so carry-forward and PostCommit gating see them")
	})

	// uncorrelated guards agents whose SubagentEnd carries no correlation ID at
	// all — Copilot CLI keys every subagent on "", so the exactly-once claim
	// would match the first one's completed record and drop each later
	// subagent's files. They must merge instead.
	t.Run("uncorrelated subagents merge instead of claiming once", func(t *testing.T) {
		repoDir, headHash := setupSubagentEndTestRepo(t)
		ctx := context.Background()
		sessionID := "uncorrelated-session"

		saveInFlightSession(ctx, t, sessionID, headHash)

		for _, file := range []string{"first.txt", "second.txt"} {
			testutil.WriteFile(t, repoDir, file, "written by an uncorrelated subagent")
			require.NoError(t, handleLifecycleSubagentEnd(ctx, newMockAgent(), &agent.Event{
				Type:      agent.SubagentEnd,
				SessionID: sessionID,
				Timestamp: time.Now(),
			}))
		}

		state, loadErr := strategy.LoadSessionState(ctx, sessionID)
		require.NoError(t, loadErr)
		require.Len(t, state.TaskRecords, 1, "uncorrelated subagents share one record")
		assert.Contains(t, state.FilesTouched, "first.txt")
		assert.Contains(t, state.FilesTouched, "second.txt",
			"the second uncorrelated subagent's files must not be dropped by the exactly-once claim")
	})
}

// TestHandleLifecycleSubagentEnd_SubagentStop_CapturesUsingLaunchRecordedLabel
// is the addendum from Task 1's code review: SubagentStop payloads carry no
// tool_input, so a Final capture can't derive subagent_type/description from
// the event itself (ParseSubagentTypeAndDescription yields empty strings on a
// nil ToolInput). The launch-recorded labels must survive on the completed
// record — exactly once — and no shadow task step may be written anymore.
func TestHandleLifecycleSubagentEnd_SubagentStop_CapturesUsingLaunchRecordedLabel(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "stop-capture-session"
	toolUseID := "toolu_stop1"

	saveInFlightSession(ctx, t, sessionID, headHash, session.TaskRecord{
		ToolUseID:       toolUseID,
		AgentID:         "agent-stop1",
		StartedAt:       time.Now(),
		SubagentType:    "reviewer",
		TaskDescription: "Review the PR",
	})

	// event.ToolInput is empty, as a real SubagentStop payload's would be —
	// event.SubagentType/TaskDescription resolve to "" at the top of
	// handleLifecycleSubagentEnd, so the label can only come from the marker.
	ag := newMockAgent()
	err := handleLifecycleSubagentEnd(ctx, ag, finalSubagentEvent(sessionID, toolUseID, "agent-stop1"))
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit(headHash, "")
	if testutil.BranchExists(t, repoDir, shadowBranch) {
		t.Error("a Final completion must write a task record, not a shadow task step")
	}

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	assert.Empty(t, state.LiveTaskRecords(), "subagent-stop must complete the record (no longer live), even though it persists for the materializer")
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	assert.Equal(t, "reviewer", rec.SubagentType, "the completed record must keep the launch-recorded subagent type")
	assert.Equal(t, "Review the PR", rec.TaskDescription, "the completed record must keep the launch-recorded description")
	assert.True(t, state.HasTaskContent(),
		"a no-changes Final completion must leave the record in place so condensation triggers see it")
	assert.Zero(t, state.StepCount,
		"a transcript-only completion must NOT consume StepCount — its ==0/==1 values carry SaveStep's first-checkpoint-baseline and transcript-anchor semantics")
}

// TestHandleLifecycleSubagentEnd_SubagentStop_MissingState_DoesNotResurrect
// pins Step 2.4b's late-arrival guard: SaveTaskStep's ensureSessionInitialized
// re-creates session state unconditionally, so a late SubagentStop for a
// session that was already ended and swept must never call it — that would
// resurrect a zombie session and mint a shadow branch nothing condenses,
// exactly the class of bug the session sweep feature exists to prevent.
func TestHandleLifecycleSubagentEnd_SubagentStop_MissingState_DoesNotResurrect(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "swept-session"
	// Deliberately no strategy.SaveSessionState call: this session's state was
	// already removed (ended + swept, or never existed).

	ag := newMockAgent()
	err := handleLifecycleSubagentEnd(ctx, ag, finalSubagentEvent(sessionID, "toolu_swept1", "agent-swept1"))
	require.NoError(t, err)

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	assert.Nil(t, state, "a late subagent-stop for a missing session must not resurrect session state")

	shadowBranch := checkpoint.ShadowBranchNameForCommit(headHash, "")
	if testutil.BranchExists(t, repoDir, shadowBranch) {
		t.Error("a late subagent-stop for a missing session must not mint a shadow branch")
	}
}

// TestHandleLifecycleSubagentEnd_SubagentStop_PhaseEnded_TriggersEagerCondense
// pins the other half of Step 2.4b: when the session ended (PhaseEnded)
// before this SubagentStop arrived, the Final capture must run and then
// immediately trigger the same eager condense SessionEnd uses, so the
// newly-captured task step doesn't linger as post-condensation zombie shadow
// data.
//
// Uses a read-only capture (no file changes) — the headline motivating case,
// a background reviewer that edits nothing. Such a completion touches neither
// FilesTouched nor StepCount, so the task record itself is what keeps
// CondenseAndMarkFullyCondensed off its no-steps and no-shadow-branch
// shortcuts. The session state carries a real transcript path and a
// registered agent type so CondenseSession has actual content to condense —
// proving the record was genuinely materialized into a permanent checkpoint,
// not merely that FullyCondensed flipped true via a shortcut.
func TestHandleLifecycleSubagentEnd_SubagentStop_PhaseEnded_TriggersEagerCondense(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "ended-session"
	toolUseID := "toolu_ended1"

	// The transcript must live OUTSIDE the repo: an untracked file inside the
	// repo would show up as a git-status "new file" and defeat the read-only
	// (no file changes) scenario this test is specifically covering.
	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "transcript.jsonl")
	transcript := `{"type":"human","uuid":"u1","message":{"content":"please review the diff"}}
{"type":"assistant","uuid":"u2","message":{"content":"Reviewed; looks correct."}}
`
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))

	saveInFlightTranscriptSession(ctx, t, sessionID, headHash, session.PhaseEnded, transcriptPath,
		session.TaskRecord{ToolUseID: toolUseID, AgentID: "agent-ended1", StartedAt: time.Now(), SubagentType: "reviewer"})

	ag := newMockAgent()
	err := handleLifecycleSubagentEnd(ctx, ag, finalSubagentEvent(sessionID, toolUseID, "agent-ended1"))
	require.NoError(t, err)

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	assert.True(t, state.FullyCondensed, "a late capture on an ended session must trigger the eager condense SessionEnd uses")
	assert.Empty(t, state.TaskRecords, "the completed record must be removed once materialized into the permanent checkpoint")

	// The real assertion: the eager condense must have MATERIALIZED the
	// record under the permanent checkpoint's tasks/ subtree — the #2058
	// pointer-model contract — not merely flipped FullyCondensed.
	taskJSON, found := readCheckpointTaskFile(ctx, t, repoDir, sessionID, "tasks/"+toolUseID+"/task.json")
	require.True(t, found, "the completed record must materialize as tasks/%s/task.json in the permanent checkpoint", toolUseID)
	assert.Contains(t, taskJSON, "reviewer", "task.json must carry the launch-recorded subagent type")
}

// TestHandleLifecycleSubagentEnd_SubagentStop_SessionEndsMidCapture_StillCondenses
// is the regression for the eager-condense decision using a pre-capture phase
// snapshot: handleSubagentStopFinal loads session state once at function
// entry, then runs completeSubagentTaskRecord, and only
// THEN decided whether to eagerly condense — using that stale, pre-capture
// snapshot. If a racing SessionEnd flips the session to PhaseEnded during
// that capture window (both serialize on the per-session gate, so the
// interleaving reduces to ordering), the stale snapshot still said idle, the
// eager condense was skipped, and the task step just captured became
// post-condensation zombie shadow data.
//
// The session here starts idle (PhaseActive, via saveInFlightTranscriptSession),
// not ended — unlike the sibling PhaseEnded_TriggersEagerCondense test above,
// which starts already ended. The mock analyzer's onExtract callback fires
// from inside ExtractModifiedFilesFromOffset, i.e. mid-way through
// completeSubagentTaskRecord, and flips the persisted session to PhaseEnded via
// strategy.MutateSessionState — simulating exactly where a racing SessionEnd
// would land. It deliberately does not run a full endSessionNow; only the
// phase transition matters here.
func TestHandleLifecycleSubagentEnd_SubagentStop_SessionEndsMidCapture_StillCondenses(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "midcapture-session"
	toolUseID := "toolu_midcapture1"
	agentID := "agent-midcapture1"

	// Real transcript content so CondenseSession has something to condense —
	// proving the shadow branch's content was genuinely consumed into a
	// permanent checkpoint, not merely that FullyCondensed flipped true via
	// CondenseAndMarkFullyCondensed's no-steps shortcut.
	mainTranscriptPath, subagentTranscriptPath := writeSubagentTranscripts(t, agentID)

	saveInFlightTranscriptSession(ctx, t, sessionID, headHash, session.PhaseActive, mainTranscriptPath,
		session.TaskRecord{ToolUseID: toolUseID, AgentID: agentID, StartedAt: time.Now(), SubagentType: "reviewer"})

	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: newMockAgent(),
		onExtract: func() {
			require.NoError(t, strategy.MutateSessionState(ctx, sessionID, func(s *strategy.SessionState) error {
				s.Phase = session.PhaseEnded
				return nil
			}))
		},
	}

	event := finalSubagentEvent(sessionID, toolUseID, agentID)
	event.SubagentTranscriptPath = subagentTranscriptPath

	err := handleLifecycleSubagentEnd(ctx, ag, event)
	require.NoError(t, err)

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	assert.True(t, state.FullyCondensed, "a SessionEnd landing mid-capture must still trigger the eager condense: the decision must use the phase as of AFTER capture, not the snapshot loaded at function entry")
	assert.Empty(t, state.TaskRecords, "the completed record must be removed once materialized")

	// The real assertion: the record completed mid-race must reach permanent
	// storage — its transcript materialized under tasks/ — not be stranded.
	transcript, found := readCheckpointTaskFile(ctx, t, repoDir, sessionID, "tasks/"+toolUseID+"/agent-"+agentID+".jsonl")
	require.True(t, found, "the record's transcript must materialize under the permanent checkpoint's tasks/ subtree")
	assert.Contains(t, transcript, "assistant")
}

// TestHandleLifecycleSubagentEnd_SubagentStop_ClaimPreventsDoubleCapture pins
// the exactly-once completion: a second Final event for the same ToolUseID (a
// duplicate SubagentStop delivery, or a real SubagentStop racing the
// SessionEnd final sweep) sees the record already completed and skips —
// completing exactly once instead of twice. The end-to-end pin is
// TestSubagentCheckpoints_ForegroundDoubleFire_CapturesOnce.
func TestHandleLifecycleSubagentEnd_SubagentStop_ClaimPreventsDoubleCapture(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	_, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "claim-race-session"
	toolUseID := "toolu_claim1"

	saveInFlightSession(ctx, t, sessionID, headHash,
		session.TaskRecord{ToolUseID: toolUseID, AgentID: "agent-claim1", StartedAt: time.Now(), SubagentType: "dev"})

	ag := newMockAgent()
	makeEvent := func() *agent.Event { return finalSubagentEvent(sessionID, toolUseID, "agent-claim1") }

	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, makeEvent()))

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero(), "first Final event must complete the record")
	firstCompletedAt := rec.CompletedAt

	// A second Final event for the same ToolUseID: the record is already
	// completed, so this must be a no-op rather than a second completion.
	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, makeEvent()))

	state, loadErr = strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	rec = state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	assert.Equal(t, firstCompletedAt, rec.CompletedAt, "a duplicate Final event must not re-complete the record")
	assert.Len(t, state.TaskRecords, 1, "a duplicate Final event must not register a second record")
}

// TestHandleLifecycleSubagentEnd_SubagentStop_Background_ExcludesForeignWorktreeChanges
// is the regression for a verified external-review finding: a background
// Final (SubagentStop) capture used to merge DetectFileChanges' whole-worktree
// git-status scan (vs. the launch-time pre-task baseline) into the task step.
// That scan is correct for a FOREGROUND capture — the parent is blocked on the
// subagent, so the worktree delta since launch really is the subagent's — but
// a background task's Final can arrive minutes to hours after launch, by
// which point the scan sweeps in whatever the parent (or another concurrent
// agent) wrote to the worktree in the meantime, misattributing it to this
// task's checkpoint: a long-running background agent's checkpoint absorbing
// the parent's later edits. A background Final capture must include only
// event.ModifiedFiles plus the analyzer-extracted files from the subagent's
// own transcript, never the worktree-wide scan — pinned here on the completed
// record's Files and the session's FilesTouched.
func TestHandleLifecycleSubagentEnd_SubagentStop_Background_ExcludesForeignWorktreeChanges(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "bg-foreign-session"
	toolUseID := "toolu_foreign1"
	agentID := "agent-foreign1"

	// The subagent's transcript must exist at the resolvable path: a background
	// Final capture is analyzer-only, and when the subagent transcript is
	// unresolvable it deliberately skips the analyzer scan entirely rather
	// than falling back to the parent transcript (see the unresolvable-
	// transcript test below), so attribution here requires the real file.
	transcriptDir := t.TempDir()
	mainTranscriptPath := filepath.Join(transcriptDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(mainTranscriptPath, []byte(`{"type":"human","message":{"content":"implement widget"}}`+"\n"), 0o600))
	subagentsDir := paths.SubagentsDir(transcriptDir, sessionID)
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))
	subagentTranscriptPath := filepath.Join(subagentsDir, paths.AgentTranscriptFileName(agentID))
	require.NoError(t, os.WriteFile(subagentTranscriptPath, []byte(`{"type":"assistant","message":{"content":"wrote agent-work.txt"}}`+"\n"), 0o600))

	saveInFlightSession(ctx, t, sessionID, headHash, session.TaskRecord{
		ToolUseID:       toolUseID,
		AgentID:         agentID,
		StartedAt:       time.Now(),
		SubagentType:    "dev",
		TaskDescription: "Implement widget",
	})

	// The subagent's own work: named by the transcript analyzer, and present
	// on disk so it can actually be written into the shadow tree.
	testutil.WriteFile(t, repoDir, "agent-work.txt", "written by the background subagent")

	// A foreign file: written to the worktree after launch but NOT by this
	// subagent — the transcript analyzer never names it. This simulates the
	// parent's own edit (or another concurrent agent's) landing during the
	// long window between a background launch and its eventual SubagentStop.
	testutil.WriteFile(t, repoDir, "foreign.txt", "written by someone else after launch")

	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: newMockAgent(),
		analyzerFiles:      []string{"agent-work.txt"},
	}
	event := finalSubagentEvent(sessionID, toolUseID, agentID)
	event.SessionRef = mainTranscriptPath

	err := handleLifecycleSubagentEnd(ctx, ag, event)
	require.NoError(t, err)

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	assert.Contains(t, rec.Files, "agent-work.txt", "the subagent's own analyzer-reported file must still be captured")
	assert.NotContains(t, rec.Files, "foreign.txt", "a background Final capture must not absorb a foreign worktree file via the whole-worktree DetectFileChanges scan — that is another agent's or the parent's work, not this task's")
	assert.Contains(t, state.FilesTouched, "agent-work.txt")
	assert.NotContains(t, state.FilesTouched, "foreign.txt")
}

// TestHandleLifecycleSubagentEnd_SubagentStop_TranscriptOnlyBeforeFirstSaveStep_PreservesBaseline
// is the regression for overloading StepCount with transcript-only task
// steps: StepCount's ==0 value drives SaveStep's IsFirstCheckpoint (the FIRST
// shadow checkpoint snapshots the user's whole pre-existing uncommitted
// worktree state — checkpoint/ephemeral.go's collectChangedFiles baseline)
// and its ==1 value anchors TranscriptIdentifierAtStart. A transcript-only
// background Final landing BEFORE the session's first SaveStep must therefore
// register only on the task record, not StepCount —
// otherwise the subsequent first SaveStep sees StepCount==1, skips the
// baseline capture (silently dropping the user's pre-existing dirt from every
// checkpoint), and never sets the transcript anchor.
func TestHandleLifecycleSubagentEnd_SubagentStop_TranscriptOnlyBeforeFirstSaveStep_PreservesBaseline(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "baseline-order-session"
	toolUseID := "toolu_baseline1"

	// The user's pre-existing uncommitted work, on disk BEFORE the session
	// saves anything. Only the first SaveStep's baseline capture picks it up.
	testutil.WriteFile(t, repoDir, "user-dirt.txt", "pre-existing uncommitted work")

	saveInFlightSession(ctx, t, sessionID, headHash,
		session.TaskRecord{ToolUseID: toolUseID, AgentID: "agent-baseline1", StartedAt: time.Now(), SubagentType: "reviewer"})

	// A read-only background subagent completes first: transcript-only Final
	// capture (no file changes; subagent transcript unresolvable is fine here —
	// the point is the step registers without touching StepCount).
	ag := newMockAgent()
	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, finalSubagentEvent(sessionID, toolUseID, "agent-baseline1")))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Zero(t, state.StepCount, "transcript-only task step must not consume StepCount")
	require.True(t, state.HasTaskContent(), "the completed record must register as pending task content")

	// Now the session's FIRST SaveStep. It must still get first-checkpoint
	// semantics: baseline capture of user-dirt.txt and the transcript anchor.
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(repoDir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(`{"type":"human","message":{"content":"do the work"}}`+"\n"), 0o644))

	require.NoError(t, GetStrategy(ctx).SaveStep(ctx, strategy.StepContext{
		SessionID:                sessionID,
		ModifiedFiles:            []string{},
		NewFiles:                 []string{},
		DeletedFiles:             []string{},
		MetadataDir:              metadataDir,
		MetadataDirAbs:           metadataDirAbs,
		CommitMessage:            "Checkpoint 1",
		AuthorName:               "Test",
		AuthorEmail:              "test@test.com",
		StepTranscriptIdentifier: "anchor-uuid-1",
	}))

	state, err = strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, 1, state.StepCount, "the first SaveStep must be checkpoint #1")
	assert.Equal(t, "anchor-uuid-1", state.TranscriptIdentifierAtStart,
		"the first SaveStep must set the transcript anchor (StepCount==1 semantics)")

	shadowBranch := checkpoint.ShadowBranchNameForCommit(headHash, "")
	content, gotDirt := readShadowBranchFile(t, repoDir, shadowBranch, "user-dirt.txt")
	assert.True(t, gotDirt,
		"the first SaveStep must snapshot pre-existing uncommitted state (IsFirstCheckpoint baseline) even after a transcript-only task step")
	assert.Equal(t, "pre-existing uncommitted work", content)
}

// TestHandleLifecycleSubagentEnd_SubagentStop_UnresolvableTranscript_SkipsParentAttribution
// pins the no-parent-fallback rule for background Final captures: when the
// subagent transcript is unresolvable (payload carried none and the AgentID
// resolves to no file), the analyzer must NOT fall back to scanning
// event.SessionRef — the PARENT transcript — from offset 0. In
// analyzer-files-only mode that fallback would attribute the whole session's
// file activity to this one background task. The capture must proceed
// transcript-less and file-less instead.
func TestHandleLifecycleSubagentEnd_SubagentStop_UnresolvableTranscript_SkipsParentAttribution(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "bg-unresolvable-session"
	toolUseID := "toolu_unresolvable1"

	// A parent transcript that names a file (via the mock analyzer). No
	// subagent transcript exists anywhere for this AgentID.
	transcriptDir := t.TempDir()
	mainTranscriptPath := filepath.Join(transcriptDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(mainTranscriptPath, []byte(`{"type":"assistant","message":{"content":"parent wrote parent-work.txt"}}`+"\n"), 0o600))

	// The file the parent's transcript would attribute, present on disk so it
	// WOULD be committed if the parent scan ran.
	testutil.WriteFile(t, repoDir, "parent-work.txt", "the parent session's work")

	saveInFlightSession(ctx, t, sessionID, headHash,
		session.TaskRecord{ToolUseID: toolUseID, AgentID: "agent-unresolvable1", StartedAt: time.Now(), SubagentType: "reviewer"})

	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: newMockAgent(),
		analyzerFiles:      []string{"parent-work.txt"},
	}
	event := finalSubagentEvent(sessionID, toolUseID, "agent-unresolvable1")
	event.SessionRef = mainTranscriptPath
	require.NoError(t, handleLifecycleSubagentEnd(ctx, ag, event))

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	assert.Empty(t, state.LiveTaskRecords(), "the record is still completed — the capture ran, just without file attribution")
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	assert.Empty(t, rec.Files,
		"an unresolvable subagent transcript must not trigger a parent-transcript scan that attributes the whole session's files to one background task")
	assert.Empty(t, rec.DeclaredTranscriptPath, "an unresolvable transcript completes the record with declared-path-empty")
	assert.NotContains(t, state.FilesTouched, "parent-work.txt")
}

// TestHandleLifecycleSubagentEnd_SubagentStop_AnalyzerError_FailsInsteadOfEmptyStep
// pins the analyzer-error semantics for background Final captures: the
// analyzer scan is the capture's ONLY file source (analyzer-files-only mode
// has no worktree-diff backup), so a transient read error must fail the
// capture — a completed zero-file record would permanently misstate the task
// as read-only while looking perfectly healthy. The error returns BEFORE
// completion, so the record stays LIVE and the SessionEnd sweep retries it.
func TestHandleLifecycleSubagentEnd_SubagentStop_AnalyzerError_FailsInsteadOfEmptyStep(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "bg-analyzer-error-session"
	toolUseID := "toolu_analyzererr1"

	// The subagent transcript exists (so the analyzer is actually consulted);
	// the analyzer itself fails, simulating a transient read/parse error.
	_, subagentTranscriptPath := writeSubagentTranscripts(t, "analyzererr1")

	saveInFlightSession(ctx, t, sessionID, headHash,
		session.TaskRecord{ToolUseID: toolUseID, AgentID: "agent-analyzererr1", StartedAt: time.Now(), SubagentType: "dev"})

	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: newMockAgent(),
		analyzerErr:        errors.New("transient transcript read failure"),
	}
	event := finalSubagentEvent(sessionID, toolUseID, "agent-analyzererr1")
	event.SubagentTranscriptPath = subagentTranscriptPath
	err := handleLifecycleSubagentEnd(ctx, ag, event)
	require.Error(t, err, "an analyzer error in analyzer-files-only mode must fail the capture, not complete a clean-looking empty record")

	shadowBranch := checkpoint.ShadowBranchNameForCommit(headHash, "")
	if testutil.BranchExists(t, repoDir, shadowBranch) {
		t.Error("nothing may be written when the analyzer scan failed — a zero-file record would permanently misstate the task as read-only")
	}

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	assert.Len(t, state.LiveTaskRecords(), 1,
		"the record must stay LIVE — the error returns before completion, so the SessionEnd sweep retries it")
}

// TestHandleLifecycleSessionEnd_InFlightTask_FinalCapture pins invariant 4: a
// session ending before SubagentStop arrives must not lose the subagent's
// work. SessionEnd runs completeLiveTaskRecords — the same completion
// SubagentStop would have performed — BEFORE endSessionNow's eager condense,
// so the condense materializes the just-completed records' transcripts into
// the permanent checkpoint.
func TestHandleLifecycleSessionEnd_InFlightTask_FinalCapture(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	repoDir, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "sessionend-inflight-session"
	toolUseID := "toolu_sessionend1"
	agentID := "agent-sessionend1"

	transcriptDir := t.TempDir()
	mainTranscriptPath := filepath.Join(transcriptDir, "main.jsonl")
	require.NoError(t, os.WriteFile(mainTranscriptPath, []byte(`{"type":"human","message":{"content":"please review the diff"}}`+"\n"), 0o600))

	// The subagent's own transcript, at the path the marker's AgentID resolves
	// to (ResolveAgentTranscriptPath, keyed off the MAIN transcript's dir and
	// session ID). This pins the marker.AgentID → transcript-resolution
	// plumbing: the SessionEnd sweep's synthesized event carries no
	// SubagentTranscriptPath, so the stored transcript can only get there via
	// resolution from the marker's AgentID.
	const subagentSentinel = "reviewed the diff; verdict: LGTM-sentinel"
	subagentsDir := paths.SubagentsDir(filepath.Dir(mainTranscriptPath), sessionID)
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))
	subagentTranscriptPath := filepath.Join(subagentsDir, paths.AgentTranscriptFileName(agentID))
	require.NoError(t, os.WriteFile(subagentTranscriptPath,
		[]byte(`{"type":"assistant","message":{"content":"`+subagentSentinel+`"}}`+"\n"), 0o600))

	saveInFlightTranscriptSession(ctx, t, sessionID, headHash, session.PhaseActive, mainTranscriptPath,
		session.TaskRecord{ToolUseID: toolUseID, AgentID: agentID, StartedAt: time.Now(), SubagentType: "reviewer", TaskDescription: "Review the diff"})

	ag := newMockAgent()

	// First: run the sweep directly (the exact call handleLifecycleSessionEnd
	// makes) and pin the marker.AgentID → transcript-resolution plumbing: the
	// sweep's synthesized event carries no SubagentTranscriptPath, so the
	// DeclaredTranscriptPath on the completed record can only have gotten
	// there by resolving the marker's AgentID to the file on disk.
	completeLiveTaskRecords(ctx, ag, sessionID, mainTranscriptPath)

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero(), "the sweep must complete the record")
	assert.Equal(t, subagentTranscriptPath, rec.DeclaredTranscriptPath,
		"the sweep must record the transcript path resolved from the marker's AgentID")

	// Re-arm a second in-flight marker so the full SessionEnd handler below
	// still demonstrably performs the sweep itself (the first marker was
	// claimed by the direct call above — the same dedup a racing/duplicate
	// Final event hits).
	secondToolUseID := "toolu_sessionend2"
	require.NoError(t, strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		state.AddTaskRecord(session.TaskRecord{
			ToolUseID: secondToolUseID, AgentID: "agent-sessionend2", StartedAt: time.Now(), SubagentType: "reviewer",
		})
		return nil
	}))

	event := &agent.Event{
		Type:       agent.SessionEnd,
		SessionID:  sessionID,
		SessionRef: mainTranscriptPath,
		Timestamp:  time.Now(),
	}

	err := handleLifecycleSessionEnd(ctx, ag, event)
	require.NoError(t, err)

	state, loadErr = strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	// Both records were completed by the sweep and then materialized by
	// endSessionNow's eager condense: the durable-records materializer (#2058)
	// stores each completed record's payload under the permanent checkpoint's
	// tasks/<id>/ subtree and resetCheckpointWindow removes it from state.
	assert.Empty(t, state.TaskRecords, "completed records must be removed from session state once materialized into the permanent checkpoint")
	assert.True(t, state.FullyCondensed, "the sweep must run before endSessionNow's eager condense, so the condense materializes it")

	storedTranscript, found := readCheckpointTaskFile(ctx, t, repoDir, sessionID, "tasks/"+toolUseID+"/agent-"+agentID+".jsonl")
	require.True(t, found, "the swept task's transcript must materialize under the permanent checkpoint's tasks/ subtree")
	assert.Contains(t, storedTranscript, subagentSentinel,
		"the stored subagent transcript must carry the subagent's actual content")
}

// TestCompleteLiveTaskRecords_CompletesEveryRecord pins that the SessionEnd
// sweep is uncapped: it is every record's last completion chance, so it must
// claim and capture every live marker, however many are in flight.
func TestCompleteLiveTaskRecords_CompletesEveryRecord(t *testing.T) {
	// NOT parallel: uses t.Chdir via setupSubagentEndTestRepo.
	_, headHash := setupSubagentEndTestRepo(t)
	ctx := context.Background()
	sessionID := "uncapped-final-session"

	const taskCount = 9
	saveInFlightSession(ctx, t, sessionID, headHash, makeNInFlightTasks(taskCount)...)

	ag := newMockAgent()
	completeLiveTaskRecords(ctx, ag, sessionID, "")

	state, loadErr := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, state)
	require.Len(t, state.TaskRecords, taskCount, "records must persist after completion — the materializer reads them at condensation")
	assert.Empty(t, state.LiveTaskRecords(), "the SessionEnd sweep must complete every record")
}

func TestAppendEventSkillEventsToState_ReturnsOnlyNewlyAppended(t *testing.T) {
	t.Parallel()

	first := agent.SkillEvent{
		ID:        "evt-1",
		EventType: agent.SkillEventTypePromptInvocation,
		Skill:     agent.SkillEventSkill{Name: "search"},
		Source:    agent.SkillEventSource{Agent: "claude-code", Signal: agent.SkillSignalPromptSlashCommand},
	}
	second := agent.SkillEvent{
		ID:        "evt-2",
		EventType: agent.SkillEventTypeToolInvocation,
		Skill:     agent.SkillEventSkill{Name: "review"},
		Source:    agent.SkillEventSource{Agent: "claude-code", Signal: agent.SkillSignalClaudeSkillToolUse},
	}

	state := &strategy.SessionState{TurnID: "turn-1"}

	appended := appendEventSkillEventsToState(&agent.Event{SkillEvents: []agent.SkillEvent{first}}, state)
	require.Len(t, appended, 1)
	require.Equal(t, "search", appended[0].Skill.Name)
	require.Equal(t, "turn-1", appended[0].TurnID, "TurnID should be backfilled before append")

	// Re-delivering the first event appends nothing; only the new one comes back.
	appended = appendEventSkillEventsToState(&agent.Event{SkillEvents: []agent.SkillEvent{first, second}}, state)
	require.Len(t, appended, 1)
	require.Equal(t, "review", appended[0].Skill.Name)
	require.Len(t, state.SkillEvents, 2)

	// Full re-delivery is a no-op.
	require.Nil(t, appendEventSkillEventsToState(&agent.Event{SkillEvents: []agent.SkillEvent{first, second}}, state))
}
