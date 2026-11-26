package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Shell discovery for cross-repository linking: an agent can work in another
// repository through its shell alone (`cd ../d && git commit`, `echo > /abs`),
// which no file-edit tool records. The transcript still holds the commands and
// their output, so this reads them agent-neutrally: it walks every JSON value
// and takes shell commands from "command"/"cmd" fields (Claude Code, Pi,
// OpenCode, and Codex's JSON-encoded arguments), working directories from
// "workdir", and commit SHAs from `git commit` summary lines anywhere in tool
// output. Commands are matched against, never stored or logged.

// maxShellDirs bounds the directories one scan reports; each costs a git call.
const maxShellDirs = 32

// maxShellCommits bounds the commit SHAs one scan reports.
const maxShellCommits = 64

// maxNestedJSON bounds the size of a string value decoded as nested JSON.
const maxNestedJSON = 1 << 20

// commitSummaryLine matches git commit's summary line, "[branch sha] subject",
// including "[branch (root-commit) sha]" and "[detached HEAD sha]".
var commitSummaryLine = regexp.MustCompile(`(?m)^\[[^\]\n]{1,256} ([0-9a-f]{7,40})\] `)

// shellSeparators splits a command line into simple commands.
var shellSeparators = regexp.MustCompile(`&&|\|\||[;|\n()]`)

// shellActivity is what a transcript's shell commands show of the agent's work
// in other directories.
type shellActivity struct {
	dirs    []string // absolute paths commands worked in or named
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

func (s *shellScan) walk(v any) {
	switch t := v.(type) {
	case map[string]any:
		for key, val := range t {
			switch key {
			case "command", "cmd":
				s.command(val)
			case "workdir":
				if dir, ok := val.(string); ok {
					s.addDir(dir)
				}
			}
			s.walk(val)
		}
	case []any:
		for _, val := range t {
			s.walk(val)
		}
	case string:
		s.text(t)
	}
}

// text takes commit SHAs from a string, and walks it when it holds JSON
// (Codex encodes tool arguments and some output that way).
func (s *shellScan) text(str string) {
	for _, m := range commitSummaryLine.FindAllStringSubmatch(str, -1) {
		if sha := m[1]; !s.seenCommits[sha] && len(s.out.commits) < maxShellCommits {
			s.seenCommits[sha] = true
			s.out.commits = append(s.out.commits, sha)
		}
	}
	trimmed := strings.TrimSpace(str)
	if len(trimmed) < 2 || len(trimmed) > maxNestedJSON || (trimmed[0] != '{' && trimmed[0] != '[') {
		return
	}
	var nested any
	if json.Unmarshal([]byte(trimmed), &nested) == nil {
		s.walk(nested)
	}
}

// command reads a shell command: a string, or an argv array (Codex).
func (s *shellScan) command(v any) {
	switch t := v.(type) {
	case string:
		s.commandLine(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, p := range t {
			if str, ok := p.(string); ok {
				parts = append(parts, str)
			}
		}
		s.commandLine(strings.Join(parts, " "))
	}
}

// commandLine takes the directories a command line works in: `cd <dir>`,
// `git -C <dir>`, and absolute paths among its arguments and redirections.
func (s *shellScan) commandLine(line string) {
	for _, simple := range shellSeparators.Split(line, -1) {
		fields := strings.Fields(simple)
		for i, raw := range fields {
			tok := unquote(raw)
			next := ""
			if i+1 < len(fields) {
				next = unquote(fields[i+1])
			}
			switch {
			case tok == "cd" && next != "":
				s.addDir(next)
			case tok == "-C" && next != "" && slicesContainsGit(fields[:i]):
				s.addDir(next)
			default:
				if path := pathArgument(tok); path != "" {
					s.addDir(path)
				}
			}
		}
	}
}

func slicesContainsGit(fields []string) bool {
	for _, f := range fields {
		if filepath.Base(unquote(f)) == hooksGitCmdName {
			return true
		}
	}
	return false
}

// pathArgument returns the absolute (or home-relative) path a token names,
// after a redirection operator or an `=`, or "".
func pathArgument(tok string) string {
	if i := strings.LastIndexAny(tok, "<>="); i >= 0 {
		tok = tok[i+1:]
	}
	tok = strings.TrimRight(unquote(tok), ",:")
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~/") {
		return tok
	}
	return ""
}

func unquote(tok string) string {
	return strings.Trim(tok, `"'`)
}

func (s *shellScan) addDir(dir string) {
	if len(s.out.dirs) >= maxShellDirs || dir == "" || dir == "-" {
		return
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		home, err := userdirs.Home()
		if err != nil {
			return
		}
		dir = filepath.Join(home, rest)
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
