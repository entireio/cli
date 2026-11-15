package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/stretchr/testify/require"
)

// setupTestEnv creates a temp dir, sets CWD and CODEX_HOME for test isolation.
// Cannot be parallel (uses t.Chdir and t.Setenv which are process-global).
func setupTestEnv(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	// Installing hooks anchors a process-wide os.Root on the worktree root
	// (worktreedir.OpenAt -> osroot.Shared), which is never closed. Windows
	// cannot remove a directory while a handle to it is open, so the registry
	// must be closed before t.TempDir's RemoveAll — t.Cleanup is LIFO, and
	// TempDir registered its removal first, so this runs before it.
	t.Cleanup(osroot.ResetShared)
	t.Chdir(tempDir)
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, ".codex-home"))
	return tempDir
}

func TestInstallHooks_CreatesHooksJSONOnly(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)

	hooksFile, data := readHooksFile(t, tempDir)

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.SessionEnd, agentpkg.WrapProductionSilentHookCommand("entire hooks codex session-end"), "SessionEnd")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
	assertCodexEntries(t, data)

	// Hooks are enabled by default in Codex, so no .codex/config.toml is
	// written. A TOML file there is actively harmful when the repo lives
	// inside <CODEX_HOME>/agents, where Codex's agent-role scanner rejects
	// it at startup (entireio/cli#842).
	projectConfig := filepath.Join(tempDir, ".codex", "config.toml")
	_, err = os.Stat(projectConfig)
	require.True(t, os.IsNotExist(err), "install must not create .codex/config.toml")
}

// wantCodexEntries is every entry InstallHooks writes, spelled out from the
// shared wrappers rather than from managedHooks, so a change to what Codex gets
// shows up here as a deliberate edit.
func wantCodexEntries() map[string]HookEntry {
	silent := func(verb string, timeout int) HookEntry {
		cmd := "entire hooks codex " + verb
		return HookEntry{
			Type:           "command",
			Command:        agentpkg.WrapProductionSilentHookCommand(cmd),
			CommandWindows: agentpkg.WrapWindowsProductionSilentHookCommand(cmd),
			Timeout:        timeout,
		}
	}
	const start = "entire hooks codex session-start"
	return map[string]HookEntry{
		"SessionStart": {
			Type:           "command",
			Command:        agentpkg.WrapProductionJSONWarningHookCommand(start, agentpkg.WarningFormatSingleLine),
			CommandWindows: agentpkg.WrapWindowsProductionNestedPlainTextWarningHookCommand(start, agentpkg.WarningFormatSingleLine),
			Timeout:        defaultHookTimeoutSec,
		},
		"SessionEnd":       silent("session-end", SessionEndTimeoutSec),
		"UserPromptSubmit": silent("user-prompt-submit", defaultHookTimeoutSec),
		"Stop":             silent("stop", defaultHookTimeoutSec),
		"PostToolUse":      silent("post-tool-use", defaultHookTimeoutSec),
		"SubagentStart":    silent("subagent-start", defaultHookTimeoutSec),
		"SubagentStop":     silent("subagent-stop", defaultHookTimeoutSec),
	}
}

