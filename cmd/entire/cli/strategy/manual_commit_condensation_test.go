package strategy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttestutil "github.com/entireio/cli/cmd/entire/cli/agent/testutil"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	// Register agents so GetByAgentType works in tests.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/pi"
)

// calculateTokenUsage is a test helper that looks up an agent by type and
// calculates token usage from pre-loaded transcript bytes.
func calculateTokenUsage(agentType types.AgentType, data []byte, offset int) *agent.TokenUsage {
	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		return nil
	}
	return agent.CalculateTokenUsage(context.Background(), ag, data, offset, "")
}

func writeStrategyExternalSummaryAgentBinary(t *testing.T, dir, name string) {
	t.Helper()

	script := `#!/bin/sh
case "$1" in
  info)
    echo '{"protocol_version":1,"name":"` + name + `","type":"` + name + ` Agent","description":"External summary test agent","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{"hooks":false,"transcript_analyzer":false,"transcript_preparer":false,"token_calculator":false,"compact_transcript":false,"text_generator":true,"hook_response_writer":false,"subagent_aware_extractor":false}}'
    ;;
  detect)
    echo '{"present": true}'
    ;;
  generate-text)
    echo '{"text":"{\"intent\":\"Intent\",\"outcome\":\"Outcome\",\"learnings\":{\"repo\":[],\"code\":[],\"workflow\":[]},\"friction\":[],\"open_items\":[]}"}'
    ;;
  *)
    echo '{}'
    ;;
esac
`

	agenttestutil.WriteExternalAgentBinary(t, dir, name, script)
}
func TestCalculateTokenUsage_CursorAlwaysNil(t *testing.T) {
	t.Parallel()

	// Cursor transcripts don't contain token usage data, so CalculateTokenUsage
	// should always return nil (not an empty struct) to signal "no data
	// available" — regardless of transcript shape or offset.
	tests := []struct {
		name       string
		transcript []byte
		offset     int
	}{
		{"single-line transcript", []byte(`{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`), 0},
		{"multi-line real transcript", []byte(cursorSampleTranscript), 0},
		{"real transcript with offset", []byte(cursorSampleTranscript), 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
			if err != nil {
				t.Fatalf("GetByAgentType(Cursor) error: %v", err)
			}
			result := agent.CalculateTokenUsage(context.Background(), ag, tt.transcript, tt.offset, "")
			if result != nil {
				t.Errorf("CalculateTokenUsage(Cursor) = %+v, want nil", result)
			}
		})
	}
}

func TestBuildSummaryGenerator_ExternalProvider(t *testing.T) { //nolint:paralleltest // uses t.Chdir and t.Setenv
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	const provider = "strategy-external-summary"
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"external_agents":true,"summary_generation":{"provider":"`+provider+`","model":"test-model"}}`),
		0o644,
	))

	externalDir := t.TempDir()
	writeStrategyExternalSummaryAgentBinary(t, externalDir, provider)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if generator := buildSummaryGenerator(context.Background()); generator == nil {
		t.Fatal("buildSummaryGenerator() = nil for external text_generator provider")
	}
}

func TestBuildSummaryGenerator_BuiltInProviderSkipsExternalDiscovery(t *testing.T) { //nolint:paralleltest // uses t.Chdir and package-level stubs
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"summary_generation":{"provider":"claude-code","model":"test-model"}}`),
		0o644,
	))

	originalDiscover := discoverExternalSummaryProviders
	originalAvailable := isSummaryProviderCLIAvailable
	t.Cleanup(func() {
		discoverExternalSummaryProviders = originalDiscover
		isSummaryProviderCLIAvailable = originalAvailable
	})
	discoverExternalSummaryProviders = func(context.Context) {
		t.Fatal("registered built-in summary provider should not trigger external discovery")
	}
	isSummaryProviderCLIAvailable = func(types.AgentName) bool { return true }

	if generator := buildSummaryGenerator(context.Background()); generator == nil {
		t.Fatal("buildSummaryGenerator() = nil for registered built-in provider")
	}
}

func TestCalculateTokenUsage_EmptyData(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, nil, 0, "")
	require.NotNil(t, result, "CalculateTokenUsage(empty) = nil, want non-nil empty struct")
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Errorf("expected zero tokens for empty data, got %+v", result)
	}
}

