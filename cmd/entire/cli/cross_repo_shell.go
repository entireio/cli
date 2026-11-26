package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Shell discovery for cross-repository linking: an agent can work in another
// repository through its shell alone (`cd ../d && git commit`, `echo > /abs`),
// which no file-edit tool records. The transcript still holds the commands and
// their output, so this reads them: only the inputs of the agent's own shell
// tool calls (shellToolInput), never tool results or other text, and from
// those only evidence of writing (shellScan.commandLine): a commit-making git
// command and the directory it ran in, a redirection's target, or a
// file-writing command's targets. A command that only reads (`cat /x`,
// `git -C /x log`) names no directory, so it never leads to an install. Commit
// SHAs come from `git commit` summary lines in any string of the transcript.
// Commands are matched against, never stored or logged.

// maxShellDirs bounds the directories one scan reports; each costs a git call.
const maxShellDirs = 32

// maxShellCommits bounds the commit SHAs one scan reports.
const maxShellCommits = 64

// maxEncodedArguments bounds the size of a tool call's JSON-encoded arguments.
const maxEncodedArguments = 1 << 20

// gitExecutable is git's executable name, as a shell command names it.
const gitExecutable = "git"

// commitSummaryLine matches git commit's summary line, "[branch sha] subject",
// including "[branch (root-commit) sha]" and "[detached HEAD sha]".
var commitSummaryLine = regexp.MustCompile(`(?m)^\[[^\]\n]{1,256} ([0-9a-f]{7,40})\] `)

// shellSeparators splits a command line into simple commands.
var shellSeparators = regexp.MustCompile(`&&|\|\||[;|\n()]`)

// shellToolNames are the shell tools of the supported agents, lowercased:
// Claude Code's Bash, Pi's and OpenCode's bash, Codex's shell, exec_command
// and shell_command, Gemini's run_shell_command, Copilot's run_in_terminal.
var shellToolNames = []string{"bash", "shell", "exec_command", "shell_command", "local_shell", "container.exec", "run_shell_command", "run_in_terminal"}

// shellToolInputKeys are where a tool call keeps its input: Claude Code's
// "input", Pi's and Codex's "arguments" (Codex's JSON-encoded), Gemini's
// "args", others' "parameters".
var shellToolInputKeys = []string{"input", "arguments", "args", "parameters"}

// toolResultKeys hold tool results rather than calls. They are read for
// commit SHAs only, so a result quoting a transcript adds no directory.
var toolResultKeys = []string{"toolUseResult", "tool_result", "output", "result"}

// toolResultTypes and toolResultRoles mark a tool result object.
var (
	toolResultTypes = strings.Fields("tool_result function_call_output custom_tool_call_output")
	toolResultRoles = strings.Fields("toolResult tool")
)

// toolNameKeys hold a tool call's tool name.
var toolNameKeys = strings.Fields("name tool toolName")

// gitCommitVerbs are git subcommands that make commits.
var gitCommitVerbs = strings.Fields("commit merge rebase cherry-pick revert am pull")

// writeAllArgs are commands that write every path argument.
var writeAllArgs = []string{"touch", "mkdir", "rm", "rmdir", "tee", "truncate"}

// writeLastArg are commands that write their last path argument.
var writeLastArg = []string{"cp", "mv", "ln", "install", "rsync"}

// shellActivity is what a transcript's shell commands show of the agent's work
// in other directories.
type shellActivity struct {
	dirs    []string // absolute paths commands wrote in
	commits []string // abbreviated SHAs `git commit` reported
}

// scanShellActivity reads the transcript data from entry fromEntry on (a
// JSONL line, or an OpenCode export's message index). Relative directories
// resolve against cwd.
func scanShellActivity(data []byte, fromEntry int, cwd string) shellActivity {
	s := &shellScan{cwd: cwd, seenDirs: map[string]bool{}, seenCommits: map[string]bool{}}
	for _, v := range transcriptEntries(data, fromEntry) {
		s.walk(v)
	}
	return s.out
}