// assertCodexEntries checks that each managed event holds exactly one Entire
// hook, with both commands and the timeout InstallHooks writes.
func assertCodexEntries(t *testing.T, data []byte) {
	t.Helper()
	var topLevel struct {
		Hooks map[string][]MatcherGroup `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &topLevel))
	want := wantCodexEntries()
	require.Len(t, want, len(managedHooks))
	for event, entry := range want {
		var got []HookEntry
		for _, group := range topLevel.Hooks[event] {
			for _, hook := range group.Hooks {
				if isEntireHook(hook.Command) {
					got = append(got, hook)
				}
			}
		}
		require.Len(t, got, 1, "%s should have exactly one Entire hook", event)
		require.Equal(t, entry.Command, got[0].Command, "%s command", event)
		require.Equal(t, entry.CommandWindows, got[0].CommandWindows, "%s commandWindows", event)
		require.Equal(t, entry.Timeout, got[0].Timeout, "%s timeout", event)
	}
}

// Codex picks commandWindows over command when it runs on Windows, so the file
// is the same whichever host or shell writes it: the sh probe that used to pick
// one wrapper for `command` decided by the PATH of the shell `entire enable`
// ran in, which says nothing about the shell Codex runs hooks in (#2884).
func TestInstallHooks_WritesTheSameConfigOnEveryHost(t *testing.T) {
	hosts := []struct {
		name     string
		goos     string
		shWorked bool
	}{
		{"linux", "linux", true},
		{"windows with sh", "windows", true},
		{"windows without sh", "windows", false},
	}
	var first []byte
	for _, host := range hosts {
		t.Run(host.name, func(t *testing.T) {
			tempDir := setupTestEnv(t)
			withCodexHookEnvironment(t, host.goos, host.shWorked)

			count, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
			require.NoError(t, err)
			require.Equal(t, len(managedHooks), count)

			_, data := readHooksFile(t, tempDir)
			assertCodexEntries(t, data)
			if first == nil {
				first = data
				return
			}
			require.Equal(t, string(first), string(data), "hooks.json must not depend on the host")
		})
	}
}

// A hooks.json written by an older release is brought up to date in one
// install: one from a Windows host whose sh probe failed holds cmd.exe syntax in
// `command` (which Codex runs through sh on macOS and Linux), and one from
// anywhere else has no commandWindows (so Codex runs the sh wrapper on Windows
// too). The Entire hook is rewritten in place, so fields the user added to it
// survive; a command_windows it carried is dropped, since Codex refuses a
// handler with both spellings. A second Entire hook for the event is removed.
func TestInstallHooks_MigratesConfigsWrittenBeforeCommandWindows(t *testing.T) {
	cases := []struct {
		name     string
		commands func(h managedHook) []string
	}{
		{"windows probe failed", func(h managedHook) []string { return []string{h.legacyWindows("entire hooks codex " + h.verb)} }},
		{"posix only", func(h managedHook) []string { posix, _ := h.commands(); return []string{posix} }},
		{"both", func(h managedHook) []string {
			posix, _ := h.commands()
			return []string{h.legacyWindows("entire hooks codex " + h.verb), posix}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := setupTestEnv(t)
			events := map[string]any{}
			for _, h := range managedHooks {
				var hooks []any
				for _, command := range tc.commands(h) {
					hooks = append(hooks, map[string]any{
						"type": "command", "command": command, "timeout": h.timeout,
						"statusMessage": "Saving session", "futureField": 1, "command_windows": "old.cmd",
					})
				}
				events[h.event] = []any{map[string]any{"hooks": hooks}}
			}
			seed, err := jsonutil.MarshalIndentWithNewline(map[string]any{"hooks": events}, "", "  ")
			require.NoError(t, err)
			hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
			require.NoError(t, os.MkdirAll(filepath.Dir(hooksPath), 0o750))
			require.NoError(t, os.WriteFile(hooksPath, seed, 0o600))

			count, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
			require.NoError(t, err)
			require.Equal(t, len(managedHooks), count)
			_, data := readHooksFile(t, tempDir)
			assertCodexEntries(t, data)

			var doc struct {
				Hooks map[string][]struct {
					Hooks []map[string]any `json:"hooks"`
				} `json:"hooks"`
			}
			require.NoError(t, json.Unmarshal(data, &doc))
			for _, h := range managedHooks {
				for _, group := range doc.Hooks[h.event] {
					for _, hook := range group.Hooks {
						require.Equal(t, "Saving session", hook["statusMessage"], "%s: user fields survive the upgrade", h.event)
						require.InDelta(t, 1, hook["futureField"], 0, "%s: unknown fields survive the upgrade", h.event)
						require.NotContains(t, hook, "command_windows", "%s: only one spelling of commandWindows", h.event)
					}
				}
			}

			count, err = (&CodexAgent{}).InstallHooks(context.Background(), false)
			require.NoError(t, err)
			require.Zero(t, count, "a migrated config must be current")
		})
	}
}

// An Entire hook found under a matcher moves to the unfiltered group on
// install, keeping its fields, whether it is outdated or already current: left
// under the matcher it would fire only for that tool. The user's own hook in
// that group stays where it is, and doctor then reports the file current.
func TestInstallHooks_UpgradeMovesEntireHookOutOfMatcherGroup(t *testing.T) {
	var post managedHook
	for _, h := range managedHooks {
		if h.event == "PostToolUse" {
			post = h
		}
	}
	posix, windows := post.commands()
	outdated := `{"type":"command","command":` + strconv.Quote(posix) + `,"timeout":30,"statusMessage":"Saving"}`
	current := `{"type":"command","command":` + strconv.Quote(posix) + `,"commandWindows":` + strconv.Quote(windows) +
		`,"timeout":` + strconv.Itoa(post.timeout) + `,"statusMessage":"Saving"}`
	matched := func(entire string) string {
		return `{"matcher":"apply_patch","hooks":[{"type":"command","command":"./audit.sh"},` + entire + `]}`
	}
	for _, tc := range []struct{ name, seed string }{
		{"outdated", `{"hooks":{"PostToolUse":[` + matched(outdated) + `]}}`},
		{"current, beside an unfiltered outdated one", `{"hooks":{"PostToolUse":[` + matched(current) +
			`,{"hooks":[{"type":"command","command":` + strconv.Quote(posix) + `,"timeout":30}]}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := setupTestEnv(t)
			hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
			require.NoError(t, os.MkdirAll(filepath.Dir(hooksPath), 0o750))
			require.NoError(t, os.WriteFile(hooksPath, []byte(tc.seed), 0o600))

			_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
			require.NoError(t, err)
			_, data := readHooksFile(t, tempDir)
			assertCodexEntries(t, data)
			var doc struct {
				Hooks map[string][]struct {
					Matcher *string          `json:"matcher"`
					Hooks   []map[string]any `json:"hooks"`
				} `json:"hooks"`
			}
			require.NoError(t, json.Unmarshal(data, &doc))
			groups := doc.Hooks["PostToolUse"]
			require.Len(t, groups, 2)
			require.NotNil(t, groups[0].Matcher)
			require.Equal(t, []map[string]any{{"type": "command", "command": "./audit.sh"}}, groups[0].Hooks)
			require.Nil(t, groups[1].Matcher)
			require.Len(t, groups[1].Hooks, 1)
			require.Equal(t, "Saving", groups[1].Hooks[0]["statusMessage"])
			require.Equal(t, agentpkg.HooksCurrent, (&CodexAgent{}).CheckHookConfig(context.Background()))
		})
	}
}

// Doctor counts Entire's hook as current only in an unfiltered group, the
// place install leaves it, so the two agree.
func TestManagedHookIsCurrent_RequiresUnfilteredGroup(t *testing.T) {
	t.Parallel()
	want := managedEntry(managedHooks[0])
	matcher := "apply_patch"
	require.True(t, managedHookIsCurrent([]MatcherGroup{{Hooks: []HookEntry{want}}}, want))
	require.False(t, managedHookIsCurrent([]MatcherGroup{{Matcher: &matcher, Hooks: []HookEntry{want}}}, want))
}

// Reported on #2884: `entire doctor` flagged hooks.json as out of date
// depending on whether it ran from PowerShell or Git Bash, because the expected
// `command` came from the same sh probe. The expected entry no longer depends
// on it.
func TestCheckHookConfig_CurrentRegardlessOfShProbe(t *testing.T) {
	setupTestEnv(t)
	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)

	for _, host := range []struct {
		goos     string
		shWorked bool
	}{{"linux", true}, {"windows", true}, {"windows", false}} {
		restore := agentpkg.SetWindowsHookProbeForTesting(host.goos, func(context.Context, string) bool { return host.shWorked })
		state := (&CodexAgent{}).CheckHookConfig(context.Background())
		restore()
		require.Equal(t, agentpkg.HooksCurrent, state, "goos=%s sh=%v", host.goos, host.shWorked)
	}
}

