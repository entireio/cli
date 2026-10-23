package opencode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// pluginV2Driver loads the plugin's OpenCode 2 entry point under Node with a
// fake setup context and replays a scenario of hook calls and events, in
// order. Each event is handed to the plugin's subscription and the next step
// waits until the plugin asks for the following event, so events are fully
// handled in sequence.
const pluginV2Driver = `
import { pathToFileURL } from "node:url"
import { readFileSync } from "node:fs"

const { default: plugin } = await import(pathToFileURL(process.argv[2]).href)
const { parents, steps, reduced } = JSON.parse(readFileSync(process.argv[3], "utf8"))
const hooks = {}
const pending = []
let waiting = null
let handled = null
const ctx = {
  location: { directory: process.cwd() },
  session: {
    hook: async (name, fn) => { hooks["session." + name] = fn },
    get: async ({ sessionID }) => (sessionID in parents ? { id: sessionID, parentID: parents[sessionID] ?? undefined } : undefined),
  },
  tool: { hook: async (name, fn) => { hooks["tool." + name] = fn } },
  event: {
    subscribe: () => ({
      [Symbol.asyncIterator]() {
        return {
          next: () => new Promise((resolve) => {
            if (handled) { handled(); handled = null }
            if (pending.length) resolve({ value: pending.shift(), done: false })
            else waiting = resolve
          }),
        }
      },
    }),
  },
}
if (reduced) {
  // OpenCode 1 calls setup too, without the OpenCode 2 APIs.
  const out = await plugin.setup({ directory: process.cwd() })
  if (out !== undefined) throw new Error("setup on a reduced context returned " + typeof out)
  process.exit(0)
}
const cleanup = await plugin.setup(ctx)
for (const step of steps) {
  if (step.prompt) await hooks["session.prompt"](step.prompt)
  if (step.context) await hooks["session.context"](step.context)
  if (step.before) await hooks["tool.execute.before"](step.before)
  if (step.after) {
    // noWait: OpenCode keeps delivering events while this hook runs.
    const ran = hooks["tool.execute.after"](step.after)
    if (step.noWait) await new Promise((resolve) => setTimeout(resolve, 10))
    else await ran
  }
  if (step.event?.location?.directory === "$CWD") step.event.location.directory = process.cwd()
  if (step.event) {
    const delivered = new Promise((resolve) => {
      handled = resolve
      if (waiting) { const w = waiting; waiting = null; w({ value: step.event, done: false }) }
      else pending.push(step.event)
    })
    // progressNoWait: OpenCode does not wait for a plugin to take an event,
    // so the next step may overlap it. Otherwise the plugin, which queues
    // events and handles them asynchronously, gets time to handle it first.
    if (step.progressNoWait) await new Promise((resolve) => setTimeout(resolve, 10))
    else {
      await delivered
      await new Promise((resolve) => setTimeout(resolve, 250))
    }
  }
}
if (cleanup) await cleanup()
`

type v2Hook struct {
	Name    string
	Payload map[string]any
}

func (h v2Hook) String() string {
	s := h.Name + ":" + fmt.Sprint(h.Payload["session_id"])
	if call, ok := h.Payload["tool_use_id"]; ok {
		s += ":" + fmt.Sprint(call)
	}
	return s
}

// runPluginV2Scenario replays steps against the OpenCode 2 entry point and
// returns the hooks it fired, in the order they ran. parents maps each session
// OpenCode knows to its parent ("" for a top-level session). Detached hooks
// can outlive the driver, so the log is read until it stops growing.
func runPluginV2Scenario(t *testing.T, parents map[string]string, steps []map[string]any) []v2Hook {
	t.Helper()
	return runPluginV2(t, map[string]any{"parents": parentsJSON(parents), "steps": steps})
}

func parentsJSON(parents map[string]string) map[string]any {
	out := make(map[string]any, len(parents))
	for id, parent := range parents {
		if parent == "" {
			out[id] = nil
		} else {
			out[id] = parent
		}
	}
	return out
}