func TestCalculateTokenUsage_ClaudeCodeBasic(t *testing.T) {
	t.Parallel()

	// Claude Code JSONL: "usage" with "id" lives inside the "message" JSON object
	lines := []string{
		`{"type":"human","uuid":"u1","message":{"content":"hello"}}`,
		`{"type":"assistant","uuid":"u2","message":{"id":"msg_001","usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, data, 0, "")
	require.NotNil(t, result, "CalculateTokenUsage(ClaudeCode) = nil, want non-nil")
	if result.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", result.OutputTokens)
	}
	if result.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", result.APICallCount)
	}
}

func TestCalculateTokenUsage_ClaudeCodeWithOffset(t *testing.T) {
	t.Parallel()

	// 4-line transcript; start at offset 2 to only count the second pair
	lines := []string{
		`{"type":"human","uuid":"u1","message":{"content":"first"}}`,
		`{"type":"assistant","uuid":"u2","message":{"id":"msg_001","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"human","uuid":"u3","message":{"content":"second"}}`,
		`{"type":"assistant","uuid":"u4","message":{"id":"msg_002","usage":{"input_tokens":20,"output_tokens":15}}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	full := agent.CalculateTokenUsage(context.Background(), ag, data, 0, "")
	sliced := agent.CalculateTokenUsage(context.Background(), ag, data, 2, "")

	require.NotNil(t, full, "expected non-nil full result")
	require.NotNil(t, sliced, "expected non-nil sliced result")
	if full.OutputTokens != 20 {
		t.Errorf("full OutputTokens = %d, want 20", full.OutputTokens)
	}
	if sliced.OutputTokens != 15 {
		t.Errorf("sliced OutputTokens = %d, want 15", sliced.OutputTokens)
	}
}

// cursorSampleTranscript is a subset of a real Cursor session transcript.
// Cursor uses "role" (not "type") and wraps user text in <user_query> tags.
var cursorSampleTranscript = strings.Join([]string{
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncreate a file with contents 'a' and commit, then create another file with contents 'b' and commit\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Creating two files (contents 'a' and 'b') and committing each."}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Both files are tracked and the working tree is clean."}]}}`,
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncreate a file with contents 'c' and commit\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Created c.txt with contents c and committed it."}]}}`,
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nadd a file called bingo and commit\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Created bingo and committed it."}]}}`,
}, "\n") + "\n"

func TestCountTranscriptItems_Cursor(t *testing.T) {
	t.Parallel()

	count := countTranscriptItems(agent.AgentTypeCursor, cursorSampleTranscript)
	if count != 7 {
		t.Errorf("countTranscriptItems(Cursor) = %d, want 7", count)
	}
}

func TestCountTranscriptItems_CursorEmpty(t *testing.T) {
	t.Parallel()

	count := countTranscriptItems(agent.AgentTypeCursor, "")
	if count != 0 {
		t.Errorf("countTranscriptItems(Cursor, empty) = %d, want 0", count)
	}
}

func TestSessionStateBackfillTokenUsage_CopilotUsesZeroInputSessionAggregate(t *testing.T) {
	t.Parallel()

	transcript := []byte(strings.Join([]string{
		`{"type":"user.message","data":{"content":"hello"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"assistant.message","data":{"content":"hi","outputTokens":25},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":"1"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"claude-sonnet-4.6":{"requests":{"count":3},"usage":{"inputTokens":0,"outputTokens":50,"cacheReadTokens":20,"cacheWriteTokens":10}}}},"id":"3","timestamp":"2026-03-03T00:00:02Z","parentId":""}`,
	}, "\n") + "\n")

	ag, err := agent.GetByAgentType(agent.AgentTypeCopilotCLI)
	require.NoError(t, err)

	checkpointUsage := calculateTokenUsage(agent.AgentTypeCopilotCLI, transcript, 1)
	require.NotNil(t, checkpointUsage)
	require.Zero(t, checkpointUsage.InputTokens)
	require.Equal(t, 25, checkpointUsage.OutputTokens)

	backfillUsage := sessionStateBackfillTokenUsage(context.Background(), ag, agent.AgentTypeCopilotCLI, transcript, checkpointUsage)
	require.NotNil(t, backfillUsage)
	require.Zero(t, backfillUsage.InputTokens)
	require.Equal(t, 50, backfillUsage.OutputTokens)
	require.Equal(t, 20, backfillUsage.CacheReadTokens)
	require.Equal(t, 10, backfillUsage.CacheCreationTokens)
	require.Equal(t, 3, backfillUsage.APICallCount)
}

func TestSessionStateBackfillModel_PiReadsModelFromTranscript(t *testing.T) {
	t.Parallel()

	// Pi records the model on message.model but never reports it through hooks,
	// so the model is backfilled from the transcript at condensation time.
	transcript := []byte(strings.Join([]string{
		`{"type":"session","version":3,"id":"pi-uuid","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","parentId":null,"message":{"role":"user","content":[{"type":"text","text":"Hi"}]}}`,
		`{"type":"message","id":"m2","parentId":"m1","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}],"model":"gpt-5.5","provider":"openai-codex","usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0}}}`,
	}, "\n") + "\n")

	ag, err := agent.GetByAgentType(agent.AgentTypePi)
	require.NoError(t, err)

	model := sessionStateBackfillModel(context.Background(), ag, transcript)
	require.Equal(t, "gpt-5.5", model)
}

func TestSessionStateBackfillModel_ClaudeCodeReadsModelFromTranscript(t *testing.T) {
	t.Parallel()

	// Claude Code reports the model only on the SessionStart hook payload. When
	// that never fired (hooks installed mid-session, a resumed session, or a
	// cleared model hint) the model would otherwise be empty and checkpoints fall
	// back to "Unknown" attribution (issue #1804). The transcript still records
	// it on message.model, so backfill recovers it at condensation time.
	transcript := []byte(strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"cc-uuid","model":"claude-opus-4-8[1m]"}`,
		`{"type":"assistant","message":{"model":"claude-opus-4-8","id":"m1","role":"assistant","content":[]}}`,
	}, "\n") + "\n")

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	require.NoError(t, err)

	model := sessionStateBackfillModel(context.Background(), ag, transcript)
	require.Equal(t, "claude-opus-4-8", model)
}

func TestSessionStateBackfillModel_EmptyTranscript(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypePi)
	require.NoError(t, err)

	require.Empty(t, sessionStateBackfillModel(context.Background(), ag, nil))
}

func TestSessionStateBackfillModel_AgentWithoutSupport(t *testing.T) {
	t.Parallel()

	// Cursor doesn't implement ModelExtractor, so backfill is a no-op even with
	// transcript data present.
	ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
	require.NoError(t, err)

	require.Empty(t, sessionStateBackfillModel(context.Background(), ag, []byte("{}\n")))
}

// droidMessage builds a Droid JSONL "message" line with the given id, role, and optional usage.
func droidMessage(t *testing.T, id, role string, usage map[string]int) string {
	t.Helper()
	inner := map[string]interface{}{
		"role":    role,
		"content": []interface{}{},
	}
	if usage != nil {
		inner["id"] = id
		inner["usage"] = usage
	}
	msg, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("failed to marshal inner message: %v", err)
	}
	line := map[string]interface{}{
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(msg),
	}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("failed to marshal droid line: %v", err)
	}
	return string(b)
}

func TestCalculateTokenUsage_DroidStartOffsetSkipsNonMessageLines(t *testing.T) {
	t.Parallel()

	// Build a Droid transcript with non-message entries interspersed:
	// Line 0: session_start (non-message)
	// Line 1: user message (no tokens)
	// Line 2: assistant message with 10 input, 20 output tokens
	// Line 3: session_event (non-message)
	// Line 4: assistant message with 5 input, 30 output tokens
	transcript := "" +
		`{"type":"session_start","id":"s1"}` + "\n" +
		droidMessage(t, "m1", "user", nil) + "\n" +
		droidMessage(t, "m2", "assistant", map[string]int{
			"input_tokens": 10, "output_tokens": 20,
		}) + "\n" +
		`{"type":"session_event","data":"heartbeat"}` + "\n" +
		droidMessage(t, "m3", "assistant", map[string]int{
			"input_tokens": 5, "output_tokens": 30,
		}) + "\n"

	data := []byte(transcript)

	// With startOffset=0: should count all messages (m2 + m3)
	usageAll := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 0)
	if usageAll.InputTokens != 15 {
		t.Errorf("startOffset=0: InputTokens = %d, want 15", usageAll.InputTokens)
	}
	if usageAll.OutputTokens != 50 {
		t.Errorf("startOffset=0: OutputTokens = %d, want 50", usageAll.OutputTokens)
	}
	if usageAll.APICallCount != 2 {
		t.Errorf("startOffset=0: APICallCount = %d, want 2", usageAll.APICallCount)
	}

	// With startOffset=3: skip lines 0-2 (session_start, m1, m2).
	// Only line 3 (session_event, filtered) and line 4 (m3) remain.
	// Should count only m3's tokens.
	usageFrom3 := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 3)
	if usageFrom3.InputTokens != 5 {
		t.Errorf("startOffset=3: InputTokens = %d, want 5", usageFrom3.InputTokens)
	}
	if usageFrom3.OutputTokens != 30 {
		t.Errorf("startOffset=3: OutputTokens = %d, want 30", usageFrom3.OutputTokens)
	}
	if usageFrom3.APICallCount != 1 {
		t.Errorf("startOffset=3: APICallCount = %d, want 1", usageFrom3.APICallCount)
	}

	// Regression: using the OLD buggy code would have parsed all messages (ignoring
	// non-message entries), producing [m1, m2, m3], then sliced at index 3 which
	// is out of bounds — returning all tokens instead of just m3's.
	// With startOffset=1: skip only line 0 (session_start).
	// Lines 1 (m1), 2 (m2), 3 (session_event, filtered), 4 (m3) remain.
	usageFrom1 := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 1)
	if usageFrom1.InputTokens != 15 {
		t.Errorf("startOffset=1: InputTokens = %d, want 15", usageFrom1.InputTokens)
	}
	if usageFrom1.APICallCount != 2 {
		t.Errorf("startOffset=1: APICallCount = %d, want 2", usageFrom1.APICallCount)
	}
}

// Verify that startOffset beyond transcript length returns empty usage.
func TestCalculateTokenUsage_DroidStartOffsetBeyondEnd(t *testing.T) {
	t.Parallel()

	data := []byte(
		`{"type":"session_start","id":"s1"}` + "\n" +
			droidMessage(t, "m1", "assistant", map[string]int{
				"input_tokens": 10, "output_tokens": 20,
			}) + "\n",
	)

	usage := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 100)
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
	}
	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
}

// TestCondenseSession_TagsCheckpointSummaryWithHasInvestigation verifies that
// when state.Kind is KindAgentInvestigate, condensation propagates the kind
// through to CheckpointSummary.HasInvestigation on the metadata branch and
// writes the per-session investigate fields into the per-session
// Metadata. Mirrors the (untested) review-tagging path so future
// regressions in either flow are caught here.
//
// Tests in this file use t.Chdir for CWD-based git resolution, so this
// cannot be a parallel test.
func TestCondenseSession_TagsCheckpointSummaryWithHasInvestigation(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "2026-05-08-investigate-condensation"

	// Stage a transcript and a SaveStep so condensation has something to
	// process. Then mark the session as KindAgentInvestigate before
	// CondenseSession runs.
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"investigate flake"}}
{"type":"assistant","message":{"content":"On it."}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	// Modify a tracked file so SaveStep produces a non-empty session.
	trackedFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(trackedFile, []byte("agent-modified content"), 0o644))

	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.txt"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Investigate checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)

	// Tag the session as an investigation BEFORE condensation. Mirrors what
	// adoptInvestigateEnv does on the live session-state file.
	state.Kind = session.KindAgentInvestigate
	state.InvestigateRunID = "0123456789ab"
	state.InvestigateTopic = "Why is checkout flaky?"
	require.NoError(t, SaveSessionState(context.Background(), state))

	checkpointID := id.MustCheckpointID("aabbccdd1122")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped, "condensation must not skip when files are touched")

	// Read CheckpointSummary off the metadata branch and assert the
	// HasInvestigation umbrella flag flowed through.
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	checkpointTree, err := tree.Tree(checkpointID.Path())
	require.NoError(t, err)

	rootMeta, err := checkpointTree.File(paths.MetadataFileName)
	require.NoError(t, err)
	rootBytes, err := rootMeta.Contents()
	require.NoError(t, err)
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(rootBytes), &summary))

	require.True(t, summary.HasInvestigation, "CheckpointSummary.HasInvestigation must be true after investigate condensation")
	require.False(t, summary.HasReview, "CheckpointSummary.HasReview must remain false")

	// Per-session metadata must round-trip the investigate fields.
	sessionMeta, err := checkpointTree.File(checkpointID.Path() + "/0/" + paths.MetadataFileName)
	if err != nil {
		// Path style varies by tree iteration. Fall back to subtree lookup.
		subtree, subErr := checkpointTree.Tree("0")
		require.NoError(t, subErr)
		sessionMeta, err = subtree.File(paths.MetadataFileName)
		require.NoError(t, err)
	}
	sessionBytes, err := sessionMeta.Contents()
	require.NoError(t, err)
	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(sessionBytes), &meta))

	require.Equal(t, string(session.KindAgentInvestigate), meta.Kind, "per-session Kind")
	require.Equal(t, "0123456789ab", meta.InvestigateRunID, "per-session InvestigateRunID")
	require.Equal(t, "Why is checkout flaky?", meta.InvestigateTopic, "per-session InvestigateTopic")
}

func setupEndedSessionWithoutFiles(t *testing.T, s *ManualCommitStrategy, repo *git.Repository, dir, sessionID string) *SessionState {
	t.Helper()
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	endedAt := time.Now().UTC()
	state.Phase = session.PhaseEnded
	state.EndedAt = &endedAt
	state.FilesTouched = nil
	require.NoError(t, s.saveSessionState(context.Background(), state))
	return state
}

func TestCondenseSessionByID_ReusesCheckpointFromInterruptedEagerCondense(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	sessionID := "interrupted-eager-condense"
	setupEndedSessionWithoutFiles(t, s, repo, dir, sessionID)

	staleState, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	orphanID := id.MustCheckpointID("111111111111")
	result, err := s.CondenseSession(context.Background(), repo, orphanID, staleState, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	checkpoints, err := store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)

	require.NoError(t, s.CondenseSessionByID(context.Background(), sessionID))

	checkpoints, err = store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)
	assert.Equal(t, orphanID, checkpoints[0].CheckpointID)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, orphanID, state.LastCheckpointID)
	assert.Equal(t, result.TotalTranscriptLines, state.CheckpointTranscriptStart)
}

func TestCondenseAndMarkFullyCondensed_ReusesReservedAttemptAfterInterruptedWrite(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	sessionID := "reserved-eager-condense"
	setupEndedSessionWithoutFiles(t, s, repo, dir, sessionID)

	var reservedID id.CheckpointID
	reserveErr := MutateSessionState(context.Background(), sessionID, func(state *SessionState) error {
		var err error
		reservedID, _, err = ensureCondensationAttemptID(context.Background(), state)
		return err
	})
	require.NoError(t, reserveErr)

	staleState, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	result, err := s.CondenseSession(context.Background(), repo, reservedID, staleState, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	require.NoError(t, s.CondenseAndMarkFullyCondensed(context.Background(), sessionID))

	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	checkpoints, err := store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)
	assert.Equal(t, reservedID, checkpoints[0].CheckpointID)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, reservedID, state.LastCheckpointID)
	assert.True(t, state.PendingCondensationID().IsEmpty())
	assert.True(t, state.FullyCondensed)
}

func TestPrepareCommitMsg_ReusesReservedAttemptAfterSessionResume(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	sessionID := "resumed-interrupted-condense"
	setupEndedSessionWithoutFiles(t, s, repo, dir, sessionID)

	reservedID := id.MustCheckpointID("111111111111")
	require.NoError(t, MutateSessionState(context.Background(), sessionID, func(state *SessionState) error {
		state.BeginCondensationAttempt(reservedID)
		return nil
	}))

	require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, "", "continue", ""))
	resumed, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActive, resumed.Phase)
	require.Equal(t, reservedID, resumed.PendingCondensationID())

	commitMsgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("commit after resume\n"), 0o600))
	require.NoError(t, s.PrepareCommitMsg(context.Background(), commitMsgFile, "message"))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	checkpointID, found := trailers.ParseCheckpoint(string(content))
	require.True(t, found)
	assert.Equal(t, reservedID, checkpointID)
}

func TestCondensationSessionWrites_KeepSharedReservedCheckpointInOneBackend(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()

	ctx := settings.WithWorktreeRoot(t.Context(), dir)
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{})
	require.NoError(t, err)

	// The ULID was selected while git-refs was primary; the current default
	// primary is git-branch. Both sessions share the preselected checkpoint ID.
	checkpointID := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV")

	write := func(sessionID string) {
		t.Helper()
		opts := checkpoint.WriteOptions{
			CheckpointID: checkpointID,
			SessionID:    sessionID,
			Strategy:     StrategyNameManualCommit,
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
		}
		require.NoError(t, stores.Persistent.Write(ctx, condensationSessionWriteRequest(opts)))
	}

	write("reserved-session")
	write("ordinary-session")

	summary, err := stores.Persistent.Read(ctx, checkpointID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 2, "all sessions sharing a checkpoint ID must remain visible together")
}

func TestCondenseSessionByID_DoesNotReuseCheckpointAfterSessionAdvances(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)
	defer repo.Close()

	s := &ManualCommitStrategy{}
	sessionID := "advanced-after-interrupted-condense"
	setupEndedSessionWithoutFiles(t, s, repo, dir, sessionID)

	staleState, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	orphanID := id.MustCheckpointID("111111111111")
	result, err := s.CondenseSession(context.Background(), repo, orphanID, staleState, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	metadataDir := paths.EntireMetadataDir + "/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	advancedTranscript := testTranscriptPromptResponse + `{"type":"human","message":{"content":"another prompt"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(advancedTranscript), 0o644))
	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}))

	require.NoError(t, s.CondenseSessionByID(context.Background(), sessionID))

	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	checkpoints, err := store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, checkpoints, 2)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	assert.NotEqual(t, orphanID, state.LastCheckpointID)
}