// Every commandWindows Entire writes must parse the same under both shells
// Codex can run it in on Windows. That holds only while the whole cmd.exe line
// sits in one double-quoted string with nothing inside it that PowerShell
// (`"`, `$`, backtick) would act on, and it must look entire up on PATH only,
// never in the checkout; see agent.WrapWindowsProductionSilentHookCommand. The
// one variable allowed is the guard's %SystemRoot%: the outer cmd.exe expands
// it and PowerShell leaves it to the inner cmd.exe, which ends in the same
// directory. Any other % would be expanded under one shell only. Execution
// under both shells is covered on Windows by TestWindowsNestedWrappers_CodexShells.
func TestManagedHooks_WindowsCommandsAreShellNeutral(t *testing.T) {
	t.Parallel()
	const prefix = `cmd.exe /d /s /c "`
	for _, h := range managedHooks {
		_, windows := h.commands()
		require.True(t, strings.HasPrefix(windows, prefix), "%s: %s", h.event, windows)
		require.True(t, strings.HasSuffix(windows, `"`), "%s: %s", h.event, windows)
		body := strings.TrimSuffix(strings.TrimPrefix(windows, prefix), `"`)
		require.NotContainsf(t, body, `"`, "%s: a quote inside the cmd.exe line ends PowerShell's string: %s", h.event, windows)
		require.NotContainsf(t, body, `$`, "%s: PowerShell expands $ inside double quotes: %s", h.event, windows)
		require.NotContainsf(t, body, "`", "%s: backtick is PowerShell's escape: %s", h.event, windows)
		require.NotContainsf(t, strings.ReplaceAll(body, `%SystemRoot%`, ""), `%`, "%s: only one of the two shells would expand this %%: %s", h.event, windows)
		require.Truef(t, strings.HasPrefix(body, "set NoDefaultCurrentDirectoryInExePath=1& "), "%s: cmd.exe must not look for entire in the checkout: %s", h.event, windows)
		require.True(t, agentpkg.IsManagedHookCommand(windows), "%s: Entire must recognize its own Windows command", h.event)
	}
}

