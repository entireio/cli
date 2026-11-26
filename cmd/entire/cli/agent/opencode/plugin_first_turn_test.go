package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pluginFirstTurnDriver loads the OpenCode 1 plugin under Node and delivers
// each event without waiting for the previous handler, as OpenCode may, then
// builds the first model call's system prompt 100 ms after the user's
// message, before the session goes idle.
const pluginFirstTurnDriver = `
import { pathToFileURL } from "node:url"
import { readFileSync } from "node:fs"

const { default: EntirePlugin } = await import(pathToFileURL(process.argv[2]).href)
const handlers = await EntirePlugin({ directory: process.cwd() })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const running = []
const [created, message, idle] = JSON.parse(readFileSync(process.argv[3], "utf8"))
running.push(handlers.event({ event: created }))
running.push(handlers.event({ event: message }))
await sleep(100)
const output = { system: [] }
await handlers["experimental.chat.system.transform"]({ sessionID: "ses_1" }, output)
console.log("FIRST_LLM_CALL_SYSTEM=" + JSON.stringify(output.system))
running.push(handlers.event({ event: idle }))
await Promise.all(running)
`

// The first turn's context reaches the first model call, and its turn-start
// runs before its turn-end, even while session-start is still running: on
// OpenCode 1 the plugin's turn-start stays synchronous rather than waiting
// for session-start, which let OpenCode build the first call (and `opencode
// run` end the turn) before turn-start ran. The cost: a session-start
// refresh of a cold live sessions cache reaches only a later turn there.
func TestPlugin_FirstTurnContextReachesTheFirstCall(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hooks.log")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	events := []map[string]any{
		{"type": "session.created", "properties": map[string]any{"info": map[string]any{"id": "ses_1"}}},
		{"type": "message.updated", "properties": map[string]any{"info": map[string]any{"id": "msg_1", "sessionID": "ses_1", "role": "user"}}},
		{"type": "session.status", "properties": map[string]any{"sessionID": "ses_1", "status": map[string]any{"type": "idle"}}},
	}
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat > /dev/null\n" +
		"if [ \"$3\" = session-start ]; then sleep 1; fi\n" +
		"echo \"$3\" >> " + logPath + "\n" +
		"if [ \"$3\" = turn-start ]; then echo '{\"inject_context\":\"ENTIRE BASE TEXT\"}'; fi\n"
	for path, content := range map[string]string{
		filepath.Join(dir, "entire.ts"):   renderPlugin(),
		filepath.Join(dir, "driver.mjs"):  pluginFirstTurnDriver,
		filepath.Join(dir, "events.json"): string(eventsJSON),
		filepath.Join(binDir, "entire"):   script,
	} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.CommandContext(t.Context(), "node", "--experimental-strip-types",
		filepath.Join(dir, "driver.mjs"), filepath.Join(dir, "entire.ts"), filepath.Join(dir, "events.json"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "OPENCODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `FIRST_LLM_CALL_SYSTEM=["ENTIRE BASE TEXT"]`) {
		t.Errorf("the first model call lacks the turn's context:\n%s", out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, h := range strings.Fields(string(data)) {
		if h == "turn-start" || h == "turn-end" {
			order = append(order, h)
		}
	}
	if strings.Join(order, " ") != "turn-start turn-end" {
		t.Errorf("turn hooks ran %v; want turn-start before turn-end", order)
	}
}
