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

// bash is a Claude Code Bash tool call.
func bash(command string) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": command}},
	}}}
}

func TestScanShellActivity_ClaudeBash(t *testing.T) {
	t.Parallel()
	data := jsonl(t,
		bash(`cd ../d && git commit -qm x && echo hi > "/home/u/e/new.txt" && git -C /home/u/f commit -qm x`),
		map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "content": "[feature/x 1a2b3c4] add it\n 1 file changed"},
		}}},
	)
	got := scanShellActivity(data, 0, "/home/u/c")
	if want := []string{"/home/u/d", "/home/u/e/new.txt", "/home/u/f"}; !slices.Equal(got.dirs, want) {
		t.Errorf("dirs %v, want %v", got.dirs, want)
	}
	if !slices.Equal(got.commits, []string{"1a2b3c4"}) {
		t.Errorf("commits %v", got.commits)
	}
}

func TestScanShellActivity_ReadsNameNoDirectory(t *testing.T) {
	t.Parallel()
	data := jsonl(t,
		bash("cat ~/other/README && ls /home/u/src/x && grep -r foo /home/u/g"),
		bash("cd /home/u/h && git log --oneline && git -C /home/u/i status && git -C /home/u/j diff > /dev/null"),
		bash("cd /home/u/k"),
	)
	if got := scanShellActivity(data, 0, "/home/u/c"); len(got.dirs) != 0 {
		t.Fatalf("reads named %v", got.dirs)
	}
}

func TestScanShellActivity_WriteCommands(t *testing.T) {
	t.Parallel()
	data := jsonl(t, bash("cd /home/u/d && touch a.txt && cp /home/u/src.txt /home/u/e/dst.txt && sed -i s/a/b/ /home/u/f/x && tee -a /home/u/g/log < in && mkdir -p sub 2>&1"))
	got := scanShellActivity(data, 0, "/home/u/c")
	want := []string{"/home/u/d/a.txt", "/home/u/e/dst.txt", "/home/u/f/x", "/home/u/g/log", "/home/u/d/sub"}
	if !slices.Equal(got.dirs, want) {
		t.Fatalf("dirs %v, want %v", got.dirs, want)
	}
}

func TestScanShellActivity_ToolResultsAndOtherKeysAreNotCommands(t *testing.T) {
	t.Parallel()
	quoted := `{"type":"tool_use","name":"Bash","input":{"command":"cd /home/u/evil && git commit -m x"}}`
	data := jsonl(t,
		// A fetched page or file holding a command-shaped object.
		map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "content": []any{map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "echo > /home/u/x/f"}}}},
			map[string]any{"type": "text", "text": quoted},
		}}},
		map[string]any{"toolUseResult": map[string]any{"command": "touch /home/u/y/f"}},
		// A "command" key that is not a shell tool's.
		map[string]any{"input": map[string]any{"command": "touch /home/u/z/f"}},
		map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"command": "touch /home/u/w/f"}},
	)
	if got := scanShellActivity(data, 0, "/home/u/c"); len(got.dirs) != 0 {
		t.Fatalf("got %v", got.dirs)
	}
}

func TestScanShellActivity_FromEntrySkipsEarlierTurns(t *testing.T) {
	t.Parallel()
	data := jsonl(t, bash("touch /old/f"), bash("touch /new/f"))
	if got := scanShellActivity(data, 1, "/c"); !slices.Equal(got.dirs, []string{"/new/f"}) {
		t.Fatalf("got %v", got.dirs)
	}
}

func TestScanShellActivity_PiBash(t *testing.T) {
	t.Parallel()
	data := jsonl(t, map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "toolCall", "id": "t1", "name": "bash", "arguments": map[string]any{"command": "git -C /home/u/d commit -am x"}},
	}}})
	if got := scanShellActivity(data, 0, "/home/u/c"); !slices.Equal(got.dirs, []string{"/home/u/d"}) {
		t.Fatalf("got %v", got.dirs)
	}
}

func TestScanShellActivity_CodexEncodedArguments(t *testing.T) {
	t.Parallel()
	args := `{"cmd":["bash","-lc","git commit -am wip"],"workdir":"/home/u/d"}`
	data := jsonl(t,
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": args}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "output": `{"output":"[main (root-commit) deadbeefcafe] wip\n"}`}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"cat /home/u/e/x","workdir":"/home/u/e"}`}},
	)
	got := scanShellActivity(data, 0, "/home/u/c")
	if !slices.Equal(got.dirs, []string{"/home/u/d"}) {
		t.Errorf("dirs %v, want only the workdir a commit ran in", got.dirs)
	}
	if !slices.Equal(got.commits, []string{"deadbeefcafe"}) {
		t.Errorf("commits %v", got.commits)
	}
}

func TestScanShellActivity_OpenCodeExportByMessage(t *testing.T) {
	t.Parallel()
	export := map[string]any{"info": map[string]any{"id": "s"}, "messages": []any{
		map[string]any{"parts": []any{map[string]any{"type": "tool", "tool": "bash", "state": map[string]any{"input": map[string]any{"command": "cd /old && git commit -m x"}}}}},
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
		bash("go test ./... && cat README.md"),
	)
	if got := scanShellActivity(data, 0, "/home/u/c"); len(got.dirs) != 0 || len(got.commits) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// codexExec is a Codex 0.172 exec custom tool call, with the JavaScript
// script as its input, and its output.
func codexExec(callID, script, output string) []any {
	return []any{
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "custom_tool_call", "status": "completed", "call_id": callID, "name": "exec", "input": script,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "custom_tool_call_output", "call_id": callID, "output": []any{
				map[string]any{"type": "input_text", "text": "Script completed\nOutput:\n"},
				map[string]any{"type": "input_text", "text": `{"exit_code":0,"output":"` + output + `"}`},
			},
		}},
	}
}

