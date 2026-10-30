package antigravity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// The step shapes below are agy 1.4.2's, captured from a live invoke_subagent
// run (paths and prompt text shortened).

const (
	testParentID = "4010e6d3-6d62-4c4b-9a0e-1f2b3c4d5e6f"
	testChildID  = "9fca78fd-a601-4996-a47c-6126c87772a4"
)

func parentInvokeLines(childID string) []string {
	return []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nDelegate to a subagent\n</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"invoke_subagent","args":{"Model":"inherit","Prompt":"Read README.md and report its contents.","Role":"README Reader","TypeName":"research"}}],"input_tokens":7071,"cache_read_tokens":0,"output_tokens":62}`,
		fmt.Sprintf(`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created At: 2026-10-19T17:12:09-07:00\nCompleted At: 2026-10-19T17:12:09-07:00\nCreated the following subagent:\n{\n  \"conversationId\": \"%s\",\n  \"logAbsoluteUri\": \"file:///brain/%s/.system_generated/logs/transcript.jsonl\"\n}\nThe subagent will send you a message when it has completed its task."}`, childID, childID),
	}
}

func childLines(senderID string) []string {
	return []string{
		fmt.Sprintf(`{"step_index":0,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"The following is a <SYSTEM_MESSAGE> not actually sent by the user.\n\n<SYSTEM_MESSAGE>\n[Message] timestamp=2026-10-20T00:12:09Z sender=%s priority=MESSAGE_PRIORITY_HIGH content=Read README.md and report its contents.\n</SYSTEM_MESSAGE>"}`, senderID),
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"view_file","args":{"AbsolutePath":"/repo/README.md"}}],"input_tokens":5846,"cache_read_tokens":0,"output_tokens":119}`,
	}
}