// transcriptEntries decodes data's entries from fromEntry on: an OpenCode
// export (one JSON document with a "messages" array) by message, otherwise
// JSONL by line.
func transcriptEntries(data []byte, fromEntry int) []any {
	fromEntry = max(fromEntry, 0)
	var doc struct {
		Messages []any `json:"messages"`
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(trimmed, &doc) == nil && doc.Messages != nil {
		if fromEntry >= len(doc.Messages) {
			return nil
		}
		return doc.Messages[fromEntry:]
	}
	var out []any
	for i, line := range bytes.Split(data, []byte("\n")) {
		if i < fromEntry || len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var v any
		if json.Unmarshal(line, &v) == nil {
			out = append(out, v)
		}
	}
	return out
}

type shellScan struct {
	cwd         string
	out         shellActivity
	seenDirs    map[string]bool
	seenCommits map[string]bool
}

// walk finds shell tool calls and commit summary lines in v. Tool results
// are read for commit SHAs only.
func (s *shellScan) walk(v any) {
	switch t := v.(type) {
	case map[string]any:
		if input, ok := shellToolInput(t); ok {
			s.toolCall(input)
		}
		result := isToolResult(t)
		for key, val := range t {
			if result || slices.Contains(toolResultKeys, key) {
				s.commitsIn(val)
				continue
			}
			s.walk(val)
		}
	case []any:
		for _, val := range t {
			s.walk(val)
		}
	case string:
		s.commitsIn(t)
	}
}

// commitsIn takes commit SHAs from every string in v.
func (s *shellScan) commitsIn(v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, val := range t {
			s.commitsIn(val)
		}
	case []any:
		for _, val := range t {
			s.commitsIn(val)
		}
	case string:
		for _, m := range commitSummaryLine.FindAllStringSubmatch(t, -1) {
			if sha := m[1]; !s.seenCommits[sha] && len(s.out.commits) < maxShellCommits {
				s.seenCommits[sha] = true
				s.out.commits = append(s.out.commits, sha)
			}
		}
		// Codex JSON-encodes some output.
		if trimmed := strings.TrimSpace(t); len(trimmed) > 1 && len(trimmed) <= maxEncodedArguments && trimmed[0] == '{' {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				s.commitsIn(nested)
			}
		}
	}
}

// isToolResult reports whether obj is a tool result (Claude Code's
// tool_result, Codex's function_call_output, Pi's toolResult message).
func isToolResult(obj map[string]any) bool {
	typ, _ := obj["type"].(string)  //nolint:errcheck // absent is not a result
	role, _ := obj["role"].(string) //nolint:errcheck // absent is not a result
	return slices.Contains(toolResultTypes, typ) || slices.Contains(toolResultRoles, role)
}

// shellToolInput returns the input of obj when obj is a call of a shell tool:
// a tool named in shellToolNames (by "name", "tool" or "toolName") with its
// input under one of shellToolInputKeys (or OpenCode's "state.input"), or a
// Codex local_shell_call's "action".
func shellToolInput(obj map[string]any) (map[string]any, bool) {
	if typ, _ := obj["type"].(string); typ == "local_shell_call" { //nolint:errcheck // absent is no call
		action, ok := obj["action"].(map[string]any)
		return action, ok
	}
	if isToolResult(obj) || !slices.Contains(shellToolNames, strings.ToLower(toolName(obj))) {
		return nil, false
	}
	if state, ok := obj["state"].(map[string]any); ok {
		if input, ok := state["input"].(map[string]any); ok {
			return input, true
		}
	}
	for _, key := range shellToolInputKeys {
		switch in := obj[key].(type) {
		case map[string]any:
			return in, true
		case string:
			var decoded map[string]any
			if len(in) <= maxEncodedArguments && json.Unmarshal([]byte(in), &decoded) == nil {
				return decoded, true
			}
		}
	}
	return nil, false
}

func toolName(obj map[string]any) string {
	for _, key := range toolNameKeys {
		if name, ok := obj[key].(string); ok {
			return name
		}
	}
	return ""
}

// toolCall reads a shell tool call's command and working directory.
func (s *shellScan) toolCall(input map[string]any) {
	workdir := ""
	for _, key := range []string{"workdir", "cwd", "working_directory"} {
		if dir, ok := input[key].(string); ok && dir != "" {
			workdir = dir
			break
		}
	}
	for _, key := range []string{"command", "cmd"} {
		if line := commandText(input[key]); line != "" {
			s.commandLine(line, workdir)
		}
	}
}

// commandText is a shell command's text: a string, or an argv array (Codex),
// whose script is the argument after a shell's -c (or -lc).
func commandText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		argv := make([]string, 0, len(t))
		for _, p := range t {
			if str, ok := p.(string); ok {
				argv = append(argv, str)
			}
		}
		for i := 1; i+1 < len(argv); i++ {
			if strings.HasPrefix(argv[i], "-") && strings.HasSuffix(argv[i], "c") {
				return argv[i+1]
			}
		}
		return strings.Join(argv, " ")
	}
	return ""
}

