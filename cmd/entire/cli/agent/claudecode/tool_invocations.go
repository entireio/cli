package claudecode

import (
	"bytes"
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// toolUseToken is the literal that must appear in a raw transcript line for it
// to carry a tool_use content block. Both quotes are part of the token on
// purpose: it makes the check exact without whitespace normalization, and it is
// what keeps `"tool_use_id"` — which appears on every tool_result line, the
// single largest false-positive class for any substring probe — from matching.
var toolUseToken = []byte(`"tool_use"`)

// ScanToolInvocations walks the Bash/Task-style tool calls recorded in a Claude
// Code JSONL transcript. See agent.ToolInvocationScanner for the contract,
// including that hints is a performance filter and not a semantic one.
//
// Lines are split by hand with bytes.IndexByte rather than with bufio.Scanner:
// a single Claude Code transcript line routinely carries a multi-megabyte tool
// result, so Scanner's 64KiB default token limit would silently truncate the
// walk. Two byte-level prefilters run before any JSON work, so a transcript
// with no candidate line costs one pass and zero allocations — the same order
// as the substring probe this replaces, on the miss path that dominates.
func (c *ClaudeCodeAgent) ScanToolInvocations(transcriptData []byte, hints [][]byte, visit func(agent.ToolInvocation) bool) bool {
	for rest := transcriptData; len(rest) > 0; {
		line := rest
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			line, rest = rest[:idx], rest[idx+1:]
		} else {
			rest = nil
		}
		if !lineMayCarryInvocation(line, hints) {
			continue
		}
		if scanLineInvocations(line, visit) {
			return true
		}
	}
	return false
}

// lineMayCarryInvocation applies the two prefilters: the caller's hints (nil
// means visit every line) and the tool_use token. Hints are checked first
// because they are the selective half — a transcript typically has thousands of
// tool_use lines and a handful mentioning whatever the caller is looking for.
func lineMayCarryInvocation(line []byte, hints [][]byte) bool {
	if hints != nil && !containsAny(line, hints) {
		return false
	}
	return bytes.Contains(line, toolUseToken)
}

// containsAny reports whether line contains at least one of needles.
func containsAny(line []byte, needles [][]byte) bool {
	for _, needle := range needles {
		if bytes.Contains(line, needle) {
			return true
		}
	}
	return false
}

// scanLineInvocations decodes one candidate line and offers each of its
// tool_use blocks to visit. Malformed lines are skipped rather than reported:
// the transcript is written by another process and may be mid-write, and this
// is a telemetry read, never a correctness one.
func scanLineInvocations(line []byte, visit func(agent.ToolInvocation) bool) bool {
	var parsed transcript.Line
	if err := json.Unmarshal(line, &parsed); err != nil || len(parsed.Message) == 0 {
		return false
	}
	// Only assistant envelopes carry live tool calls — the same gate every
	// other tool_use consumer in this package applies (ExtractModifiedFiles,
	// ExtractSkillEvents, ExtractAllModifiedFiles). Without it, a non-assistant
	// envelope whose message happens to decode into tool_use-shaped content
	// (replayed/sidechain or summary lines) would count as a real invocation.
	if parsed.Type != envelopeTypeAssistant {
		return false
	}
	var msg assistantMessage
	if err := json.Unmarshal(parsed.Message, &msg); err != nil {
		return false
	}
	for _, block := range msg.Content {
		if block.Type != transcript.ContentTypeToolUse || len(block.Input) == 0 {
			continue
		}
		var input toolInput
		if err := json.Unmarshal(block.Input, &input); err != nil {
			continue
		}
		if visit(agent.ToolInvocation{
			Tool:         block.Name,
			Command:      input.Command,
			SubagentType: input.SubagentType,
		}) {
			return true
		}
	}
	return false
}
