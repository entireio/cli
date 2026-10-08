package opencode

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pluginTaskDriver loads the plugin under Node and replays a scenario of
// tool.execute.before/after calls and events, in order.
const pluginTaskDriver = `
import { pathToFileURL } from "node:url"
import { readFileSync } from "node:fs"

const { EntirePlugin } = await import(pathToFileURL(process.argv[2]).href)
const handlers = await EntirePlugin({ directory: process.cwd() })
for (const step of JSON.parse(readFileSync(process.argv[3], "utf8"))) {
  if (step.before) await handlers["tool.execute.before"](step.before, { args: {} })
  if (step.after) await handlers["tool.execute.after"](step.after.input, step.after.output ?? undefined)
  if (step.event) await handlers.event({ event: step.event })
}
`

type taskHook struct {
	Hook      string
	ToolUseID string
	SessionID string
	ChildID   string
	StartedAt int64
}

func (h taskHook) key() string { return h.Hook + ":" + h.ToolUseID }

// runPluginTaskScenario replays steps against the plugin and returns the
// subagent hooks it fired, in order. A fake `entire` on PATH records each
// hook name and its stdin payload.
func runPluginTaskScenario(t *testing.T, steps []map[string]any) []taskHook {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	pluginPath := filepath.Join(dir, "entire.ts")
	logPath := filepath.Join(dir, "hooks.log")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		pluginPath:                       renderPlugin(),
		filepath.Join(dir, "driver.mjs"): pluginTaskDriver,
		filepath.Join(dir, "steps.json"): string(stepsJSON),
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nprintf '%s\\t' \"$3\" >> " + logPath + "\ncat >> " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "entire"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "node", "--experimental-strip-types",
		filepath.Join(dir, "driver.mjs"), pluginPath, filepath.Join(dir, "steps.json"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}

	f, err := os.Open(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var hooks []taskHook
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, body, ok := strings.Cut(sc.Text(), "\t")
		if !ok || !strings.HasPrefix(name, "subagent-") {
			continue
		}
		var p struct {
			SessionID  string `json:"session_id"`
			ToolUseID  string `json:"tool_use_id"`
			SubagentID string `json:"subagent_id"`
			StartedAt  int64  `json:"started_at"`
		}
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("bad %s payload %q: %v", name, body, err)
		}
		hooks = append(hooks, taskHook{name, p.ToolUseID, p.SessionID, p.SubagentID, p.StartedAt})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return hooks
}

func before(sessionID, hookKey string) map[string]any {
	return map[string]any{"before": map[string]any{"tool": "task", "sessionID": sessionID, "callID": hookKey}}
}

// after fires the top-level session's tool.execute.after; an empty childID
// passes no output, as a failed command subtask does.
func after(hookKey, childID string, background bool) map[string]any {
	step := map[string]any{"input": map[string]any{"tool": "task", "sessionID": "ses_parent", "callID": hookKey, "args": map[string]any{}}}
	if childID != "" {
		step["output"] = map[string]any{"metadata": map[string]any{"sessionId": childID, "background": background}}
	}
	return map[string]any{"after": step}
}

func taskPart(sessionID, partID, callID, childID, status string) map[string]any {
	state := map[string]any{"status": status, "input": map[string]any{}}
	if childID != "" {
		state["metadata"] = map[string]any{"sessionId": childID}
	}
	if status == "error" {
		state["error"] = "Tool execution aborted"
	}
	return map[string]any{"event": map[string]any{
		"type": "message.part.updated",
		"properties": map[string]any{"part": map[string]any{
			"id": partID, "messageID": "msg_" + partID, "sessionID": sessionID,
			"type": "tool", "tool": "task", "callID": callID, "state": state,
		}},
	}}
}

// childStatus reports the child session's (ses_child) session.status.
func childStatus(status string) map[string]any {
	return map[string]any{"event": map[string]any{
		"type":       "session.status",
		"properties": map[string]any{"sessionID": "ses_child", "status": map[string]any{"type": status}},
	}}
}

// backgroundResult is the synthetic part OpenCode injects into the launching
// session when a background job ends.
func backgroundResult(state string) map[string]any {
	return map[string]any{"event": map[string]any{
		"type": "message.part.updated",
		"properties": map[string]any{"part": map[string]any{
			"id": "prt_result", "messageID": "msg_result", "sessionID": "ses_parent", "type": "text", "synthetic": true,
			"text": `<task id="ses_child" state="` + state + `">` + "\n<summary>Background task " + state + "</summary>",
		}},
	}}
}

func sessionAborted(sessionID string) map[string]any {
	return map[string]any{"event": map[string]any{
		"type":       "session.error",
		"properties": map[string]any{"sessionID": sessionID, "error": map[string]any{"name": "MessageAbortedError"}},
	}}
}