// taskTranscriptSecret is a string with Shannon entropy > 4.5 that will
// trigger redaction — mirrors checkpoint_test.go's highEntropySecret so task
// transcript redaction is verified the same way session transcript redaction
// is.
const taskTranscriptSecret = "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA"

// setupCondensableSessionWithTranscript creates a git repo, writes a session
// transcript, and runs SaveStep so the session has a shadow branch and passes
// CondenseSession's existing no-transcript-no-files skip gate — the fixture
// shared by the task-record materializer tests below.
func setupCondensableSessionWithTranscript(t *testing.T, sessionID string) (*git.Repository, *SessionState) {
	t.Helper()
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"dispatch a subagent"}}
{"type":"assistant","message":{"content":"On it."}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	trackedFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(trackedFile, []byte("agent-modified content"), 0o644))

	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.txt"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	return repo, state
}

// checkpointTaskFile reads one file from a committed checkpoint's
// tasks/<tool-use-id>/ subtree off the metadata branch, returning ("", false)
// when the file doesn't exist.
func checkpointTaskFile(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID, relPath string) (string, bool) {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	file, err := tree.File(checkpointID.Path() + "/" + relPath)
	if err != nil {
		return "", false
	}
	content, err := file.Contents()
	require.NoError(t, err)
	return content, true
}