// A user's own hooks keep every field Codex supports, and any it adds later,
// through an install and an uninstall: the hook entry model used to know only
// type, command and timeout, so rewriting the file dropped a commandWindows,
// statusMessage, async flag or an mcp_tool handler's server and tool (#2884).
func TestInstallAndUninstallHooks_PreserveUserHookFields(t *testing.T) {
	tempDir := setupTestEnv(t)
	userStop := `{"matcher":null,"hooks":[` +
		`{"type":"command","command":"./check.sh","commandWindows":"powershell -File check.ps1","timeout":0,"statusMessage":"Checking","async":true},` +
		`{"type":"command","command":"./lint.sh","command_windows":"lint.cmd","additionalContextLimit":0,"futureField":{"x":1}}` +
		`],"futureGroupField":"kept"}`
	userPost := `{"matcher":"apply_patch","hooks":[{"type":"mcp_tool","server":"audit","tool":"record","input":{"k":"v"},"timeout":5}]}`
	seed := `{"hooks":{"Stop":[` + userStop + `],"PostToolUse":[` + userPost + `]},"topLevelSetting":true}`
	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(hooksPath), 0o750))
	require.NoError(t, os.WriteFile(hooksPath, []byte(seed), 0o600))

	userGroups := func(t *testing.T, data []byte) map[string][]any {
		t.Helper()
		var doc struct {
			Hooks map[string][]map[string]any `json:"hooks"`
			Top   bool                        `json:"topLevelSetting"`
		}
		require.NoError(t, json.Unmarshal(data, &doc))
		require.True(t, doc.Top, "top-level settings must survive")
		out := map[string][]any{}
		for event, groups := range doc.Hooks {
			for _, group := range groups {
				hooks, ok := group["hooks"].([]any)
				require.True(t, ok, "%s: hooks is not a list", event)
				var kept []any
				for _, hook := range hooks {
					fields, ok := hook.(map[string]any)
					require.True(t, ok, "%s: hook is not an object", event)
					if command, isString := fields["command"].(string); !isString || !agentpkg.IsManagedHookCommand(command) {
						kept = append(kept, hook)
					}
				}
				if len(kept) > 0 {
					group["hooks"] = kept
					out[event] = append(out[event], group)
				}
			}
		}
		return out
	}
	var want map[string][]any
	{
		var doc struct {
			Hooks map[string][]any `json:"hooks"`
		}
		require.NoError(t, json.Unmarshal([]byte(seed), &doc))
		want = doc.Hooks
	}

	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	_, data := readHooksFile(t, tempDir)
	assertCodexEntries(t, data)
	require.Equal(t, want, userGroups(t, data), "install must leave user hooks as written, an explicit zero timeout included")

	require.NoError(t, (&CodexAgent{}).UninstallHooks(context.Background()))
	data, err = os.ReadFile(hooksPath)
	require.NoError(t, err)
	require.Equal(t, want, userGroups(t, data), "uninstall must leave user hooks as written")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	count1, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count1)

	count2, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, 0, count2)
}

func TestInstallHooks_RejectsOversizedHooksFile(t *testing.T) {
	tempDir := setupTestEnv(t)
	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(hooksPath), 0o750))
	contents := `{"padding":"` + strings.Repeat("x", maxHooksFileBytes) + `"}`
	require.NoError(t, os.WriteFile(hooksPath, []byte(contents), 0o600))

	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.ErrorContains(t, err, "exceeds 1048576 bytes")
}

