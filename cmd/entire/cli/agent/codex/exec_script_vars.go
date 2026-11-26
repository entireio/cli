package codex

import (
	"regexp"
	"strings"
)

// Codex sometimes batches commands in one script, binding a path to a
// constant and interpolating it into template literals:
//
//	const p="/abs/work";
//	const commands=[`git -C ${p}/D commit -m d1`];
//	for (const c of commands) text(await tools.exec_command({cmd:c}));
//
// The script is still never evaluated. Only a name declared once with a plain
// literal and never assigned again is substituted, and only whole literals
// (not operands of + or any other operator) are read as commands.

// jsDeclaration matches the start of `const|let|var NAME =`.
var jsDeclaration = regexp.MustCompile(`(?:^|[^\w$.])(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*`)

// shellCommandStart matches a literal that opens with a program name and an
// argument, such as `git -C /d commit`.
var shellCommandStart = regexp.MustCompile(`^\s*[A-Za-z_./~][\w./~+-]*\s+\S`)

// execScriptBindings returns the names script declares once, as a string
// literal without substitutions, and never assigns again.
func execScriptBindings(script string) map[string]string {
	vars := map[string]string{}
	declared := map[string]int{}
	for _, m := range jsDeclaration.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		declared[name]++
		start := m[1]
		if start >= len(script) || !strings.ContainsRune("\"'`", rune(script[start])) {
			continue
		}
		if lit, end, ok := jsStringLiteral(script, start, nil); ok && endsStatement(script, end) {
			vars[name] = lit
		}
	}
	for name := range vars {
		if declared[name] != 1 || reassigned(script, name) {
			delete(vars, name)
		}
	}
	return vars
}

// endsStatement reports whether only a statement or declarator end follows
// src[i] on its line.
func endsStatement(src string, i int) bool {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return i == len(src) || strings.IndexByte(";,\r\n", src[i]) >= 0
}

// reassigned reports whether script assigns name after declaring it (an
// assignment operator, ++ or --). The declaration is one match.
func reassigned(script, name string) bool {
	q := regexp.QuoteMeta(name)
	assign := regexp.MustCompile(`(?:^|[^\w$.])` + q + `\s*(?:\*\*|<<|>>>?|&&|\|\||\?\?|[-+*/%&|^])?=(?:[^=]|$)`)
	step := regexp.MustCompile(`(?:^|[^\w$.])` + q + `\s*(?:\+\+|--)|(?:\+\+|--)\s*` + q + `(?:[^\w$]|$)`)
	return len(assign.FindAllStringIndex(script, -1)) > 1 || step.MatchString(script)
}

// templateVar reads the substitution whose expression starts at src[i] (after
// "${"). found is true when the expression is a bare name in vars; next is the
// index after its closing brace.
func templateVar(src string, i int, vars map[string]string) (val string, next int, found bool) {
	end := strings.IndexByte(src[i:], '}')
	if end < 0 {
		return "", 0, false
	}
	val, found = vars[strings.TrimSpace(src[i:i+end])]
	return val, i + end + 1, found
}

// scriptCommandLiterals returns script's whole string literals, with
// substitutions resolved from vars, that look like shell commands.
func scriptCommandLiterals(script string, vars map[string]string) []string {
	var cmds []string
	for i := 0; i < len(script); {
		switch c := script[i]; {
		case strings.HasPrefix(script[i:], "//"):
			i = skipTo(script, i, "\n")
		case strings.HasPrefix(script[i:], "/*"):
			i = skipTo(script, i+2, "*/")
		case c == '"' || c == '\'' || c == '`':
			lit, end, ok := jsStringLiteral(script, i, vars)
			if ok && wholeLiteral(script, i, end) && shellCommandStart.MatchString(lit) {
				cmds = append(cmds, lit)
			}
			i = max(end, i+1)
		default:
			i++
		}
	}
	return cmds
}

// skipTo returns the index after the next sep from i, or len(src).
func skipTo(src string, i int, sep string) int {
	if j := strings.Index(src[i:], sep); j >= 0 {
		return i + j + len(sep)
	}
	return len(src)
}

// wholeLiteral reports whether the literal at src[start:end] is a whole value:
// an element, argument, property value, or initializer, not an operand.
func wholeLiteral(src string, start, end int) bool {
	p := start - 1
	for p >= 0 && isJSSpace(src[p]) {
		p--
	}
	if p >= 0 && strings.IndexByte("[(,=:;{", src[p]) < 0 {
		return false
	}
	if p > 0 && src[p] == '=' && strings.IndexByte("=!<>", src[p-1]) >= 0 {
		return false
	}
	n, newline := end, false
	for n < len(src) && isJSSpace(src[n]) {
		newline = newline || src[n] == '\n'
		n++
	}
	if n == len(src) || strings.IndexByte(",])};", src[n]) >= 0 {
		return true
	}
	return newline && strings.IndexByte("+-*/%.?&|<>=([`", src[n]) < 0
}
