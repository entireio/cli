package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"
)

// TestSessionMetadataToJSON_CarriesSubagentTokenFields pins that the per-session
// token_usage in --json carries the whole persisted TokenUsage. The export used
// to copy four fields into its own struct, which silently dropped the subagent
// totals, their completeness marker, and the API call count.
func TestSessionMetadataToJSON_CarriesSubagentTokenFields(t *testing.T) {
	t.Parallel()

	complete := true
	meta := &checkpoint.Metadata{
		SessionID: "subagent-tokens",
		TokenUsage: &types.TokenUsage{
			InputTokens:  100,
			OutputTokens: 20,
			APICallCount: 3,
			SubagentTokens: &types.TokenUsage{
				InputTokens:  40,
				OutputTokens: 8,
				APICallCount: 2,
			},
			SubagentTokensComplete: &complete,
		},
	}

	raw, err := json.Marshal(sessionMetadataToJSON(0, meta))
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	usage, ok := decoded["token_usage"].(map[string]any)
	require.True(t, ok, "token_usage missing: %s", raw)
	require.InEpsilon(t, float64(3), usage["api_call_count"], 0.0001, "api_call_count: %s", raw)
	require.Equal(t, true, usage["subagent_tokens_complete"], "subagent_tokens_complete: %s", raw)
	sub, ok := usage["subagent_tokens"].(map[string]any)
	require.True(t, ok, "subagent_tokens missing: %s", raw)
	require.InEpsilon(t, float64(40), sub["input_tokens"], 0.0001)
	require.InEpsilon(t, float64(8), sub["output_tokens"], 0.0001)
}

// writeCheckpointWithTasks writes a one-session checkpoint carrying two subagent
// task records: one with a stored transcript and one whose transcript was
// unavailable at condensation.
func writeCheckpointWithTasks(t *testing.T, cpID id.CheckpointID) []byte {
	t.Helper()
	repo := setupExportRepo(t)
	childTranscript := []byte(`{"type":"assistant","message":"child work"}` + "\n")
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteOptions{
		SessionID:  "session-with-tasks",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":"parent"}` + "\n")),
		Tasks: []checkpoint.TaskPayload{
			{
				ToolUseID:       "toolu_stored",
				AgentID:         "agentstored",
				SubagentType:    "Explore",
				TaskDescription: "find the bug",
				Transcript:      redact.AlreadyRedacted(childTranscript),
				Files:           []string{"a.go"},
				TokenUsage:      &types.TokenUsage{InputTokens: 7, OutputTokens: 3, APICallCount: 1},
				StartedAt:       started,
				CompletedAt:     started.Add(time.Minute),
			},
			{
				ToolUseID:                   "toolu_missing",
				AgentID:                     "agentmissing",
				SubagentType:                "general-purpose",
				StartedAt:                   started,
				TranscriptUnavailableReason: "transcript unreadable",
			},
		},
	})
	return childTranscript
}

func TestRunExplainExport_JSONListsSubagentTasks(t *testing.T) {
	cpID := id.MustCheckpointID("dada11112222")
	writeCheckpointWithTasks(t, cpID)

	var stdout, stderr bytes.Buffer
	err := runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       cpID.String(),
		json:         true,
		sessionIndex: -1,
	})
	require.NoError(t, err, "stderr: %s", stderr.String())

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded), "output: %s", stdout.String())
	require.NotContains(t, decoded, "partial")
	tasks, ok := decoded["tasks"].([]any)
	require.True(t, ok, "tasks array missing: %s", stdout.String())
	require.Len(t, tasks, 2)

	byID := map[string]map[string]any{}
	for _, raw := range tasks {
		task, isMap := raw.(map[string]any)
		require.True(t, isMap)
		toolUseID, isString := task["tool_use_id"].(string)
		require.True(t, isString)
		byID[toolUseID] = task
	}

	stored := byID["toolu_stored"]
	require.NotNil(t, stored, "toolu_stored missing: %s", stdout.String())
	require.Equal(t, "agentstored", stored["agent_id"])
	require.Equal(t, "Explore", stored["subagent_type"])
	require.Equal(t, "find the bug", stored["task_description"])
	require.Equal(t, []any{"a.go"}, stored["files"])
	require.Equal(t, true, stored["transcript_stored"])
	require.Equal(t, "2026-09-01T10:00:00Z", stored["started_at"])
	require.Equal(t, "2026-09-01T10:01:00Z", stored["completed_at"])
	usage, ok := stored["token_usage"].(map[string]any)
	require.True(t, ok)
	require.InEpsilon(t, float64(7), usage["input_tokens"], 0.0001)
	require.NotContains(t, stored, "transcript_unavailable_reason")

	missing := byID["toolu_missing"]
	require.NotNil(t, missing, "toolu_missing missing: %s", stdout.String())
	require.Equal(t, false, missing["transcript_stored"])
	require.Equal(t, "transcript unreadable", missing["transcript_unavailable_reason"])
	require.NotContains(t, missing, "completed_at", "an in-flight task must not report a zero completion time")

	// The JSON envelope stays metadata-only: no transcript bytes.
	require.NotContains(t, stdout.String(), "child work")
}

