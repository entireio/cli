package codexitem

import (
	"path/filepath"
	"strconv"
	"strings"
)

// scriptOfArgv and scriptOfString give the script of a command run as
// `<shell> -lc <script>` or `<shell> -c <script>`, from the rollout's argv or
// the daemon's string (which quotes its words as a shell does). Anything else,
// including a -c of a program that isn't a shell (grep -c), is its words
// joined with spaces, so both encodings read the same. A daemon string that
// shellWords can't split is returned whole.
func scriptOfArgv(argv []string) string {
	if len(argv) == 3 && isShell(argv[0]) && (argv[1] == "-lc" || argv[1] == "-c") {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

func scriptOfString(cmd string) string {
	words, ok := shellWords(cmd)
	if !ok {
		return cmd
	}
	return scriptOfArgv(words)
}

// isShell reports whether program is a shell whose -c takes a script.
func isShell(program string) bool {
	switch filepath.Base(program) {
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
		return true
	}
	return false
}

// shellWords splits s into words as a POSIX shell would, for the quoting the
// daemon uses: single quotes (literal), double quotes (with \" \\ \$ \` and
// \newline escapes), ANSI-C quotes ($'...', see ansiCQuoted) and
// backslashes outside quotes. ok is false for an unterminated quote and for an
// ANSI-C escape it doesn't read.
func shellWords(s string) (words []string, ok bool) {
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			inWord = true
			end, ok := ansiCQuoted(&cur, s, i+2)
			if !ok {
				return nil, false
			}
			i = end
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
					if s[i] == '\n' {
						continue
					}
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, false
			}
		case c == '\\' && i+1 < len(s):
			inWord = true
			i++
			cur.WriteByte(s[i])
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

// ansiCQuoted decodes the body of a $'...' word starting at s[i] (just after
// the opening quote) into cur, and returns the index of the closing quote.
// It reads the escapes Codex can write: \n \t \r \\ \' \" \a \b \e \f \v,
// \xHH and \NNN (octal up to \377). As in bash, a NUL (\0, \x00) ends the
// word's text: the rest up to the closing quote is dropped. ok is false for
// any other escape (\u, \c, ...) and for an unterminated word, so the caller
// keeps the command whole.
func ansiCQuoted(cur *strings.Builder, s string, i int) (end int, ok bool) {
	nul := false // after \0 (or another escape giving NUL), bash drops the rest
	for ; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			return i, true
		}
		if nul {
			if c == '\\' && i+1 < len(s) {
				i++ // an escaped quote does not end the word
			}
			continue
		}
		if c != '\\' {
			cur.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return 0, false
		}
		switch e := s[i]; e {
		case 'n':
			cur.WriteByte('\n')
		case 't':
			cur.WriteByte('\t')
		case 'r':
			cur.WriteByte('\r')
		case 'a':
			cur.WriteByte('\a')
		case 'b':
			cur.WriteByte('\b')
		case 'e':
			cur.WriteByte(0x1b)
		case 'f':
			cur.WriteByte('\f')
		case 'v':
			cur.WriteByte('\v')
		case '\\', '\'', '"':
			cur.WriteByte(e)
		case 'x':
			used := digitRun(s[i+1:], "0123456789abcdefABCDEF", 2)
			v, err := strconv.ParseUint(s[i+1:i+1+used], 16, 8)
			if err != nil {
				return 0, false
			}
			if v == 0 {
				nul = true
			} else {
				cur.WriteByte(byte(v))
			}
			i += used
		default:
			used := digitRun(s[i:], "01234567", 3)
			v, err := strconv.ParseUint(s[i:i+used], 8, 8)
			if err != nil { // not an escape it reads, or above \377
				return 0, false
			}
			if v == 0 {
				nul = true
			} else {
				cur.WriteByte(byte(v))
			}
			i += used - 1
		}
	}
	return 0, false
}

// digitRun is the length of the run of digits (bytes of set) that s starts
// with, at most maxDigits.
func digitRun(s, set string, maxDigits int) int {
	n := 0
	for n < maxDigits && n < len(s) && strings.IndexByte(set, s[n]) >= 0 {
		n++
	}
	return n
}
