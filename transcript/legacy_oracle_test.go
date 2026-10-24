package transcript

// This file preserves the two-pass JSONL compactor that compactJSONLWith used
// before it became "records adapter, then fold". It is the byte-for-byte
// oracle for the equivalence tests in jsonl_records_test.go. Do not edit it to match
// new behavior; the intentional divergences are marked below.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func legacyCompactJSONLWith(content []byte, opts Options, preprocess linePreprocessor) ([]byte, error) {
	base := newTranscriptLine(opts)

	// Pass 1: parse all lines into intermediate entries.
	entries, err := legacyParseJSONLEntries(content, preprocess)
	if err != nil {
		return nil, err
	}

	// Pass 2: merge, then attach tool results, then emit.
	//
	// Intentional divergence: the original inlined only the results in the
	// user entry immediately after an assistant. That dropped the results of
	// parallel tool calls, which Claude Code writes one user entry each, so
	// results now attach by tool_use_id to the last assistant holding that
	// tool_use, wherever they fall; a result without one attaches to the
	// last assistant until the next user entry with text or images. Output
	// is collected first because a result can land after a later assistant.
	type legacyOut struct {
		entry     parsedEntry
		assistant bool
	}
	var outs []legacyOut
	owners := make(map[string]int) // tool_use ID → index in outs of the last assistant holding it
	last := -1                     // index of the last assistant in outs, since the last user line
	for i := 0; i < len(entries); i++ {
		e := entries[i]

		switch e.kind {
		case TypeAssistant:
			// Merge consecutive assistant entries with the same message ID.
			merged := e
			for i+1 < len(entries) && entries[i+1].kind == TypeAssistant && entries[i+1].id == e.id {
				i++
				merged = mergeAssistantEntries(merged, entries[i])
			}
			outs = append(outs, legacyOut{entry: merged, assistant: true})
			last = len(outs) - 1
			for _, id := range legacyToolUseIDs(merged.content) {
				owners[id] = last
			}

		case TypeUser:
			for _, tr := range e.toolResults {
				idx := last
				if tr.toolUseID != "" {
					var ok bool
					if idx, ok = owners[tr.toolUseID]; !ok {
						continue
					}
				}
				if idx < 0 {
					continue
				}
				outs[idx].entry = legacyInlineToolResults(outs[idx].entry, parsedEntry{toolResults: []toolResultEntry{tr}})
			}
			// A user entry that is purely tool results has no line.
			if legacyHasToolResults(e) && e.userText == "" && len(e.userImages) == 0 {
				continue
			}
			outs = append(outs, legacyOut{entry: e})
			last = -1
		}
	}

	var result []byte
	for _, out := range outs {
		switch {
		case !out.assistant:
			legacyEmitUser(&result, base, out.entry)
		// Intentional divergence: the original emitted an assistant whose
		// content was `[]` when a user entry with text followed its results.
		// The fold skips empty assistant messages unconditionally.
		case !legacyIsEmptyContentArray(out.entry.content):
			legacyEmitAssistant(&result, base, out.entry)
		}
	}

	return result, nil
}

// legacyToolUseIDs returns the IDs of the tool_use blocks in content.
func legacyToolUseIDs(content json.RawMessage) []string {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var ids []string
	for _, b := range blocks {
		if unquote(b["type"]) == ContentTypeToolUse && unquote(b["id"]) != "" {
			ids = append(ids, unquote(b["id"]))
		}
	}
	return ids
}

func legacyEmitAssistant(result *[]byte, base transcriptLine, e parsedEntry) {
	line := base
	line.Type = TypeAssistant
	line.TS = e.ts
	line.ID = e.id
	line.InputTokens = int64(e.inputTokens)
	line.OutputTokens = int64(e.outputTokens)
	line.Content = e.content
	appendLine(result, line)
}

func legacyEmitUser(result *[]byte, base transcriptLine, e parsedEntry) {
	var blocks []json.RawMessage

	// Text block (with optional prompt ID).
	if e.userText != "" || len(e.userImages) == 0 {
		b, err := json.Marshal(userTextBlock{ID: e.userID, Text: e.userText})
		if err != nil {
			return
		}
		blocks = append(blocks, b)
	}

	// Image blocks passed through verbatim.
	blocks = append(blocks, e.userImages...)

	contentJSON, err := json.Marshal(blocks)
	if err != nil {
		return
	}

	line := base
	line.Type = TypeUser
	line.TS = e.ts
	line.Content = contentJSON
	appendLine(result, line)
}

// legacyParseJSONLEntries parses all JSONL lines into intermediate entries,
// filtering dropped types and malformed lines.
func legacyParseJSONLEntries(content []byte, preprocess linePreprocessor) ([]parsedEntry, error) {
	reader := bufio.NewReader(bytes.NewReader(content))
	var entries []parsedEntry

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading JSONL line: %w", err)
		}

		if len(bytes.TrimSpace(lineBytes)) > 0 {
			if e, ok := parseLine(lineBytes, preprocess); ok {
				entries = append(entries, e)
			}
		}

		if err == io.EOF {
			break
		}
	}

	return entries, nil
}
func legacyInlineToolResults(assistant, user parsedEntry) parsedEntry {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(assistant.content, &blocks) != nil || len(blocks) == 0 {
		return assistant
	}

	for _, tr := range user.toolResults {
		// Find the tool_use block matching this tool_use_id.
		idx := -1
		for i := len(blocks) - 1; i >= 0; i-- {
			if unquote(blocks[i]["type"]) == ContentTypeToolUse {
				if tr.toolUseID == "" || unquote(blocks[i]["id"]) == tr.toolUseID {
					idx = i
					break
				}
			}
		}
		// No matching tool_use block: do not attach a result to unrelated content.
		if idx == -1 {
			continue
		}

		blocks[idx]["result"] = buildToolResult(tr)
	}

	if data, err := json.Marshal(blocks); err == nil {
		assistant.content = data
	}

	return assistant
}
func legacyIsEmptyContentArray(raw json.RawMessage) bool {
	var arr []json.RawMessage
	return json.Unmarshal(raw, &arr) == nil && len(arr) == 0
}

func legacyHasToolResults(e parsedEntry) bool {
	return len(e.toolResults) > 0
}