// writeBrainTranscript writes a conversation's transcript into a brain
// directory set up by brainDir and returns its path.
func writeBrainTranscript(t *testing.T, brain, conversationID string, lines ...string) string {
	t.Helper()
	path := (&AntigravityAgent{}).ResolveSessionFile(brain, conversationID)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

// brainDir points the agent's session store at a fresh brain directory. Uses
// t.Setenv, so callers cannot t.Parallel.
func brainDir(t *testing.T) string {
	t.Helper()
	brain := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli", "brain")
	t.Setenv(antigravityTestBrainDirEnv, brain)
	return brain
}

func TestResolveSubagentSession_LinksChildToParent(t *testing.T) {
	brain := brainDir(t)
	parentPath := writeBrainTranscript(t, brain, testParentID, parentInvokeLines(testChildID)...)
	childPath := writeBrainTranscript(t, brain, testChildID, childLines(testParentID)...)

	link, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
	require.True(t, ok)
	require.Equal(t, agent.SubagentSessionLink{
		ParentSessionID:      testParentID,
		ToolUseID:            testChildID,
		ParentTranscriptPath: parentPath,
		SubagentType:         "research",
		TaskDescription:      "README Reader",
	}, link)
}

func TestResolveSubagentSession_TopLevelConversationIsNotASubagent(t *testing.T) {
	brain := brainDir(t)
	parentPath := writeBrainTranscript(t, brain, testParentID, parentInvokeLines(testChildID)...)

	_, ok := (&AntigravityAgent{}).ResolveSubagentSession(parentPath)
	require.False(t, ok)
}

// The sender must have created this child: a message from some other
// conversation does not make a session its subagent.
func TestResolveSubagentSession_SenderThatDidNotCreateTheChild(t *testing.T) {
	brain := brainDir(t)
	writeBrainTranscript(t, brain, testParentID, parentInvokeLines("11111111-2222-3333-4444-555555555555")...)
	childPath := writeBrainTranscript(t, brain, testChildID, childLines(testParentID)...)

	_, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
	require.False(t, ok)
}

func TestResolveSubagentSession_FailsClosed(t *testing.T) {
	t.Run("parent transcript missing", func(t *testing.T) {
		brain := brainDir(t)
		childPath := writeBrainTranscript(t, brain, testChildID, childLines(testParentID)...)
		_, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
		require.False(t, ok)
	})
	t.Run("child transcript not written yet", func(t *testing.T) {
		brain := brainDir(t)
		writeBrainTranscript(t, brain, testParentID, parentInvokeLines(testChildID)...)
		childPath := (&AntigravityAgent{}).ResolveSessionFile(brain, testChildID)
		_, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
		require.False(t, ok)
	})
	t.Run("sender that is not a session id", func(t *testing.T) {
		brain := brainDir(t)
		childPath := writeBrainTranscript(t, brain, testChildID, childLines("..")...)
		_, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
		require.False(t, ok)
	})
	t.Run("transcript outside the brain directory", func(t *testing.T) {
		brainDir(t)
		outside := filepath.Join(t.TempDir(), "transcript_full.jsonl")
		require.NoError(t, os.WriteFile(outside, []byte(strings.Join(childLines(testParentID), "\n")), 0o600))
		_, ok := (&AntigravityAgent{}).ResolveSubagentSession(outside)
		require.False(t, ok)
	})
}

// A parent that invoked several subagents in one step still resolves; only
// the labels are unknown.
func TestResolveSubagentSession_SeveralCallsInOneStepDropsLabels(t *testing.T) {
	brain := brainDir(t)
	lines := parentInvokeLines(testChildID)
	lines[1] = `{"step_index":1,"type":"PLANNER_RESPONSE","tool_calls":[{"name":"invoke_subagent","args":{"TypeName":"research","Role":"A"}},{"name":"invoke_subagent","args":{"TypeName":"research","Role":"B"}}]}`
	writeBrainTranscript(t, brain, testParentID, lines...)
	childPath := writeBrainTranscript(t, brain, testChildID, childLines(testParentID)...)

	link, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
	require.True(t, ok)
	require.Equal(t, testParentID, link.ParentSessionID)
	require.Empty(t, link.SubagentType)
	require.Empty(t, link.TaskDescription)
}

func preInvocationEvent(t *testing.T, transcriptPath string, invocationNum int) *agent.Event {
	t.Helper()
	stdin := fmt.Sprintf(`{"conversationId":%q,"transcriptPath":%q,"invocationNum":%d,"initialNumSteps":1}`,
		testParentID, transcriptPath, invocationNum)
	ev, err := (&AntigravityAgent{}).ParseHookEvent(context.Background(), HookNamePreInvocation, strings.NewReader(stdin))
	require.NoError(t, err)
	require.NotNil(t, ev)
	require.Equal(t, agent.TurnStart, ev.Type)
	return ev
}

// A new prompt's USER_INPUT step is on disk at its PreInvocation; a parent
// resumed by its subagent's message is not, so that execution continues the
// turn instead of restarting it.
func TestParsePreInvocation_WakeUpFromSubagentIsConditional(t *testing.T) {
	brain := brainDir(t)

	prompt := writeBrainTranscript(t, brain, testParentID, parentInvokeLines(testChildID)[0])
	require.False(t, preInvocationEvent(t, prompt, 0).SuppressIfSessionActive, "a new prompt starts a turn")

	wake := writeBrainTranscript(t, brain, testParentID, parentInvokeLines(testChildID)...)
	require.True(t, preInvocationEvent(t, wake, 0).SuppressIfSessionActive, "a wake-up continues the turn")

	missing := (&AntigravityAgent{}).ResolveSessionFile(brain, testChildID)
	require.False(t, preInvocationEvent(t, missing, 0).SuppressIfSessionActive,
		"a conversation's first call, before its transcript exists, starts a turn")

	require.True(t, preInvocationEvent(t, prompt, 1).SuppressIfSessionActive, "follow-up calls stay conditional")
}

// A turn's subagents are the children this parent created, in any turn: a
// later turn can message a child created earlier. A conversation the parent
// never created is not its subagent, whether it messaged the parent or the
// parent messaged it, so it cannot hold the parent's turn open.
func TestTurnSubagents_OnlyChildrenThisParentCreated(t *testing.T) {
	brain := brainDir(t)
	const foreignID = "77777777-8888-4999-aaaa-bbbbbbbbbbbb"
	message := func(step int, sender string) string {
		return fmt.Sprintf(`{"step_index":%d,"type":"SYSTEM_MESSAGE","content":"[Message] timestamp=2026-10-20T01:50:00Z sender=%s content=hi"}`, step, sender)
	}
	sendTo := func(step int, recipient string) string {
		return fmt.Sprintf(`{"step_index":%d,"type":"PLANNER_RESPONSE","tool_calls":[{"name":"send_message","args":{"Recipient":"%s"}}]}`, step, recipient)
	}
	lines := append(parentInvokeLines(testChildID), message(3, testChildID))
	firstTurnLines := len(lines)
	lines = append(lines,
		`{"step_index":4,"type":"USER_INPUT","content":"<USER_REQUEST>\nAsk again\n</USER_REQUEST>"}`,
		sendTo(5, testChildID),
		message(6, foreignID),
		sendTo(7, foreignID),
	)
	parentPath := writeBrainTranscript(t, brain, testParentID, lines...)

	got, err := (&AntigravityAgent{}).TurnSubagents(parentPath, firstTurnLines)
	require.NoError(t, err)
	require.Equal(t, map[string]int{testChildID: 1}, got)
}

// A prompt longer than the head read leaves step 0 cut off mid-line. Its
// sender, near the start, still links the child.
func TestResolveSubagentSession_LongPromptCutOffByTheHeadRead(t *testing.T) {
	brain := brainDir(t)
	writeBrainTranscript(t, brain, testParentID, parentInvokeLines(testChildID)...)
	lines := childLines(testParentID)
	lines[0] = strings.Replace(lines[0], "content=Read README.md", "content="+strings.Repeat("x", 300<<10), 1)
	require.Greater(t, len(lines[0]), subagentHeadBytes)
	childPath := writeBrainTranscript(t, brain, testChildID, lines...)

	link, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
	require.True(t, ok)
	require.Equal(t, testParentID, link.ParentSessionID)
}

// The fallback for a cut-off step reads only a SYSTEM_MESSAGE: a cut-off user
// prompt quoting a message line is not a subagent's first step.
func TestResolveSubagentSession_CutOffUserPromptIsNotASubagent(t *testing.T) {
	brain := brainDir(t)
	writeBrainTranscript(t, brain, testParentID, parentInvokeLines(testChildID)...)
	prompt := fmt.Sprintf(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"[Message] timestamp=2026-10-20T00:12:09Z sender=%s content=%s`, testParentID, strings.Repeat("x", 300<<10))
	childPath := writeBrainTranscript(t, brain, testChildID, prompt)

	_, ok := (&AntigravityAgent{}).ResolveSubagentSession(childPath)
	require.False(t, ok)
}
