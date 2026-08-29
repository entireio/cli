package transcript

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// shellHeadWords is how many command words ToolDetail keeps for a shell tool:
// the program and its subcommand (`go test`, `git log`, `npm run`).
const shellHeadWords = 2

// envAssignmentPattern matches a leading `VAR=value` word, the same shape
// strategy's entireSearchCommandPattern tolerates before a command.
var envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// bareRedirectPattern matches a redirection operator standing alone as a word
// (`>`, `>>`, `2>`, `<`, `<<<`, `&>`, `>&`), whose target is the NEXT word.
var bareRedirectPattern = regexp.MustCompile(`^&?\d*[<>]{1,3}&?$`)

// ToolDetail reduces a tool_use block to the short drill-down string token
// attribution shows beneath the tool's row. It is the ONE place those rules
// live; every agent's attributor calls it with the agent-native tool name,
// which is matched case-insensitively:
//
//   - shell tools (Bash, shell, exec, exec_command, run_terminal_cmd,
//     run_shell_command, terminal, execute): the head of the LAST command in
//     the pipeline — sanitized with stringutil.SanitizeShellCommand, split on
//     `;`, `&&`, `||`, `|` and newline, leading `VAR=x` assignments,
//     `-flag` words and redirections dropped, then the first shellHeadWords
//     words. `cd x && VAR=1 go test ./... -run X` → `go test`. The raw
//     command is never returned: it is user content and is not stored.
//   - file tools (Read, Edit, Write, MultiEdit, NotebookEdit, read_file,
//     write_file, edit_file, apply_patch): ToolInput.AnyFilePath, falling
//     back to NotebookPath.
//   - WebFetch / web_fetch / fetch: the URL's host, without port or path.
//   - Skill: the skill name.
//   - Task / Agent / spawn_agent: the subagent type, with the requested
//     model appended as ` (model)` when present.
//   - WebSearch, Grep, Glob, LS, search, list_dir and any other tool: "" —
//     the tool name is the whole label, and the query or pattern is user
//     content.
func ToolDetail(tool string, in ToolInput) string {
	switch strings.ToLower(tool) {
	case "bash", "shell", "exec", "exec_command", "run_terminal_cmd", "run_shell_command", "terminal", "execute":
		return shellCommandHead(in.Command)
	case "read", "edit", "write", "multiedit", "notebookedit", "read_file", "write_file", "edit_file", "apply_patch":
		if p := in.AnyFilePath(); p != "" {
			return p
		}
		return in.NotebookPath
	case "webfetch", "web_fetch", "fetch":
		return urlHost(in.URL)
	case "skill":
		return in.Skill
	case "task", "agent", "spawn_agent":
		return subagentDetail(in)
	default:
		return ""
	}
}

// shellCommandHead implements ToolDetail's shell rule; see its doc for the
// steps. It returns "" for an empty or all-quoted command.
func shellCommandHead(cmd string) string {
	segment := lastCommandSegment(stringutil.SanitizeShellCommand(cmd))
	words := make([]string, 0, shellHeadWords)
	skipNext := false
	for _, tok := range strings.Fields(segment) {
		tok = trimGrouping(strings.ReplaceAll(tok, "\x00", ""))
		switch {
		case tok == "":
			// A word that was entirely a quoted span, or bare grouping
			// punctuation.
		case skipNext:
			skipNext = false // the target of a bare redirection operator
		case bareRedirectPattern.MatchString(tok):
			skipNext = true
		case strings.ContainsAny(tok, "<>"):
			// Redirection with its target attached (`2>&1`, `>out`, `<<EOF`).
		case len(words) == 0 && envAssignmentPattern.MatchString(tok):
			// Leading environment assignment.
		case strings.HasPrefix(tok, "-"):
			// Flag.
		default:
			words = append(words, tok)
			if len(words) == shellHeadWords {
				return strings.Join(words, " ")
			}
		}
	}
	return strings.Join(words, " ")
}

// lastCommandSegment returns the text after the last command separator
// (`;`, newline, `|`, `||`, `&&`) in an already-sanitized command, skipping
// segments that are blank or hold only blanked spans and grouping punctuation
// — a trailing `;`, a closing `}` or a heredoc body must not hide the command
// before them. A lone `&` is not a separator here so `2>&1` stays intact.
// Backslash-escaped bytes are skipped.
func lastCommandSegment(s string) string {
	last, start := "", 0
	flush := func(end int) {
		if seg := s[start:end]; strings.Trim(seg, " \t\r\n\x00(){}") != "" {
			last = seg
		}
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case ';', '\n', '|':
			flush(i)
			start = i + 1
		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				flush(i)
				i++
				start = i + 1
			}
		}
	}
	flush(len(s))
	return last
}

// trimGrouping strips subshell and brace-group punctuation from a word —
// `(cd` → `cd`, `make)` → `make`, `}` → “ — leaving `$(...)` substitutions
// alone so they are not turned into an unbalanced fragment.
func trimGrouping(tok string) string {
	if strings.Contains(tok, "$(") {
		return tok
	}
	return strings.Trim(tok, "(){}")
}

// urlHost returns the hostname of raw (no port, no path), or "" when raw does
// not parse or carries no host.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// subagentDetail implements ToolDetail's Task/Agent rule: the subagent type,
// suffixed with ` (model)` when a model was requested. A Task with no subagent
// type yields "" even when a model is present — a bare `(model)` would label
// nothing.
func subagentDetail(in ToolInput) string {
	if in.SubagentType == "" {
		return ""
	}
	if in.Model != "" {
		return in.SubagentType + " (" + in.Model + ")"
	}
	return in.SubagentType
}