// TestCondenseSession_MaterializesCompletedTaskRecord_RegressionFor2058 is THE
// #2058 regression: a completed task record's transcript used to die at
// condensation because no producer ever set the (now deleted) WriteOptions
// IsTask/ToolUseID route, leaving writeTaskCheckpointEntries permanently
// unreachable. This proves CondenseSession now materializes a completed
// record's transcript (redacted) and metadata into the checkpoint tree, and
// that resetCheckpointWindow — the real post-write mutation site — then
// removes the record from session state.
func TestCondenseSession_MaterializesCompletedTaskRecord_RegressionFor2058(t *testing.T) {
	sessionID := "2026-08-19-task-record-materialize"
	repo, state := setupCondensableSessionWithTranscript(t, sessionID)

	dir := t.TempDir()
	agentTranscriptPath := filepath.Join(dir, "agent-transcript.jsonl")
	agentTranscript := `{"role":"assistant","content":"found it: ` + taskTranscriptSecret + `"}` + "\n"
	require.NoError(t, os.WriteFile(agentTranscriptPath, []byte(agentTranscript), 0o644))

	started := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	completed := started.Add(90 * time.Second)
	state.TaskRecords = []session.TaskRecord{
		{
			ToolUseID:              "toolu_regress2058",
			AgentID:                "agent-1",
			SubagentType:           "explorer",
			TaskDescription:        "find the bug",
			DeclaredTranscriptPath: agentTranscriptPath,
			Files:                  []string{"found.go"},
			StartedAt:              started,
			CompletedAt:            completed,
		},
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	checkpointID := id.MustCheckpointID("aabbccdd2058")
	result, err := (&ManualCommitStrategy{}).CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	jsonlContent, ok := checkpointTaskFile(t, repo, checkpointID, "tasks/toolu_regress2058/agent-agent-1.jsonl")
	require.True(t, ok, "tasks/toolu_regress2058/agent-agent-1.jsonl must exist — this is the #2058 regression")
	require.NotContains(t, jsonlContent, taskTranscriptSecret, "task transcript must be redacted")
	require.Contains(t, jsonlContent, "REDACTED")

	taskJSON, ok := checkpointTaskFile(t, repo, checkpointID, "tasks/toolu_regress2058/task.json")
	require.True(t, ok, "task.json must exist")
	var meta struct {
		ToolUseID       string   `json:"tool_use_id"`
		AgentID         string   `json:"agent_id"`
		SubagentType    string   `json:"subagent_type"`
		TaskDescription string   `json:"task_description"`
		Files           []string `json:"files"`
	}
	require.NoError(t, json.Unmarshal([]byte(taskJSON), &meta))
	require.Equal(t, "toolu_regress2058", meta.ToolUseID)
	require.Equal(t, "agent-1", meta.AgentID)
	require.Equal(t, "explorer", meta.SubagentType)
	require.Equal(t, "find the bug", meta.TaskDescription)
	require.Equal(t, []string{"found.go"}, meta.Files)

	// Record lifecycle: resetCheckpointWindow is the real post-write mutation
	// site all three condensation callers use. A completed record's payload
	// is now durably stored, so it must be removed from session state.
	resetCheckpointWindow(state)
	require.Empty(t, state.TaskRecords, "completed task record must be removed after materialization")
}

// TestCondenseSession_InFlightTaskRecord_TranscriptSoFarStoredRecordSurvives
// covers the in-flight half of the materializer contract: a record with
// CompletedAt still zero has its transcript-so-far stored (every checkpoint
// is self-contained), but the record itself must survive
// resetCheckpointWindow so the NEXT condensation re-materializes it.
func TestCondenseSession_InFlightTaskRecord_TranscriptSoFarStoredRecordSurvives(t *testing.T) {
	sessionID := "2026-08-19-task-record-inflight"
	repo, state := setupCondensableSessionWithTranscript(t, sessionID)

	dir := t.TempDir()
	agentTranscriptPath := filepath.Join(dir, "agent-transcript.jsonl")
	require.NoError(t, os.WriteFile(agentTranscriptPath, []byte(`{"role":"assistant","content":"still working"}`+"\n"), 0o644))

	state.TaskRecords = []session.TaskRecord{
		{
			ToolUseID:              "toolu_inflight",
			AgentID:                "agent-2",
			DeclaredTranscriptPath: agentTranscriptPath,
			StartedAt:              time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC),
			// CompletedAt intentionally left zero: still in flight.
		},
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	checkpointID := id.MustCheckpointID("aabbccddaa01")
	result, err := (&ManualCommitStrategy{}).CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	jsonlContent, ok := checkpointTaskFile(t, repo, checkpointID, "tasks/toolu_inflight/agent-agent-2.jsonl")
	require.True(t, ok, "in-flight record's transcript-so-far must be stored")
	require.Contains(t, jsonlContent, "still working")

	resetCheckpointWindow(state)
	require.Len(t, state.TaskRecords, 1, "in-flight record must survive so the next condensation re-materializes it")
	require.True(t, state.TaskRecords[0].CompletedAt.IsZero())
}

// TestCondenseSession_TaskRecordMissingTranscriptPath_RecordsUnavailableReason
// covers both unavailable-transcript shapes: a record with no resolvable path
// at all, and one whose declared path exists but reads as empty. Either way
// task.json must still be produced (with a stable, path-free reason category
// recorded, so the pointer isn't silently dropped) but no agent-<id>.jsonl,
// and condensation must otherwise proceed normally.
func TestCondenseSession_TaskRecordMissingTranscriptPath_RecordsUnavailableReason(t *testing.T) {
	tests := []struct {
		name         string
		toolUseID    string
		checkpointID string
		declaredPath func(t *testing.T) string
		wantReason   string
	}{
		{
			name:         "no declared or resolvable path",
			toolUseID:    "toolu_missing",
			checkpointID: "aabbccddaa02",
			declaredPath: func(*testing.T) string { return "" },
			wantReason:   taskTranscriptReasonUnresolvable,
		},
		{
			name:         "declared path exists but is empty",
			toolUseID:    "toolu_empty",
			checkpointID: "aabbccddaa03",
			declaredPath: func(t *testing.T) string {
				t.Helper()
				p := filepath.Join(t.TempDir(), "agent-transcript.jsonl")
				require.NoError(t, os.WriteFile(p, nil, 0o644))
				return p
			},
			wantReason: taskTranscriptReasonEmpty,
		},
		{
			name:         "declared path exceeds the blob size cap",
			toolUseID:    "toolu_toolarge",
			checkpointID: "aabbccddaa04",
			declaredPath: func(t *testing.T) string {
				t.Helper()
				p := filepath.Join(t.TempDir(), "agent-transcript.jsonl")
				// Sanitizing leaves this as-is (no Codex payloads to strip), so
				// the sanitized size the cap measures is the size written here.
				line := `{"type":"user","content":"` + strings.Repeat("x", 4096) + `"}` + "\n"
				var big strings.Builder
				for big.Len() <= agent.MaxChunkSize {
					big.WriteString(line)
				}
				require.NoError(t, os.WriteFile(p, []byte(big.String()), 0o644))
				return p
			},
			wantReason: taskTranscriptReasonTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := "2026-08-19-task-record-missing-" + tt.toolUseID
			repo, state := setupCondensableSessionWithTranscript(t, sessionID)

			state.TaskRecords = []session.TaskRecord{
				{
					ToolUseID:              tt.toolUseID,
					AgentID:                "agent-3",
					DeclaredTranscriptPath: tt.declaredPath(t),
					CompletedAt:            time.Date(2026, 8, 19, 9, 5, 0, 0, time.UTC),
				},
			}
			require.NoError(t, SaveSessionState(context.Background(), state))

			checkpointID := id.MustCheckpointID(tt.checkpointID)
			result, err := (&ManualCommitStrategy{}).CondenseSession(context.Background(), repo, checkpointID, state, nil)
			require.NoError(t, err)
			require.False(t, result.Skipped, "condensation must proceed normally even when a task transcript is unavailable")

			_, ok := checkpointTaskFile(t, repo, checkpointID, "tasks/"+tt.toolUseID+"/agent-agent-3.jsonl")
			require.False(t, ok, "no jsonl should be written when the transcript is unavailable")

			taskJSON, ok := checkpointTaskFile(t, repo, checkpointID, "tasks/"+tt.toolUseID+"/task.json")
			require.True(t, ok, "task.json must still exist, recording the unavailable reason")
			var meta struct {
				TranscriptUnavailableReason string `json:"transcript_unavailable_reason"`
			}
			require.NoError(t, json.Unmarshal([]byte(taskJSON), &meta))
			require.Equal(t, tt.wantReason, meta.TranscriptUnavailableReason)
			require.NotContains(t, meta.TranscriptUnavailableReason, string(filepath.Separator),
				"the reason must be a stable category, never a local filesystem path")

			// The session's own transcript must be unaffected.
			summary := readCommittedSummary(t, repo, checkpointID)
			require.NotEmpty(t, summary.Sessions)
		})
	}
}

