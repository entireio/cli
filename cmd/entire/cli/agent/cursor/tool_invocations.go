package cursor

import (
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Compile-time interface assertion.
var _ agent.ToolInvocationScanner = (*CursorAgent)(nil)

// ScanToolInvocations walks the tool calls recorded in a Cursor JSONL
// transcript. See agent.ToolInvocationScanner for the contract, including that
// hints is a performance filter and not a semantic one.
//
// Cursor records tool_use content blocks in the same shape as Claude Code, so
// the walk is transcript.ScanToolUseBlocks — including the envelope
// normalization Cursor specifically needs, since it keys the envelope "role"
// where Claude Code keys it "type". What is Cursor-specific is the input key,
// and it needs no divergence: Cursor's shell tool is `Shell` and it keys the
// command `command`, the same key Claude Code uses. That was the one thing
// blocking this implementation, and it was confirmed against a real session —
// see toolInput and testdata/real_session_tool_use.jsonl.
//
// SubagentType is deliberately left unset, which is not the same omission it
// would be for Claude Code. ToolInvocation.SubagentType exists so the probe can
// match a dispatch of Entire's own `entire-search` subagent, and Cursor never
// had one: the installer reported the search skill as unsupported for Cursor
// until it began scaffolding a real SKILL.md, so no Cursor session can carry a
// legacy dispatch to miss. Cursor does dispatch subagents of its own, but the
// input key naming them is unconfirmed, and guessing it is exactly the silent
// false negative agent.ToolInvocationScanner warns against. Confirm the key
// against a real session before reading anything into a subagent field here.
func (c *CursorAgent) ScanToolInvocations(transcriptData []byte, hints [][]byte, visit func(agent.ToolInvocation) bool) bool {
	return transcript.ScanToolUseBlocks(transcriptData, hints, func(block transcript.ContentBlock) bool {
		var input toolInput
		if err := json.Unmarshal(block.Input, &input); err != nil {
			return false
		}
		return visit(agent.ToolInvocation{
			Tool:    block.Name,
			Command: input.Command,
		})
	})
}
