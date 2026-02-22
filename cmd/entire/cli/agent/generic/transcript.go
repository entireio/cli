package generic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// extractModifiedFilesFromJSONL attempts to extract modified file paths from JSONL transcript bytes.
// It gracefully degrades for unknown formats — if no file paths can be extracted, returns nil.
//
// Supports common patterns across agent transcript formats:
//   - Tool calls with file_path/path/file/filename fields (OpenCode, Claude Code style)
//   - Content blocks with tool_use containing file paths
func extractModifiedFilesFromJSONL(data []byte) []string {
	if len(data) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var files []string

	reader := bufio.NewReader(bytes.NewReader(data))
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			break
		}

		if len(bytes.TrimSpace(lineBytes)) > 0 {
			for _, f := range extractFilesFromLine(lineBytes) {
				if !seen[f] {
					seen[f] = true
					files = append(files, f)
				}
			}
		}

		if err == io.EOF {
			break
		}
	}

	return files
}

// extractFilesFromLine attempts to extract file paths from a single JSONL line.
// It walks the JSON structure looking for tool-related objects with file path fields.
func extractFilesFromLine(line []byte) []string {
	var obj map[string]interface{}
	if err := json.Unmarshal(line, &obj); err != nil {
		return nil
	}

	var files []string
	walkForFiles(obj, &files)
	return files
}

// filePathKeys are common field names for file paths in tool call inputs.
var filePathKeys = []string{"file_path", "path", "file", "filename"}

// walkForFiles recursively walks a JSON object looking for file path fields
// in objects that appear to be tool calls or tool inputs.
func walkForFiles(obj map[string]interface{}, files *[]string) {
	// Check if this object has a tool-related context
	if hasToolContext(obj) {
		for _, key := range filePathKeys {
			if v, ok := obj[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					*files = append(*files, s)
				}
			}
		}
	}

	// Check nested "input" field (common in tool call structures)
	if input, ok := obj["input"].(map[string]interface{}); ok {
		for _, key := range filePathKeys {
			if v, ok := input[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					*files = append(*files, s)
				}
			}
		}
	}

	// Check nested "state" -> "input" (OpenCode style)
	if state, ok := obj["state"].(map[string]interface{}); ok {
		if input, ok := state["input"].(map[string]interface{}); ok {
			for _, key := range filePathKeys {
				if v, ok := input[key]; ok {
					if s, ok := v.(string); ok && s != "" {
						*files = append(*files, s)
					}
				}
			}
		}
	}

	// Recurse into arrays (e.g., "parts", "content")
	for _, v := range obj {
		switch val := v.(type) {
		case []interface{}:
			for _, item := range val {
				if m, ok := item.(map[string]interface{}); ok {
					walkForFiles(m, files)
				}
			}
		case map[string]interface{}:
			walkForFiles(val, files)
		}
	}
}

// hasToolContext returns true if the object looks like it's part of a tool call.
func hasToolContext(obj map[string]interface{}) bool {
	// Check for common tool-related type fields
	if t, ok := obj["type"].(string); ok {
		switch t {
		case "tool", "tool_use", "tool_result":
			return true
		}
	}
	// Check for tool name field
	if _, ok := obj["tool"].(string); ok {
		return true
	}
	if _, ok := obj["tool_name"].(string); ok {
		return true
	}
	return false
}
