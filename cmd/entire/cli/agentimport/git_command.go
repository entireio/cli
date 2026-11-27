package agentimport

import (
	"path"
	"strings"
)

type shellTokKind int

const (
	tokWord shellTokKind = iota
	tokSep
	tokRedir
	tokHeredoc
)

type shellTok struct {
	kind      shellTokKind
	val       string
	stripTabs bool
}

type pendingHeredoc struct {
	delim     string
	stripTabs bool
}

type shellLexer struct {
	s        string
	pos      int
	heredocs []pendingHeredoc
}

// runsCommitGitCommand reports whether command executes git with a subcommand
// that writes commits onto the current branch (commit, cherry-pick, revert,
// merge, rebase, or am). It lexes POSIX shell syntax so git mentions inside
// quoted arguments, comments, heredoc bodies, or non-command positions (such
// as `echo "git commit -q"` or `rg 'git commit -q' .`) do not match.
func runsCommitGitCommand(command string) bool {
	if !strings.Contains(command, "git") {
		return false
	}
	l := &shellLexer{s: command}
	var words []string
	expectRedirTarget := false
	var pendingHeredocStrip *bool

	for {
		tok, ok := l.nextToken()
		if !ok {
			return isCommitGitCommand(words)
		}
		switch tok.kind {
		case tokSep:
			expectRedirTarget = false
			pendingHeredocStrip = nil
			if isCommitGitCommand(words) {
				return true
			}
			words = words[:0]
		case tokRedir:
			expectRedirTarget = true
		case tokHeredoc:
			strip := tok.stripTabs
			pendingHeredocStrip = &strip
		case tokWord:
			switch {
			case pendingHeredocStrip != nil:
				l.heredocs = append(l.heredocs, pendingHeredoc{delim: tok.val, stripTabs: *pendingHeredocStrip})
				pendingHeredocStrip = nil
			case expectRedirTarget:
				expectRedirTarget = false
			default:
				words = append(words, tok.val)
			}
		}
	}
}

func (l *shellLexer) nextToken() (shellTok, bool) {
	for l.pos < len(l.s) {
		l.skipSpacesAndContinuations()
		if l.pos >= len(l.s) {
			return shellTok{}, false
		}
		c := l.s[l.pos]
		if c == '#' {
			l.skipComment()
			continue
		}
		if c == '\n' {
			l.pos++
			l.consumeHeredocs()
			return shellTok{kind: tokSep}, true
		}
		if tok, ok := l.readOperatorOrRedir(); ok {
			return tok, true
		}
		return shellTok{kind: tokWord, val: l.readWord()}, true
	}
	return shellTok{}, false
}

func (l *shellLexer) skipSpacesAndContinuations() {
	for l.pos < len(l.s) {
		switch l.s[l.pos] {
		case ' ', '\t', '\r':
			l.pos++
		case '\\':
			if l.pos+1 < len(l.s) && l.s[l.pos+1] == '\n' {
				l.pos += 2
				continue
			}
			return
		default:
			return
		}
	}
}

func (l *shellLexer) skipComment() {
	if nl := strings.IndexByte(l.s[l.pos:], '\n'); nl >= 0 {
		l.pos += nl
	} else {
		l.pos = len(l.s)
	}
}

func (l *shellLexer) readOperatorOrRedir() (shellTok, bool) {
	// Optional numeric file descriptor immediately before '<' or '>' (e.g. 2>, 2>&1).
	if fdEnd := l.scanFDPrefix(); fdEnd > l.pos {
		l.pos = fdEnd
		return l.readRedirOp(), true
	}
	c := l.s[l.pos]
	if c == '<' || c == '>' {
		return l.readRedirOp(), true
	}
	if c == '&' && l.pos+1 < len(l.s) && l.s[l.pos+1] == '>' {
		l.pos += 2
		if l.pos < len(l.s) && l.s[l.pos] == '>' {
			l.pos++
		}
		return shellTok{kind: tokRedir}, true
	}
	return l.readSeparatorOp()
}

func (l *shellLexer) scanFDPrefix() int {
	i := l.pos
	for i < len(l.s) && l.s[i] >= '0' && l.s[i] <= '9' {
		i++
	}
	if i > l.pos && i < len(l.s) && (l.s[i] == '<' || l.s[i] == '>') {
		return i
	}
	return l.pos
}

func (l *shellLexer) readRedirOp() shellTok {
	rest := l.s[l.pos:]
	switch {
	case strings.HasPrefix(rest, "<<<"):
		l.pos += 3
		return shellTok{kind: tokRedir}
	case strings.HasPrefix(rest, "<<-"):
		l.pos += 3
		return shellTok{kind: tokHeredoc, stripTabs: true}
	case strings.HasPrefix(rest, "<<"):
		l.pos += 2
		return shellTok{kind: tokHeredoc}
	case strings.HasPrefix(rest, ">>"), strings.HasPrefix(rest, "<&"),
		strings.HasPrefix(rest, ">&"), strings.HasPrefix(rest, "<>"),
		strings.HasPrefix(rest, ">|"):
		l.pos += 2
		return shellTok{kind: tokRedir}
	default:
		l.pos++
		return shellTok{kind: tokRedir}
	}
}

func (l *shellLexer) readSeparatorOp() (shellTok, bool) {
	rest := l.s[l.pos:]
	switch {
	case strings.HasPrefix(rest, ";;&"):
		l.pos += 3
		return shellTok{kind: tokSep}, true
	case strings.HasPrefix(rest, "&&"), strings.HasPrefix(rest, "||"),
		strings.HasPrefix(rest, "|&"), strings.HasPrefix(rest, ";;"),
		strings.HasPrefix(rest, ";&"):
		l.pos += 2
		return shellTok{kind: tokSep}, true
	case rest[0] == ';', rest[0] == '&', rest[0] == '|', rest[0] == '(', rest[0] == ')':
		l.pos++
		return shellTok{kind: tokSep}, true
	default:
		return shellTok{}, false
	}
}