func TestInstallAndUninstallHooks_RejectRedirectedTargets(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("symlink creation is not generally available on Windows")
	}
	tempDir := setupTestEnv(t)
	outside := t.TempDir()
	outsideHooks := filepath.Join(outside, HooksFileName)
	require.NoError(t, os.WriteFile(outsideHooks, []byte(`{"keep":true}`), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(tempDir, ".codex")))

	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.Error(t, err)
	data, readErr := os.ReadFile(outsideHooks)
	require.NoError(t, readErr)
	require.Equal(t, `{"keep":true}`, string(data))

	require.Error(t, (&CodexAgent{}).UninstallHooks(context.Background()))
	data, readErr = os.ReadFile(outsideHooks)
	require.NoError(t, readErr)
	require.Equal(t, `{"keep":true}`, string(data))
}

func TestInstallAndUninstallHooks_RejectRedirectedHooksFile(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("symlink creation is not generally available on Windows")
	}
	tempDir := setupTestEnv(t)
	outside := t.TempDir()
	outsideHooks := filepath.Join(outside, HooksFileName)
	require.NoError(t, os.WriteFile(outsideHooks, []byte(`{"keep":true}`), 0o600))
	projectDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.Mkdir(projectDir, 0o750))
	require.NoError(t, os.Symlink(outsideHooks, filepath.Join(projectDir, HooksFileName)))

	_, err := (&CodexAgent{}).InstallHooks(context.Background(), false)
	require.Error(t, err)
	require.Error(t, (&CodexAgent{}).UninstallHooks(context.Background()))
	data, readErr := os.ReadFile(outsideHooks)
	require.NoError(t, readErr)
	require.Equal(t, `{"keep":true}`, string(data))
}

func TestInstallHooks_ReplacesLegacyLocalDevHook(t *testing.T) {
	tempDir := setupTestEnv(t)
	ctx := context.Background()
	ag := &CodexAgent{}

	testutil.AssertLegacyHookReplaced(t,
		filepath.Join(tempDir, ".codex", HooksFileName),
		agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"),
		testutil.LegacyLocalDevCommand("hooks codex stop"),
		func() {
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
		})
}

func TestInstallHooks_Force(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	count, err := ag.InstallHooks(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)
}

// Codex clamps SessionEnd handlers to SESSION_END_MAX_TIMEOUT_SEC = 3 and
// prints "clamping SessionEnd hook timeout" at every startup when a config asks
// for more, so SessionEnd must be installed at exactly the ceiling while the
// between-turn hooks keep the standard timeout.
func TestInstallHooks_SessionEndUsesCodexTimeoutCeiling(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	hooksFile, _ := readHooksFile(t, tempDir)

	require.Equal(t, SessionEndTimeoutSec, entireHookTimeout(t, hooksFile.Hooks.SessionEnd, "SessionEnd"))
	require.Equal(t, defaultHookTimeoutSec, entireHookTimeout(t, hooksFile.Hooks.Stop, "Stop"))
}

// A SessionEnd hook left behind by an older Entire carries the 30s default,
// which makes Codex warn on every startup. Reinstalling must rewrite it rather
// than treat the command match alone as up to date.
func TestInstallHooks_RewritesSessionEndWithStaleTimeout(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	staleCommand := agentpkg.WrapProductionSilentHookCommand("entire hooks codex session-end")
	stale := HooksFile{Hooks: HookEvents{
		SessionEnd: []MatcherGroup{{
			Hooks: []HookEntry{{Type: "command", Command: staleCommand, Timeout: 30}},
		}},
	}}
	staleData, err := json.Marshal(stale)
	require.NoError(t, err)
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, staleData, 0o600))

	ag := &CodexAgent{}
	_, err = ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	hooksFile, _ := readHooksFile(t, tempDir)

	require.Equal(t, SessionEndTimeoutSec, entireHookTimeout(t, hooksFile.Hooks.SessionEnd, "SessionEnd"))
}

// readHooksFile reads and parses .codex/hooks.json under repoRoot, returning
// both the parsed form and the raw bytes (some assertions check the literal
// text, e.g. that no POSIX shell wrapper leaked into a Windows config).
func readHooksFile(t *testing.T, repoRoot string) (HooksFile, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, ".codex", HooksFileName))
	require.NoError(t, err)
	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))
	return hooksFile, data
}