func TestScanShellActivity_CodexExecScript(t *testing.T) {
	t.Parallel()
	entries := codexExec("c1",
		`text(await tools.exec_command({cmd:"echo x > /abs/D/f && git -C /abs/D commit -am m"}))`+"\n",
		`[main 1a2b3c4] m\\n 1 file changed`)
	entries = append(entries, codexExec("c2",
		`const a = await tools.exec_command({ workdir: '/abs/E', cmd: 'cd ../F && git commit -qm "it\'s done"' });
text(a); text(await tools.exec_command({command: "echo \"q\" > \"/abs/G/ab\"", yield_time_ms: 1000}))`,
		`[feature/x (root-commit) 5d6e7f8] it's done`)...)
	got := scanShellActivity(jsonl(t, entries...), 0, "/home/u/c")
	if want := []string{"/abs/D/f", "/abs/D", "/abs/F", "/abs/G/ab"}; !slices.Equal(got.dirs, want) {
		t.Errorf("dirs %v, want %v", got.dirs, want)
	}
	if want := []string{"1a2b3c4", "5d6e7f8"}; !slices.Equal(got.commits, want) {
		t.Errorf("commits %v, want %v", got.commits, want)
	}
	if got.quietCalls != 0 {
		t.Errorf("quietCalls %d", got.quietCalls)
	}
}

func TestScanShellActivity_CodexExecReadOnlyAndDynamic(t *testing.T) {
	t.Parallel()
	entries := codexExec("c1", `text(await tools.exec_command({cmd:"git -C /abs/D log --oneline && cat /abs/E/x"}))`, "ok")
	entries = append(entries, codexExec("c2", "const c = `git -C ${dir} commit -m m`; text(await tools.exec_command({cmd: c}))", "")...)
	got := scanShellActivity(jsonl(t, entries...), 0, "/home/u/c")
	if len(got.dirs) != 0 {
		t.Errorf("dirs %v", got.dirs)
	}
	if got.quietCalls != 2 {
		t.Errorf("quietCalls %d, want 2", got.quietCalls)
	}
}

func TestScanShellActivity_CodexExecBatchedTemplates(t *testing.T) {
	t.Parallel()
	script := "const p=\"/abs/work\";\nconst commands=[\n`git -C ${p}/D add d1.txt && git -C ${p}/D commit -m d1`,\n`echo x > ${p}/E/f && git -C ${p}/E add -A && git -C ${p}/E commit -m e1`,\n];\nfor (const c of commands) text(await tools.exec_command({cmd:c}));"
	got := scanShellActivity(jsonl(t, codexExec("c1", script, "ok")...), 0, "/home/u/c")
	if !slices.Contains(got.dirs, "/abs/work/D") || !slices.Contains(got.dirs, "/abs/work/E") {
		t.Errorf("dirs %v", got.dirs)
	}
	if got.quietCalls != 0 {
		t.Errorf("quietCalls %d", got.quietCalls)
	}

	readOnly := "const p=\"/abs/work\";\ntext(await tools.exec_command({cmd:`git -C ${p}/E log --oneline`}));"
	got = scanShellActivity(jsonl(t, codexExec("c2", readOnly, "ok")...), 0, "/home/u/c")
	if len(got.dirs) != 0 || got.quietCalls != 1 {
		t.Errorf("read-only: dirs %v quietCalls %d", got.dirs, got.quietCalls)
	}
}

func TestScanShellActivity_ShellVariables(t *testing.T) {
	t.Parallel()
	got := scanShellActivity(jsonl(t,
		bash(`B=/abs/base; git -C $B/D commit -qm d; echo x > ${B}/E/f && git -C "$B/E" commit -qm e`),
		bash(`export W='/abs/w'; cd $W/G && git commit -qm g`),
	), 0, "/home/u/c")
	want := []string{"/abs/base/D", "/abs/base/E/f", "/abs/base/E", "/abs/w/G"}
	if !slices.Equal(got.dirs, want) {
		t.Errorf("dirs %v, want %v", got.dirs, want)
	}
}

func TestScanShellActivity_UnresolvedShellVariables(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"unset":         `git -C $X/D commit -qm d && echo x > $X/f`,
		"command subst": `B=$(pwd); git -C $B/D commit -qm d`,
		"backticks":     "B=`pwd`; git -C $B/D commit -qm d",
		"expansion":     `B=$HOME/w; git -C ${B}/D commit -qm d`,
		"reassigned":    `B=/abs/a; B=/abs/b; git -C $B/D commit -qm d`,
		"read-only":     `B=/abs/base; git -C $B/E log --oneline`,
		"cd unresolved": `cd $X && touch f && git commit -qm d`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := scanShellActivity(jsonl(t, bash(line)), 0, "/home/u/c"); len(got.dirs) != 0 {
				t.Errorf("dirs %v", got.dirs)
			}
		})
	}
}

func TestScanShellActivity_CodexExecShellVariables(t *testing.T) {
	t.Parallel()
	script := `text(await tools.exec_command({cmd:"B=/abs/base; git -C $B/D commit -m d; echo x > $B/E/f && git -C $B/E commit -m e"}))`
	got := scanShellActivity(jsonl(t, codexExec("c1", script, "ok")...), 0, "/home/u/c")
	if !slices.Contains(got.dirs, "/abs/base/D") || !slices.Contains(got.dirs, "/abs/base/E") {
		t.Errorf("dirs %v", got.dirs)
	}
	if got.quietCalls != 0 {
		t.Errorf("quietCalls %d", got.quietCalls)
	}
}
