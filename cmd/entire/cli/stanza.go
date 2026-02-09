package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// FindStanza locates a named stanza in content.
// Returns version, body, found. If not found, version=-1, body="", found=false.
func FindStanza(content, name string) (version int, body string, found bool) {
	beginPrefix := "# BEGIN " + name + " (v"
	endLine := "# END " + name

	lines := strings.Split(content, "\n")
	inStanza := false
	stanzaVersion := -1
	var bodyLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inStanza {
			if strings.HasPrefix(trimmed, beginPrefix) && strings.HasSuffix(trimmed, ")") {
				// Extract version number
				vStr := trimmed[len(beginPrefix) : len(trimmed)-1]
				v, err := strconv.Atoi(vStr)
				if err != nil {
					continue
				}
				inStanza = true
				stanzaVersion = v
				bodyLines = nil
				continue
			}
		} else {
			if trimmed == endLine {
				return stanzaVersion, strings.Join(bodyLines, "\n"), true
			}
			bodyLines = append(bodyLines, line)
		}
	}

	return -1, "", false
}

// UpsertStanza inserts or replaces a named stanza. Returns new file content.
func UpsertStanza(content string, name string, version int, body string) string {
	beginPrefix := "# BEGIN " + name + " (v"
	endLine := "# END " + name
	beginLine := fmt.Sprintf("# BEGIN %s (v%d)", name, version)

	stanzaBlock := beginLine + "\n" + body + "\n" + endLine

	lines := strings.Split(content, "\n")
	var result []string
	inStanza := false
	replaced := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inStanza {
			if strings.HasPrefix(trimmed, beginPrefix) && strings.HasSuffix(trimmed, ")") {
				// Validate it has a version number
				vStr := trimmed[len(beginPrefix) : len(trimmed)-1]
				if _, err := strconv.Atoi(vStr); err != nil {
					result = append(result, line)
					continue
				}
				inStanza = true
				// Add the replacement stanza
				result = append(result, stanzaBlock)
				replaced = true
				continue
			}
			result = append(result, line)
		} else if trimmed == endLine {
			inStanza = false
			// Skip old stanza lines (already replaced)
		}
	}

	if !replaced {
		// Append to end, ensuring a blank line separator
		trimmedContent := strings.TrimRight(content, "\n")
		if trimmedContent == "" {
			return stanzaBlock + "\n"
		}
		return trimmedContent + "\n\n" + stanzaBlock + "\n"
	}

	return strings.Join(result, "\n")
}

// RemoveStanza removes a named stanza. Returns content unchanged if not found.
func RemoveStanza(content, name string) string {
	beginPrefix := "# BEGIN " + name + " (v"
	endLine := "# END " + name

	lines := strings.Split(content, "\n")
	var result []string
	inStanza := false
	found := false
	// Track index where stanza starts to remove preceding blank line
	stanzaStartIdx := -1

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inStanza {
			if strings.HasPrefix(trimmed, beginPrefix) && strings.HasSuffix(trimmed, ")") {
				// Validate it has a version number
				vStr := trimmed[len(beginPrefix) : len(trimmed)-1]
				if _, err := strconv.Atoi(vStr); err != nil {
					result = append(result, line)
					continue
				}
				inStanza = true
				found = true
				stanzaStartIdx = len(result)
				continue
			}
			result = append(result, line)
		} else if trimmed == endLine {
			inStanza = false
		}
		// Skip stanza body lines when inStanza
	}

	if !found {
		return content
	}

	// Remove blank line before the stanza if it exists
	if stanzaStartIdx > 0 && strings.TrimSpace(result[stanzaStartIdx-1]) == "" {
		result = append(result[:stanzaStartIdx-1], result[stanzaStartIdx:]...)
	}

	// Clean up trailing empty lines — keep at most one trailing newline
	output := strings.Join(result, "\n")
	output = strings.TrimRight(output, "\n")
	if output != "" {
		output += "\n"
	}

	return output
}