// entireHookTimeout returns the timeout of the single Entire-managed hook in
// groups, failing if there is not exactly one.
func entireHookTimeout(t *testing.T, groups []MatcherGroup, label string) int {
	t.Helper()
	var timeouts []int
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if isEntireHook(hook.Command) {
				timeouts = append(timeouts, hook.Timeout)
			}
		}
	}
	require.Len(t, timeouts, 1, "%s should have exactly one Entire hook", label)
	return timeouts[0]
}

func TestUninstallHooks(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	err = ag.UninstallHooks(context.Background())
	require.NoError(t, err)

	installed, hooksErr := ag.AreHooksInstalled(context.Background())
	require.NoError(t, hooksErr)
	require.False(t, installed)
}

// TestUninstallHooks_UnreadableHooksFileErrors pins the absent-vs-unreadable
// split: an absent hooks.json means nothing to uninstall, but a read error
// must surface instead of reporting success with hooks still on disk. The
// hooks path is created as a directory so os.ReadFile fails with a
// non-ErrNotExist error on every platform.
func TestUninstallHooks_UnreadableHooksFileErrors(t *testing.T) {
	tempDir := setupTestEnv(t)

	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, ".codex", HooksFileName), 0o755))

	require.Error(t, (&CodexAgent{}).UninstallHooks(context.Background()),
		"UninstallHooks() must error for an unreadable hooks file")
}

func TestUninstallHooks_PreservesUserHookContainingEntireSubstring(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"Stop": [
				{
					"matcher": null,
					"hooks": [
						{"type": "command", "command": "echo \"the entire workflow finished\""}
					]
				}
			]
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	err = ag.UninstallHooks(context.Background())
	require.NoError(t, err)

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.Contains(t, string(data), `echo \"the entire workflow finished\"`)
	require.NotContains(t, string(data), "entire hooks codex stop")
}

func TestAreHooksInstalled_NoFile(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}
	installed, hooksErr := ag.AreHooksInstalled(context.Background())
	require.NoError(t, hooksErr)
	require.False(t, installed)
}

func TestAreHooksInstalled_WithHooks(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	installed, hooksErr := ag.AreHooksInstalled(context.Background())
	require.NoError(t, hooksErr)
	require.True(t, installed)
}

func TestAreHooksInstalled_PartialHooks(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(`{
		"hooks": {
			"Stop": [
				{
					"matcher": null,
					"hooks": [
						{"type": "command", "command": "entire hooks codex stop", "timeout": 30}
					]
				}
			]
		}
	}`), 0o600))

	ag := &CodexAgent{}
	installed, hooksErr := ag.AreHooksInstalled(context.Background())
	require.NoError(t, hooksErr)
	require.False(t, installed)
}

// TestAreHooksInstalled_PreSessionEndInstall — a user who enabled Codex before
// SessionEnd and the subagent hooks joined the install set still counts as
// installed, so Codex keeps
// appearing in `entire status` and the agent pickers instead of vanishing until
// they re-run enable. The gap is drift, and missingEntireHooks reports it.
func TestAreHooksInstalled_PreSessionEndInstall(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(`{
		"hooks": {
			"SessionStart": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex session-start", "timeout": 30}]}],
			"UserPromptSubmit": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex user-prompt-submit", "timeout": 30}]}],
			"Stop": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex stop", "timeout": 30}]}],
			"PostToolUse": [{"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex post-tool-use", "timeout": 30}]}]
		}
	}`), 0o600))

	ag := &CodexAgent{}
	installed, hooksErr := ag.AreHooksInstalled(context.Background())
	require.NoError(t, hooksErr)
	require.True(t, installed)
	require.Equal(t, []string{"session_end", "subagent_start", "subagent_stop"}, missingEntireHooks(tempDir))
}

func TestInstallHooks_PreservesExistingHooksJSON(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"PreToolUse": [
				{
					"matcher": "^Bash$",
					"hooks": [
						{"type": "command", "command": "my-custom-hook"}
					]
				}
			]
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(existingConfig), 0o600))

	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(codexDir, HooksFileName))
	require.NoError(t, err)
	require.Contains(t, string(data), "my-custom-hook")
	require.Contains(t, string(data), "entire hooks codex stop")
}

func TestInstallHooks_ErrorsOnMalformedManagedHook(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"SessionStart": {"not": "an array"},
			"PreToolUse": [
				{
					"matcher": "^Bash$",
					"hooks": [
						{"type": "command", "command": "my-custom-hook"}
					]
				}
			]
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse SessionStart hooks")

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.JSONEq(t, existingConfig, string(data))
}

