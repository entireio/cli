package pi

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// extensionDriver loads the extension under Node with a minimal stand-in for
// Pi's ExtensionAPI. Each "instance" calls the extension factory again in the
// same process, as Pi does on /reload, /new and /resume. Events in "fire" are
// dispatched to the last instance. With "spawnChild", the driver re-runs itself
// as a child process that inherits this process's environment, the way a
// subagent extension spawns a nested `pi`.
const extensionDriver = `
import { pathToFileURL } from "node:url"
import { spawnSync } from "node:child_process"

const { default: register } = await import(pathToFileURL(process.argv[2]).href)
const cfg = JSON.parse(process.argv[3])

function load() {
  const handlers = {}
  register({ on(event, fn) { (handlers[event] ??= []).push(fn) } })
  return handlers
}

const out = { instances: [] }
let last = {}
for (let i = 0; i < cfg.instances; i++) {
  last = load()
  out.instances.push(Object.keys(last).sort())
}
const ctx = { cwd: process.cwd(), sessionManager: { getSessionFile: () => "/tmp/pi-session.jsonl" } }
for (const f of cfg.fire ?? []) {
  const event = { prompt: "x".repeat(f.promptBytes ?? 1) }
  for (const h of last[f.event] ?? []) await h(event, ctx)
}
if (cfg.spawnChild) {
  const r = spawnSync(process.execPath, [...process.execArgv, process.argv[1], process.argv[2], JSON.stringify({ instances: 2 })], { encoding: "utf8" })
  if (r.status !== 0) throw new Error("child driver failed: " + r.stderr)
  out.child = JSON.parse(r.stdout).instances
}
// Give asynchronously emitted stream errors a chance to surface before exit.
await new Promise((resolve) => setTimeout(resolve, 200))
console.log(JSON.stringify(out))
`

type driverConfig struct {
	Instances  int         `json:"instances"`
	Fire       []fireEvent `json:"fire,omitempty"`
	SpawnChild bool        `json:"spawnChild,omitempty"`
}

type fireEvent struct {
	Event       string `json:"event"`
	PromptBytes int    `json:"promptBytes,omitempty"`
}

type driverResult struct {
	Instances [][]string `json:"instances"`
	Child     [][]string `json:"child"`
	HookLog   string     `json:"-"`
}

// runExtension renders the extension, puts entireScript on PATH as `entire`,
// and runs the driver with env appended to the test's environment (minus any
// inherited ENTIRE_PI_NESTED). It fails the test if Node exits non-zero.
func runExtension(t *testing.T, cfg driverConfig, entireScript string, env ...string) driverResult {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "hooks.log")
	entireScript = strings.ReplaceAll(entireScript, "$LOG", logPath)
	for path, content := range map[string]string{
		filepath.Join(dir, "index.ts"):   renderExtension(),
		filepath.Join(dir, "driver.mjs"): extensionDriver,
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "entire"), []byte(entireScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "node", "--experimental-strip-types",
		filepath.Join(dir, "driver.mjs"), filepath.Join(dir, "index.ts"), string(cfgJSON))
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, piNestedEnvVar+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = ee.Stderr
		}
		t.Fatalf("node driver failed: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	var res driverResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("bad driver output %q: %v", out, err)
	}
	if b, err := os.ReadFile(logPath); err == nil {
		res.HookLog = string(b)
	}
	return res
}

// recordingEntire logs each hook name with the ENTIRE_PI_NESTED value it saw.
const recordingEntire = "#!/bin/sh\ncat >/dev/null\nprintf '%s nested=%s\\n' \"$3\" \"$ENTIRE_PI_NESTED\" >> $LOG\n"

// TestExtension_ReplacementInstanceKeepsForwarding pins that a top-level Pi
// keeps forwarding lifecycle after /reload, /new or /resume, which build a new
// extension instance inside the same process.
func TestExtension_ReplacementInstanceKeepsForwarding(t *testing.T) {
	t.Parallel()
	res := runExtension(t, driverConfig{
		Instances: 2,
		Fire:      []fireEvent{{Event: "session_start"}},
	}, recordingEntire)

	for i, events := range res.Instances {
		if !slices.Contains(events, "session_start") {
			t.Errorf("instance %d registered %v, want lifecycle handlers", i+1, events)
		}
	}
	if !strings.HasPrefix(res.HookLog, "session_start nested=") {
		t.Fatalf("replacement instance did not forward session_start; hook log: %q", res.HookLog)
	}
	// Entire's own hook subprocesses still inherit the marker.
	if strings.HasPrefix(res.HookLog, "session_start nested=\n") {
		t.Errorf("hook subprocess did not inherit %s; hook log: %q", piNestedEnvVar, res.HookLog)
	}
}

// TestExtension_NestedProcessSkipsLifecycle pins the subagent guard: a Pi
// process spawned by a Pi that loaded the extension registers only the bash
// hardening, in its first instance and in any replacement instance.
func TestExtension_NestedProcessSkipsLifecycle(t *testing.T) {
	t.Parallel()
	res := runExtension(t, driverConfig{Instances: 1, SpawnChild: true}, recordingEntire)

	if !slices.Contains(res.Instances[0], "session_start") {
		t.Fatalf("parent registered %v, want lifecycle handlers", res.Instances[0])
	}
	if len(res.Child) != 2 {
		t.Fatalf("child instances = %v, want 2", res.Child)
	}
	for i, events := range res.Child {
		if !slices.Equal(events, []string{"tool_call"}) {
			t.Errorf("nested instance %d registered %v, want only [tool_call]", i+1, events)
		}
	}
}

// TestExtension_MarkerFromOlderExtensionStillNests pins compatibility with a
// parent running an older extension, which set the marker to "1".
func TestExtension_MarkerFromOlderExtensionStillNests(t *testing.T) {
	t.Parallel()
	res := runExtension(t, driverConfig{Instances: 2}, recordingEntire, piNestedEnvVar+"=1")
	for i, events := range res.Instances {
		if !slices.Equal(events, []string{"tool_call"}) {
			t.Errorf("instance %d registered %v, want only [tool_call]", i+1, events)
		}
	}
}

// TestExtension_HookStdinErrorDoesNotCrash pins that a hook which exits without
// reading its payload (EPIPE on the stdin write) cannot take the host process
// down: hooks are best effort.
func TestExtension_HookStdinErrorDoesNotCrash(t *testing.T) {
	t.Parallel()
	res := runExtension(t, driverConfig{
		Instances: 1,
		Fire:      []fireEvent{{Event: "before_agent_start", PromptBytes: 4 << 20}},
	}, "#!/bin/sh\nexit 0\n")
	if len(res.Instances) != 1 {
		t.Fatalf("instances = %v, want 1", res.Instances)
	}
}
