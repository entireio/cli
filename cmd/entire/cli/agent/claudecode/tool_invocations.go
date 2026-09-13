package claudecode

import (
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// ScanToolInvocations walks the Bash/Task-style tool calls recorded in a Claude
// Code JSONL transcript. See agent.ToolInvocationScanner for the contract,
// including that hints is a performance filter and not a semantic one.
//
// The walk itself — line splitting, the two byte-level prefilters, the
// assistant-envelope gate, and why each is done the way it is — lives in
// transcript.ScanToolUseBlocks, shared with Cursor. What stays here is the only
// Claude-specific part: which input keys carry a shell command and a dispatched
// subagent.
//
// A block whose input fails to decode is skipped, not reported: the transcript
// is written by another process and this is a telemetry read.
func (c *ClaudeCodeAgent) ScanToolInvocations(transcriptData []byte, hints [][]byte, visit func(agent.ToolInvocation) bool) bool {
	return transcript.ScanToolUseBlocks(transcriptData, hints, func(block transcript.ContentBlock) bool {
		var input toolInput
		if err := json.Unmarshal(block.Input, &input); err != nil {
			return false
		}
		return visit(agent.ToolInvocation{
			Tool:         block.Name,
			Command:      input.Command,
			SubagentType: input.SubagentType,
		})
	})
}