func runPluginV2(t *testing.T, scenario map[string]any) []v2Hook {
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
	scenarioJSON, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		pluginPath:                          renderPlugin(),
		filepath.Join(dir, "driver.mjs"):    pluginV2Driver,
		filepath.Join(dir, "scenario.json"): string(scenarioJSON),
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One line per hook: name, tab, payload. A short sleep makes ordering
	// races between detached hooks visible.
	script := "#!/bin/sh\nbody=$(cat)\n" +
		"[ \"$3\" = subagent-start ] && [ -n \"$SLOW_SUBAGENT_START\" ] && sleep 1\n" +
		"sleep 0.05\nprintf '%s\\t%s\\n' \"$3\" \"$body\" >> " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "entire"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "node", "--experimental-strip-types",
		filepath.Join(dir, "driver.mjs"), pluginPath, filepath.Join(dir, "scenario.json"))
	cmd.Dir = dir
	// OpenCode 2 does not set OPENCODE; a test run from inside OpenCode 1
	// would inherit it and load the OpenCode 1 plugin instead.
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "OPENCODE=")
	if slow, _ := scenario["slowSubagentStart"].(bool); slow { //nolint:errcheck // absent means false
		cmd.Env = append(cmd.Env, "SLOW_SUBAGENT_START=1")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}

	var size int64 = -1
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		info, err := os.Stat(logPath)
		if err != nil {
			if os.IsNotExist(err) && size == -1 {
				size = 0
				continue
			}
			break
		}
		if info.Size() == size {
			break
		}
		size = info.Size()
	}
	f, err := os.Open(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var hooks []v2Hook
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, body, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("bad %s payload %q: %v", name, body, err)
		}
		hooks = append(hooks, v2Hook{name, payload})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return hooks
}

func hookStrings(hooks []v2Hook) []string {
	out := make([]string, len(hooks))
	for i, h := range hooks {
		out[i] = h.String()
	}
	return out
}

func v2Prompt(sessionID, text string) map[string]any {
	return map[string]any{"prompt": map[string]any{"sessionID": sessionID, "messageID": "msg_" + text, "delivery": "steer", "prompt": map[string]any{"text": text}}}
}

func v2Event(eventType string, data map[string]any) map[string]any {
	return map[string]any{"event": map[string]any{"type": eventType, "data": data}}
}

func v2Execution(sessionID, outcome string) map[string]any {
	return v2Event("session.execution."+outcome, map[string]any{"sessionID": sessionID})
}

// v2Shutdown is location.shutdown for dir; "$CWD" is the plugin's own
// directory.
func v2Shutdown(dir string) map[string]any {
	return map[string]any{"event": map[string]any{"type": "location.shutdown", "data": map[string]any{}, "location": map[string]any{"directory": dir}}}
}

func v2SubagentBefore(callID string, background bool) map[string]any {
	return map[string]any{"before": map[string]any{"tool": "subagent", "sessionID": "ses_a", "id": callID,
		"input": map[string]any{"agent": "general", "description": "child work", "background": background}}}
}

func v2SubagentProgress(callID string) map[string]any {
	return v2Event("session.tool.progress", map[string]any{"sessionID": "ses_a", "id": callID,
		"metadata": map[string]any{"sessionID": "ses_child", "status": "running"}})
}

func v2SubagentAfter(callID, status string) map[string]any {
	return map[string]any{"after": map[string]any{"tool": "subagent", "sessionID": "ses_a", "id": callID, "status": "completed",
		"input":  map[string]any{"agent": "general", "description": "child work"},
		"result": map[string]any{"output": map[string]any{"sessionID": "ses_child", "status": status}}}}
}

// A turn: session-start and turn-start (with the prompt) before the
// execution runs, turn-end when it ends, session-end at shutdown, in that
// order even though turn-end and session-end run detached.
func TestPluginV2_TurnLifecycle(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_a": ""}, []map[string]any{
		v2Prompt("ses_a", "make it red"),
		{"context": map[string]any{"sessionID": "ses_a", "model": map[string]any{"id": "m1", "providerID": "p"}, "system": []any{}}},
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_a"}),
		v2Execution("ses_a", "succeeded"),
		v2Shutdown("$CWD"),
		v2Prompt("ses_a", "after shutdown"),
	})
	// The shutdown ends the session; nothing ends it a second time at cleanup.
	want := []string{"session-start:ses_a", "turn-start:ses_a", "turn-end:ses_a", "session-end:ses_a",
		"session-start:ses_a", "turn-start:ses_a", "turn-end:ses_a", "session-end:ses_a"}
	if got := hookStrings(hooks); !slices.Equal(got, want) {
		t.Fatalf("hooks = %v, want %v", got, want)
	}
	if hooks[1].Payload["prompt"] != "make it red" {
		t.Errorf("turn-start prompt = %v", hooks[1].Payload["prompt"])
	}
	if hooks[2].Payload["model"] != "m1" {
		t.Errorf("turn-end model = %v, want the context hook's model", hooks[2].Payload["model"])
	}
}

// A prompt sent while the session's execution runs joins it: one turn.
func TestPluginV2_PromptDuringTurnJoinsIt(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_a": ""}, []map[string]any{
		v2Prompt("ses_a", "first"),
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_a"}),
		v2Prompt("ses_a", "steer"),
		v2Execution("ses_a", "succeeded"),
		v2Prompt("ses_a", "next"),
		v2Execution("ses_a", "failed"),
	})
	want := []string{"session-start:ses_a", "turn-start:ses_a", "turn-end:ses_a", "turn-start:ses_a", "turn-end:ses_a", "session-end:ses_a"}
	if got := hookStrings(hooks); !slices.Equal(got, want) {
		t.Fatalf("hooks = %v, want %v", got, want)
	}
}

