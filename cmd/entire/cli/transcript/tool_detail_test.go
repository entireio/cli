package transcript

import (
	"strings"
	"testing"
)

func TestToolDetail(t *testing.T) {
	t.Parallel()

	const (
		toolBash     = "Bash"
		toolRead     = "Read"
		toolWebFetch = "WebFetch"
		toolTask     = "Task"
	)

	tests := []struct {
		name string
		tool string
		in   ToolInput
		want string
	}{
		// Shell tools.
		{name: "bash plain command", tool: toolBash, in: ToolInput{Command: "ls"}, want: "ls"},
		{name: "bash flags dropped", tool: toolBash, in: ToolInput{Command: "git log -p -3"}, want: "git log"},
		{name: "bash long flag with value dropped", tool: toolBash, in: ToolInput{Command: "git log --since=yesterday --oneline"}, want: "git log"},
		// Known properties, pinned so a change is deliberate; not the ideal output.
		{name: "bash flag dropped but its separate value is kept, plain third word dropped", tool: toolBash, in: ToolInput{Command: "docker -H host ps -a"}, want: "docker host"},
		{
			name: "bash cd prefix and env assignment stripped",
			tool: toolBash,
			in:   ToolInput{Command: "cd x && VAR=1 go test ./... -run X"},
			want: "go test ./...",
		},
		{name: "bash path-like third word kept", tool: toolBash, in: ToolInput{Command: "go test ./cmd/entire/... -run TestX"}, want: "go test ./cmd/entire/..."},
		{name: "bash plain third word dropped", tool: toolBash, in: ToolInput{Command: "npm run build"}, want: "npm run"},
		{name: "bash directory as second word", tool: toolBash, in: ToolInput{Command: "ls -la src/"}, want: "ls src/"},
		{name: "bash glob third word kept", tool: toolBash, in: ToolInput{Command: "git add -A *.go extra"}, want: "git add *.go"},
		{name: "bash dot-prefixed third word kept", tool: toolBash, in: ToolInput{Command: "git add -p .github other"}, want: "git add .github"},
		{name: "bash fourth word never kept", tool: toolBash, in: ToolInput{Command: "go test ./... ./other/..."}, want: "go test ./..."},
		{name: "bash third word after flag kept when path-like", tool: toolBash, in: ToolInput{Command: "go vet -v ./..."}, want: "go vet ./..."},
		{name: "bash env assignment with quoted value", tool: toolBash, in: ToolInput{Command: `FOO="a b" make check`}, want: "make check"},
		{name: "bash several env assignments", tool: toolBash, in: ToolInput{Command: "A=1 B=2 cmd sub arg"}, want: "cmd sub"},
		{name: "bash dotted config operand dropped", tool: toolBash, in: ToolInput{Command: "git -c core.pager=cat log"}, want: "git log"},
		{name: "bash last pipeline stage wins", tool: toolBash, in: ToolInput{Command: "a | b -x c"}, want: "b c"},
		{name: "bash semicolon", tool: toolBash, in: ToolInput{Command: "first one; second two three"}, want: "second two"},
		{name: "bash or separator", tool: toolBash, in: ToolInput{Command: "test -f x || touch x"}, want: "touch x"},
		{name: "bash newline separator", tool: toolBash, in: ToolInput{Command: "cd x\nmake build"}, want: "make build"},
		{name: "bash trailing separator ignored", tool: toolBash, in: ToolInput{Command: "make build;"}, want: "make build"},
		{name: "bash quoted separator is not a boundary", tool: toolBash, in: ToolInput{Command: `git commit -m "a; b | c"`}, want: "git commit"},
		{name: "bash single-quoted argument is dropped", tool: toolBash, in: ToolInput{Command: "echo 'hello world' more"}, want: "echo more"},
		{name: "bash escaped separator is a word, not a boundary", tool: toolBash, in: ToolInput{Command: `echo \; b`}, want: `echo \;`},
		{name: "bash heredoc reduces to the command", tool: toolBash, in: ToolInput{Command: "cat <<'EOF'\nline one\nline; two\nEOF"}, want: "cat"},
		{name: "bash heredoc with redirect target", tool: toolBash, in: ToolInput{Command: "cat > out.txt <<EOF\nbody\nEOF\n"}, want: "cat"},
		{name: "bash command after heredoc wins", tool: toolBash, in: ToolInput{Command: "cat <<EOF\nbody\nEOF\nmake test"}, want: "make test"},
		{name: "bash stderr redirect is not a separator", tool: toolBash, in: ToolInput{Command: "go test ./... 2>&1"}, want: "go test ./..."},
		{name: "bash redirect target dropped", tool: toolBash, in: ToolInput{Command: "echo hi > out.txt"}, want: "echo hi"},
		{name: "bash append redirect target dropped", tool: toolBash, in: ToolInput{Command: "cmd >> log.txt"}, want: "cmd"},
		{name: "bash input redirect dropped", tool: toolBash, in: ToolInput{Command: "sort < in.txt"}, want: "sort"},
		{name: "bash subshell grouping stripped", tool: toolBash, in: ToolInput{Command: "(cd x && make all)"}, want: "make all"},
		{name: "bash brace group stripped", tool: toolBash, in: ToolInput{Command: "{ a; b two; }"}, want: "b two"},
		{name: "bash substitution is an expansion and dropped", tool: toolBash, in: ToolInput{Command: "echo $(date)"}, want: "echo"},
		// Known properties, pinned so a change is deliberate; not the ideal output.
		{name: "bash multi-word substitution tail is kept", tool: toolBash, in: ToolInput{Command: "echo $(cat ~/.secret) more"}, want: "echo ~/.secret"},
		{name: "bash export assignment value dropped", tool: toolBash, in: ToolInput{Command: "export API_KEY=abc"}, want: "export"},
		{name: "bash env assignment operand dropped", tool: toolBash, in: ToolInput{Command: "env FOO=bar go test ./..."}, want: "env go"},
		{name: "bash assignment after wrapper dropped", tool: toolBash, in: ToolInput{Command: "sudo FOO=bar make install"}, want: "sudo make"},
		{name: "bash dash heredoc with space reduces to the command", tool: toolBash, in: ToolInput{Command: "cat <<- EOF\n\tbody\n\tEOF"}, want: "cat"},
		{name: "bash url with credentials dropped", tool: toolBash, in: ToolInput{Command: "curl https://user:pass@example.com/x"}, want: "curl"},
		{name: "bash plain url dropped", tool: toolBash, in: ToolInput{Command: "curl -s https://example.com/api?token=abc"}, want: "curl"},
		{name: "bash variable expansion dropped", tool: toolBash, in: ToolInput{Command: "echo $TOKEN"}, want: "echo"},
		{name: "bash braced expansion dropped", tool: toolBash, in: ToolInput{Command: "echo ${TOKEN}"}, want: "echo"},
		{name: "bash git remote url dropped", tool: toolBash, in: ToolInput{Command: "git remote add origin git@example.com:o/r.git"}, want: "git remote"},
		{name: "bash scp address dropped", tool: toolBash, in: ToolInput{Command: "scp user@example.com:/x ./y"}, want: "scp ./y"},
		{name: "bash expansion then path-like word", tool: toolBash, in: ToolInput{Command: "go test $PKG ./..."}, want: "go test ./..."},
		{name: "bash only flags", tool: toolBash, in: ToolInput{Command: "--version"}, want: ""},
		{name: "bash only quoted", tool: toolBash, in: ToolInput{Command: `"whole thing"`}, want: ""},
		{name: "bash empty command", tool: toolBash, in: ToolInput{Command: ""}, want: ""},
		{name: "bash whitespace only", tool: toolBash, in: ToolInput{Command: "  \n "}, want: ""},
		{name: "bash lowercase name", tool: "bash", in: ToolInput{Command: "ls -la"}, want: "ls"},
		{name: "shell", tool: "shell", in: ToolInput{Command: "pwd"}, want: "pwd"},
		{name: "exec_command", tool: "exec_command", in: ToolInput{Command: "cargo build --release"}, want: "cargo build"},
		{name: "bash path as second word only", tool: toolBash, in: ToolInput{Command: "cat ./notes.md"}, want: "cat ./notes.md"},
		{name: "run_terminal_cmd", tool: "run_terminal_cmd", in: ToolInput{Command: "yarn test"}, want: "yarn test"},
		{name: "run_shell_command", tool: "run_shell_command", in: ToolInput{Command: "pytest tests"}, want: "pytest tests"},
		{name: "terminal", tool: "terminal", in: ToolInput{Command: "ls"}, want: "ls"},
		{name: "execute", tool: "execute", in: ToolInput{Command: "ls"}, want: "ls"},
		{name: "exec", tool: "exec", in: ToolInput{Command: "ls"}, want: "ls"},
		{name: "shell tool ignores non-command fields", tool: toolBash, in: ToolInput{Description: "run it", FilePath: "x"}, want: ""},

		// File tools.
		{name: "read file_path", tool: toolRead, in: ToolInput{FilePath: "src/main.go"}, want: "src/main.go"},
		{name: "read filePath", tool: toolRead, in: ToolInput{FilePathCamel: "src/main.go"}, want: "src/main.go"},
		{name: "read path", tool: toolRead, in: ToolInput{Path: "src/main.go"}, want: "src/main.go"},
		{name: "read prefers file_path", tool: toolRead, in: ToolInput{FilePath: "a", FilePathCamel: "b", Path: "c"}, want: "a"},
		{name: "read falls back to notebook_path", tool: toolRead, in: ToolInput{NotebookPath: "nb.ipynb"}, want: "nb.ipynb"},
		{name: "read no path", tool: toolRead, in: ToolInput{Description: "d"}, want: ""},
		{name: "edit", tool: "Edit", in: ToolInput{FilePath: "f"}, want: "f"},
		{name: "write", tool: "Write", in: ToolInput{FilePath: "f"}, want: "f"},
		{name: "multiedit", tool: "MultiEdit", in: ToolInput{FilePath: "f"}, want: "f"},
		{name: "notebookedit", tool: "NotebookEdit", in: ToolInput{NotebookPath: "nb.ipynb"}, want: "nb.ipynb"},
		{name: "read_file", tool: "read_file", in: ToolInput{Path: "f"}, want: "f"},
		{name: "write_file", tool: "write_file", in: ToolInput{Path: "f"}, want: "f"},
		{name: "edit_file", tool: "edit_file", in: ToolInput{Path: "f"}, want: "f"},
		{name: "apply_patch with path", tool: "apply_patch", in: ToolInput{Path: "f"}, want: "f"},
		{name: "apply_patch without path", tool: "apply_patch", in: ToolInput{}, want: ""},
		{name: "file tool case-insensitive", tool: "READ", in: ToolInput{FilePath: "f"}, want: "f"},

		// WebFetch.
		{name: "webfetch host only", tool: toolWebFetch, in: ToolInput{URL: "https://example.com:8443/docs/page?q=1#x"}, want: "example.com"},
		{name: "webfetch plain host", tool: toolWebFetch, in: ToolInput{URL: "http://example.com"}, want: "example.com"},
		{name: "webfetch subdomain", tool: toolWebFetch, in: ToolInput{URL: "https://docs.example.com/a"}, want: "docs.example.com"},
		{name: "webfetch malformed url", tool: toolWebFetch, in: ToolInput{URL: "://missing-scheme"}, want: ""},
		{name: "webfetch bad escape", tool: toolWebFetch, in: ToolInput{URL: "https://example.com/%zz"}, want: ""},
		{name: "webfetch no host", tool: toolWebFetch, in: ToolInput{URL: "just words"}, want: ""},
		{name: "webfetch empty", tool: toolWebFetch, in: ToolInput{}, want: ""},
		{name: "web_fetch", tool: "web_fetch", in: ToolInput{URL: "https://example.com/x"}, want: "example.com"},
		{name: "fetch", tool: "fetch", in: ToolInput{URL: "https://example.com/x"}, want: "example.com"},

		// Name-only tools.
		{name: "websearch hides query", tool: "WebSearch", in: ToolInput{Prompt: "secret", Pattern: "p"}, want: ""},
		{name: "grep hides pattern", tool: "Grep", in: ToolInput{Pattern: "TODO", Path: "src"}, want: ""},
		{name: "glob hides pattern", tool: "Glob", in: ToolInput{Pattern: "**/*.go"}, want: ""},
		{name: "ls", tool: "LS", in: ToolInput{Path: "src"}, want: ""},
		{name: "search", tool: "search", in: ToolInput{Pattern: "x"}, want: ""},
		{name: "list_dir", tool: "list_dir", in: ToolInput{Path: "x"}, want: ""},

		// Skill.
		{name: "skill", tool: "Skill", in: ToolInput{Skill: "commit"}, want: "commit"},
		{name: "skill lowercase", tool: "skill", in: ToolInput{Skill: "commit"}, want: "commit"},
		{name: "skill empty", tool: "Skill", in: ToolInput{}, want: ""},
		{name: "activate_skill", tool: "activate_skill", in: ToolInput{Skill: "artifact-design"}, want: "artifact-design"},

		// Task / Agent.
		{name: "task without model", tool: toolTask, in: ToolInput{SubagentType: "general-purpose"}, want: "general-purpose"},
		{name: "task with model", tool: toolTask, in: ToolInput{SubagentType: "Explore", Model: "haiku"}, want: "Explore (haiku)"},
		{name: "task with model but no subagent", tool: toolTask, in: ToolInput{Model: "haiku"}, want: ""},
		{name: "task empty", tool: toolTask, in: ToolInput{}, want: ""},
		{name: "agent", tool: "Agent", in: ToolInput{SubagentType: "reviewer"}, want: "reviewer"},
		{name: "spawn_agent", tool: "spawn_agent", in: ToolInput{SubagentType: "worker", Model: "sonnet"}, want: "worker (sonnet)"},
		{name: "delegate_to_agent", tool: "delegate_to_agent", in: ToolInput{SubagentType: "codebase_investigator"}, want: "codebase_investigator"},

		// Unknown.
		{name: "unknown tool", tool: "TodoWrite", in: ToolInput{Description: "d", Command: "c", FilePath: "f"}, want: ""},
		{name: "empty tool name", tool: "", in: ToolInput{Command: "ls"}, want: ""},
		{name: "empty everything", tool: toolBash, in: ToolInput{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ToolDetail(tt.tool, tt.in); got != tt.want {
				t.Errorf("ToolDetail(%q, %+v) = %q, want %q", tt.tool, tt.in, got, tt.want)
			}
		})
	}
}

