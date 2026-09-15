package transcript

import (
	"bytes"
	"encoding/json"
)

// toolUseToken is the literal that must appear in a raw transcript line for it
// to carry a tool_use content block. Both quotes are part of the token on
// purpose: it makes the check exact without whitespace normalization, and it is
// what keeps `"tool_use_id"` — which appears on every tool_result line, the
// single largest false-positive class for any substring probe — from matching.
//
//nolint:gochecknoglobals // immutable literal, read-only.
var toolUseToken = []byte(`"tool_use"`)

// ScanToolUseBlocks walks the tool_use content blocks recorded in a JSONL
// transcript of the Claude Code shape — which Cursor shares, and which is why
// this package exists — offering each to visit and returning true as soon as
// visit does.
//
// Callers decode block.Input themselves. The envelope and the content block are
// genuinely common across these agents; the input KEYS are not. Claude Code
// keys a shell command `command` and a target file `file_path`, Cursor keys the
// file `path`, and a walker that decoded either one for both would quietly
// return zero values for the other. Handing back the raw input keeps this
// function honest about the part that is actually shared.
//
// hints is a PERFORMANCE filter and not a semantic one: an implementation may
// skip any line whose raw bytes contain none of the hints, so a caller must
// pass hints that every block it can possibly match is guaranteed to contain
// literally, or nil to visit everything. See agent.ToolInvocationScanner for
// the full contract, which this function is the shared engine for.
//
// Lines are split by hand with bytes.IndexByte rather than with bufio.Scanner:
// a single transcript line routinely carries a multi-megabyte tool result, so
// Scanner's 64KiB default token limit would silently truncate the walk. Two
// byte-level prefilters run before any JSON work, so a transcript with no
// candidate line costs one pass and zero allocations — the order that matters,
// since the miss path dominates.
func ScanToolUseBlocks(data []byte, hints [][]byte, visit func(ContentBlock) bool) bool {
	for rest := data; len(rest) > 0; {
		line := rest
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			line, rest = rest[:idx], rest[idx+1:]
		} else {
			rest = nil
		}
		if !lineMayCarryToolUse(line, hints) {
			continue
		}
		if scanLineToolUseBlocks(line, visit) {
			return true
		}
	}
	return false
}

// lineMayCarryToolUse applies the two prefilters: the caller's hints (nil means
// visit every line) and the tool_use token. Hints are checked first because
// they are the selective half — a transcript typically has thousands of
// tool_use lines and a handful mentioning whatever the caller is looking for.
func lineMayCarryToolUse(line []byte, hints [][]byte) bool {
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

// scanLineToolUseBlocks decodes one candidate line and offers each of its
// tool_use blocks to visit. Malformed lines are skipped rather than reported:
// the transcript is written by another process and may be mid-write, and every
// caller here is a telemetry read, never a correctness one.
func scanLineToolUseBlocks(line []byte, visit func(ContentBlock) bool) bool {
	var parsed Line
	if err := json.Unmarshal(line, &parsed); err != nil || len(parsed.Message) == 0 {
		return false
	}
	// Cursor keys the envelope "role" where Claude Code keys it "type"; every
	// other consumer in this package reaches this line through ParseFromBytes,
	// which normalizes the same way.
	normalizeLineType(&parsed)
	// Only assistant envelopes carry live tool calls — the same gate every
	// other tool_use consumer applies (claudecode.ExtractModifiedFiles,
	// ExtractSkillEvents, cursor.ExtractModifiedFiles). Without it, a
	// non-assistant envelope whose message happens to decode into
	// tool_use-shaped content (replayed/sidechain or summary lines) would count
	// as a real invocation.
	if parsed.Type != TypeAssistant {
		return false
	}
	var msg AssistantMessage
	if err := json.Unmarshal(parsed.Message, &msg); err != nil {
		return false
	}
	for _, block := range msg.Content {
		if block.Type != ContentTypeToolUse || len(block.Input) == 0 {
			continue
		}
		if visit(block) {
			return true
		}
	}
	return false
}
