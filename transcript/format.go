package transcript

import "fmt"

// Native transcript formats Convert can be told to use through
// Options.Format. Each is named after the agent whose transcript it reads.
const (
	FormatClaudeCode = "claude-code"     // Claude Code JSONL, with per-call usage
	FormatCursor     = "cursor"          // Claude-style JSONL with per-message counts
	FormatCodex      = "codex"           // Codex rollout JSONL
	FormatCopilot    = "copilot-cli"     // Copilot CLI events JSONL
	FormatDroid      = "factoryai-droid" // Factory AI Droid JSONL
	FormatGemini     = "gemini"          // Gemini CLI session JSON
	FormatOpenCode   = "opencode"        // OpenCode export
	FormatPi         = "pi"              // Pi session JSONL
	// FormatOpenCodeMessages is one OpenCode message per line,
	// {"info":{...},"parts":[...]}, as OpenCodeSnapshotLines splits an
	// export. It is never detected, only declared.
	FormatOpenCodeMessages = "opencode-messages"
)

// KnownFormat reports whether Convert accepts format as Options.Format.
func KnownFormat(format string) bool {
	switch format {
	case FormatClaudeCode, FormatCursor, FormatCodex, FormatCopilot,
		FormatDroid, FormatGemini, FormatOpenCode, FormatPi, FormatOpenCodeMessages:
		return true
	}
	return false
}

// detectFormat picks the format of content when Options.Format is empty.
// Single-object formats (OpenCode, Gemini), Pi and Codex are detected on the
// whole transcript: slicing would cut a JSON object mid-value or drop the
// header line they are recognized by, and they apply StartLine themselves.
// The line formats are detected on the slice that will be converted.
func detectFormat(content []byte, opts Options) string {
	switch {
	case isOpenCodeFormat(content):
		return FormatOpenCode
	case isGeminiFormat(content):
		return FormatGemini
	case isPiFormat(content):
		return FormatPi
	case isCodexFormat(content):
		return FormatCodex
	}
	truncated := SliceFromLine(content, opts.StartLine)
	switch {
	case isCopilotFormat(truncated):
		return FormatCopilot
	case isDroidFormat(truncated):
		return FormatDroid
	case opts.Agent == agentClaudeCode:
		return FormatClaudeCode
	}
	return FormatCursor
}

// convertAs converts content with the converter for format.
func convertAs(content []byte, format string, opts Options) ([]byte, error) {
	switch format {
	case FormatOpenCode:
		return compactOpenCode(content, opts)
	case FormatGemini:
		return compactGemini(content, opts)
	case FormatPi:
		return compactPi(content, opts)
	case FormatCodex:
		return compactCodex(content, opts)
	}

	truncated := SliceFromLine(content, opts.StartLine)
	if truncated == nil {
		truncated = []byte{}
	}
	switch format {
	case FormatCopilot:
		return compactCopilot(truncated, opts)
	case FormatDroid:
		return compactDroid(truncated, opts)
	case FormatClaudeCode:
		return foldJSONL(truncated, opts, NewClaudeAdapter())
	case FormatCursor:
		return foldJSONL(truncated, opts, NewCursorAdapter())
	case FormatOpenCodeMessages:
		return foldOpenCodeMessages(truncated, opts)
	}
	return nil, fmt.Errorf("unknown transcript format %q", format)
}

// foldOpenCodeMessages converts one OpenCode message per line through the
// same adapter and fold a live OpenCode stream uses. A line is a message, so
// StartLine slices by message as the export path does.
func foldOpenCodeMessages(content []byte, opts Options) ([]byte, error) {
	a := NewOpenCodeAdapter()
	records := append(a.Feed(content), a.Flush()...)
	out, err := Fold(records, opts)
	if err != nil {
		return nil, fmt.Errorf("folding opencode messages: %w", err)
	}
	return out, nil
}