func TestRunExplainExport_JSONTasksEmptyArrayWhenNone(t *testing.T) {
	repo := setupExportRepo(t)
	cpID := id.MustCheckpointID("dada33334444")
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteOptions{
		SessionID:  "session-no-tasks",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user"}` + "\n")),
	})

	var stdout, stderr bytes.Buffer
	require.NoError(t, runExplainExport(context.Background(), &stdout, &stderr, explainExportOptions{
		target:       cpID.String(),
		json:         true,
		sessionIndex: -1,
	}))

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	require.Equal(t, []any{}, decoded["tasks"], "a checkpoint without subagents reports an empty tasks array: %s", stdout.String())
}

// TestBuildCheckpointJSONEnvelope_TaskReadErrorsArePartial: an unreadable task
// record follows the session contract — a stub entry carrying only its ID and
// the error, plus the top-level partial flag — and an unreadable task list
// surfaces as tasks_error.
func TestBuildCheckpointJSONEnvelope_TaskReadErrorsArePartial(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("dadacccc1111")
	summary := &checkpoint.CheckpointSummary{
		Sessions: []checkpoint.SessionFilePaths{{Metadata: "da/dacccc1111/0/metadata.json"}},
	}
	contents := map[int]*checkpoint.SessionContent{0: {Metadata: checkpoint.Metadata{SessionID: "ok"}}}

	reader := &stubCommittedReader{
		summary:  summary,
		contents: contents,
		tasks: []checkpoint.TaskEntry{
			{ToolUseID: "toolu_good", Record: checkpoint.TaskRecord{ToolUseID: "toolu_good", AgentID: "agentgood"}, TranscriptStored: true},
			{ToolUseID: "toolu_bad", Err: errors.New("parse task.json: unexpected end of JSON input")},
		},
	}
	envelope, failed := buildCheckpointJSONEnvelope(context.Background(), reader, summary, cpID)
	require.Empty(t, failed, "the sessions themselves read fine")
	require.True(t, envelope.Partial)
	require.Len(t, envelope.Tasks, 2)

	raw, err := json.Marshal(envelope.Tasks[1])
	require.NoError(t, err)
	var bad map[string]any
	require.NoError(t, json.Unmarshal(raw, &bad))
	require.Equal(t, map[string]any{
		"tool_use_id": "toolu_bad",
		"error":       "parse task.json: unexpected end of JSON input",
	}, bad, "an unreadable record must not carry fields that look like real data")

	var stdout, stderr bytes.Buffer
	err = writeCheckpointJSONEnvelope(&stdout, &stderr, cpID, envelope, failed)
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 task record(s) unreadable")
	require.Contains(t, stderr.String(), "toolu_bad")

	listFails := &stubCommittedReader{summary: summary, contents: contents, tasksErr: errors.New("read tasks/: object not found")}
	envelope, _ = buildCheckpointJSONEnvelope(context.Background(), listFails, summary, cpID)
	require.True(t, envelope.Partial)
	require.Nil(t, envelope.Tasks)
	require.Equal(t, "read tasks/: object not found", envelope.TasksError)
}

func runExplainCmdForTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newExplainCmd()
	// As under the root command, which silences both globally.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestExplainCmd_TaskTranscriptStreamsSubagentBytes(t *testing.T) {
	cpID := id.MustCheckpointID("dada55556666")
	child := writeCheckpointWithTasks(t, cpID)

	for _, selector := range []string{"toolu_stored", "agentstored"} {
		stdout, stderr, err := runExplainCmdForTest(t, cpID.String(), "--transcript", "--task", selector)
		require.NoError(t, err, "selector %s; stderr: %s", selector, stderr)
		require.Equal(t, string(child), stdout, "selector %s", selector)
	}
}

