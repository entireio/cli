package cli

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Codex (0.172 with some models) runs shell commands through a custom tool
// named "exec" whose input is JavaScript, e.g.
// `text(await tools.exec_command({cmd:"git -C /d commit -m m"}))`. The script
// is never evaluated: codexExecCalls only reads the string literals given as
// exec_command's cmd, command and workdir. A command built at run time (a
// variable, concatenation, or a template with ${}) is not read.

// codexExecTool is the name of Codex's JavaScript tool.
const codexExecTool = "exec"

// codexExecCallee is the nested shell tool a Codex script calls.
const codexExecCallee = "exec_command("

// execCallKeys are the exec_command arguments a scan reads.
var execCallKeys = append([]string{"workdir"}, shellCommandKeys...)

// codexExecInput returns the script of obj when obj is a Codex exec call.
func codexExecInput(obj map[string]any) (string, bool) {
	if typ, _ := obj["type"].(string); typ != "custom_tool_call" { //nolint:errcheck // absent is no call
		return "", false
	}
	if name, _ := obj["name"].(string); name != codexExecTool { //nolint:errcheck // absent is no call
		return "", false
	}
	script, ok := obj["input"].(string)
	return script, ok && len(script) <= maxEncodedArguments
}

// codexExecCalls returns the input of each exec_command call in script that
// names its command as a string literal: {"cmd": ..., "workdir": ...}.
func codexExecCalls(script string) []map[string]any {
	var calls []map[string]any
	for rest := script; ; {
		i := strings.Index(rest, codexExecCallee)
		if i < 0 {
			return calls
		}
		rest = rest[i+len(codexExecCallee):]
		if call := execCallArgs(rest); call != nil {
			calls = append(calls, call)
		}
	}
}

// execCallArgs reads the arguments after exec_command's opening parenthesis:
// a string literal (the command), or an object literal's cmd, command and
// workdir string values. It stops at the closing parenthesis.
func execCallArgs(src string) map[string]any {
	call := map[string]any{}
	depth := 1 // inside the call's parentheses; 2 is the argument object
	key := ""
	for i := 0; i < len(src) && depth > 0; {
		c := src[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			i = readExecArg(src, i, depth, key, call)
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
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
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

// readExecArg reads the string literal at src[i] into call when it is the
// command argument or one of execCallKeys' values, and returns the index after it.
func readExecArg(src string, i, depth int, key string, call map[string]any) int {
	lit, end, ok := jsStringLiteral(src, i)
	switch {
	case !ok:
	case depth == 1 && len(call) == 0:
		call["cmd"] = lit
	case depth == 2 && slices.Contains(execCallKeys, key):
		call[key] = lit
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

func isJSIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// jsStringLiteral decodes the JavaScript string literal opening at src[start]
// and returns it with the index after its closing quote. ok is false for an
// unterminated literal or a template literal with a substitution.
func jsStringLiteral(src string, start int) (lit string, end int, ok bool) {
	quote := src[start]
	var b strings.Builder
	for i := start + 1; i < len(src); {
		c := src[i]
		switch {
		case c == quote:
			return b.String(), i + 1, true
		case quote == '`' && c == '$' && i+1 < len(src) && src[i+1] == '{':
			return "", skipPast(src, i, quote), false
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
