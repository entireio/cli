//go:build integration

package integration

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// TestTokenScope_NestedSubagentCountedOnce: a subagent launched by a subagent
// is named only in its parent's transcript (Claude Code writes the launch's
// tool result there), with its own transcript beside the parent's. The Stop
// after it worked puts its tokens in the checkpoint's subagent tokens; a later
// Stop after it worked more adds only the growth.
func TestTokenScope_NestedSubagentCountedOnce(t *testing.T) {
	t.Parallel()
	r := newTokenTurnRun(t)
	const (
		parent = "a71c317bfe762dd44"
		child  = "a212f19b2ed455bdd"
	)
	subagentsDir := paths.SubagentsDir(filepath.Dir(r.s.TranscriptPath), r.s.ID)
	parentPath := filepath.Join(subagentsDir, paths.AgentTranscriptFileName(parent))
	childPath := filepath.Join(subagentsDir, paths.AgentTranscriptFileName(child))
	usageLine := func(agentID, messageID string, tokens int) map[string]any {
		return map[string]any{"type": "assistant", "isSidechain": true, "agentId": agentID, "message": map[string]any{
			"id": messageID, "role": "assistant", "usage": map[string]int{"input_tokens": tokens, "output_tokens": tokens},
		}}
	}
	launchLine := func(agentID, toolUseID, launched string) map[string]any {
		return map[string]any{"type": "user", "isSidechain": true, "agentId": agentID,
			"message": map[string]any{"role": "user", "content": []map[string]any{{
				"type": "tool_result", "tool_use_id": toolUseID,
				"content": fmt.Sprintf("Async agent launched successfully.\nagentId: %s (internal ID)", launched),
			}}},
			"toolUseResult": map[string]any{"isAsync": true, "status": "async_launched", "agentId": launched},
		}
	}

	r.start(10, 0, "a.txt")
	writeJSONLines(t, parentPath, usageLine(parent, "msg_parent_1", 3), launchLine(parent, "toolu_child", child))
	writeJSONLines(t, childPath, usageLine(child, "msg_child_1", 4))
	r.s.TranscriptBuilder.messages = append(r.s.TranscriptBuilder.messages, map[string]any{
		"uuid": "launch-" + parent, "type": "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{{
			"type": "tool_result", "tool_use_id": "toolu_parent", "content": "Async agent launched successfully.",
		}}},
		"toolUseResult": map[string]any{"isAsync": true, "status": "async_launched", "agentId": parent},
	})
	require.NoError(t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
	r.add(0, 3+4)
	r.stop()
	r.commit("a.txt")

	// The nested agent keeps working into the next turn.
	r.start(20, 0, "b.txt")
	writeJSONLines(t, childPath, usageLine(child, "msg_child_1", 4), usageLine(child, "msg_child_2", 5))
	r.add(0, 5)
	r.stop()
	r.commit("b.txt")

	r.verify()
}