func TestExplainCmd_TaskTranscriptUnavailableReportsReason(t *testing.T) {
	cpID := id.MustCheckpointID("dada77778888")
	writeCheckpointWithTasks(t, cpID)

	stdout, _, err := runExplainCmdForTest(t, cpID.String(), "--transcript", "--task", "toolu_missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "transcript unreadable")
	require.Empty(t, stdout)
}

func TestExplainCmd_TaskUnknownSelectorNamesAvailableTasks(t *testing.T) {
	cpID := id.MustCheckpointID("dada99990000")
	writeCheckpointWithTasks(t, cpID)

	_, _, err := runExplainCmdForTest(t, cpID.String(), "--transcript", "--task", "toolu_nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "toolu_nope")
	require.Contains(t, err.Error(), "toolu_stored")
}

func TestExplainCmd_TaskRequiresTranscript(t *testing.T) {
	setupExportRepo(t)

	_, _, err := runExplainCmdForTest(t, "aaaa11112222", "--task", "toolu_x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "--task only applies with --transcript")
}

func TestExplainCmd_TaskAndSessionIndexMutuallyExclusive(t *testing.T) {
	setupExportRepo(t)

	_, _, err := runExplainCmdForTest(t, "aaaa11112222", "--transcript", "--task", "toolu_x", "--session-index", "0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "none of the others can be")
}

// TestExplainCmd_TaskExactToolUseIDWinsOverAgentID: tool_use_ids and agent_ids
// share the selector namespace. When one record's agent_id equals another's
// tool_use_id, the exact tool_use_id names its record uniquely.
func TestExplainCmd_TaskExactToolUseIDWinsOverAgentID(t *testing.T) {
	repo := setupExportRepo(t)
	cpID := id.MustCheckpointID("dadadddd2222")
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteOptions{
		SessionID:  "session-overlapping-ids",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user"}` + "\n")),
		Tasks: []checkpoint.TaskPayload{
			{ToolUseID: "task-a", AgentID: "task-b", Transcript: redact.AlreadyRedacted([]byte("from task-a\n"))},
			{ToolUseID: "task-b", AgentID: "agent-b", Transcript: redact.AlreadyRedacted([]byte("from task-b\n"))},
		},
	})

	stdout, _, err := runExplainCmdForTest(t, cpID.String(), "--transcript", "--task", "task-b")
	require.NoError(t, err)
	require.Equal(t, "from task-b\n", stdout)
}

// TestExportTokenUsage_BoundsSubagentDepth: token usage comes from pushed
// metadata.json / task.json. A deep subagent_tokens chain grows the indented
// output quadratically, so the export bounds it at types.MaxSubagentDepth.
func TestExportTokenUsage_BoundsSubagentDepth(t *testing.T) {
	t.Parallel()

	deep := &types.TokenUsage{InputTokens: 1}
	for range 200 {
		deep = &types.TokenUsage{InputTokens: 1, SubagentTokens: deep}
	}
	depth := func(u *types.TokenUsage) int {
		n := 0
		for ; u != nil && u.SubagentTokens != nil; u = u.SubagentTokens {
			n++
		}
		return n
	}

	session := sessionMetadataToJSON(0, &checkpoint.Metadata{SessionID: "deep", TokenUsage: deep})
	require.LessOrEqual(t, depth(session.TokenUsage), types.MaxSubagentDepth)
	require.Equal(t, 1, session.TokenUsage.InputTokens)

	task := taskEntryToJSON(checkpoint.TaskEntry{ToolUseID: "toolu_deep", Record: checkpoint.TaskRecord{TokenUsage: deep}})
	require.LessOrEqual(t, depth(task.TokenUsage), types.MaxSubagentDepth)
}

// TestExplainCmd_TaskAmbiguousSelectorFails: a resumed subagent keeps its
// agent_id across Task calls, so an agent_id can name several task records.
// Picking one silently would stream the wrong transcript.
func TestExplainCmd_TaskAmbiguousSelectorFails(t *testing.T) {
	repo := setupExportRepo(t)
	cpID := id.MustCheckpointID("dadaaaaabbbb")
	writeCheckpointForExport(t, repo, cpID, checkpoint.WriteOptions{
		SessionID:  "session-resumed-agent",
		Transcript: redact.AlreadyRedacted([]byte(`{"type":"user"}` + "\n")),
		Tasks: []checkpoint.TaskPayload{
			{ToolUseID: "toolu_first", AgentID: "agentshared", Transcript: redact.AlreadyRedacted([]byte("first\n"))},
			{ToolUseID: "toolu_second", AgentID: "agentshared", Transcript: redact.AlreadyRedacted([]byte("second\n"))},
		},
	})

	stdout, _, err := runExplainCmdForTest(t, cpID.String(), "--transcript", "--task", "agentshared")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ambiguous")
	require.Contains(t, err.Error(), "toolu_first")
	require.Contains(t, err.Error(), "toolu_second")
	require.Empty(t, stdout)

	// The tool_use_id stays an unambiguous selector.
	stdout, _, err = runExplainCmdForTest(t, cpID.String(), "--transcript", "--task", "toolu_second")
	require.NoError(t, err)
	require.Equal(t, "second\n", stdout)
}