// Events reach every repository's plugin in the shared service; a session this
// instance never saw in its prompt hook is someone else's.
func TestPluginV2_IgnoresOtherSessions(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_other": ""}, []map[string]any{
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_other"}),
		v2Execution("ses_other", "succeeded"),
		v2Event("session.compaction.ended", map[string]any{"sessionID": "ses_other"}),
		v2Shutdown("/elsewhere"),
	})
	if len(hooks) != 0 {
		t.Fatalf("hooks fired for a session this instance does not own: %v", hookStrings(hooks))
	}
}

// A subagent's child session never gets lifecycle hooks of its own; the call
// is reported on the parent as one subagent-start and one subagent-stop.
func TestPluginV2_ForegroundSubagent(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_a": "", "ses_child": "ses_a"}, []map[string]any{
		v2Prompt("ses_a", "delegate"),
		v2SubagentBefore("call_1", false),
		v2Prompt("ses_child", "You are a subagent"),
		v2SubagentProgress("call_1"),
		v2SubagentProgress("call_1"),
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_child"}),
		v2Execution("ses_child", "succeeded"),
		v2SubagentAfter("call_1", "completed"),
		v2Execution("ses_a", "succeeded"),
	})
	want := []string{"session-start:ses_a", "turn-start:ses_a", "subagent-start:ses_a:call_1", "subagent-stop:ses_a:call_1", "turn-end:ses_a", "session-end:ses_a"}
	if got := hookStrings(hooks); !slices.Equal(got, want) {
		t.Fatalf("hooks = %v, want %v", got, want)
	}
	start := hooks[2].Payload
	if start["subagent_id"] != "ses_child" || start["subagent_type"] != "general" || start["task_description"] != "child work" {
		t.Errorf("subagent-start payload = %v", start)
	}
	if started, ok := start["started_at"].(float64); !ok || started <= 0 {
		t.Errorf("started_at = %v, want the before hook's clock", start["started_at"])
	}
}

// A background call returns at launch, before its first progress update; the
// stop is held until the child's execution ends, and the result's delivery to
// the parent is a turn with no prompt.
func TestPluginV2_BackgroundSubagent(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_a": "", "ses_child": "ses_a"}, []map[string]any{
		v2Prompt("ses_a", "launch"),
		v2SubagentBefore("call_bg", true),
		v2Prompt("ses_child", "You are a subagent"),
		v2SubagentAfter("call_bg", "running"),
		v2SubagentProgress("call_bg"),
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_child"}),
		v2Execution("ses_a", "succeeded"),
		v2Execution("ses_child", "succeeded"),
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_a"}),
		v2Execution("ses_a", "succeeded"),
	})
	want := []string{
		"session-start:ses_a", "turn-start:ses_a", "subagent-start:ses_a:call_bg", "turn-end:ses_a",
		"subagent-stop:ses_a:call_bg", "turn-start:ses_a", "turn-end:ses_a", "session-end:ses_a",
	}
	if got := hookStrings(hooks); !slices.Equal(got, want) {
		t.Fatalf("hooks = %v, want %v", got, want)
	}
	if hooks[5].Payload["prompt"] != "" {
		t.Errorf("wake-up turn-start prompt = %v, want none", hooks[5].Payload["prompt"])
	}
}

// Interrupting the launcher ends a busy background child's held call when
// the child's execution ends, even if it ends successfully.
func TestPluginV2_InterruptedLauncherDrainsBackgroundCalls(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_a": "", "ses_child": "ses_a"}, []map[string]any{
		v2Prompt("ses_a", "launch"),
		v2SubagentBefore("call_1", true),
		v2Prompt("ses_child", "You are a subagent"),
		v2SubagentAfter("call_1", "running"),
		v2SubagentBefore("call_2", true),
		v2SubagentAfter("call_2", "running"),
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_child"}),
		v2Execution("ses_a", "interrupted"),
		v2Execution("ses_child", "succeeded"),
	})
	var stops []string
	for _, h := range hooks {
		if h.Name == "subagent-stop" {
			stops = append(stops, h.String())
		}
	}
	slices.Sort(stops)
	if want := []string{"subagent-stop:ses_a:call_1", "subagent-stop:ses_a:call_2"}; !slices.Equal(stops, want) {
		t.Fatalf("stops = %v, want both held calls ended", stops)
	}
}

// OpenCode 1 calls setup with a reduced context; the OpenCode 2 entry point
// must leave it to the OpenCode 1 plugin.
func TestPluginV2_SetupIgnoresOpenCode1Context(t *testing.T) {
	t.Parallel()
	if hooks := runPluginV2(t, map[string]any{"parents": map[string]any{}, "steps": []any{}, "reduced": true}); len(hooks) != 0 {
		t.Fatalf("hooks = %v", hookStrings(hooks))
	}
}