// TestCondenseSession_PoisonedTaskRecord_SkippedNotWedged is the regression
// for the "poisoned record must not wedge condensation forever" hardening: a
// record with an unsafe ToolUseID must not abort the whole checkpoint write
// (which would re-fail on every future condensation, since completed records
// are only removed after a successful write), nor should it silently produce
// a task.json it can't safely be placed under. Alongside a valid record, the
// valid one must still materialize, condensation must still succeed, and the
// poisoned record — being completed — must still be dropped by
// resetCheckpointWindow's batch removal: it can never materialize, so keeping
// it around would retry it forever.
func TestCondenseSession_PoisonedTaskRecord_SkippedNotWedged(t *testing.T) {
	sessionID := "2026-08-19-task-record-poisoned"
	repo, state := setupCondensableSessionWithTranscript(t, sessionID)

	dir := t.TempDir()
	validTranscriptPath := filepath.Join(dir, "agent-transcript.jsonl")
	require.NoError(t, os.WriteFile(validTranscriptPath, []byte(`{"role":"assistant","content":"done"}`+"\n"), 0o644))

	completedAt := time.Date(2026, 8, 19, 9, 10, 0, 0, time.UTC)
	state.TaskRecords = []session.TaskRecord{
		{
			// Path-unsafe: fails validation.ValidateToolUseID.
			ToolUseID:              "../escape",
			AgentID:                "agent-poison",
			DeclaredTranscriptPath: validTranscriptPath,
			CompletedAt:            completedAt,
		},
		{
			ToolUseID:              "toolu_valid",
			AgentID:                "agent-valid",
			DeclaredTranscriptPath: validTranscriptPath,
			CompletedAt:            completedAt,
		},
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	checkpointID := id.MustCheckpointID("aabbccddaa04")
	result, err := (&ManualCommitStrategy{}).CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err, "a poisoned task record must not fail the whole checkpoint write")
	require.False(t, result.Skipped)

	jsonlContent, ok := checkpointTaskFile(t, repo, checkpointID, "tasks/toolu_valid/agent-agent-valid.jsonl")
	require.True(t, ok, "the valid record alongside the poisoned one must still materialize")
	require.Contains(t, jsonlContent, "done")

	// The poisoned record must produce no payload at all: tasks/ must contain
	// exactly the valid record's directory, nothing derived from "../escape".
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	commitTree, err := commit.Tree()
	require.NoError(t, err)
	tasksTree, err := commitTree.Tree(checkpointID.Path() + "/tasks")
	require.NoError(t, err)
	var taskDirs []string
	for _, entry := range tasksTree.Entries {
		taskDirs = append(taskDirs, entry.Name)
	}
	require.Equal(t, []string{"toolu_valid"}, taskDirs,
		"tasks/ must contain only the valid record's directory")

	resetCheckpointWindow(state)
	remaining := map[string]bool{}
	for _, r := range state.TaskRecords {
		remaining[r.ToolUseID] = true
	}
	require.False(t, remaining["../escape"],
		"a completed poisoned record can never materialize, so it must still be removed rather than retried forever")
	require.False(t, remaining["toolu_valid"], "the completed valid record was materialized and must also be removed")
}

