//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// agy runs a subagent as a conversation with its own hooks. The hook order and
// step shapes below are agy 1.4.2's, captured from a live invoke_subagent run:
// the child's transcript does not exist during its first hooks, and the parent
// is resumed by a new execution (invocationNum 0) once the child replies,
// before the child's message reaches the parent's transcript.
func TestAntigravity_SubagentWorkBelongsToTheParent(t *testing.T) {
	t.Parallel()
	env := newAntigravityEnv(t)
	extraEnv := []string{
		"ENTIRE_ANTIGRAVITY_STATUS_DIR=" + t.TempDir(),
		"ENTIRE_ANTIGRAVITY_CONFIG_DIR=" + t.TempDir(),
	}
	const (
		parentID = "4010e6d3-6d62-4c4b-9a0e-1f2b3c4d5e6f"
		childID  = "9fca78fd-a601-4996-a47c-6126c87772a4"
	)
	transcript := func(id string) string {
		return filepath.Join(antigravityBrainDir(env.RepoDir), id, ".system_generated", "logs", "transcript_full.jsonl")
	}
	hook := func(id, name string, fields map[string]any) {
		t.Helper()
		base := map[string]any{
			"conversationId":        id,
			"workspacePaths":        []string{env.RepoDir},
			"transcriptPath":        transcript(id),
			"artifactDirectoryPath": filepath.Join(antigravityBrainDir(env.RepoDir), id),
		}
		require.NoError(t, runAntigravityHookWithEnv(t, env.RepoDir, name, mergeMaps(base, fields), extraEnv), name)
	}
	planner := func(step, input, output int, extra string) string {
		return fmt.Sprintf(`{"step_index":%d,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE"%s,"input_tokens":%d,"cache_read_tokens":0,"output_tokens":%d}`,
			step, extra, input, output)
	}
	stop := map[string]any{"executionNum": 0, "terminationReason": "NO_TOOL_CALL", "error": ""}

	// The parent's prompt and its invoke_subagent call.
	appendLines(t, transcript(parentID),
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nDelegate the change to a subagent\n</USER_REQUEST>"}`)
	hook(parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 1})
	appendLines(t, transcript(parentID),
		planner(1, 7071, 62, `,"tool_calls":[{"name":"invoke_subagent","args":{"Prompt":"Create child.txt","Role":"File Writer","TypeName":"research"}}]`),
		fmt.Sprintf(`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created the following subagent:\n{\n  \"conversationId\": \"%s\"\n}"}`, childID))
	hook(parentID, "pre-invocation", map[string]any{"invocationNum": 1, "initialNumSteps": 3})
	appendLines(t, transcript(parentID), planner(3, 7460, 116, ""))
	hook(parentID, "stop", mergeMaps(stop, map[string]any{"fullyIdle": false}))

	// The child: its first call runs before its transcript exists.
	hook(childID, "session-start", nil)
	hook(childID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 0})
	appendLines(t, transcript(childID),
		fmt.Sprintf(`{"step_index":0,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-10-20T00:12:09Z sender=%s priority=MESSAGE_PRIORITY_HIGH content=Create child.txt\n</SYSTEM_MESSAGE>"}`, parentID))
	env.WriteFile("child.txt", "from the subagent\n")
	hook(childID, "pre-tool-use", map[string]any{
		"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": "child.txt"}},
		"stepIdx":  1,
	})
	appendLines(t, transcript(childID), planner(1, 5846, 119, `,"tool_calls":[{"name":"write_to_file","args":{"TargetFile":"child.txt"}}]`))

	child, err := env.GetSessionState(childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	require.Equal(t, parentID, child.ParentSessionID, "the child's first tool call marks it")
	require.Empty(t, child.FilesTouched, "the child's changes are recorded on the parent")

	// A commit while the child still runs links the parent only, carrying the
	// child's work as its task.
	_, _ = commitCheckpointTokens(t, env, "", "Add child.txt", "child.txt")
	first := env.GetLatestCheckpointID()
	summaryRaw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(first))
	require.True(t, ok)
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryRaw), &summary))
	require.Len(t, summary.Sessions, 1, "the child is never a checkpoint session of its own: %s", summaryRaw)
	_, ok = env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(first, childID, paths.AgentTranscriptFileName(childID)))
	require.True(t, ok, "the parent's checkpoint carries the child's transcript so far")

	// The child replies and finishes.
	appendLines(t, transcript(childID), planner(3, 6224, 115, fmt.Sprintf(`,"tool_calls":[{"name":"send_message","args":{"Recipient":"%s"}}]`, parentID)), planner(5, 6449, 20, ""))
	hook(childID, "stop", mergeMaps(stop, map[string]any{"fullyIdle": true}))

	// The parent resumes, before the child's message is in its transcript.
	hook(parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 4})
	appendLines(t, transcript(parentID),
		fmt.Sprintf(`{"step_index":4,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-10-20T00:12:15Z sender=%s content=done\n</SYSTEM_MESSAGE>"}`, childID),
		planner(5, 7694, 71, ""))
	env.WriteFile("parent.txt", "from the parent\n")
	hook(parentID, "pre-tool-use", map[string]any{
		"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": "parent.txt"}},
		"stepIdx":  6,
	})
	hook(parentID, "stop", mergeMaps(stop, map[string]any{"fullyIdle": true}))

	// The parent's turn kept its start through the wake-up: all three of its
	// calls count, and none of the child's.
	require.Equal(t, &agent.TokenUsage{InputTokens: 7071 + 7460 + 7694, OutputTokens: 62 + 116 + 71, APICallCount: 3},
		sessionTokenTotal(t, env, parentID))
	parent, err := env.GetSessionState(parentID)
	require.NoError(t, err)
	rec := parent.FindTaskRecord(childID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero())
	require.Equal(t, &agent.TokenUsage{InputTokens: 5846 + 6224 + 6449, OutputTokens: 119 + 115 + 20, APICallCount: 3}, rec.TokenUsage)
	child, err = env.GetSessionState(childID)
	require.NoError(t, err)
	if child != nil && child.TokenUsage != nil {
		require.Zero(t, child.TokenUsage.InputTokens, "the child's usage is on the parent's task, not its own session")
	}
	// The parent's displayed total adds the child once, as its subagent usage.
	total := parent.TotalTokenUsage()
	require.NotNil(t, total.SubagentTokens)
	require.Equal(t, 5846+6224+6449, total.SubagentTokens.InputTokens)
	require.Equal(t, 3, total.SubagentTokens.APICallCount)

	_, _ = commitCheckpointTokens(t, env, first, "Add parent.txt", "parent.txt")
	second := env.GetLatestCheckpointID()
	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(second, childID, "task.json"))
	require.True(t, ok, "the completed task is in the next checkpoint")
	var task checkpoint.TaskRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &task))
	require.Equal(t, childID, task.AgentID)
	require.Equal(t, "research", task.SubagentType)
	require.Equal(t, "File Writer", task.TaskDescription)
	require.NotContains(t, task.Files, "child.txt", "the first commit already took child.txt: %s", raw)
	require.Equal(t, 5846+6224+6449, task.TokenUsage.InputTokens, "%s", raw)
}

