package codex

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Codex (0.172 with some models) runs shell commands through a custom tool
// named "exec" whose input is JavaScript, e.g.
// `text(await tools.exec_command({cmd:"git -C /d commit -m m"}))`. The script
// is never evaluated: ExecScriptShellCalls reads the string literals given as
// exec_command's cmd, command and workdir, and every other whole string
// literal in the script that looks like a shell command (such as an element
// of an array of commands looped over). A template's ${NAME} is substituted
// only when NAME is bound once to a plain literal (see execScriptBindings); a
// command built any other way (concatenation, a call, an unresolved ${}) is
// not read.

// execScriptTool is the name of Codex's JavaScript tool.
const execScriptTool = "exec"

// execScriptCallee is the nested shell tool a Codex script calls.
const execScriptCallee = "exec_command("

// maxExecScript bounds the size of a script that is read.
const maxExecScript = 1 << 20

// execCallKeys are the exec_command arguments a scan reads.
var execCallKeys = []string{"workdir", "command", "cmd"}

// ExecScriptShellCalls reports whether obj (a decoded rollout payload) is a
// Codex exec custom tool call, and returns the input of each exec_command
// call in its script that names its command as a string literal:
// {"cmd": ..., "workdir": ...}.
func ExecScriptShellCalls(obj map[string]any) ([]map[string]any, bool) {
	if typ, _ := obj["type"].(string); typ != "custom_tool_call" { //nolint:errcheck // absent is no call
		return nil, false
	}
	if name, _ := obj["name"].(string); name != execScriptTool { //nolint:errcheck // absent is no call
		return nil, false
	}
	script, ok := obj["input"].(string)
	if !ok || len(script) > maxExecScript {
		return nil, false
	}
	return execScriptCalls(script), true
}

// execScriptCalls returns the exec_command inputs of script, then each other
// command-like whole literal as {"cmd": ...} when script calls exec_command.
func execScriptCalls(script string) []map[string]any {
	vars := execScriptBindings(script)
	calls := execCommandCalls(script, vars)
	if !strings.Contains(script, execScriptCallee) {
		return calls
	}
	seen := map[string]bool{}
	for _, call := range calls {
		if cmd, ok := call["cmd"].(string); ok {
			seen[cmd] = true
		}
		if cmd, ok := call["command"].(string); ok {
			seen[cmd] = true
		}
	}
	for _, cmd := range scriptCommandLiterals(script, vars) {
		if !seen[cmd] {
			seen[cmd] = true
			calls = append(calls, map[string]any{"cmd": cmd})
		}
	}
	return calls
}

// execCommandCalls returns the input of each exec_command call in script that
// names its command as a string literal: {"cmd": ..., "workdir": ...}.
func execCommandCalls(script string, vars map[string]string) []map[string]any {
	var calls []map[string]any
	for rest := script; ; {
		i := strings.Index(rest, execScriptCallee)
		if i < 0 {
			return calls
		}
		rest = rest[i+len(execScriptCallee):]
		if call := execCallArgs(rest, vars); call != nil {
			calls = append(calls, call)
		}
	}
}

// execCallArgs reads the arguments after exec_command's opening parenthesis:
// a string literal (the command), or an object literal's cmd, command and
// workdir string values. It stops at the closing parenthesis.
func execCallArgs(src string, vars map[string]string) map[string]any {
	call := map[string]any{}
	depth := 1 // inside the call's parentheses; 2 is the argument object
	key := ""
	firstArg := true // no token of the positional argument read yet
	for i := 0; i < len(src) && depth > 0; {
		c := src[i]
		positional := depth == 1 && firstArg
		if depth == 1 && !isJSSpace(c) {
			firstArg = false
		}
		switch {
		case c == '"' || c == '\'' || c == '`':
			i = readExecArg(src, i, vars, execArgPos{depth, positional, key}, call)
			key = ""
		case c == '(' || c == '{' || c == '[':
			depth++
			key = ""
			i++
		case c == ')' || c == '}' || c == ']':
			depth--
			key = ""
			i++
		case isJSIdentStart(c):
			key, i = readJSKey(src, i)
		case isJSSpace(c):
			i++
		default:
			key = ""
			i++
		}
	}
	if len(call) == 0 {
		return nil
	}
	return call
}

