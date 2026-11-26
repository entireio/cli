package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func jsonl(t *testing.T, entries ...any) []byte {
	t.Helper()
	var lines []string
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestScanShellActivity_ClaudeBash(t *testing.T) {
	t.Parallel()
	data := jsonl(t,
		map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{
				"command": `cd ../d && echo hi > "/home/u/e/new.txt" && git -C /home/u/f commit -qm x`,
			}},
		}}},
		map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "content": "[feature/x 1a2b3c4] add it\n 1 file changed"},
		}}},
	)
	got := scanShellActivity(data, 0, "/home/u/c")
	for _, want := range []string{"/home/u/d", "/home/u/e/new.txt", "/home/u/f"} {
		if !slices.Contains(got.dirs, want) {
			t.Errorf("dirs %v lack %s", got.dirs, want)
		}
	}
	if !slices.Equal(got.commits, []string{"1a2b3c4"}) {
		t.Errorf("commits %v", got.commits)
	}
}

func TestScanShellActivity_FromEntrySkipsEarlierTurns(t *testing.T) {
	t.Parallel()
	data := jsonl(t,
		map[string]any{"input": map[string]any{"command": "cd /old"}},
		map[string]any{"input": map[string]any{"command": "cd /new"}},
	)
	if got := scanShellActivity(data, 1, "/c"); !slices.Equal(got.dirs, []string{"/new"}) {
		t.Fatalf("got %v", got.dirs)
	}
}

func TestScanShellActivity_CodexEncodedArguments(t *testing.T) {
	t.Parallel()
	args := `{"cmd":["bash","-lc","git commit -am wip"],"workdir":"/home/u/d"}`
	data := jsonl(t,
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": args}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "output": `{"output":"[main (root-commit) deadbeefcafe] wip\n"}`}},
	)
	got := scanShellActivity(data, 0, "/home/u/c")
	if !slices.Contains(got.dirs, "/home/u/d") {
		t.Errorf("dirs %v lack the workdir", got.dirs)
	}
	if !slices.Equal(got.commits, []string{"deadbeefcafe"}) {
		t.Errorf("commits %v", got.commits)
	}
}

func TestScanShellActivity_OpenCodeExportByMessage(t *testing.T) {
	t.Parallel()
	export := map[string]any{"info": map[string]any{"id": "s"}, "messages": []any{
		map[string]any{"parts": []any{map[string]any{"type": "tool", "tool": "bash", "state": map[string]any{"input": map[string]any{"command": "cd /old"}}}}},
		map[string]any{"parts": []any{map[string]any{"type": "tool", "tool": "bash", "state": map[string]any{
			"input": map[string]any{"command": "cd /home/u/d && git commit -m x"}, "output": "[main abcdef0] x",
		}}}},
	}}
	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got := scanShellActivity(data, 1, "/home/u/c")
	if !slices.Equal(got.dirs, []string{"/home/u/d"}) || !slices.Equal(got.commits, []string{"abcdef0"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestScanShellActivity_IgnoresProseAndRelativeArguments(t *testing.T) {
	t.Parallel()
	data := jsonl(t,
		map[string]any{"message": map[string]any{"content": "I will look at /home/u/other later"}},
		map[string]any{"input": map[string]any{"command": "go test ./... && cat README.md"}},
	)
	if got := scanShellActivity(data, 0, "/home/u/c"); len(got.dirs) != 0 || len(got.commits) != 0 {
		t.Fatalf("got %+v", got)
	}
}
