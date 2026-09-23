package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

// Compile-time check
var _ agent.HookSupport = (*OpenCodeAgent)(nil)

// Note: Hook tests cannot use t.Parallel() because t.Chdir() modifies process state.

func TestInstallHooks_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	count, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 hook installed, got %d", count)
	}

	// Verify plugin file was created
	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	// The plugin names the entire binary directly, guarded by a PATH probe.
	if !strings.Contains(content, `command -v entire >/dev/null 2>&1`) {
		t.Error("plugin file does not contain production command constant")
	}
	if !strings.Contains(content, "hooks opencode") {
		t.Error("plugin file does not contain 'hooks opencode'")
	}
	if !strings.Contains(content, "EntirePlugin") {
		t.Error("plugin file does not contain 'EntirePlugin' export")
	}
	// Should use production command
	if strings.Contains(content, "go run") {
		t.Error("plugin file contains 'go run' in production mode")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// First install
	count1, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	if count1 != 1 {
		t.Errorf("first install: expected 1, got %d", count1)
	}

	// Second install — should be idempotent
	count2, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("second install failed: %v", err)
	}
	if count2 != 0 {
		t.Errorf("second install: expected 0 (idempotent), got %d", count2)
	}
}

func TestInstallHooks_SessionStartIsGuardedBySessionSwitch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	guard := "if (resetSessionTracking(session.id)) {"
	hook := `await callHook("session-start", {`

	guardIdx := strings.Index(content, guard)
	hookIdx := strings.Index(content, hook)

	if guardIdx == -1 {
		t.Fatalf("plugin file missing guard %q", guard)
	}
	if hookIdx == -1 {
		t.Fatalf("plugin file missing session-start hook call %q", hook)
	}
	if guardIdx >= hookIdx {
		t.Fatalf("expected guarded session-start call after guard, got guard=%d hook=%d",
			guardIdx, hookIdx)
	}
	if !strings.Contains(content, `if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec entire hooks opencode ${hookName}`) {
		t.Fatal("plugin file missing silent production hook command")
	}
	if strings.Contains(content, "Bun.spawn") || strings.Contains(content, "Bun.spawnSync") {
		t.Fatal("plugin must not call Bun globals — Desktop runs the server on Node (#2014)")
	}
	if !strings.Contains(content, `from "node:child_process"`) {
		t.Fatal("plugin should spawn hooks via node:child_process")
	}
}

func TestInstallHooks_TurnStartUsesSyncHook(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	// turn-start is dispatched via fireTurnStart, which fires synchronously
	// (spawnSync) so session state is ready before any mid-turn commit, and
	// also captures the hook's stdout to apply Entire's one-time context injection.
	if !strings.Contains(content, `fireTurnStart({`) {
		t.Fatal("plugin file should dispatch turn-start via fireTurnStart")
	}
	if !strings.Contains(content, `spawnSync(cmd, args, {`) {
		t.Fatal("fireTurnStart should dispatch turn-start synchronously via spawnSync")
	}
	if !strings.Contains(content, `hookCmd("turn-start")`) {
		t.Fatal("fireTurnStart should invoke the turn-start hook")
	}
	if strings.Contains(content, `await callHook("turn-start", {`) {
		t.Fatal("plugin file should not dispatch turn-start via async callHook")
	}
}

