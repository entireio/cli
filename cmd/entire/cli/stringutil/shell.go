package stringutil

import "strings"

// SanitizeShellCommand prepares a raw shell command for structural matching by
// blanking the spans whose content must not be read as shell structure:
// single-quoted strings, double-quoted strings, and heredoc bodies. Each
// blanked span becomes a single NUL byte. NUL is not a shell separator and
// appears in no command word, so a replacement can neither fabricate a command
// boundary nor join two fragments into a phrase that was never contiguous in
// the input. Every non-NUL byte of the output is copied verbatim, in order,
// from the input: any word found in the sanitized text exists literally in the
// raw command, which is what lets callers treat the output as a subsequence
// of the input.
//
// This is a matcher aid, not a shell parser. It understands just enough —
// backslash escapes outside quotes, `\"` inside double quotes, `<<`/`<<-`
// heredocs with optionally quoted delimiters (which must start with a letter
// or underscore, so `$((1<<2))` is not misread as one) — to keep quoted
// arguments and heredoc data from being read as commands. Where it errs, it
// errs toward blanking too much. Heredoc openers are copied through
// unblanked; only the body lines that follow are replaced.
//
// Callers: the strategy package's `entire search` command matcher, and
// transcript.ToolDetail's shell-command reduction.
func SanitizeShellCommand(cmd string) string {
	var b strings.Builder
	b.Grow(len(cmd))
	var pendingHeredocs []string
	for i := 0; i < len(cmd); {
		switch c := cmd[i]; c {
		case '\\':
			// An escaped character is literal text; copy the pair so the
			// escaped byte can't be read as a quote or separator opener.
			b.WriteByte(c)
			if i+1 < len(cmd) {
				b.WriteByte(cmd[i+1])
				i += 2
			} else {
				i++
			}
		case '\'':
			if end := strings.IndexByte(cmd[i+1:], '\''); end >= 0 {
				i += end + 2
			} else {
				i = len(cmd) // unterminated: blank to end
			}
			b.WriteByte(0)
		case '"':
			j := i + 1
			for j < len(cmd) && cmd[j] != '"' {
				if cmd[j] == '\\' {
					j++ // skip the escaped byte too
				}
				j++
			}
			if j < len(cmd) {
				i = j + 1
			} else {
				i = len(cmd) // unterminated: blank to end
			}
			b.WriteByte(0)
		case '<':
			if delim, next, ok := parseHeredocStart(cmd, i); ok {
				pendingHeredocs = append(pendingHeredocs, delim)
				b.WriteString(cmd[i:next])
				i = next
			} else {
				b.WriteByte(c)
				i++
			}
		case '\n':
			b.WriteByte(c)
			i++
			// The lines that follow are heredoc bodies, not commands. Blank
			// each pending body through its delimiter line, keeping a newline
			// in its place so a real command on the line AFTER the body still
			// sits on a command boundary.
			for _, delim := range pendingHeredocs {
				i = skipHeredocBody(cmd, i, delim)
				b.WriteByte(0)
				b.WriteByte('\n')
			}
			pendingHeredocs = pendingHeredocs[:0]
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// parseHeredocStart reports whether cmd[i:] opens a heredoc (`<<` or `<<-`,
// not the `<<<` here-string), returning the delimiter word and the offset just
// past it (past the closing quote when the delimiter was quoted). The
// delimiter must start with a letter or underscore so arithmetic like
// `$((1<<2))` is not misread as a heredoc.
func parseHeredocStart(cmd string, i int) (delim string, next int, ok bool) {
	if i+1 >= len(cmd) || cmd[i+1] != '<' || (i+2 < len(cmd) && cmd[i+2] == '<') {
		return "", 0, false
	}
	if i > 0 && cmd[i-1] == '<' {
		return "", 0, false // the tail of a `<<<` here-string, not an opener
	}
	j := i + 2
	if j < len(cmd) && cmd[j] == '-' {
		j++
	}
	for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
		j++
	}
	var quote byte
	if j < len(cmd) && (cmd[j] == '\'' || cmd[j] == '"') {
		quote = cmd[j]
		j++
	}
	start := j
	for j < len(cmd) && (isShellWordByte(cmd[j]) || (j > start && cmd[j] >= '0' && cmd[j] <= '9')) {
		j++
	}
	delim = cmd[start:j]
	if delim == "" || !isShellWordByte(delim[0]) {
		return "", 0, false
	}
	if quote != 0 && j < len(cmd) && cmd[j] == quote {
		j++
	}
	return delim, j, true
}

// isShellWordByte reports whether c can start a heredoc delimiter word: an
// ASCII letter or underscore.
func isShellWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// skipHeredocBody returns the offset just past the line matching delim
// (tolerating the leading tabs `<<-` allows), or len(cmd) when the body is
// unterminated.
func skipHeredocBody(cmd string, i int, delim string) int {
	for i < len(cmd) {
		line := cmd[i:]
		next := len(cmd)
		if eol := strings.IndexByte(line, '\n'); eol >= 0 {
			line = line[:eol]
			next = i + eol + 1
		}
		i = next
		if strings.TrimLeft(line, "\t") == delim {
			return i
		}
	}
	return i
}
