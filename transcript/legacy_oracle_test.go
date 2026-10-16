package transcript

// This file preserves the two-pass JSONL compactor that compactJSONLWith used
// before it became "records adapter, then fold". It is the byte-for-byte
// oracle for the equivalence tests in jsonl_records_test.go. Do not edit it to match
// new behavior; the one intentional divergence is marked below.

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

	// Pass 2: merge and emit.
	var result []byte
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

			// Look ahead for user tool_result entries to inline.
			if i+1 < len(entries) && entries[i+1].kind == TypeUser && legacyHasToolResults(entries[i+1]) {
				userEntry := entries[i+1]
				merged = legacyInlineToolResults(merged, userEntry)
				i++ // consume the user tool_result entry

				// If the consumed user entry also had text or image content, emit it
				// as a separate user line after the assistant.
				if userEntry.userText != "" || len(userEntry.userImages) > 0 {
					// Intentional divergence: the original emitted this assistant
					// even when its content was `[]`. The fold skips empty
					// assistant messages unconditionally, so the oracle does too.
					if !legacyIsEmptyContentArray(merged.content) {
						legacyEmitAssistant(&result, base, merged)
					}
					legacyEmitUser(&result, base, userEntry)
					continue
				}
			}

			if legacyIsEmptyContentArray(merged.content) {
				continue
			}

			legacyEmitAssistant(&result, base, merged)

		case TypeUser:
			// User entries that are purely tool results were already consumed
			// by the assistant look-ahead above. If we reach one here it was
			// not preceded by an assistant with a matching tool_use, so emit
			// it only if it has text or image content.
			if legacyHasToolResults(e) && e.userText == "" && len(e.userImages) == 0 {
				continue
			}
			legacyEmitUser(&result, base, e)
		}
	}

	return result, nil
}

func legacyEmitAssistant(result *[]byte, base transcriptLine, e parsedEntry) {
	line := base
	line.Type = TypeAssistant
	line.TS = e.ts
	line.ID = e.id
	line.InputTokens = e.inputTokens
	line.OutputTokens = e.outputTokens
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
