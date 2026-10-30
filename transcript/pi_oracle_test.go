package transcript

// This file preserves the Pi compactor that Convert used before it became
// "PiAdapter, then fold". It is the byte-for-byte oracle for the equivalence
// tests in pi_records_test.go. Do not edit it to match new behavior. It fails
// on a line of pijsonl.MaxScannerLine or more, which the adapter reads whole.

import (
	"encoding/json"
	"fmt"

	"github.com/entireio/cli/internal/transcript/pijsonl"
)

func legacyCompactPi(content []byte, opts Options) ([]byte, error) {
	active := pijsonl.ResolveActiveBranch(content)
	results, err := legacyPiCollectToolResults(content, active)
	if err != nil {
		return nil, err
	}

	emit := content
	if opts.StartLine > 0 {
		emit = pijsonl.SkipLines(content, opts.StartLine)
		if emit == nil {
			return []byte{}, nil
		}
	}

	base := newTranscriptLine(opts)
	var out []byte

	scanner := pijsonl.NewScanner(emit)
	for scanner.Scan() {
		var entry pijsonl.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != pijsonl.EntryTypeMessage {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}

		switch entry.Message.Role {
		case pijsonl.RoleUser:
			blocks := piEmitUserContent(entry.Message.Content)
			if len(blocks) == 0 {
				continue
			}
			contentJSON, err := json.Marshal(blocks)
			if err != nil {
				return nil, fmt.Errorf("marshal pi user content: %w", err)
			}
			line := base
			line.Type = pijsonl.RoleUser
			line.TS = piTimestampJSON(entry.Timestamp)
			line.Content = contentJSON
			appendLine(&out, line)

		case pijsonl.RoleAssistant:
			blocks := piEmitAssistantContent(entry.Message.Content, results)
			usage := piCallUsage(entry.Message)
			// Intentional divergence: the original wrote a call with usage
			// but no visible content as "content":[]. The fold hides an
			// assistant message with content [] or null, so the adapter
			// sends none, which the fold prints as "content":null.
			var blocksOrNil any = blocks
			if len(blocks) == 0 {
				if !piUsageHasCounts(usage) {
					continue
				}
				blocksOrNil = nil
			}
			contentJSON, err := json.Marshal(blocksOrNil)
			if err != nil {
				return nil, fmt.Errorf("marshal pi assistant content: %w", err)
			}
			line := base
			line.Type = pijsonl.RoleAssistant
			line.TS = piTimestampJSON(entry.Timestamp)
			line.ID = entry.ID
			line.Content = contentJSON
			if usage != nil {
				applyCallUsage(&line, usage)
			}
			appendLine(&out, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan pi transcript: %w", err)
	}
	return out, nil
}

func legacyPiCollectToolResults(data []byte, active map[string]bool) (map[string]piCompactToolResult, error) {
	results := map[string]piCompactToolResult{}
	scanner := pijsonl.NewScanner(data)
	for scanner.Scan() {
		var entry pijsonl.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != pijsonl.EntryTypeMessage || entry.Message.Role != pijsonl.RoleToolResult {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}
		if entry.Message.ToolCallID == "" {
			continue
		}
		results[entry.Message.ToolCallID] = piCompactToolResult{
			Output: piDecodeResultOutput(entry.Message.Content),
			Status: piResultStatus(entry.Message.IsError),
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan pi tool results: %w", err)
	}
	return results, nil
}
