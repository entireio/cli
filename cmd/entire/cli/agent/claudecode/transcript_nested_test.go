package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Subagents can launch subagents of their own. Claude Code records such a
// launch's tool result in the launching subagent's transcript, not the main
// one, and writes the launched agent's transcript to the same flat
// subagents/agent-<id>.jsonl directory (shapes from a real Claude Code 2.1.291
// session).

// subagentLine is a line of subagent agentID's transcript: every one carries
// the agent's own ID at the top level.
func subagentLine(agentID, body string) string {
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":true,"agentId":%q,%s}`, agentID, body)
}

// subagentUsageLine is an assistant message of subagent agentID.
func subagentUsageLine(agentID, messageID string, in, out int) string {
	return subagentLine(agentID, fmt.Sprintf(`"type":"assistant","message":{"id":%q,"role":"assistant","usage":{"input_tokens":%d,"output_tokens":%d}}`, messageID, in, out))
}

// subagentLaunchLine is subagent parentID's tool result for an Agent call
// that launched childID in the background.
func subagentLaunchLine(parentID, toolUseID, childID string) string {
	return subagentLine(parentID, fmt.Sprintf(`"type":"user","message":{"role":"user","content":[{"tool_use_id":%q,"type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully.\nagentId: %s (internal ID)"}]}]},`+
		`"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":%q,"description":"look around"}`, toolUseID, childID, childID))
}

// subagentTextResultLine is subagent parentID's tool result that has only
// text, as older transcripts do.
func subagentTextResultLine(parentID, toolUseID, text string) string {
	return subagentLine(parentID, fmt.Sprintf(`"type":"user","message":{"role":"user","content":[{"tool_use_id":%q,"type":"tool_result","content":[{"type":"text","text":%q}]}]}`, toolUseID, text))
}

// subagentToolUseLine is subagent agentID's assistant call of tool name.
func subagentToolUseLine(agentID, toolUseID, name string) string {
	return subagentLine(agentID, fmt.Sprintf(`"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}`, toolUseID, name))
}

// mainLaunchLine is the main transcript's tool result for an Agent call that
// launched agentID in the background.
func mainLaunchLine(toolUseID, agentID string) string {
	return agentResultLine(toolUseID, agentID, "async_launched", "Async agent launched successfully.\nagentId: "+agentID+" (internal ID)")
}

const mainUsageLine = `{"type":"assistant","uuid":"a-main","message":{"id":"m-main","usage":{"input_tokens":1000,"output_tokens":100}}}`

func newSubagentsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sess", "subagents")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

func writeAgentTranscript(t *testing.T, subagentsDir, agentID string, lines ...string) {
	t.Helper()
	writeJSONLFile(t, filepath.Join(subagentsDir, "agent-"+agentID+".jsonl"), lines...)
}

func subagentUsage(t *testing.T, data []byte, subagentsDir string) *agent.TokenUsage {
	t.Helper()
	usage, err := (&ClaudeCodeAgent{}).CalculateTotalTokenUsage(data, 0, subagentsDir)
	require.NoError(t, err)
	assert.Equal(t, 1000, usage.InputTokens, "main usage must not absorb subagent usage")
	assert.Equal(t, 1, usage.APICallCount)
	return usage.SubagentTokens
}