func TestInstallHooks_AppliesContextInjection(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	// The plugin must read the injection envelope from the turn-start hook's
	// stdout and apply it to the system prompt via the chat system transform.
	if !strings.Contains(content, `inject_context`) {
		t.Fatal("plugin file should parse the inject_context envelope")
	}
	if !strings.Contains(content, `"experimental.chat.system.transform"`) {
		t.Fatal("plugin file should apply injection via experimental.chat.system.transform")
	}
	if !strings.Contains(content, `output.system.push(pendingInjection)`) {
		t.Fatal("plugin file should push the injection onto the system prompt")
	}
	// Every session-reset site must clear the stashed injection so a session
	// change cannot leak the prior session's context into the next session.
	resetSites := []struct{ name, start, end string }{
		{"resetSessionTracking", "function resetSessionTracking", "return true"},
		{"session.deleted", `case "session.deleted"`, `callHookSync("session-end"`},
		{"server.instance.disposed", `case "server.instance.disposed"`, `callHookSync("session-end"`},
	}
	for _, site := range resetSites {
		_, after, found := strings.Cut(content, site.start)
		if !found {
			t.Fatalf("plugin file missing reset site %q", site.name)
		}
		body, _, _ := strings.Cut(after, site.end)
		if !strings.Contains(body, `pendingInjection = null`) {
			t.Fatalf("%s should clear pendingInjection to avoid cross-session leakage", site.name)
		}
	}
}

func TestInstallHooks_MessageUpdatedFallsBackToSessionStart(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, `if (msg.sessionID && resetSessionTracking(msg.sessionID)) {`) {
		t.Fatal("plugin file should bootstrap session tracking from message.updated")
	}
	if !strings.Contains(content, `callHookSync("session-start", {`) {
		t.Fatal("plugin file should dispatch fallback session-start via callHookSync")
	}
	if !strings.Contains(content, `session_id: msg.sessionID,`) {
		t.Fatal("plugin file should pass msg.sessionID in fallback session-start")
	}
}

func TestInstallHooks_MessageUpdatedFallsBackToPromptUpdate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, `if (msg.role === "user" && !seenUserMessages.has(msg.id)) {`) {
		t.Fatal("plugin file should use message.updated as a fallback turn-start source")
	}
	if !strings.Contains(content, `prompt: "",`) {
		t.Fatal("plugin file should send an empty prompt for fallback turn-start")
	}
	if !strings.Contains(content, `promptlessTurnStarts.has(msg.id) && part.text`) {
		t.Fatal("plugin file should repair the prompt once the text part arrives")
	}
	if !strings.Contains(content, `callHookSync("prompt-update", {`) {
		t.Fatal("plugin file should use the prompt-only update hook for late text")
	}
	if !strings.Contains(content, `clearSessionMessages(session.id)`) {
		t.Fatal("plugin file should clear only the deleted session's message tracking")
	}
	if got := strings.Count(content, `promptlessTurnStarts.clear()`); got != 1 {
		t.Fatalf("plugin file should clear all promptless tracking only at server shutdown, got %d", got)
	}
}