// A background child can finish before its launch result reaches the plugin;
// its held stop must still fire, after its start.
func TestPluginV2_BackgroundChildEndsBeforeLaunchResult(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_a": "", "ses_child": "ses_a"}, []map[string]any{
		v2Prompt("ses_a", "launch"),
		v2SubagentBefore("call_bg", true),
		v2Prompt("ses_child", "You are a subagent"),
		v2Event("session.execution.started", map[string]any{"sessionID": "ses_child"}),
		v2Execution("ses_child", "succeeded"),
		v2SubagentAfter("call_bg", "running"),
		v2Execution("ses_a", "succeeded"),
	})
	want := []string{"session-start:ses_a", "turn-start:ses_a", "subagent-start:ses_a:call_bg", "subagent-stop:ses_a:call_bg", "turn-end:ses_a", "session-end:ses_a"}
	if got := hookStrings(hooks); !slices.Equal(got, want) {
		t.Fatalf("hooks = %v, want %v", got, want)
	}
}

// A foreground call whose child finishes at once: the stop arrives while the
// start hook still runs, and must land after it. A late progress update does
// not announce the call again.
func TestPluginV2_FastForegroundSubagentStartsBeforeItStops(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2Scenario(t, map[string]string{"ses_a": "", "ses_child": "ses_a"}, []map[string]any{
		v2Prompt("ses_a", "delegate"),
		v2SubagentBefore("call_1", false),
		v2Prompt("ses_child", "You are a subagent"),
		{"progressNoWait": true, "event": v2SubagentProgress("call_1")["event"]},
		v2SubagentAfter("call_1", "completed"),
		v2SubagentProgress("call_1"),
		v2Execution("ses_a", "succeeded"),
	})
	want := []string{"session-start:ses_a", "turn-start:ses_a", "subagent-start:ses_a:call_1", "subagent-stop:ses_a:call_1", "turn-end:ses_a", "session-end:ses_a"}
	if got := hookStrings(hooks); !slices.Equal(got, want) {
		t.Fatalf("hooks = %v, want %v", got, want)
	}
}

// One session's slow hook must not hold up another session's events: B's
// turn end has to be handled before B's next prompt, or that prompt would
// find B's turn still active and skip its turn-start.
func TestPluginV2_SlowHookDoesNotBlockOtherSessions(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2(t, map[string]any{
		"slowSubagentStart": true,
		"parents":           parentsJSON(map[string]string{"ses_a": "", "ses_b": "", "ses_child": "ses_a"}),
		"steps": []map[string]any{
			v2Prompt("ses_a", "delegate"),
			v2Prompt("ses_b", "first"),
			v2SubagentBefore("call_1", false),
			v2Prompt("ses_child", "You are a subagent"),
			{"progressNoWait": true, "event": v2SubagentProgress("call_1")["event"]},
			{"progressNoWait": true, "event": v2Execution("ses_b", "succeeded")["event"]},
			v2Prompt("ses_b", "second"),
		},
	})
	var b []string
	for _, h := range hooks {
		if h.Payload["session_id"] == "ses_b" {
			b = append(b, h.Name)
		}
	}
	want := []string{"session-start", "turn-start", "turn-end", "turn-start"}
	if !slices.Equal(b[:min(len(b), len(want))], want) {
		t.Fatalf("ses_b hooks = %v, want %v first", b, want)
	}
}

// A held stop released while its call's subagent-start is still running
// lands before session-end at shutdown.
func TestPluginV2_ShutdownWaitsForHeldStopsBehindTheirStart(t *testing.T) {
	t.Parallel()
	hooks := runPluginV2(t, map[string]any{
		"slowSubagentStart": true,
		"parents":           parentsJSON(map[string]string{"ses_a": "", "ses_child": "ses_a"}),
		"steps": []map[string]any{
			v2Prompt("ses_a", "launch"),
			v2SubagentBefore("call_bg", true),
			v2Prompt("ses_child", "You are a subagent"),
			{"noWait": true, "after": v2SubagentAfter("call_bg", "running")["after"]},
			v2Event("session.execution.started", map[string]any{"sessionID": "ses_child"}),
			v2Execution("ses_child", "succeeded"),
			v2Execution("ses_a", "succeeded"),
			v2Shutdown("$CWD"),
		},
	})
	got := hookStrings(hooks)
	stop, end := slices.Index(got, "subagent-stop:ses_a:call_bg"), slices.Index(got, "session-end:ses_a")
	if stop < 0 || end < 0 || stop > end {
		t.Fatalf("hooks = %v; want the held subagent-stop before session-end", got)
	}
}
