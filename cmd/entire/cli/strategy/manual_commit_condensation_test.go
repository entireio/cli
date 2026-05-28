package strategy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	// Register agents so GetByAgentType works in tests.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
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

	if err := os.WriteFile(filepath.Join(dir, "entire-agent-"+name), []byte(script), 0o755); err != nil {
		t.Fatalf("write external summary agent binary: %v", err)
	}
}

func TestCalculateTokenUsage_CursorReturnsNil(t *testing.T) {
	t.Parallel()

	// Cursor transcripts don't contain token usage data, so CalculateTokenUsage
	// should return nil (not an empty struct) to signal "no data available".
	transcript := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`)

	ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("GetByAgentType(Cursor) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, transcript, 0, "")
	if result != nil {
		t.Errorf("CalculateTokenUsage(Cursor) = %+v, want nil", result)
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

func TestExtractUserPrompts_Cursor(t *testing.T) {
	t.Parallel()

	// Cursor uses "role":"user" instead of "type":"human". extractUserPromptsFromLines
	// handles both via the "role" fallback.
	prompts := extractUserPrompts(agent.AgentTypeCursor, cursorSampleTranscript)
	if len(prompts) != 3 {
		t.Fatalf("extractUserPrompts(Cursor) returned %d prompts, want 3", len(prompts))
	}

	if !strings.Contains(prompts[0], "create a file with contents 'a'") {
		t.Errorf("prompt[0] = %q, expected to contain file creation request", prompts[0])
	}
	if !strings.Contains(prompts[2], "bingo") {
		t.Errorf("prompt[2] = %q, expected to contain 'bingo'", prompts[2])
	}

	// Verify <user_query> tags are stripped
	for i, p := range prompts {
		if strings.Contains(p, "<user_query>") || strings.Contains(p, "</user_query>") {
			t.Errorf("prompt[%d] still contains <user_query> tags: %q", i, p)
		}
	}
}

func TestExtractUserPrompts_CursorEmpty(t *testing.T) {
	t.Parallel()

	prompts := extractUserPrompts(agent.AgentTypeCursor, "")
	if len(prompts) != 0 {
		t.Errorf("extractUserPrompts(Cursor, empty) = %v, want empty", prompts)
	}
}

func TestCalculateTokenUsage_CursorRealTranscript(t *testing.T) {
	t.Parallel()

	// Even with a multi-line real transcript, Cursor should return nil
	ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("GetByAgentType(Cursor) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, []byte(cursorSampleTranscript), 0, "")
	if result != nil {
		t.Errorf("CalculateTokenUsage(Cursor, real transcript) = %+v, want nil", result)
	}
}

func TestCalculateTokenUsage_CursorWithOffset(t *testing.T) {
	t.Parallel()

	// Offset should not matter — Cursor always returns nil
	ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("GetByAgentType(Cursor) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, []byte(cursorSampleTranscript), 3, "")
	if result != nil {
		t.Errorf("CalculateTokenUsage(Cursor, offset=3) = %+v, want nil", result)
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
// CommittedMetadata. Mirrors the (untested) review-tagging path so future
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
	var meta checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(sessionBytes), &meta))

	require.Equal(t, string(session.KindAgentInvestigate), meta.Kind, "per-session Kind")
	require.Equal(t, "0123456789ab", meta.InvestigateRunID, "per-session InvestigateRunID")
	require.Equal(t, "Why is checkout flaky?", meta.InvestigateTopic, "per-session InvestigateTopic")
}