func TestGeneratedPlugin_PreservesPromptAfterMessageUpdatedFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the generated plugin invokes hooks through sh")
	}
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is required to execute the generated OpenCode plugin")
	}

	dir := t.TempDir()
	pluginPath := filepath.Join(dir, "entire.ts")
	if err := os.WriteFile(pluginPath, []byte(renderPlugin()), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookLog := filepath.Join(dir, "prompt-hooks.log")
	fakeEntire := `#!/bin/sh
if [ "$3" = "turn-start" ] || [ "$3" = "prompt-update" ]; then
  printf '%s\t' "$3" >> "$HOOK_LOG"
  cat >> "$HOOK_LOG"
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "entire"), []byte(fakeEntire), 0o755); err != nil {
		t.Fatal(err)
	}

	harness := `
import { EntirePlugin } from "./entire.ts"

const plugin = await EntirePlugin({ directory: process.cwd() } as any)
await plugin.event!({
  event: {
    type: "message.updated",
    properties: { info: { id: "message-1", role: "user", sessionID: "session-1" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
	properties: { part: { messageID: "message-1", type: "text", text: "" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "message-1", type: "text", text: "preserve this prompt" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "message-1", type: "text", text: "duplicate update" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.updated",
    properties: { info: { id: "unrelated-delete-message", role: "user", sessionID: "session-1" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "session.deleted",
    properties: { info: { id: "unrelated-session" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "unrelated-delete-message", type: "text", text: "survives unrelated deletion" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.updated",
    properties: { info: { id: "removed-message", role: "user", sessionID: "session-1" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.removed",
    properties: { sessionID: "session-1", messageID: "removed-message" },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "removed-message", type: "text", text: "stale removed prompt" } },
  },
} as any)
for (const id of ["message-2", "message-3"]) {
  await plugin.event!({
    event: {
      type: "message.updated",
      properties: { info: { id, role: "user", sessionID: "session-1" } },
    },
  } as any)
}
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "message-2", type: "text", text: "" } },
  },
} as any)
for (const [messageID, text] of [["message-3", "third prompt"], ["message-2", "second prompt"], ["message-3", "duplicate third prompt"]]) {
  await plugin.event!({
    event: {
      type: "message.part.updated",
      properties: { part: { messageID, type: "text", text } },
    },
  } as any)
}
await plugin.event!({
  event: {
    type: "message.updated",
    properties: { info: { id: "deleted-message", role: "user", sessionID: "session-1" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "session.deleted",
    properties: { info: { id: "session-1" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "deleted-message", type: "text", text: "stale deleted prompt" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.updated",
    properties: { info: { id: "interleaved-message", role: "user", sessionID: "session-1" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "session.created",
    properties: { info: { id: "session-2" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "interleaved-message", type: "text", text: "survives session interleaving" } },
  },
} as any)
await plugin.event!({
  event: {
    type: "message.updated",
    properties: { info: { id: "disposed-message", role: "user", sessionID: "session-2" } },
  },
} as any)
await plugin.event!({
  event: { type: "server.instance.disposed", properties: {} },
} as any)
await plugin.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "disposed-message", type: "text", text: "stale disposed prompt" } },
  },
} as any)

const pluginWithoutCurrentSession = await EntirePlugin({ directory: process.cwd() } as any)
await pluginWithoutCurrentSession.event!({
  event: {
    type: "message.updated",
    properties: { info: { id: "retained-message", role: "user", sessionID: "session-3" } },
  },
} as any)
await pluginWithoutCurrentSession.event!({
  event: {
    type: "session.created",
    properties: { info: { id: "session-4" } },
  },
} as any)
await pluginWithoutCurrentSession.event!({
  event: {
    type: "session.deleted",
    properties: { info: { id: "session-4" } },
  },
} as any)
await pluginWithoutCurrentSession.event!({
  event: { type: "server.instance.disposed", properties: {} },
} as any)
await pluginWithoutCurrentSession.event!({
  event: {
    type: "message.part.updated",
    properties: { part: { messageID: "retained-message", type: "text", text: "stale shutdown prompt" } },
  },
} as any)
`
	harnessPath := filepath.Join(dir, "harness.ts")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOOK_LOG", hookLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd := exec.CommandContext(context.Background(), bunPath, "run", harnessPath)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("execute generated plugin: %v\n%s", err, output)
	}

	logFile, err := os.Open(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	type hookCall struct {
		Name   string
		Prompt string
	}
	var calls []hookCall
	scanner := bufio.NewScanner(logFile)
	for scanner.Scan() {
		hookName, payloadJSON, found := strings.Cut(scanner.Text(), "\t")
		if !found {
			t.Fatalf("decode hook log line %q", scanner.Text())
		}
		var payload struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			t.Fatalf("decode %s payload %q: %v", hookName, payloadJSON, err)
		}
		calls = append(calls, hookCall{Name: hookName, Prompt: payload.Prompt})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	want := []hookCall{
		{Name: "turn-start", Prompt: ""},
		{Name: "prompt-update", Prompt: "preserve this prompt"},
		{Name: "turn-start", Prompt: ""},
		{Name: "prompt-update", Prompt: "survives unrelated deletion"},
		{Name: "turn-start", Prompt: ""},
		{Name: "turn-start", Prompt: ""},
		{Name: "turn-start", Prompt: ""},
		{Name: "prompt-update", Prompt: "third prompt"},
		{Name: "prompt-update", Prompt: "second prompt"},
		{Name: "turn-start", Prompt: ""},
		{Name: "turn-start", Prompt: ""},
		{Name: "prompt-update", Prompt: "survives session interleaving"},
		{Name: "turn-start", Prompt: ""},
		{Name: "turn-start", Prompt: ""},
	}
	t.Logf("captured prompt hooks: %+v", calls)
	if !slices.Equal(calls, want) {
		t.Fatalf("prompt hooks = %+v, want %+v", calls, want)
	}
}

func TestInstallHooks_ForceReinstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// First install
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("first install failed: %v", err)
	}

	// Force reinstall
	count, err := ag.InstallHooks(context.Background(), true)
	if err != nil {
		t.Fatalf("force install failed: %v", err)
	}
	if count != 1 {
		t.Errorf("force install: expected 1, got %d", count)
	}
}

func TestInstallHooks_RewritesWhenContentDiffers(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	count, err := ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	if count != 1 {
		t.Errorf("first install: expected 1, got %d", count)
	}

	// Seed the render the removed local-dev mode used to write: a plugin that
	// shells out to a script inside the working tree.
	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	legacy := legacyLocalDevRender()
	if err := os.WriteFile(pluginPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file: %v", err)
	}
	if !strings.Contains(string(before), "scripts/entire-dev") {
		t.Fatal("expected seeded legacy content to reference scripts/entire-dev")
	}

	// Reinstalling (content differs) must rewrite it to the binary form.
	count, err = ag.InstallHooks(context.Background(), false)
	if err != nil {
		t.Fatalf("second install failed: %v", err)
	}
	if count != 1 {
		t.Errorf("second install with different content: expected 1, got %d", count)
	}

	after, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file after rewrite: %v", err)
	}
	if strings.Contains(string(after), "scripts/entire-dev") {
		t.Error("expected production content after rewrite, but still references scripts/entire-dev")
	}
	if !strings.Contains(string(after), `exec entire hooks opencode `) {
		t.Error("expected the rewritten plugin to invoke the entire binary")
	}
}

func TestUninstallHooks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Error("plugin file still exists after uninstall")
	}
}

func TestUninstallHooks_NoFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// Should not error when no plugin file exists
	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("uninstall with no file should not error: %v", err)
	}
}

func TestAreHooksInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if hooksInstalledNow(t, ag) {
		t.Error("hooks should not be installed initially")
	}

	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	if !hooksInstalledNow(t, ag) {
		t.Error("hooks should be installed after InstallHooks")
	}

	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	if hooksInstalledNow(t, ag) {
		t.Error("hooks should not be installed after UninstallHooks")
	}
}

// TestCheckHookConfig covers the drift states for the generated plugin file.
// Same exposure as Pi's extension: .opencode/plugins/entire.ts is a generated
// file repos commit so every clone is covered, and a committed copy goes stale
// as the template evolves while AreHooksInstalled keeps returning true.
func TestCheckHookConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := context.Background()
	a := &OpenCodeAgent{}

	if got := a.CheckHookConfig(ctx); got != agent.HooksAbsent {
		t.Errorf("no plugin: CheckHookConfig = %v, want HooksAbsent", got)
	}

	if _, err := a.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksCurrent {
		t.Errorf("fresh install: CheckHookConfig = %v, want HooksCurrent", got)
	}

	path := filepath.Join(dir, ".opencode", pluginDirName, pluginFileName)

	// A plugin left behind by the removed local-dev mode shells out to a script
	// inside the working tree. It must read as ours-but-outdated so it gets
	// rewritten to the binary form, not as current.
	legacy := legacyLocalDevRender()
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hooksInstalledNow(t, a) {
		t.Error("AreHooksInstalled = false; a legacy local-dev plugin is still ours")
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksOutdated {
		t.Errorf("legacy local-dev plugin: CheckHookConfig = %v, want HooksOutdated", got)
	}

	stale := "// " + entireMarker + "\n// an older release wrote this\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hooksInstalledNow(t, a) {
		t.Error("AreHooksInstalled = false; a stale-but-marked plugin is still installed")
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksOutdated {
		t.Errorf("stale plugin: CheckHookConfig = %v, want HooksOutdated", got)
	}

	if err := os.WriteFile(path, []byte("// someone else's plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksAbsent {
		t.Errorf("foreign file: CheckHookConfig = %v, want HooksAbsent", got)
	}
}

// TestCommittedDogfoodPluginIsCurrent guards the copy of this plugin that the
// repo commits for its own use against drifting from the template.
func TestCommittedDogfoodPluginIsCurrent(t *testing.T) {
	t.Parallel()
	testutil.AssertCommittedDogfoodFile(t, ".opencode/plugins/entire.ts", renderPlugin())
}

// TestPlugin_SpawnsHooksUnderNode is the #2014 canary: load the installed plugin
// under Node (Desktop's runtime) and assert hooks actually spawn. Covers both
// async spawn (session-start via session.created) and sync spawnSync (turn-end
// via session.status). Before the child_process fix this threw
// ReferenceError: Bun is not defined inside a swallowed catch, so hooks
// silently never ran.
func TestPlugin_SpawnsHooksUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}

	dir := t.TempDir()
	t.Chdir(dir)

	ag := &OpenCodeAgent{}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath, err := filepath.Abs(filepath.Join(dir, ".opencode", "plugins", "entire.ts"))
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, "hook-ran.txt")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fake `entire` on PATH: record the hook name and drain stdin.
	fakeEntire := filepath.Join(binDir, "entire")
	script := "#!/bin/sh\n" +
		"echo \"$3\" >> " + markerPath + "\n" +
		"cat >/dev/null\n"
	if err := os.WriteFile(fakeEntire, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	driverPath := filepath.Join(dir, "canary.mjs")
	driver := `
import { pathToFileURL } from "node:url"

const pluginPath = process.argv[2]
const { EntirePlugin } = await import(pathToFileURL(pluginPath).href)
const handlers = await EntirePlugin({ directory: process.cwd() })
await handlers.event({
  event: {
    type: "session.created",
    properties: { info: { id: "sess-canary" } },
  },
})
await handlers.event({
  event: {
    type: "session.status",
    properties: { status: { type: "idle" }, sessionID: "sess-canary" },
  },
})
`
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "node", "--experimental-strip-types", driverPath, pluginPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node canary failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("hook never spawned (marker missing): %v\nnode output:\n%s", err, out)
	}
	marker := string(got)
	if !strings.Contains(marker, "session-start") {
		t.Fatalf("expected session-start (async spawn) in marker, got %q\nnode output:\n%s", marker, out)
	}
	if !strings.Contains(marker, "turn-end") {
		t.Fatalf("expected turn-end (sync spawnSync) in marker, got %q\nnode output:\n%s", marker, out)
	}
}

// legacyLocalDevRender reproduces the plugin the removed local-dev mode wrote: the
// same template, but shelling out to a launcher script inside the working tree.
func legacyLocalDevRender() string {
	return strings.ReplaceAll(
		renderPlugin(),
		"command -v entire >/dev/null 2>&1; then exit 0; fi; exec entire hooks opencode",
		"command -v "+testutil.LegacyLocalDevCommand("")+" >/dev/null 2>&1; then exit 0; fi; exec "+testutil.LegacyLocalDevCommand("hooks opencode"),
	)
}

// hooksInstalledNow reports whether the agent's hooks are installed, failing the
// test if it could not tell. Built-in agents read a local config file where
// absent means absent, so an error here is a bug, not a state to tolerate.
func hooksInstalledNow(t *testing.T, ag interface {
	AreHooksInstalled(ctx context.Context) (bool, error)
},
) bool {
	t.Helper()

	installed, err := ag.AreHooksInstalled(context.Background())
	if err != nil {
		t.Fatalf("AreHooksInstalled() error = %v", err)
	}
	return installed
}