// TestPlugin_TaskStartAndStopPair pins that every subagent-start is matched by
// exactly one subagent-stop under the same tool_use_id, across OpenCode's call
// paths: the model's task tool (hooks keyed by callID) and a command subtask
// (hooks keyed by the task part's id), completed, failed, or aborted.
func TestPlugin_TaskStartAndStopPair(t *testing.T) {
	t.Parallel()
	const parent, child = "ses_parent", "ses_child"

	tests := []struct {
		name  string
		steps []map[string]any
		want  []string
	}{
		{
			name: "task tool completes",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, false),
			},
			want: []string{"subagent-start:c1", "subagent-stop:c1"},
		},
		{
			name: "command subtask completes",
			steps: []map[string]any{
				before(parent, "p1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("p1", child, false),
				taskPart(parent, "p1", "c1", child, "completed"),
			},
			want: []string{"subagent-start:c1", "subagent-stop:c1"},
		},
		{
			name: "task tool aborted skips after",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				taskPart(parent, "p1", "c1", child, "error"),
			},
			want: []string{"subagent-start:c1", "subagent-stop:c1"},
		},
		{
			name: "command subtask interrupted skips after",
			steps: []map[string]any{
				before(parent, "p1"),
				taskPart(parent, "p1", "c1", child, "running"),
				taskPart(parent, "p1", "c1", child, "error"),
			},
			want: []string{"subagent-start:c1", "subagent-stop:c1"},
		},
		{
			name: "command subtask fails: after has no output, then error part",
			steps: []map[string]any{
				before(parent, "p1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("p1", "", false),
				taskPart(parent, "p1", "c1", child, "error"),
			},
			want: []string{"subagent-start:c1", "subagent-stop:c1"},
		},
		{
			name: "error part before after stops once",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				taskPart(parent, "p1", "c1", child, "error"),
				after("c1", child, false),
			},
			want: []string{"subagent-start:c1", "subagent-stop:c1"},
		},
		{
			name: "after before the running event opens no task",
			steps: []map[string]any{
				before(parent, "c1"),
				after("c1", child, false),
				taskPart(parent, "p1", "c1", child, "running"),
			},
			want: []string{"subagent-stop:c1"},
		},
		{
			name: "subtask after before the running event opens no task",
			steps: []map[string]any{
				before(parent, "p1"),
				after("p1", child, false),
				taskPart(parent, "p1", "c1", child, "running"),
			},
			want: []string{"subagent-stop:p1"},
		},
		{
			name: "task failing before its child binds fires nothing",
			steps: []map[string]any{
				before(parent, "p1"),
				taskPart(parent, "p1", "c1", "", "error"),
			},
			want: nil,
		},
		{
			name: "resumed child: only the failed call stops on error",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, false),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				taskPart(parent, "p2", "c2", child, "error"),
			},
			want: []string{"subagent-start:c1", "subagent-stop:c1", "subagent-start:c2", "subagent-stop:c2"},
		},
		{
			name: "first run's idle ends only the first call queued on a background child",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				childStatus("busy"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				after("c2", child, true),
				childStatus("idle"),
				// an errored run reports idle twice; the second must not end c2
				childStatus("idle"),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-stop:c1"},
		},
		{
			name: "calls queued on one background child end at their own runs' idle",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				childStatus("busy"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				after("c2", child, true),
				childStatus("idle"),
				// an errored run reports idle twice; the second must not end c2
				childStatus("idle"),
				childStatus("busy"),
				childStatus("idle"),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-stop:c1", "subagent-stop:c2"},
		},
		{
			name: "failed job drops the queued run: its result ends the joined call",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				childStatus("busy"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				after("c2", child, true),
				childStatus("idle"),
				backgroundResult("error"),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-stop:c1", "subagent-stop:c2"},
		},
		{
			name: "aborting the parent ends the calls queued on its background child",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				childStatus("busy"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				after("c2", child, true),
				childStatus("idle"),
				// no idle follows for c2: its queued run was dropped
				sessionAborted(parent),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-stop:c1", "subagent-stop:c2"},
		},
		{
			name: "aborting the parent mid joined run ends the joined call at the child's idle",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				childStatus("busy"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				after("c2", child, true),
				childStatus("idle"),
				childStatus("busy"),
				// the job is cancelled but the executing joined run is not
				sessionAborted(parent),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-stop:c1"},
		},
		{
			name: "aborting the parent mid joined run: the child's idle then ends the joined call",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				childStatus("busy"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				after("c2", child, true),
				before(parent, "c3"),
				taskPart(parent, "p3", "c3", child, "running"),
				after("c3", child, true),
				childStatus("idle"),
				childStatus("busy"),
				sessionAborted(parent),
				// c3's queued run was dropped with the job, so this idle ends both
				childStatus("idle"),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-start:c3", "subagent-stop:c1", "subagent-stop:c2", "subagent-stop:c3"},
		},
		{
			name: "completed job's result after the last idle stops nothing more",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				childStatus("busy"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				after("c2", child, true),
				childStatus("idle"),
				childStatus("busy"),
				childStatus("idle"),
				backgroundResult("completed"),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-stop:c1", "subagent-stop:c2"},
		},
		{
			name: "background call held while a foreground call on its child fails",
			steps: []map[string]any{
				before(parent, "c1"),
				taskPart(parent, "p1", "c1", child, "running"),
				after("c1", child, true),
				taskPart(parent, "p1", "c1", child, "error"),
				before(parent, "c2"),
				taskPart(parent, "p2", "c2", child, "running"),
				taskPart(parent, "p2", "c2", child, "error"),
				childStatus("busy"),
				childStatus("idle"),
			},
			want: []string{"subagent-start:c1", "subagent-start:c2", "subagent-stop:c2", "subagent-stop:c1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hooks := runPluginTaskScenario(t, tt.steps)
			var got []string
			for _, h := range hooks {
				got = append(got, h.key())
				if h.SessionID != parent || h.ChildID != child {
					t.Errorf("%s: session %q child %q, want %q %q", h.key(), h.SessionID, h.ChildID, parent, child)
				}
				if h.StartedAt == 0 {
					t.Errorf("%s: started_at not carried from tool.execute.before", h.key())
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("hooks = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPlugin_NestedTaskErrorStopsOnTopLevelSession pins that a grandchild
// task that ends in error is stopped on the top-level session, like its start.
func TestPlugin_NestedTaskErrorStopsOnTopLevelSession(t *testing.T) {
	t.Parallel()
	hooks := runPluginTaskScenario(t, []map[string]any{
		before("ses_parent", "c1"),
		taskPart("ses_parent", "p1", "c1", "ses_child", "running"),
		before("ses_child", "c2"),
		taskPart("ses_child", "p2", "c2", "ses_grandchild", "running"),
		taskPart("ses_child", "p2", "c2", "ses_grandchild", "error"),
	})
	var got []string
	for _, h := range hooks {
		got = append(got, h.key()+"@"+h.SessionID)
	}
	want := "subagent-start:c1@ses_parent,subagent-start:c2@ses_parent,subagent-stop:c2@ses_parent"
	if strings.Join(got, ",") != want {
		t.Errorf("hooks = %v, want %s", got, want)
	}
}

// TestPlugin_ReusedCallIDStillStops pins that a callID reused by a later task
// (some providers number tool calls per conversation) still fires that task's
// stop, though its start is deduplicated away.
func TestPlugin_ReusedCallIDStillStops(t *testing.T) {
	t.Parallel()
	hooks := runPluginTaskScenario(t, []map[string]any{
		before("ses_parent", "functions.task:0"),
		taskPart("ses_parent", "p1", "functions.task:0", "ses_child", "running"),
		after("functions.task:0", "ses_child", false),
		before("ses_parent", "functions.task:0"),
		taskPart("ses_parent", "p2", "functions.task:0", "ses_child2", "running"),
		after("functions.task:0", "ses_child2", false),
	})
	var got []string
	for _, h := range hooks {
		got = append(got, h.key()+"@"+h.ChildID)
	}
	want := "subagent-start:functions.task:0@ses_child,subagent-stop:functions.task:0@ses_child,subagent-stop:functions.task:0@ses_child2"
	if strings.Join(got, ",") != want {
		t.Errorf("hooks = %v, want %s", got, want)
	}
}

// TestPlugin_MissedBeforeHookUsesThePartStart pins that a task whose
// tool.execute.before the plugin never saw (loaded mid-call) still reports
// OpenCode's own call start, from the task part, rather than none.
func TestPlugin_MissedBeforeHookUsesThePartStart(t *testing.T) {
	t.Parallel()
	running := map[string]any{"event": map[string]any{
		"type": "message.part.updated",
		"properties": map[string]any{"part": map[string]any{
			"id": "p1", "messageID": "msg_p1", "sessionID": "ses_parent", "type": "tool", "tool": "task", "callID": "c1",
			"state": map[string]any{
				"status": "running", "input": map[string]any{},
				"metadata": map[string]any{"sessionId": "ses_child"},
				"time":     map[string]any{"start": 1708300001},
			},
		}},
	}}
	hooks := runPluginTaskScenario(t, []map[string]any{running, after("c1", "ses_child", false)})
	if len(hooks) != 2 {
		t.Fatalf("hooks = %v, want a start and a stop", hooks)
	}
	for _, h := range hooks {
		if h.StartedAt != 1708300001 {
			t.Errorf("%s started_at = %d, want the part's start 1708300001", h.key(), h.StartedAt)
		}
	}
}