// TestCondenseAndMarkFullyCondensed_RecordsOnlySessionMaterializes is the
// trigger half of invariant 7: a records-only session (read-only background
// subagent; no SaveStep, no shadow branch, no files, no parent transcript)
// must condense into a real checkpoint carrying tasks/<id>/. FullyCondensed is
// already true here because the task may complete after SessionEnd condensed
// the earlier state; the new task content must make the session eligible again.
func TestCondenseAndMarkFullyCondensed_RecordsOnlySessionMaterializes(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)

	agentTranscriptPath := filepath.Join(t.TempDir(), "agent-transcript.jsonl")
	require.NoError(t, os.WriteFile(agentTranscriptPath, []byte(`{"role":"assistant","content":"reviewed the diff; verdict: LGTM"}`+"\n"), 0o644))

	sessionID := "2026-08-20-records-only"
	now := time.Now()
	s := &ManualCommitStrategy{}
	require.NoError(t, s.saveSessionState(context.Background(), &SessionState{
		SessionID: sessionID, StartedAt: now, Phase: session.PhaseEnded, FullyCondensed: true,
		TaskRecords: []session.TaskRecord{{
			ToolUseID: "toolu_recordsonly", AgentID: "agent-ro", SubagentType: "reviewer",
			DeclaredTranscriptPath: agentTranscriptPath, StartedAt: now, CompletedAt: now,
		}},
	}))

	require.NoError(t, s.CondenseAndMarkFullyCondensed(context.Background(), sessionID))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.True(t, state.FullyCondensed)
	require.False(t, state.LastCheckpointID.IsEmpty(), "a records-only session must write a real checkpoint, not skip")
	require.Empty(t, state.TaskRecords, "the materialized record must be removed from state")

	jsonl, ok := checkpointTaskFile(t, repo, state.LastCheckpointID, "tasks/toolu_recordsonly/agent-agent-ro.jsonl")
	require.True(t, ok, "records-only condensation must materialize the record under tasks/<id>/")
	require.Contains(t, jsonl, "LGTM")
}