// execArgPos is where a literal sits in exec_command's arguments.
type execArgPos struct {
	depth      int
	positional bool
	key        string
}

// readExecArg reads the string literal at src[i] into call when it is the
// command argument or one of execCallKeys' values, and returns the index after
// it. A literal that is only part of an expression, such as "cp x " + name,
// is not read: its value is not the command.
func readExecArg(src string, i int, vars map[string]string, pos execArgPos, call map[string]any) int {
	lit, end, ok := jsStringLiteral(src, i, vars)
	switch {
	case !ok || !endsValue(src, end):
	case pos.positional:
		call["cmd"] = lit
	case pos.depth == 2 && slices.Contains(execCallKeys, pos.key):
		call[pos.key] = lit
	}
	return end
}

// readJSKey reads the identifier at src[i] and returns it as a key when a
// colon follows, with the index after the identifier (and colon).
func readJSKey(src string, i int) (key string, next int) {
	j := i + 1
	for j < len(src) && (isJSIdentStart(src[j]) || src[j] >= '0' && src[j] <= '9') {
		j++
	}
	k := j
	for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n') {
		k++
	}
	if k < len(src) && src[k] == ':' {
		return src[i:j], k + 1
	}
	return "", k
}

// endsValue reports whether the value ending at src[i] is a whole argument or
// property value: only a comma or closing bracket follows it.
func endsValue(src string, i int) bool {
	for i < len(src) && isJSSpace(src[i]) {
		i++
	}
	return i == len(src) || src[i] == ',' || src[i] == '}' || src[i] == ')'
}

func isJSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isJSIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// jsStringLiteral decodes the JavaScript string literal opening at src[start]
// and returns it with the index after its closing quote. A template's ${NAME}
// takes vars[NAME]; ok is false for an unterminated literal or a template
// with any other substitution.
func jsStringLiteral(src string, start int, vars map[string]string) (lit string, end int, ok bool) {
	quote := src[start]
	var b strings.Builder
	for i := start + 1; i < len(src); {
		c := src[i]
		switch {
		case c == quote:
			return b.String(), i + 1, true
		case quote == '`' && c == '$' && i+1 < len(src) && src[i+1] == '{':
			val, next, found := templateVar(src, i+2, vars)
			if !found {
				return "", skipPast(src, i, quote), false
			}
			b.WriteString(val)
			i = next
		case c == '\\' && i+1 < len(src):
			i = decodeJSEscape(src, i+1, &b)
		case c == '\n' && quote != '`':
			return "", i, false
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", len(src), false
}

// skipPast returns the index after the next unescaped quote from i.
func skipPast(src string, i int, quote byte) int {
	for ; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case quote:
			return i + 1
		}
	}
	return len(src)
}

// decodeJSEscape writes the escape sequence whose letter is at src[i] and
// returns the index after it.
func decodeJSEscape(src string, i int, b *strings.Builder) int {
	switch c := src[i]; c {
	case 'n':
		b.WriteByte('\n')
	case 't':
		b.WriteByte('\t')
	case 'r':
		b.WriteByte('\r')
	case '0':
		b.WriteByte(0)
	case '\n': // line continuation
	case 'x':
		if r, ok := hexRune(src, i+1, i+3); ok {
			b.WriteRune(r)
			return i + 3
		}
		b.WriteByte(c)
	case 'u':
		if i+1 < len(src) && src[i+1] == '{' {
			if brace := strings.IndexByte(src[i+1:], '}'); brace > 0 {
				if r, ok := hexRune(src, i+2, i+1+brace); ok {
					b.WriteRune(r)
					return i + 2 + brace
				}
			}
		} else if r, ok := hexRune(src, i+1, i+5); ok {
			b.WriteRune(r)
			return i + 5
		}
		b.WriteByte(c)
	default:
		b.WriteByte(c)
	}
	return i + 1
}

// hexRune parses src[from:to] as a hexadecimal code point.
func hexRune(src string, from, to int) (rune, bool) {
	if to > len(src) || from >= to {
		return 0, false
	}
	if to-from > 6 {
		return 0, false
	}
	var r rune
	for _, c := range src[from:to] {
		switch c = unicode.ToLower(c); {
		case c >= '0' && c <= '9':
			r = r<<4 | (c - '0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | (c - 'a' + 10)
		default:
			return 0, false
		}
	}
	return r, utf8.ValidRune(r)
}
