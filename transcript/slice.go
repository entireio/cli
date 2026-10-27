package transcript

// SliceFromLine returns the content starting from line number `startLine` (0-indexed).
// This is used to extract only the checkpoint-specific portion of a cumulative transcript.
// For example, if startLine is 2, lines 0 and 1 are skipped and the result starts at line 2.
// Returns empty slice if startLine exceeds the number of lines.
func SliceFromLine(content []byte, startLine int) []byte {
	if len(content) == 0 || startLine <= 0 {
		return content
	}

	// Find the byte offset where startLine begins
	lineCount := 0
	offset := 0
	for i, b := range content {
		if b == '\n' {
			lineCount++
			if lineCount == startLine {
				offset = i + 1
				break
			}
		}
	}

	// If we didn't find enough lines, return empty
	if lineCount < startLine {
		return nil
	}

	// If offset is beyond content, return empty
	if offset >= len(content) {
		return nil
	}

	return content[offset:]
}