func TestUninstallHooks_ErrorsOnMalformedManagedHook(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"Stop": {"not": "an array"}
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	err := ag.UninstallHooks(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse Stop hooks")

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.JSONEq(t, existingConfig, string(data))
}

func TestInstallHooks_DoesNotModifyUserConfig(t *testing.T) {
	setupTestEnv(t)
	codexHome := os.Getenv("CODEX_HOME")

	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	existingConfig := "model = \"gpt-4.1\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false)
	require.NoError(t, err)

	configData, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	require.NoError(t, err)
	require.Contains(t, string(configData), "model = \"gpt-4.1\"")
	require.NotContains(t, string(configData), `trust_level = "trusted"`)
}

// TestInstallHooks_LeavesExistingLocalConfigUntouched pins that install
// never reads, rewrites, or deletes a project-local .codex/config.toml —
// whether it's a user's own file or a feature-flag leftover from an older
// entire version. The CLI no longer manages that file at all; leftovers
// under <CODEX_HOME>/agents must be removed manually (entireio/cli#842).
func TestInstallHooks_LeavesExistingLocalConfigUntouched(t *testing.T) {
	contents := map[string]string{
		"old entire leftover": "[features]\nhooks = true\n",
		"user file":           "model = \"gpt-4.1\"\n",
	}
	for name, content := range contents {
		t.Run(name, func(t *testing.T) {
			tempDir := setupTestEnv(t)

			codexDir := filepath.Join(tempDir, ".codex")
			require.NoError(t, os.MkdirAll(codexDir, 0o750))
			configPath := filepath.Join(codexDir, "config.toml")
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0o600))

			ag := &CodexAgent{}
			_, err := ag.InstallHooks(context.Background(), false)
			require.NoError(t, err)

			data, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.Equal(t, content, string(data), "install must not touch an existing .codex/config.toml")
		})
	}
}

// assertHookCommand verifies that one of the hook entries in groups contains the expected command.
func assertHookCommand(t *testing.T, groups []MatcherGroup, expectedCmd, label string) {
	t.Helper()
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Command == expectedCmd {
				return
			}
		}
	}
	t.Errorf("%s: expected hook command not found: %s", label, expectedCmd)
}

func withCodexHookEnvironment(t *testing.T, goos string, wrapperWorks bool) {
	t.Helper()
	withCodexHookEnvironmentFunc(t, goos, func(context.Context, string) bool {
		return wrapperWorks
	})
}

func withCodexHookEnvironmentFunc(t *testing.T, goos string, wrapperWorks func(context.Context, string) bool) {
	t.Helper()
	t.Cleanup(agentpkg.SetWindowsHookProbeForTesting(goos, wrapperWorks))
}

// TestInstallHooks_DropsLegacyHookAlongsideCurrent is the regression test for
// syncHookCommand returning early when the current command was already present,
// which left a legacy local-dev hook beside it so both fired.
func TestInstallHooks_DropsLegacyHookAlongsideCurrent(t *testing.T) {
	tempDir := setupTestEnv(t)
	ctx := context.Background()
	ag := &CodexAgent{}

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	current := agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop")
	legacy := testutil.LegacyLocalDevCommand("hooks codex stop")

	testutil.AssertStaleHookDroppedAlongsideCurrent(t, hooksPath, current, legacy,
		func() {
			// Install, then append the legacy hook into the same Stop group.
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("seed InstallHooks() error = %v", err)
			}
			raw, err := os.ReadFile(hooksPath)
			require.NoError(t, err)
			var topLevel map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &topLevel))
			var rawHooks map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(topLevel["hooks"], &rawHooks))
			var stop []MatcherGroup
			require.NoError(t, parseHookType(rawHooks, "Stop", &stop))
			require.NotEmpty(t, stop)
			stop[0].Hooks = append(stop[0].Hooks, HookEntry{Type: "command", Command: legacy, Timeout: 30})
			marshalHookType(rawHooks, "Stop", stop)
			// Same marshaller InstallHooks uses: the production command contains
			// `>`, which encoding/json would escape to >.
			hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
			require.NoError(t, err)
			topLevel["hooks"] = hooksJSON
			out, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(hooksPath, out, 0o600))
		},
		func() {
			if _, err := ag.InstallHooks(ctx, false); err != nil {
				t.Fatalf("InstallHooks() error = %v", err)
			}
		})
}