// TestResetCheckpointWindow_RemovesCompletedTaskRecordsKeepsInFlight is a
// focused unit test on resetCheckpointWindow's task-record removal, isolated
// from the full condensation write path.
func TestResetCheckpointWindow_RemovesCompletedTaskRecordsKeepsInFlight(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		TaskRecords: []session.TaskRecord{
			{ToolUseID: "completed-1", CompletedAt: time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)},
			{ToolUseID: "inflight-1"},
			{ToolUseID: "completed-2", CompletedAt: time.Date(2026, 8, 19, 9, 1, 0, 0, time.UTC)},
			{ToolUseID: "inflight-2"},
		},
	}

	resetCheckpointWindow(state)

	require.Len(t, state.TaskRecords, 2)
	remaining := map[string]bool{}
	for _, r := range state.TaskRecords {
		remaining[r.ToolUseID] = true
	}
	require.True(t, remaining["inflight-1"])
	require.True(t, remaining["inflight-2"])
	require.False(t, remaining["completed-1"])
	require.False(t, remaining["completed-2"])
}

// TestCheckpointStepCount covers the prompt-window math that produces the
// displayed "steps" count: SessionTurnCount - PromptWindowBase, floored at 1.
func TestCheckpointStepCount(t *testing.T) {
	tests := []struct {
		name             string
		sessionTurnCount int
		promptWindowBase int
		want             int
	}{
		{"first window of three prompts", 3, 0, 3},
		{"second window of two prompts", 5, 3, 2},
		{"no turns counted floors to 1", 0, 0, 1},
		// Back-to-back checkpoint: base not yet re-anchored, so it reports the same
		// count as the prior checkpoint rather than 0.
		{"back-to-back reports same as prior", 3, 0, 3},
		{"empty window floors to 1", 3, 3, 1},
		{"negative guard floors to 1", 2, 5, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &SessionState{
				SessionTurnCount: tt.sessionTurnCount,
				PromptWindowBase: tt.promptWindowBase,
			}
			if got := checkpointStepCount(s); got != tt.want {
				t.Errorf("checkpointStepCount() = %d, want %d", got, tt.want)
			}
		})
	}
}
