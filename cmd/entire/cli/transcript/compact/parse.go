package compact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

type CondensedEntry struct {
	Type       string
	Content    string
	ToolName   string
	ToolDetail string
}

func parseLines(content []byte) ([]transcriptLine, error) {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return nil, nil
	}

	rawLines := strings.Split(trimmed, "\n")
	parsed := make([]transcriptLine, 0, len(rawLines))

	for _, rawLine := range rawLines {
		lineText := strings.TrimSpace(rawLine)
		if lineText == "" {
			continue
		}

		var line transcriptLine
		if err := json.Unmarshal([]byte(lineText), &line); err != nil {
			return nil, fmt.Errorf("parsing compact transcript line: %w", err)
		}
		if line.V == 0 || line.CLIVersion == "" {
			return nil, errors.New("not compact transcript format")
		}

		parsed = append(parsed, line)
	}

	return parsed, nil
}

func BuildCondensedEntries(content []byte) ([]CondensedEntry, error) {
	lines, err := parseLines(content)
	if err != nil {
		return nil, err
	}

	entries := make([]CondensedEntry, 0, len(lines))
	for _, line := range lines {
		var blocks []map[string]json.RawMessage
		if len(line.Content) > 0 {
			if err := json.Unmarshal(line.Content, &blocks); err != nil {
				continue
			}
		}

		switch line.Type {
		case "user":
			var parts []string
			for _, block := range blocks {
				var text string
				if err := json.Unmarshal(block["text"], &text); err == nil && text != "" {
					parts = append(parts, text)
				}
			}
			if len(parts) > 0 {
				entries = append(entries, CondensedEntry{Type: "user", Content: strings.Join(parts, "\n")})
			}

		case "assistant":
			for _, block := range blocks {
				var blockType string
				if err := json.Unmarshal(block["type"], &blockType); err != nil {
					continue
				}

				switch blockType {
				case "text":
					var text string
					if err := json.Unmarshal(block["text"], &text); err == nil && text != "" {
						entries = append(entries, CondensedEntry{Type: "assistant", Content: text})
					}
				case "tool_use":
					var toolName string
					if err := json.Unmarshal(block["name"], &toolName); err != nil {
						continue
					}

					entries = append(entries, CondensedEntry{
						Type:       "tool",
						ToolName:   toolName,
						ToolDetail: transcript.RawToolDetail(block["input"]),
					})
				}
			}
		}
	}

	if len(entries) == 0 {
		return nil, errors.New("no parseable compact transcript entries")
	}

	return entries, nil
}
