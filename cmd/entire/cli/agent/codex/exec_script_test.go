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