// agyParentChild drives one parent conversation and its subagent through the
// hooks, with the step shapes agy 1.4.2 writes.
type agyParentChild struct {
	t                 *testing.T
	env               *TestEnv
	extraEnv          []string
	parentID, childID string
}

func newAgyParentChild(t *testing.T) *agyParentChild {
	t.Helper()
	return &agyParentChild{
		t:        t,
		env:      newAntigravityEnv(t),
		extraEnv: []string{"ENTIRE_ANTIGRAVITY_STATUS_DIR=" + t.TempDir(), "ENTIRE_ANTIGRAVITY_CONFIG_DIR=" + t.TempDir()},
		parentID: "a0528243-0000-4000-8000-000000000001",
		childID:  "a3a70f0a-0000-4000-8000-000000000002",
	}
}

func (s *agyParentChild) path(id string) string {
	return filepath.Join(antigravityBrainDir(s.env.RepoDir), id, ".system_generated", "logs", "transcript_full.jsonl")
}

func (s *agyParentChild) hook(id, name string, fields map[string]any) {
	s.t.Helper()
	base := map[string]any{"conversationId": id, "workspacePaths": []string{s.env.RepoDir}, "transcriptPath": s.path(id)}
	require.NoError(s.t, runAntigravityHookWithEnv(s.t, s.env.RepoDir, name, mergeMaps(base, fields), s.extraEnv), name)
}

func (s *agyParentChild) stop(id string, fullyIdle bool) {
	s.t.Helper()
	s.hook(id, "stop", map[string]any{"executionNum": 0, "error": "", "fullyIdle": fullyIdle})
}

func agyCall(step, input, output int, extra string) string {
	return fmt.Sprintf(`{"step_index":%d,"type":"PLANNER_RESPONSE"%s,"input_tokens":%d,"cache_read_tokens":0,"output_tokens":%d}`, step, extra, input, output)
}

