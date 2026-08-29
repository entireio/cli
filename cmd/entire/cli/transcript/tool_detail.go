package transcript

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// shellHeadWords is how many command words ToolDetail always keeps for a
// shell tool: the program and its subcommand (`go test`, `git log`,
// `npm run`). A third word survives only when isPathLikeWord accepts it.
const shellHeadWords = 2

// envAssignmentPattern matches a `KEY=value` word: a leading environment
// assignment, an `export` / `env` operand, or a dotted config operand such as
// git's `-c core.pager=cat` — none of which is the command or its target.
var envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*=`)

// bareRedirectPattern matches a redirection operator standing alone as a word
// (`>`, `>>`, `2>`, `<`, `<<<`, `<<-`, `&>`, `>&`), whose target — or heredoc
// delimiter — is the NEXT word.
var bareRedirectPattern = regexp.MustCompile(`^&?\d*[<>]{1,3}-?&?$`)

// ToolDetail reduces a tool_use block to the short drill-down string token
// attribution shows beneath the tool's row. It is the ONE place those rules
// live; every agent's attributor calls it with the agent-native tool name,
// which is matched case-insensitively:
//
//   - shell tools (Bash, shell, exec, exec_command, run_terminal_cmd,
//     run_shell_command, terminal, execute): the head of the LAST command in
//     the pipeline — sanitized with stringutil.SanitizeShellCommand (quoted
//     spans become NUL and are dropped as words), split on `;`, `&&`, `||`,
//     `|` and newline, `(`/`{`/`)`/`}` grouping punctuation trimmed,
//     `KEY=x` assignments (`core.pager=cat` included), `-flag` words and
//     redirections dropped, then the
//     first two words plus a third ONLY when it is path-like
//     (isPathLikeWord: contains `/`, starts with `.`, or contains `*`) —
//     command, subcommand, and the path or glob it targets.
//     `go test ./cmd/entire/... -run TestX` →
//     `go test ./cmd/entire/...`; `git log -p -3` → `git log`;
//     `npm run build` → `npm run`. Words that can carry secrets are never
//     kept (isSensitiveWord: contain `://` or `@`, or start with `$`), so
//     `curl https://user:pass@example.com/x` → `curl` and `echo $TOKEN` →
//     `echo`. The raw command is never returned: it is user content and is
//     not stored.
//   - file tools (Read, Edit, Write, MultiEdit, NotebookEdit, read_file,
//     write_file, edit_file, apply_patch): ToolInput.AnyFilePath, falling
//     back to NotebookPath.
//   - WebFetch / web_fetch / fetch: the URL's host, without port or path.
//   - Skill / activate_skill: the skill name.
//   - Task / Agent / spawn_agent / delegate_to_agent: the subagent type, with
//     the requested model appended as ` (model)` when present.
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
	case "skill", "activate_skill":
		return in.Skill
	case "task", "agent", "spawn_agent", "delegate_to_agent":
		return subagentDetail(in)
	default:
		return ""
	}
}

// shellCommandHead implements ToolDetail's shell rule; see its doc for the
// steps. It returns "" for an empty or all-quoted command.
func shellCommandHead(cmd string) string {
	segment := lastCommandSegment(stringutil.SanitizeShellCommand(cmd))
	words := make([]string, 0, shellHeadWords+1)
	skipNext := false
	for _, tok := range strings.Fields(segment) {
		tok = trimGrouping(strings.ReplaceAll(tok, "\x00", ""))
		switch {
		case tok == "":
			// A word that was entirely a quoted span, or bare grouping
			// punctuation.
		case skipNext:
			skipNext = false // the target of a bare redirection operator
		case isSensitiveWord(tok):
			// URL, address or variable expansion: may carry a credential.
		case bareRedirectPattern.MatchString(tok):
			skipNext = true
		case strings.ContainsAny(tok, "<>"):
			// Redirection with its target attached (`2>&1`, `>out`, `<<EOF`).
		case envAssignmentPattern.MatchString(tok):
			// Assignment — leading `VAR=x`, `export`/`env` operands, or a
			// dotted `-c key=value` config operand — whose values are as
			// sensitive as the command's arguments are not.
		case strings.HasPrefix(tok, "-"):
			// Flag.
		case len(words) == shellHeadWords:
			// The head is complete; a third word is kept only when it is
			// the path or glob the command targets.
			if isPathLikeWord(tok) {
				words = append(words, tok)
			}
			return strings.Join(words, " ")
		default:
			words = append(words, tok)
		}
	}
	return strings.Join(words, " ")
}

// isSensitiveWord reports whether a command word could carry a credential or
// other secret and so must never appear in a detail: a URL or anything else
// with `://` (userinfo, tokens in paths), an `@` (user@host, git remotes,
// email addresses), or a `$` expansion whose value is unknown.
func isSensitiveWord(tok string) bool {
	return strings.Contains(tok, "://") || strings.Contains(tok, "@") || strings.HasPrefix(tok, "$")
}

// isPathLikeWord reports whether a command word names a path or glob target
// rather than a plain argument: it contains a `/`, starts with `.` (`./...`,
// `.github`), or contains a `*`. `./cmd/entire/...` and `src/` qualify;
// `build` and `host` do not.
func isPathLikeWord(tok string) bool {
	return strings.Contains(tok, "/") || strings.HasPrefix(tok, ".") || strings.Contains(tok, "*")
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
// `(cd` → `cd`, `make)` → `make`, `}` → "" — leaving the token containing
// `$(` alone so it is not turned into an unbalanced fragment. The tail tokens
// of a multi-word substitution (`$(cat x)` → `x)`) are trimmed and kept like
// any other word.
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