// commandLine takes the directories a command line wrote in, starting in
// workdir (or the session's cwd): the directory a commit-making git command
// ran in (after `cd`, or its -C), a redirection's target, and the targets of
// file-writing commands. `cd` alone, and reads, name nothing.
func (s *shellScan) commandLine(line, workdir string) {
	dir := s.cwd
	if workdir != "" {
		dir = s.resolve(s.cwd, workdir)
	}
	for _, simple := range shellSeparators.Split(line, -1) {
		fields := strings.Fields(simple)
		if len(fields) == 0 {
			continue
		}
		cmd := filepath.Base(unquote(fields[0]))
		args := fields[1:]
		if cmd == "cd" {
			if len(args) > 0 {
				dir = s.resolve(dir, unquote(args[0]))
			}
			continue
		}
		for _, target := range redirectTargets(fields) {
			s.addDir(s.resolve(dir, target))
		}
		for _, target := range writeTargets(cmd, args) {
			s.addDir(s.resolve(dir, target))
		}
	}
}

// writeTargets returns what one simple command writes, relative to the
// directory it runs in: that directory (or its -C) for a commit-making git
// command, or a file-writing command's path arguments.
func writeTargets(cmd string, args []string) []string {
	paths := pathArgs(args)
	switch {
	case cmd == gitExecutable:
		gitDir, verb := gitInvocation(args)
		if !slices.Contains(gitCommitVerbs, verb) {
			return nil
		}
		if gitDir == "" {
			gitDir = "."
		}
		return []string{gitDir}
	case slices.Contains(writeAllArgs, cmd):
		return paths
	case slices.Contains(writeLastArg, cmd) && len(paths) > 1:
		return paths[len(paths)-1:]
	case (cmd == "sed" || cmd == "perl") && len(paths) > 1 && slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-i") }):
		return paths[1:] // the first is the script
	}
	return nil
}

// gitInvocation returns a git command's -C directory and its subcommand.
func gitInvocation(args []string) (dir, verb string) {
	for i := 0; i < len(args); i++ {
		arg := unquote(args[i])
		switch {
		case arg == "-C" && i+1 < len(args):
			i++
			dir = unquote(args[i])
		case arg == "-c" || arg == "--git-dir" || arg == "--work-tree" || arg == "--namespace":
			i++
		case strings.HasPrefix(arg, "-"):
		default:
			return dir, arg
		}
	}
	return dir, ""
}

// redirectTargets returns the files a simple command's output redirections
// (`>`, `>>`, `2>`, `&>`) write, attached or as the next field.
func redirectTargets(fields []string) []string {
	var out []string
	for i, field := range fields {
		j := strings.LastIndexByte(field, '>')
		if j < 0 {
			continue
		}
		target := field[j+1:]
		if target == "" && i+1 < len(fields) {
			target = fields[i+1]
		}
		if target = unquote(target); target != "" && !strings.HasPrefix(target, "&") && target != "/dev/null" {
			out = append(out, target)
		}
	}
	return out
}

// pathArgs are a command's arguments that are neither flags nor redirections.
func pathArgs(args []string) []string {
	var out []string
	for i, arg := range args {
		arg = unquote(arg)
		redirected := i > 0 && strings.ContainsAny(args[i-1][len(args[i-1])-1:], "<>")
		if arg == "" || redirected || strings.HasPrefix(arg, "-") || strings.ContainsAny(arg, "<>") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func unquote(tok string) string {
	return strings.Trim(tok, `"'`)
}

// resolve resolves path against dir, when path is relative and dir is set.
func (s *shellScan) resolve(dir, path string) string {
	if path == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "~/") || dir == "" {
		return path
	}
	return filepath.Join(expandHome(dir), path)
}

// expandHome expands a leading "~/", or returns "" when the home directory
// is unknown.
func expandHome(path string) string {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path
	}
	home, err := userdirs.Home()
	if err != nil {
		return ""
	}
	return filepath.Join(home, rest)
}

func (s *shellScan) addDir(dir string) {
	if len(s.out.dirs) >= maxShellDirs || dir == "" || dir == "-" {
		return
	}
	if dir = expandHome(dir); dir == "" {
		return
	}
	if !filepath.IsAbs(dir) {
		if !filepath.IsAbs(s.cwd) {
			return
		}
		dir = filepath.Join(s.cwd, dir)
	}
	dir = filepath.Clean(dir)
	if !s.seenDirs[dir] {
		s.seenDirs[dir] = true
		s.out.dirs = append(s.out.dirs, dir)
	}
}