func agyMessage(step int, sender string) string {
	return fmt.Sprintf(`{"step_index":%d,"type":"SYSTEM_MESSAGE","content":"[Message] timestamp=2026-10-20T01:50:00Z sender=%s content=hi"}`, step, sender)
}

// start runs the parent's prompt and invoke_subagent, the parent's first
// pause, and the child's first call and tool call (which marks it).
func (s *agyParentChild) start() {
	s.t.Helper()
	appendLines(s.t, s.path(s.parentID), `{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\nDelegate\n</USER_REQUEST>"}`)
	s.hook(s.parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 1})
	appendLines(s.t, s.path(s.parentID),
		agyCall(1, 6975, 161, `,"tool_calls":[{"name":"invoke_subagent","args":{"Role":"File Creator","TypeName":"self"}}]`),
		fmt.Sprintf(`{"step_index":2,"type":"GENERIC","content":"Created the following subagent:\n{\"conversationId\": \"%s\"}"}`, s.childID),
		agyCall(3, 7461, 25, ""))
	s.stop(s.parentID, false)

	s.hook(s.childID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 0})
	appendLines(s.t, s.path(s.childID), agyMessage(0, s.parentID))
	s.env.WriteFile("notes2.txt", "second note\n")
	s.hook(s.childID, "pre-tool-use", map[string]any{"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": "notes2.txt"}}})
}

// childReplies writes the child's reply call and its last step.
func (s *agyParentChild) childReplies() {
	s.t.Helper()
	appendLines(s.t, s.path(s.childID),
		agyCall(1, 7300, 60, ""),
		agyCall(3, 7400, 70, fmt.Sprintf(`,"tool_calls":[{"name":"send_message","args":{"Recipient":"%s"}}]`, s.parentID)),
		agyCall(5, 7548, 60, ""))
}

// parentReadsReply is the parent resumed by the child's reply, answering it.
func (s *agyParentChild) parentReadsReply() {
	s.t.Helper()
	s.hook(s.parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 4})
	appendLines(s.t, s.path(s.parentID), agyMessage(4, s.childID), agyCall(5, 7618, 107, ""))
}

func (s *agyParentChild) requireParentEnded() {
	s.t.Helper()
	parent, err := s.env.GetSessionState(s.parentID)
	require.NoError(s.t, err)
	require.NotNil(s.t, parent)
	require.Equal(s.t, "idle", string(parent.Phase), "the parent's turn is ended for it")
	require.Equal(s.t, &agent.TokenUsage{InputTokens: 6975 + 7461 + 7618, OutputTokens: 161 + 25 + 107, APICallCount: 3}, parent.TokenUsage)
	rec := parent.FindTaskRecord(s.childID)
	require.NotNil(s.t, rec)
	require.Equal(s.t, &agent.TokenUsage{InputTokens: 7300 + 7400 + 7548, OutputTokens: 60 + 70 + 60, APICallCount: 3}, rec.TokenUsage)
}

func (s *agyParentChild) requireParentActive(why string) {
	s.t.Helper()
	parent, err := s.env.GetSessionState(s.parentID)
	require.NoError(s.t, err)
	require.Equal(s.t, "active", string(parent.Phase), why)
}

// When the parent's last Stop (fullyIdle=false) and its subagent's end land
// in either order, agy sends the parent no further Stop. The parent's turn is
// ended for it, once, with all its calls counted.
func TestAntigravity_ParentTurnEndsWhenItsSubagentFinishes(t *testing.T) {
	t.Parallel()
	for name, parentStopsFirst := range map[string]bool{"parent stops first": true, "subagent ends first": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newAgyParentChild(t)
			s.start()
			s.childReplies()
			s.parentReadsReply()
			if parentStopsFirst {
				s.stop(s.parentID, false)
				s.stop(s.childID, true)
			} else {
				s.stop(s.childID, true)
				s.stop(s.parentID, false)
			}
			s.requireParentEnded()
		})
	}
}

// A reply the parent has not read yet will wake it, so its turn is not over
// even though the subagent has stopped.
func TestAntigravity_ParentTurnWaitsForAnUnreadReply(t *testing.T) {
	t.Parallel()
	s := newAgyParentChild(t)
	s.start()
	s.childReplies()
	s.stop(s.childID, true)
	s.requireParentActive("the child's reply is still on its way to the parent")

	s.parentReadsReply()
	s.stop(s.parentID, false)
	s.requireParentEnded()
}

