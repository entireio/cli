package compact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	transcriptlib "github.com/entireio/cli/transcript"
)

type CondensedEntry struct {
	Type       string
	Content    string
	ToolName   string
	ToolDetail string
}

func BuildCondensedEntries(content []byte) ([]CondensedEntry, error) {
	lines, err := transcriptlib.Decode(content)
	if err != nil {
		return nil, fmt.Errorf("parsing compact transcript: %w", err)
	}

	entries := make([]CondensedEntry, 0, len(lines))
	for _, line := range lines {
		switch line.Type {
		case transcriptlib.TypeUser:
			var parts []string
			for _, block := range line.Blocks {
				if block.Text != "" {
					parts = append(parts, block.Text)
				}
			}
			if len(parts) > 0 {
				entries = append(entries, CondensedEntry{Type: transcriptlib.TypeUser, Content: strings.Join(parts, "\n")})
			}

		case transcriptlib.TypeAssistant:
			for _, block := range line.Blocks {
				switch block.Type {
				case transcriptlib.BlockText:
					if block.Text != "" {
						entries = append(entries, CondensedEntry{Type: transcriptlib.TypeAssistant, Content: block.Text})
					}
				case transcriptlib.BlockToolUse:
					if block.Name == "" {
						continue
					}
					var input map[string]any
					if len(block.Input) > 0 {
						if err := json.Unmarshal(block.Input, &input); err != nil {
							input = nil
						}
					}
					entries = append(entries, CondensedEntry{
						Type:       "tool",
						ToolName:   block.Name,
						ToolDetail: extractToolDetail(input),
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

func extractToolDetail(input map[string]interface{}) string {
	for _, key := range []string{"description", "command", "file_path", "filePath", "path", "pattern"} {
		if v, ok := input[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
