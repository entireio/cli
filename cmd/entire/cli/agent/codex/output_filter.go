package codex

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// csiRegex matches ANSI/CSI escape sequences including extended parameter forms.
// Matches: ESC [ [?;0-9]* [a-zA-Z]
// Examples: \x1b[?25l (cursor hide), \x1b[?25h (cursor show), \x1b[2K (erase line)
var csiRegex = regexp.MustCompile(`\x1b\[[?;0-9]*[a-zA-Z]`)

// execBlockRegex matches codex exec-block lines in two forms:
//   - Bare "exec" (exactly, possibly with trailing whitespace)
//   - "exec <cmd> in /<path>" (exec block header reporting cwd)
//
// Anchored with ^ and $ to avoid matching narrative text that contains "exec"
// as a substring (e.g. "execute the following", "exec succeeded").
var execBlockRegex = regexp.MustCompile(`^exec(?:\s+\S.*\s+in\s+/\S*)?$`)

// hookFiringRegex matches codex hook-firing notice lines.
// Example: "[hooks] firing user-prompt-submit for session abc123"
var hookFiringRegex = regexp.MustCompile(`^\[hooks\]`)

// timestampLogRegex matches ISO-8601 timestamped log lines emitted by codex.
// Requires a log-level word (ERROR, WARN, INFO, DEBUG) after the timestamp to
// avoid false-positives on benign narrative like
// "2026-01-13T10:00:00 was the deploy time, see changelog".
// Example: "2026-04-30T10:00:00.000Z ERROR: something failed"
var timestampLogRegex = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z?\s+(ERROR|WARN|INFO|DEBUG)\b`)

// bannerRegex matches codex banner/header lines. These appear at the start of
// a codex exec session and include:
//   - Visual separator lines: "─────── codex ───────", "─────────────────────"
//   - Version header lines:   "version 0.1.0 (linux)"
//
// The version alternative is anchored at both ends and allows an optional patch
// segment and a parenthesized platform suffix so that narrative text like
// "version 1.2.3 of go-git fixes the issue" is NOT dropped.
var bannerRegex = regexp.MustCompile(`^[─\-\s]+(?:codex|version)[─\-\s]*$|^[─\-]+$|^version\s+\d+\.\d+(?:\.\d+)?\s*(?:\([^)]+\))?\s*$`)

// FilterLine applies all codex-specific chrome filters to a single line.
// It returns the cleaned line and true if the line should be emitted, or
// ("", false) if the line is chrome and must be dropped.
//
// Filtering order:
//  1. Strip CSI/ANSI escape sequences.
//  2. Drop blank lines after stripping.
//  3. Drop banner/header lines.
//  4. Drop exec-block lines (anchored to avoid false positives).
//  5. Drop hook-firing notice lines.
//  6. Drop timestamped error/log lines.
func FilterLine(line string) (string, bool) {
	// 1. Strip CSI sequences.
	cleaned := csiRegex.ReplaceAllString(line, "")

	// 2. Drop blank after stripping. Use TrimSpace ONLY for the empty-check;
	// the returned line preserves leading whitespace so markdown/code-block
	// indentation in narrative output is not flattened.
	if strings.TrimSpace(cleaned) == "" {
		return "", false
	}

	// Use the trim-right form for chrome-pattern matching (so trailing
	// whitespace doesn't break anchored regexes) while keeping the leading
	// whitespace on the returned value.
	trimmedRight := strings.TrimRight(cleaned, " \t")

	// 3. Drop banner/separator lines.
	if bannerRegex.MatchString(trimmedRight) {
		return "", false
	}

	// 4. Drop exec-block lines.
	if execBlockRegex.MatchString(trimmedRight) {
		return "", false
	}

	// 5. Drop hook-firing notices.
	if hookFiringRegex.MatchString(trimmedRight) {
		return "", false
	}

	// 6. Drop timestamped log lines.
	if timestampLogRegex.MatchString(trimmedRight) {
		return "", false
	}

	return trimmedRight, true
}

// Strip wraps r in a filtered reader that removes codex chrome line-by-line.
// The returned reader emits only lines that pass FilterLine, each terminated
// by a newline.
func Strip(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		state := codexStripNormal
		for scanner.Scan() {
			done, err := collectFinalCodexLine(scanner.Text(), &state, pw)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if done {
				state = codexStripAfterTokens
			}
		}
		if err := scanner.Err(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	return pr
}

type codexStripState int

const (
	codexStripNormal codexStripState = iota
	codexStripUserBlock
	codexStripAssistantBlock
	codexStripExecBlock
	codexStripAfterTokens
)

func collectFinalCodexLine(raw string, state *codexStripState, w io.Writer) (bool, error) {
	cleaned := csiRegex.ReplaceAllString(raw, "")
	trimmed := strings.TrimSpace(cleaned)
	trimmedRight := strings.TrimRight(cleaned, " \t")

	if *state == codexStripAfterTokens {
		return true, nil
	}
	if isTokensUsedMarker(trimmed) {
		return true, nil
	}

	switch *state {
	case codexStripUserBlock:
		if isCodexRoleMarker(trimmed) {
			*state = codexStripAssistantBlock
		}
		return false, nil
	case codexStripAssistantBlock:
		if isCodexRoleMarker(trimmed) {
			return false, nil
		}
		if isUserRoleMarker(trimmed) {
			*state = codexStripUserBlock
			return false, nil
		}
		if trimmedRight == codexExecCommand || execBlockRegex.MatchString(trimmedRight) {
			*state = codexStripExecBlock
			return false, nil
		}
		if isCodexMetadataLine(trimmed) {
			return false, nil
		}
		if line, ok := FilterLine(raw); ok {
			if err := writeCodexLine(w, line); err != nil {
				return false, err
			}
		}
		return false, nil
	case codexStripExecBlock:
		if trimmed == "" {
			*state = codexStripNormal
			return false, nil
		}
		if isCodexRoleMarker(trimmed) {
			*state = codexStripAssistantBlock
			return false, nil
		}
		return false, nil
	case codexStripNormal:
		// Continue below.
	case codexStripAfterTokens:
		return true, nil
	}

	if isUserRoleMarker(trimmed) {
		*state = codexStripUserBlock
		return false, nil
	}
	if isCodexRoleMarker(trimmed) {
		*state = codexStripAssistantBlock
		return false, nil
	}
	if isCodexMetadataLine(trimmed) {
		return false, nil
	}
	if trimmedRight == codexExecCommand {
		*state = codexStripExecBlock
		return false, nil
	}
	if execBlockRegex.MatchString(trimmedRight) {
		return false, nil
	}

	if line, ok := FilterLine(raw); ok {
		if err := writeCodexLine(w, line); err != nil {
			return false, err
		}
	}
	return false, nil
}

func writeCodexLine(w io.Writer, line string) error {
	if _, err := w.Write([]byte(line + "\n")); err != nil {
		return fmt.Errorf("write filtered codex output: %w", err)
	}
	return nil
}

func isTokensUsedMarker(trimmed string) bool {
	return strings.EqualFold(trimmed, "tokens used")
}

func isUserRoleMarker(trimmed string) bool {
	return trimmed == "user"
}

func isCodexRoleMarker(trimmed string) bool {
	return trimmed == "codex"
}

func isCodexMetadataLine(trimmed string) bool {
	if strings.HasPrefix(trimmed, "OpenAI Codex v") {
		return true
	}
	for _, prefix := range []string{
		"workdir:",
		"model:",
		"provider:",
		"approval:",
		"sandbox:",
		"reasoning:",
		"session id:",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}