func (l *shellLexer) readWord() string {
	var buf strings.Builder
	for l.pos < len(l.s) {
		c := l.s[l.pos]
		if isShellWordBreak(c) {
			break
		}
		switch {
		case c == '\'':
			l.readSingleQuoted(&buf)
		case c == '$' && l.pos+1 < len(l.s) && l.s[l.pos+1] == '\'':
			l.readANSICQuoted(&buf)
		case c == '"':
			l.readDoubleQuoted(&buf)
		case c == '\\':
			l.readEscape(&buf)
		default:
			buf.WriteByte(c)
			l.pos++
		}
	}
	return buf.String()
}

func isShellWordBreak(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

func (l *shellLexer) readSingleQuoted(buf *strings.Builder) {
	l.pos++ // skip opening '
	if end := strings.IndexByte(l.s[l.pos:], '\''); end >= 0 {
		buf.WriteString(l.s[l.pos : l.pos+end])
		l.pos += end + 1
		return
	}
	buf.WriteString(l.s[l.pos:])
	l.pos = len(l.s)
}

func (l *shellLexer) readANSICQuoted(buf *strings.Builder) {
	l.pos += 2 // skip $'
	for l.pos < len(l.s) {
		c := l.s[l.pos]
		l.pos++
		if c == '\'' {
			return
		}
		if c == '\\' && l.pos < len(l.s) {
			buf.WriteByte(l.s[l.pos])
			l.pos++
			continue
		}
		buf.WriteByte(c)
	}
}

func (l *shellLexer) readDoubleQuoted(buf *strings.Builder) {
	l.pos++ // skip opening "
	for l.pos < len(l.s) {
		c := l.s[l.pos]
		l.pos++
		if c == '"' {
			return
		}
		if c == '\\' && l.pos < len(l.s) {
			next := l.s[l.pos]
			if next == '\n' {
				l.pos++
				continue
			}
			if next == '"' || next == '\\' || next == '$' || next == '`' {
				buf.WriteByte(next)
				l.pos++
				continue
			}
		}
		buf.WriteByte(c)
	}
}

func (l *shellLexer) readEscape(buf *strings.Builder) {
	l.pos++ // skip \
	if l.pos >= len(l.s) {
		buf.WriteByte('\\')
		return
	}
	if l.s[l.pos] == '\n' {
		l.pos++ // line continuation
		return
	}
	buf.WriteByte(l.s[l.pos])
	l.pos++
}

func (l *shellLexer) consumeHeredocs() {
	for len(l.heredocs) > 0 {
		hd := l.heredocs[0]
		l.heredocs = l.heredocs[1:]
		for l.pos < len(l.s) {
			var line string
			if nl := strings.IndexByte(l.s[l.pos:], '\n'); nl < 0 {
				line = l.s[l.pos:]
				l.pos = len(l.s)
			} else {
				line = l.s[l.pos : l.pos+nl]
				l.pos += nl + 1
			}
			line = strings.TrimSuffix(line, "\r")
			if hd.stripTabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == hd.delim {
				break
			}
		}
	}
}

func isCommitGitCommand(words []string) bool {
	words = stripCommandPrefixes(words)
	if len(words) < 2 || !isGitProgram(words[0]) {
		return false
	}
	switch gitSubcommand(words[1:]) {
	case "commit", "cherry-pick", "revert", "merge", "rebase", "am":
		return true
	}
	return false
}

func isGitProgram(word string) bool {
	return word == "git" || (strings.Contains(word, "/") && path.Base(word) == "git")
}

func stripCommandPrefixes(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case isShellKeywordPrefix(w), isEnvAssignment(w), w == "nohup":
			words = words[1:]
		case w == "time":
			words = skipCommandFlags(words[1:], nil)
		case w == "exec":
			words = skipCommandFlags(words[1:], map[string]bool{"-a": true})
		case w == "command":
			rest, executes := skipCommandBuiltinFlags(words[1:])
			if !executes {
				return nil
			}
			words = rest
		case w == "env":
			words = skipEnvPrefix(words[1:])
		default:
			return words
		}
	}
	return nil
}

func isShellKeywordPrefix(word string) bool {
	switch word {
	case "if", "then", "elif", "else", "do", "while", "until", "!", "{", "coproc":
		return true
	}
	return false
}

func isEnvAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i := range eq {
		c := word[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if !isLetter && (i == 0 || !isDigit) {
			return false
		}
	}
	return true
}

func skipCommandFlags(args []string, takesArg map[string]bool) []string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return args[i+1:]
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return args[i:]
		}
		if takesArg[arg] {
			i++
		}
	}
	return nil
}

func skipCommandBuiltinFlags(args []string) ([]string, bool) {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:], true
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return args[i:], true
		}
		if strings.ContainsAny(arg[1:], "vV") {
			return nil, false
		}
	}
	return nil, false
}

func skipEnvPrefix(args []string) []string {
	takesArg := map[string]bool{"-u": true, "-C": true, "-S": true, "--unset": true, "--chdir": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return args[i+1:]
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			if takesArg[arg] {
				i++
			}
			continue
		}
		if isEnvAssignment(arg) {
			continue
		}
		return args[i:]
	}
	return nil
}

func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return ""
		}
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
		if gitOptionTakesArg(arg) {
			i++
		}
	}
	return ""
}

func gitOptionTakesArg(opt string) bool {
	switch opt {
	case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--attr-source":
		return true
	}
	return false
}