// TestCommittedDogfoodHooksIsCurrent guards this repo's own committed agent config against drifting from what
// InstallHooks writes. A stale committed config is how the pi extension ended up
// invoking a launcher script that had been deleted.
func TestCommittedDogfoodHooksIsCurrent(t *testing.T) {
	testutil.AssertCommittedDogfoodConfigStable(t, ".codex/hooks.json", func(t *testing.T, dir string) (int, error) {
		t.Helper()
		t.Chdir(dir)
		return (&CodexAgent{}).InstallHooks(context.Background(), false)
	})
}

// readHooksFileForMutation is the read behind InstallHooks, UninstallHooks and
// AreHooksInstalled. .codex/hooks.json is committed to the repository, so its
// size, its type and what it points at all arrive through a pull request.
//
// The symlink case guards this function's own OpenNoFollow: swapping it for a
// plain root.Open makes that subtest fail. It does not distinguish this
// function from the cfg.Read() it replaced, which refused links too.
func TestReadHooksFileForMutation(t *testing.T) {
	newConfig := func(t *testing.T, tempDir string) *agentpkg.HookConfigFile {
		t.Helper()
		cfg, err := agentpkg.OpenHookConfig(tempDir, ".codex/"+HooksFileName)
		require.NoError(t, err)
		return cfg
	}

	t.Run("an absent file is reported absent, not as an error", func(t *testing.T) {
		tempDir := setupTestEnv(t)
		data, exists, err := readHooksFileForMutation(newConfig(t, tempDir))
		require.NoError(t, err)
		require.False(t, exists)
		require.Nil(t, data)
	})

	// The refusal is what this pins, not its ordering. The bound now comes off
	// the stat size before any of the file is read, and the outcome of that is
	// indistinguishable from the post-read length check it replaced, so no
	// assertion here can tell them apart. What would need a huge sparse file to
	// observe is the memory the old shape spent getting to the same answer.
	t.Run("a file over the bound is refused", func(t *testing.T) {
		tempDir := setupTestEnv(t)
		hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
		require.NoError(t, os.MkdirAll(filepath.Dir(hooksPath), 0o750))
		require.NoError(t, os.WriteFile(hooksPath, []byte(strings.Repeat("x", maxHooksFileBytes+1)), 0o600))

		_, _, err := readHooksFileForMutation(newConfig(t, tempDir))
		require.ErrorContains(t, err, "exceeds 1048576 bytes")
	})

	t.Run("a non-regular file at the hooks path is refused", func(t *testing.T) {
		tempDir := setupTestEnv(t)
		require.NoError(t, os.MkdirAll(filepath.Join(tempDir, ".codex", HooksFileName), 0o750))

		_, _, err := readHooksFileForMutation(newConfig(t, tempDir))
		require.Error(t, err)
	})

	t.Run("a symlink that stays inside the worktree is refused", func(t *testing.T) {
		if runtime.GOOS == testWindowsOS {
			t.Skip("symlink creation is not generally available on Windows")
		}
		tempDir := setupTestEnv(t)
		require.NoError(t, os.MkdirAll(filepath.Join(tempDir, ".codex"), 0o750))
		victim := filepath.Join(tempDir, "victim.json")
		require.NoError(t, os.WriteFile(victim, []byte(`{"keep":true}`), 0o600))
		// Relative and in-repo, so os.Root confinement permits it. Refusing it
		// is this function's own doing.
		require.NoError(t, os.Symlink(filepath.Join("..", "victim.json"),
			filepath.Join(tempDir, ".codex", HooksFileName)))

		_, _, err := readHooksFileForMutation(newConfig(t, tempDir))
		require.ErrorIs(t, err, osroot.ErrSymlinkedPath)

		data, readErr := os.ReadFile(victim)
		require.NoError(t, readErr)
		require.Equal(t, `{"keep":true}`, string(data), "the link target must not be read as ours")
	})
}

// missingEntireHooks reports the managed Codex events that are not present in
// a repository-local hooks file.
func missingEntireHooks(repoRoot string) []string {
	document, err := readHooksDocument(filepath.Join(repoRoot, ".codex", HooksFileName))
	if err != nil || !document.exists {
		return nil
	}
	var missing []string
	for _, hook := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(document.rawHooks, hook.event, &groups); err != nil {
			return nil
		}
		if !hasEntireHook(groups) {
			missing = append(missing, hook.label)
		}
	}
	return missing
}