func TestToolInput_AnyFilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   ToolInput
		want string
	}{
		{name: "none", in: ToolInput{}, want: ""},
		{name: "file_path", in: ToolInput{FilePath: "a"}, want: "a"},
		{name: "filePath", in: ToolInput{FilePathCamel: "b"}, want: "b"},
		{name: "path", in: ToolInput{Path: "c"}, want: "c"},
		{name: "file_path beats filePath", in: ToolInput{FilePath: "a", FilePathCamel: "b"}, want: "a"},
		{name: "filePath beats path", in: ToolInput{FilePathCamel: "b", Path: "c"}, want: "b"},
		{name: "notebook_path is not a file path", in: ToolInput{NotebookPath: "nb"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.in.AnyFilePath(); got != tt.want {
				t.Errorf("AnyFilePath(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestToolInputFromMap pins the exact-key, strings-only pick: every known key
// lands in its field, a non-string under a known key and every unknown key
// (a multi-megabyte `content` included) are ignored, and a nil map is fine.
func TestToolInputFromMap(t *testing.T) {
	t.Parallel()

	full := map[string]any{
		"file_path": "a", "filePath": "b", "path": "c", "notebook_path": "nb",
		"description": "d", "command": "ls", "pattern": "*.go", "skill": "s",
		"subagent_type": "Explore", "model": "haiku", "url": "https://x", "prompt": "p",
	}
	want := ToolInput{
		FilePath: "a", FilePathCamel: "b", Path: "c", NotebookPath: "nb",
		Description: "d", Command: "ls", Pattern: "*.go", Skill: "s",
		SubagentType: "Explore", Model: "haiku", URL: "https://x", Prompt: "p",
	}
	if got := ToolInputFromMap(full); got != want {
		t.Errorf("ToolInputFromMap(full) = %+v, want %+v", got, want)
	}

	mixed := map[string]any{
		"command":    42,                         // non-string under a known key: ignored
		"file_path":  "f",                        // the rest still populate
		"Command":    "not matched: exact keys",  // case is not folded
		"content":    strings.Repeat("x", 1<<20), // unknown key, never read
		"old_string": map[string]any{"k": "v"},   // unknown key of another type
	}
	if got, want := ToolInputFromMap(mixed), (ToolInput{FilePath: "f"}); got != want {
		t.Errorf("ToolInputFromMap(mixed) = %+v, want %+v", got, want)
	}
	if got := ToolInputFromMap(nil); got != (ToolInput{}) {
		t.Errorf("ToolInputFromMap(nil) = %+v, want zero", got)
	}
}

// TestToolInputFromJSON pins the best-effort decode: a non-string under a
// known key leaves that field empty while the others populate, invalid JSON
// yields a zero value, and encoding/json's case folding is inherited.
func TestToolInputFromJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want ToolInput
	}{
		{name: "known keys", raw: `{"command":"ls","file_path":"f","subagent_type":"Explore","model":"haiku"}`, want: ToolInput{Command: "ls", FilePath: "f", SubagentType: "Explore", Model: "haiku"}},
		{name: "non-string under a known key is skipped, rest populate", raw: `{"command":42,"file_path":"f","pattern":"p"}`, want: ToolInput{FilePath: "f", Pattern: "p"}},
		{name: "unknown keys ignored", raw: `{"content":"big","file_path":"f"}`, want: ToolInput{FilePath: "f"}},
		{name: "case folded (encoding/json)", raw: `{"Command":"ls"}`, want: ToolInput{Command: "ls"}},
		{name: "invalid json", raw: `{"command":`, want: ToolInput{}},
		{name: "empty", raw: ``, want: ToolInput{}},
		{name: "not an object", raw: `"ls"`, want: ToolInput{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ToolInputFromJSON([]byte(tt.raw)); got != tt.want {
				t.Errorf("ToolInputFromJSON(%s) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}