// A subagent woken again by the parent is running, whatever its task record
// says about its earlier turn.
func TestAntigravity_ParentTurnWaitsForAReWokenSubagent(t *testing.T) {
	t.Parallel()
	s := newAgyParentChild(t)
	s.start()
	s.childReplies()
	s.stop(s.childID, true)

	// The parent reads the reply and messages the child again; the child wakes.
	s.hook(s.parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 4})
	appendLines(t, s.path(s.parentID), agyMessage(4, s.childID),
		agyCall(5, 7618, 107, fmt.Sprintf(`,"tool_calls":[{"name":"send_message","args":{"Recipient":"%s"}}]`, s.childID)))
	appendLines(t, s.path(s.childID), agyMessage(6, s.parentID))
	s.hook(s.childID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 7})
	s.stop(s.parentID, false)
	s.requireParentActive("the woken subagent is still running")

	appendLines(t, s.path(s.childID), agyCall(7, 100, 10, fmt.Sprintf(`,"tool_calls":[{"name":"send_message","args":{"Recipient":"%s"}}]`, s.parentID)))
	s.stop(s.childID, true)
	s.requireParentActive("its second reply is unread")

	s.hook(s.parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 6})
	appendLines(t, s.path(s.parentID), agyMessage(6, s.childID), agyCall(7, 50, 5, ""))
	s.stop(s.parentID, false)
	parent, err := s.env.GetSessionState(s.parentID)
	require.NoError(t, err)
	require.Equal(t, "idle", string(parent.Phase))
	require.Equal(t, 3+1, parent.TokenUsage.APICallCount, "every parent call, once")
}

// A subagent's first tool call can be a shell command that edits and commits;
// it is marked there, before any commit could link it on its own.
func TestAntigravity_SubagentMarkedAtAShellCommand(t *testing.T) {
	t.Parallel()
	s := newAgyParentChild(t)
	appendLines(t, s.path(s.parentID), `{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\nDelegate\n</USER_REQUEST>"}`)
	s.hook(s.parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 1})
	appendLines(t, s.path(s.parentID),
		agyCall(1, 6975, 161, `,"tool_calls":[{"name":"invoke_subagent","args":{"TypeName":"self"}}]`),
		fmt.Sprintf(`{"step_index":2,"type":"GENERIC","content":"Created the following subagent:\n{\"conversationId\": \"%s\"}"}`, s.childID))
	s.hook(s.childID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 0})
	appendLines(t, s.path(s.childID), agyMessage(0, s.parentID))
	s.hook(s.childID, "pre-tool-use", map[string]any{"toolCall": map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "git commit -am x"}}})

	child, err := s.env.GetSessionState(s.childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	require.Equal(t, s.parentID, child.ParentSessionID)
}

// A Stop that arrives after the parent's turn was already ended for it (agy
// can still report fullyIdle=true later) ends nothing more: the turn's step and
// tokens were saved once, by that turn end.
func TestAntigravity_LateStopAfterSubagentTurnEndCountsNothing(t *testing.T) {
	t.Parallel()
	s := newAgyParentChild(t)
	s.start()
	s.childReplies()
	s.parentReadsReply()
	s.env.WriteFile("parent.txt", "from the parent\n")
	s.hook(s.parentID, "pre-tool-use", map[string]any{"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": "parent.txt"}}})
	s.stop(s.parentID, false)
	s.stop(s.childID, true)
	s.requireParentEnded()
	before, err := s.env.GetSessionState(s.parentID)
	require.NoError(t, err)
	require.Positive(t, before.StepCount, "the turn changed a file, so its end saved a step")

	s.stop(s.parentID, true)

	after, err := s.env.GetSessionState(s.parentID)
	require.NoError(t, err)
	require.Equal(t, before.TokenUsage, after.TokenUsage, "the late Stop adds no tokens")
	require.Equal(t, before.StepCount, after.StepCount, "the late Stop saves no step")
}

// A subagent busy for over an hour is still running: its own hooks keep it
// from aging into a stuck session that its waiting parent would count done.
func TestAntigravity_LongRunningSubagentKeepsItsParentWaiting(t *testing.T) {
	t.Parallel()
	s := newAgyParentChild(t)
	s.start()
	child, err := s.env.GetSessionState(s.childID)
	require.NoError(t, err)
	require.Equal(t, s.parentID, child.ParentSessionID)
	longAgo := time.Now().Add(-2 * time.Hour)
	child.LastInteractionTime = &longAgo
	require.NoError(t, s.env.WriteSessionState(s.childID, child))

	s.hook(s.childID, "pre-tool-use", map[string]any{"toolCall": map[string]any{"name": "view_file", "args": map[string]any{"AbsolutePath": "notes2.txt"}}})
	s.stop(s.parentID, false)
	s.requireParentActive("the subagent's tool call shows it is still running")
}