func TestCalculateTotalTokenUsage_NestedSubagents(t *testing.T) {
	t.Parallel()

	const (
		parent = "a71c317bfe762dd44"
		child  = "a212f19b2ed455bdd"
		other  = "a0000000000other1"
	)
	for name, tc := range map[string]struct {
		parentLines []string
		childLines  []string
		otherLines  []string
		mainLines   []string
		want        agent.TokenUsage
	}{
		"launched by a subagent": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentLaunchLine(parent, "toolu_p1", child),
			},
			childLines: []string{subagentUsageLine(child, "m-c", 20, 2)},
			want:       agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2},
		},
		// Two results naming the child (a launch and a follow-up), and a
		// sibling that names it too: one transcript, read once.
		"launched twice": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentLaunchLine(parent, "toolu_p1", child),
				subagentLaunchLine(parent, "toolu_p2", child),
			},
			childLines: []string{subagentUsageLine(child, "m-c", 20, 2)},
			otherLines: []string{
				subagentUsageLine(other, "m-o", 40, 4),
				subagentLaunchLine(other, "toolu_o1", child),
			},
			mainLines: []string{mainLaunchLine("toolu_other", other)},
			want:      agent.TokenUsage{InputTokens: 70, OutputTokens: 7, APICallCount: 3},
		},
		"launches that form a cycle": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentLaunchLine(parent, "toolu_p1", child),
			},
			childLines: []string{
				subagentUsageLine(child, "m-c", 20, 2),
				subagentLaunchLine(child, "toolu_c1", parent),
			},
			want: agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2},
		},
		// A fork of the parent copies the parent's history, so its calls
		// merge with the parent's by message.id.
		"nested fork": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentLaunchLine(parent, "toolu_p1", child),
			},
			childLines: []string{
				subagentUsageLine(child, "m-p", 10, 1),
				subagentUsageLine(child, "m-c", 20, 2),
			},
			want: agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2},
		},
		// A subagent's text can quote any agent's ID (a Read of a log, a
		// Bash grep), so only its structured results and its Agent calls'
		// results launch: a mention in other text is not followed, even
		// when the transcript exists.
		"named by text only": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentTextResultLine(parent, "toolu_p1", "done\nagentId: "+child+" (for resuming)"),
			},
			childLines: []string{subagentUsageLine(child, "m-c", 20, 2)},
			want:       agent.TokenUsage{InputTokens: 10, OutputTokens: 1, APICallCount: 1},
		},
		// Claude Code 2.1.311 writes no toolUseResult in subagent
		// transcripts: a nested launch is named only in the text of the
		// Agent call's tool result.
		"async launch named by Agent result text only": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentToolUseLine(parent, "toolu_p1", "Agent"),
				subagentTextResultLine(parent, "toolu_p1", "Async agent launched successfully.\nagentId: "+child+" (internal ID - do not mention to user. Use SendMessage with to: '"+child+"' to continue this agent.)\nThe agent is working in the background."),
			},
			childLines: []string{subagentUsageLine(child, "m-c", 20, 2)},
			want:       agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2},
		},
		"foreground launch named by Agent result text only": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentToolUseLine(parent, "toolu_p1", "Agent"),
				subagentTextResultLine(parent, "toolu_p1", "Found two call sites.\nagentId: "+child+" (use SendMessage with to: '"+child+"' to continue this agent)"),
			},
			childLines: []string{subagentUsageLine(child, "m-c", 20, 2)},
			want:       agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2},
		},
		// The launched agent's own Agent result is followed in turn.
		"chain named by Agent result text only": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentToolUseLine(parent, "toolu_p1", "Agent"),
				subagentTextResultLine(parent, "toolu_p1", "Async agent launched successfully.\nagentId: "+child+" (internal ID)"),
			},
			childLines: []string{
				subagentUsageLine(child, "m-c", 20, 2),
				subagentToolUseLine(child, "toolu_c1", "Agent"),
				subagentTextResultLine(child, "toolu_c1", "Async agent launched successfully.\nagentId: "+other+" (internal ID)"),
			},
			otherLines: []string{subagentUsageLine(other, "m-o", 40, 4)},
			want:       agent.TokenUsage{InputTokens: 70, OutputTokens: 7, APICallCount: 3},
		},
		// The report before the trailer can quote another agent; as in the
		// main transcript, only the last "agentId:" names the launch.
		"Agent result quoting another ID": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentToolUseLine(parent, "toolu_p1", "Agent"),
				subagentTextResultLine(parent, "toolu_p1", "The log mentions agentId: "+other+".\nagentId: "+child+" (internal ID)"),
			},
			childLines: []string{subagentUsageLine(child, "m-c", 20, 2)},
			otherLines: []string{subagentUsageLine(other, "m-o", 40, 4)},
			want:       agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2},
		},
		"named by a Read result's text": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentToolUseLine(parent, "toolu_p1", "Read"),
				subagentTextResultLine(parent, "toolu_p1", "log.txt: Async agent launched successfully.\nagentId: "+child+" (internal ID)"),
			},
			childLines: []string{subagentUsageLine(child, "m-c", 20, 2)},
			want:       agent.TokenUsage{InputTokens: 10, OutputTokens: 1, APICallCount: 1},
		},
		"quoted ID with no transcript": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentTextResultLine(parent, "toolu_p1", "The log said agentId: a00000000missing"),
			},
			want: agent.TokenUsage{InputTokens: 10, OutputTokens: 1, APICallCount: 1},
		},
		"quoted ID of a sibling": {
			parentLines: []string{
				subagentUsageLine(parent, "m-p", 10, 1),
				subagentTextResultLine(parent, "toolu_p1", "The other agent was agentId: "+other),
			},
			otherLines: []string{subagentUsageLine(other, "m-o", 40, 4)},
			mainLines:  []string{mainLaunchLine("toolu_other", other)},
			want:       agent.TokenUsage{InputTokens: 50, OutputTokens: 5, APICallCount: 2},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			subagentsDir := newSubagentsDir(t)
			writeAgentTranscript(t, subagentsDir, parent, tc.parentLines...)
			if tc.childLines != nil {
				writeAgentTranscript(t, subagentsDir, child, tc.childLines...)
			}
			if tc.otherLines != nil {
				writeAgentTranscript(t, subagentsDir, other, tc.otherLines...)
			}
			mainLines := append([]string{mainLaunchLine("toolu_parent", parent)}, tc.mainLines...)
			mainLines = append(mainLines, mainUsageLine)

			got := subagentUsage(t, buildJSONL(mainLines...), subagentsDir)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, *got)
		})
	}
}

