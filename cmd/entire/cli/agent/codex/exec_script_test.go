package codex

import "testing"

func execCall(script string) map[string]any {
	return map[string]any{"type": "custom_tool_call", "name": "exec", "input": script}
}

func TestExecScriptShellCalls_Literals(t *testing.T) {
	t.Parallel()
	got, ok := ExecScriptShellCalls(execCall(`tools.exec_command("ls \x2fa\u{62}c") ; tools.exec_command({cmd: ` + "`echo hi > /t`" + `, workdir: "/w", other: {cmd: "no"}})`))
	if !ok || len(got) != 2 || got[0]["cmd"] != "ls /abc" || got[1]["cmd"] != "echo hi > /t" || got[1]["workdir"] != "/w" {
		t.Fatalf("calls %v ok %v", got, ok)
	}
}

func TestExecScriptShellCalls_NotExecCall(t *testing.T) {
	t.Parallel()
	obj := execCall(`tools.exec_command("ls")`)
	obj["name"] = "apply_patch"
	if got, ok := ExecScriptShellCalls(obj); ok || got != nil {
		t.Fatalf("calls %v ok %v", got, ok)
	}
}

// A literal that is only part of a concatenation is not the command: reading
// it would take a truncated path such as /home/u/ as a write target.
func TestExecScriptShellCalls_SkipsConcatenation(t *testing.T) {
	t.Parallel()
	for _, script := range []string{
		`tools.exec_command({cmd: "cp x /home/u/" + name})`,
		`tools.exec_command({cmd: name + "cp x /home/u/"})`,
		`tools.exec_command("cp x /home/u/" + name)`,
		`tools.exec_command(name + "cp x /home/u/")`,
		`tools.exec_command("a" + "b")`,
	} {
		got, ok := ExecScriptShellCalls(execCall(script))
		if !ok || len(got) != 0 {
			t.Errorf("%s: calls %v", script, got)
		}
	}
	got, _ := ExecScriptShellCalls(execCall(`tools.exec_command({cmd: "git commit -m m" , workdir: "/w" + d})`))
	if len(got) != 1 || got[0]["cmd"] != "git commit -m m" || got[0]["workdir"] != nil {
		t.Fatalf("calls %v", got)
	}
}

const batchedScript = "const p=\"/abs/work\";\nconst commands=[\n`git -C ${p}/D add d1.txt && git -C ${p}/D commit -m d1`,\n`echo x > ${p}/E/f && git -C ${p}/E add -A && git -C ${p}/E commit -m e1`,\n];\nfor (const c of commands) text(await tools.exec_command({cmd:c}));"

func TestExecScriptShellCalls_BatchedTemplates(t *testing.T) {
	t.Parallel()
	got, _ := ExecScriptShellCalls(execCall(batchedScript))
	if len(got) != 2 || got[0]["cmd"] != "git -C /abs/work/D add d1.txt && git -C /abs/work/D commit -m d1" ||
		got[1]["cmd"] != "echo x > /abs/work/E/f && git -C /abs/work/E add -A && git -C /abs/work/E commit -m e1" {
		t.Fatalf("calls %v", got)
	}
}

// Without an exec_command call the script runs no shell command.
func TestExecScriptShellCalls_BatchedNeedsExecCommand(t *testing.T) {
	t.Parallel()
	if got, _ := ExecScriptShellCalls(execCall(`const c = ["git -C /d commit -m m"]; text(c)`)); len(got) != 0 {
		t.Fatalf("calls %v", got)
	}
}

func TestExecScriptShellCalls_BatchedUnresolved(t *testing.T) {
	t.Parallel()
	for _, script := range []string{
		"const cs=[`git -C ${x}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
		"let p=\"/a\"; p=\"/b\"; const cs=[`git -C ${p}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
		"let p=\"/a\"; p += \"/b\"; const cs=[`git -C ${p}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
		"const p=\"/a\"; const p=\"/b\"; const cs=[`git -C ${p}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
		"const p=dir(); const cs=[`git -C ${p}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
		"const p=\"/a\" + d; const cs=[`git -C ${p}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
		"const p=`/a/${d}`; const cs=[`git -C ${p}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
		"const p=\"/a\"; const cs=[`git -C ${p + q}/E commit -m e`]; for (const c of cs) tools.exec_command({cmd:c})",
	} {
		if got, _ := ExecScriptShellCalls(execCall(script)); len(got) != 0 {
			t.Errorf("%s: calls %v", script, got)
		}
	}
}
