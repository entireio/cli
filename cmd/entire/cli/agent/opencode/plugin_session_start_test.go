package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pluginUnawaitedDriver loads the OpenCode 1 plugin under Node and delivers
// each event without waiting for the previous handler to finish, as OpenCode
// may: a session's first message can arrive while its session.created
// handler is still running session-start.
const pluginUnawaitedDriver = `
import { pathToFileURL } from "node:url"
import { readFileSync } from "node:fs"

const { default: EntirePlugin } = await import(pathToFileURL(process.argv[2]).href)
const handlers = await EntirePlugin({ directory: process.cwd() })
const running = []
for (const event of JSON.parse(readFileSync(process.argv[3], "utf8"))) {
  running.push(handlers.event({ event }))
  await new Promise((r) => setTimeout(r, 50))
}
await Promise.all(running)
`

// The first turn's turn-start must follow its session's session-start, which
// readies what the first turn injects (the live sessions cache): OpenCode 1
// delivers events without waiting for the plugin's handlers.
func TestPlugin_TurnStartWaitsForItsSessionStart(t *testing.T) {
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
		{"type": "message.part.updated", "properties": map[string]any{"part": map[string]any{
			"id": "prt_1", "messageID": "msg_1", "sessionID": "ses_1", "type": "text", "text": "hello",
		}}},
	}
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	// session-start takes a while, as a refresh at SessionStart does.
	script := "#!/bin/sh\ncat > /dev/null\nif [ \"$3\" = session-start ]; then sleep 1; fi\necho \"$3\" >> " + logPath + "\n"
	for path, content := range map[string]string{
		filepath.Join(dir, "entire.ts"):   renderPlugin(),
		filepath.Join(dir, "driver.mjs"):  pluginUnawaitedDriver,
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
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, h := range strings.Fields(string(data)) {
		if h == "session-start" || h == "turn-start" {
			order = append(order, h)
		}
	}
	if strings.Join(order, " ") != "session-start turn-start" {
		t.Fatalf("hooks ran %v; want session-start to finish before turn-start", order)
	}
}