// A subagent's Workflow run is found from the subagent's own transcript.
func TestCalculateTotalTokenUsage_WorkflowRunLaunchedBySubagent(t *testing.T) {
	t.Parallel()

	const parent = "a71c317bfe762dd44"
	subagentsDir := newSubagentsDir(t)
	runDir := filepath.Join(subagentsDir, "workflows", "wf_e5264e60-494")
	require.NoError(t, os.MkdirAll(runDir, 0o755))
	writeJSONLFile(t, filepath.Join(runDir, "agent-ae3d7b8f2930c8787.jsonl"),
		subagentUsageLine("ae3d7b8f2930c8787", "m-w", 20, 2))
	writeAgentTranscript(t, subagentsDir, parent,
		subagentUsageLine(parent, "m-p", 10, 1),
		subagentLine(parent, `"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_wf","type":"tool_result","content":"Workflow started."}]},`+
			`"toolUseResult":{"status":"async_launched","taskType":"local_workflow","runId":"wf_e5264e60-494"}`))

	got := subagentUsage(t, buildJSONL(mainLaunchLine("toolu_parent", parent), mainUsageLine), subagentsDir)
	require.NotNil(t, got)
	assert.Equal(t, agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2}, *got)
}

// Launches are followed maxSubagentLaunchDepth levels below the main
// transcript; a chain deeper than that stops there instead of reading on.
func TestCalculateTotalTokenUsage_NestedLaunchDepthIsCapped(t *testing.T) {
	t.Parallel()

	subagentsDir := newSubagentsDir(t)
	chainID := func(level int) string { return fmt.Sprintf("a%016d", level) }
	for level := 1; level <= maxSubagentLaunchDepth+3; level++ {
		writeAgentTranscript(t, subagentsDir, chainID(level),
			subagentUsageLine(chainID(level), fmt.Sprintf("m-%d", level), 1, 1),
			subagentLaunchLine(chainID(level), fmt.Sprintf("toolu_%d", level), chainID(level+1)))
	}

	got := subagentUsage(t, buildJSONL(mainLaunchLine("toolu_parent", chainID(1)), mainUsageLine), subagentsDir)
	require.NotNil(t, got)
	assert.Equal(t, maxSubagentLaunchDepth, got.APICallCount)
}

// Import computes each turn's subagent total from the transcript prefix up to
// that turn: a subagent's children count once the prefix has launched the
// subagent, and not before.
func TestCalculateTotalTokenUsage_NestedSubagentsFollowTheTranscriptPrefix(t *testing.T) {
	t.Parallel()

	const (
		parent = "a71c317bfe762dd44"
		child  = "a212f19b2ed455bdd"
	)
	subagentsDir := newSubagentsDir(t)
	writeAgentTranscript(t, subagentsDir, parent,
		subagentUsageLine(parent, "m-p", 10, 1),
		subagentLaunchLine(parent, "toolu_p1", child))
	writeAgentTranscript(t, subagentsDir, child, subagentUsageLine(child, "m-c", 20, 2))

	before := subagentUsage(t, buildJSONL(mainUsageLine), subagentsDir)
	assert.Nil(t, before, "a prefix that has not launched the parent must count none of its children")

	after := subagentUsage(t, buildJSONL(mainUsageLine, mainLaunchLine("toolu_parent", parent)), subagentsDir)
	require.NotNil(t, after)
	assert.Equal(t, agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2}, *after)
}

// Every line of a subagent's transcript carries the subagent's own top-level
// agentId, so the structured-result prefilter keys on an agentId inside the
// toolUseResult.
func TestToolUseResultMayNameAgent(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		line string
		want bool
	}{
		"subagent's launch":      {subagentLaunchLine("aparent0000000001", "toolu_1", "achild00000000001"), true},
		"main launch":            {mainLaunchLine("toolu_1", "achild00000000001"), true},
		"subagent's Read result": {subagentLine("aparent0000000001", `"type":"user","message":{"role":"user","content":[]},"toolUseResult":{"type":"text","file":{"filePath":"/repo/a.go"}}`), false},
		"subagent's text":        {subagentUsageLine("aparent0000000001", "m-1", 1, 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, toolUseResultMayNameAgent([]byte(tc.line)))
		})
	}
}
