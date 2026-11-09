package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestUseWindowsProductionHooks(t *testing.T) {
	// No t.Parallel(): mutates package-level probe/OS via the test seam.

	shWorks := func(context.Context, string) bool { return true }
	shBroken := func(context.Context, string) bool { return false }

	cases := []struct {
		name  string
		goos  string
		probe func(context.Context, string) bool
		want  bool
	}{
		{"non-windows never uses windows wrappers", "linux", shBroken, false},
		{"windows with working sh keeps sh wrappers", windowsOS, shWorks, false},
		{"windows without working sh uses windows wrappers", windowsOS, shBroken, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := SetWindowsHookProbeForTesting(tc.goos, tc.probe)
			defer restore()
			if got := UseWindowsProductionHooks(context.Background()); got != tc.want {
				t.Fatalf("UseWindowsProductionHooks() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWrapProductionJSONWarningHookCommand(t *testing.T) {
	t.Parallel()

	command := WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", WarningFormatMultiLine)

	if command == "entire hooks claude-code session-start" {
		t.Fatal("expected wrapped command, got raw command")
	}
	if strings.Contains(command, `>&2`) {
		t.Fatalf("claude wrapper should not print warning to stderr, got %q", command)
	}
	if want := `systemMessage`; !strings.Contains(command, want) {
		t.Fatalf("claude wrapper missing systemMessage JSON, got %q", command)
	}
	if !strings.Contains(command, "Entire CLI") {
		t.Fatalf("claude wrapper missing warning text, got %q", command)
	}
	if want := "exec entire hooks claude-code session-start"; !strings.Contains(command, want) {
		t.Fatalf("claude wrapper missing exec target, got %q", command)
	}
}

func TestWrapProductionPlainTextWarningHookCommand(t *testing.T) {
	t.Parallel()

	command := WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid session-start", WarningFormatSingleLine)

	if command == "entire hooks factoryai-droid session-start" {
		t.Fatal("expected wrapped command, got raw command")
	}
	if strings.Contains(command, `>&2`) {
		t.Fatalf("plain text wrapper should not print warning to stderr, got %q", command)
	}
	if !strings.Contains(command, "Entire CLI is enabled but not installed") {
		t.Fatalf("plain text wrapper missing warning text, got %q", command)
	}
	if want := "exec entire hooks factoryai-droid session-start"; !strings.Contains(command, want) {
		t.Fatalf("plain text wrapper missing exec target, got %q", command)
	}
}

func TestWrapWindowsProductionJSONWarningHookCommand(t *testing.T) {
	t.Parallel()

	command := WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", WarningFormatSingleLine)

	if command == "entire hooks codex session-start" {
		t.Fatal("expected wrapped command, got raw command")
	}
	if strings.Contains(command, "sh -c") {
		t.Fatalf("windows wrapper should not use sh, got %q", command)
	}
	if strings.HasPrefix(command, "cmd.exe ") {
		t.Fatalf("windows JSON wrapper should use Codex's existing cmd.exe shell, got %q", command)
	}
	if !strings.Contains(command, "where.exe entire") {
		t.Fatalf("windows wrapper missing PATH guard, got %q", command)
	}
	if !strings.Contains(command, "^\"systemMessage^\"") {
		t.Fatalf("windows wrapper missing escaped systemMessage JSON, got %q", command)
	}
	if !strings.Contains(command, "entire hooks codex session-start") {
		t.Fatalf("windows wrapper missing hook target, got %q", command)
	}
}

func TestWrapWindowsProductionSilentHookCommand(t *testing.T) {
	t.Parallel()

	command := WrapWindowsProductionSilentHookCommand("entire hooks codex stop")

	if command == "entire hooks codex stop" {
		t.Fatal("expected wrapped command, got raw command")
	}
	if strings.Contains(command, "sh -c") {
		t.Fatalf("windows wrapper should not use sh, got %q", command)
	}
	if !strings.HasPrefix(command, "cmd.exe ") {
		t.Fatalf("silent windows wrapper should retain its explicit cmd.exe shell, got %q", command)
	}
	if !strings.Contains(command, "where.exe entire") {
		t.Fatalf("windows wrapper missing PATH guard, got %q", command)
	}
	if strings.Contains(command, "systemMessage") {
		t.Fatalf("silent windows wrapper should not print a warning, got %q", command)
	}
	if !strings.Contains(command, "entire hooks codex stop") {
		t.Fatalf("windows wrapper missing hook target, got %q", command)
	}
}

func TestWrapWindowsProductionPlainTextWarningHookCommandUsesSingleLineWarning(t *testing.T) {
	t.Parallel()

	command := WrapWindowsProductionPlainTextWarningHookCommand("entire hooks codex session-start", WarningFormatMultiLine)

	if strings.HasPrefix(command, "cmd.exe ") {
		t.Fatalf("windows wrapper should use the hook runner's existing cmd.exe shell, got %q", command)
	}
	if strings.Contains(command, "\n") {
		t.Fatalf("windows wrapper should keep warning command single-line, got %q", command)
	}
}

func TestEscapeWindowsCMD_EscapesCmdBlockMetacharacters(t *testing.T) {
	t.Parallel()

	// `%` passes through unescaped: it's a cmd /c command line, not a batch
	// script, so caret-escaping `%` is wrong and a lone `%` is already literal.
	got := escapeWindowsCMD(`^&|<>"()%`)
	want := `^^^&^|^<^>^"^(^)%`
	if got != want {
		t.Fatalf("escapeWindowsCMD() = %q, want %q", got, want)
	}
}

func TestMissingEntireWarning(t *testing.T) {
	t.Parallel()

	if got := MissingEntireWarning(WarningFormatSingleLine); strings.Contains(got, "\n") {
		t.Fatalf("single-line warning should not contain newlines, got %q", got)
	}
	if got := MissingEntireWarning(WarningFormatMultiLine); !strings.Contains(got, "\n") {
		t.Fatalf("multiline warning should contain newlines, got %q", got)
	}
}

func TestIsManagedHookCommand_DirectPrefix(t *testing.T) {
	t.Parallel()

	if !IsManagedHookCommand("entire hooks codex stop") {
		t.Fatal("expected direct entire command to match")
	}
	if !IsManagedHookCommand(`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks codex stop`) {
		t.Fatal("expected local-dev command to match")
	}
}

func TestIsManagedHookCommand_WrappedPrefix(t *testing.T) {
	t.Parallel()

	if !IsManagedHookCommand(WrapProductionSilentHookCommand("entire hooks cursor stop")) {
		t.Fatal("expected wrapped silent command to match")
	}
	if !IsManagedHookCommand(WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", WarningFormatSingleLine)) {
		t.Fatal("expected wrapped json warning command to match")
	}
	if !IsManagedHookCommand(WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid stop", WarningFormatSingleLine)) {
		t.Fatal("expected wrapped plain text warning command to match")
	}
	if !IsManagedHookCommand(WrapWindowsProductionSilentHookCommand("entire hooks codex stop")) {
		t.Fatal("expected windows wrapped silent command to match")
	}
	if !IsManagedHookCommand(WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", WarningFormatSingleLine)) {
		t.Fatal("expected windows wrapped json warning command to match")
	}
	nestedWindowsWrapper := `cmd.exe /d /s /c "where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks codex stop)"`
	if !IsManagedHookCommand(nestedWindowsWrapper) {
		t.Fatal("expected nested windows wrapper to remain managed")
	}
}

// Warning wrappers written before the `entire --version` probe must still read
// as Entire's, and differ from the current form, so an install replaces them.
func TestIsManagedHookCommand_UnprobedWarningWrapper(t *testing.T) {
	t.Parallel()

	unprobed := `sh -c 'if ! command -v entire >/dev/null 2>&1; then printf "%s\n" "{\"systemMessage\":\"\\n\\nEntire CLI is enabled but not installed or not on PATH.\\nInstallation guide: https://docs.entire.io/cli/installation#installation-methods\"}"; exit 0; fi; exec entire hooks claude-code session-start'`
	if !IsManagedHookCommand(unprobed) {
		t.Fatal("expected unprobed warning wrapper to remain managed")
	}
	current := WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", WarningFormatMultiLine)
	if current == unprobed {
		t.Fatal("current wrapper should differ from the unprobed form so installs replace it")
	}
}

func TestIsManagedHookCommand_DoesNotMatchSubstring(t *testing.T) {
	t.Parallel()

	if IsManagedHookCommand(`echo "the entire workflow finished"`) {
		t.Fatal("unexpected match for unrelated substring command")
	}
	if IsManagedHookCommand(`sh -c 'echo "the entire workflow finished"; exit 0'`) {
		t.Fatal("unexpected match for unrelated wrapped shell command")
	}
	if IsManagedHookCommand(`sh -c 'if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec echo "the entire workflow finished"'`) {
		t.Fatal("unexpected match for wrapper that does not exec an Entire hook")
	}
}

// TestDropStaleManagedHooks covers the primitive every agent's stale-hook
// handling routes through, including the case that was silently wrong in two
// agents: a legacy and a current hook present together.
func TestDropStaleManagedHooks(t *testing.T) {
	t.Parallel()

	current := WrapProductionSilentHookCommand("entire hooks x stop")
	legacy := legacyHookCommandPrefixes[0] + "x stop"
	foreign := "echo not ours"

	id := func(s string) string { return s }

	cases := []struct {
		name        string
		entries     []string
		want        []string
		wantKept    []string
		wantDropped bool
	}{
		{"empty", nil, []string{current}, nil, false},
		{"only current is kept untouched", []string{current}, []string{current}, []string{current}, false},
		{"legacy alone is dropped", []string{legacy}, []string{current}, nil, true},
		{
			// The regression: presence of the current hook must not stop the
			// legacy one from being dropped, or both fire.
			name: "legacy alongside current drops only legacy", entries: []string{legacy, current},
			want: []string{current}, wantKept: []string{current}, wantDropped: true,
		},
		{"foreign hooks are never touched", []string{foreign}, []string{current}, []string{foreign}, false},
		{
			name: "foreign survives while legacy is dropped", entries: []string{foreign, legacy},
			want: []string{current}, wantKept: []string{foreign}, wantDropped: true,
		},
		{
			// Several Entire commands can legitimately share one hook list.
			name: "multiple wanted commands all survive", entries: []string{current, "entire hooks x other"},
			want: []string{current, "entire hooks x other"}, wantKept: []string{current, "entire hooks x other"}, wantDropped: false,
		},
		{"empty want set drops every managed hook", []string{current, legacy, foreign}, nil, []string{foreign}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept, dropped := DropStaleManagedHooks(tc.entries, id, tc.want)
			if dropped != tc.wantDropped {
				t.Errorf("dropped = %v, want %v", dropped, tc.wantDropped)
			}
			if len(kept) != len(tc.wantKept) {
				t.Fatalf("kept = %q, want %q", kept, tc.wantKept)
			}
			for i := range kept {
				if kept[i] != tc.wantKept[i] {
					t.Errorf("kept[%d] = %q, want %q", i, kept[i], tc.wantKept[i])
				}
			}
		})
	}
}

// TestDropStaleManagedHooks_NoOpReturnsInputSlice pins that the idempotent path
// does not reallocate — installs call this once per hook list.
func TestDropStaleManagedHooks_NoOpReturnsInputSlice(t *testing.T) {
	t.Parallel()

	entries := []string{"entire hooks x stop"}
	kept, dropped := DropStaleManagedHooks(entries, func(s string) string { return s }, []string{"entire hooks x stop"})
	if dropped {
		t.Fatal("nothing should have been dropped")
	}
	if &kept[0] != &entries[0] {
		t.Error("no-op should hand back the input slice rather than a copy")
	}
}

// TestIsManagedHookCommand_LeavesUserCommandsAlone pins that invoking the entire
// binary for something other than a generated hook is NOT ours.
//
// This predicate decides what gets deleted, and stale hooks are now dropped on
// every install rather than only under --force. A broad `entire ` prefix would
// therefore silently delete a user's own hook that happens to shell out to the
// CLI, on a routine `entire enable`.
func TestIsManagedHookCommand_LeavesUserCommandsAlone(t *testing.T) {
	t.Parallel()

	notOurs := []string{
		"entire search foo | notify-send",
		"entire status --json > /tmp/status",
		"entire why HEAD",
		"entirely-different-tool run",
		// A legacy launcher path, but not driving a generated hook.
		strings.TrimSuffix(legacyHookCommandPrefixes[0], "hooks ") + "search foo",
	}
	for _, command := range notOurs {
		if IsManagedHookCommand(command) {
			t.Errorf("IsManagedHookCommand(%q) = true; a command Entire never generated must not be deletable", command)
		}
	}

	ours := []string{
		"entire hooks copilot-cli agent-stop",
		"entire hooks git pre-push",
		WrapProductionSilentHookCommand("entire hooks cursor stop"),
		legacyHookCommandPrefixes[0] + "cursor stop",
	}
	for _, command := range ours {
		if !IsManagedHookCommand(command) {
			t.Errorf("IsManagedHookCommand(%q) = false; Entire generated this and must recognize it", command)
		}
	}
}

func TestHookHostIsWindows(t *testing.T) {
	// No t.Parallel(): mutates package-level probe/OS via the test seam.

	// The probe reports a working sh throughout: HookHostIsWindows must ignore
	// it. That is the whole difference from UseWindowsProductionHooks, and the
	// reason Factory Droid uses this predicate instead.
	shWorks := func(context.Context, string) bool { return true }

	for _, tc := range []struct {
		goos string
		want bool
	}{
		{"linux", false},
		{"darwin", false},
		{windowsOS, true},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			restore := SetWindowsHookProbeForTesting(tc.goos, shWorks)
			defer restore()
			if got := HookHostIsWindows(); got != tc.want {
				t.Fatalf("HookHostIsWindows() on %s = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}

func TestWrapProductionPlainTextWarningHookCommandForOS(t *testing.T) {
	t.Parallel()

	const command = "entire hooks factoryai-droid stop"

	if got, want := WrapProductionPlainTextWarningHookCommandForOS(command, WarningFormatSingleLine, false),
		WrapProductionPlainTextWarningHookCommand(command, WarningFormatSingleLine); got != want {
		t.Fatalf("useWindows=false = %q, want the sh wrapper %q", got, want)
	}

	windows := WrapProductionPlainTextWarningHookCommandForOS(command, WarningFormatSingleLine, true)
	if want := WrapWindowsProductionPlainTextWarningHookCommand(command, WarningFormatSingleLine); windows != want {
		t.Fatalf("useWindows=true = %q, want the cmd.exe wrapper %q", windows, want)
	}
	if strings.Contains(windows, "sh -c") {
		t.Fatalf("windows wrapper must not invoke sh, got %q", windows)
	}
	// The migration path depends on this: an install that switches wrapper form
	// only replaces the old entry if the new one is still recognised as ours.
	if !IsManagedHookCommand(windows) {
		t.Fatalf("windows wrapper not recognised as a managed hook command: %q", windows)
	}
}

func TestIsManagedHookCommand_RecognisesDirectWindowsSilentWrapper(t *testing.T) {
	// No t.Parallel(): sibling tests in this file mutate the package-level OS seam.
	cmd := WrapWindowsProductionSilentHookCommandDirect("entire hooks antigravity stop")
	if !strings.HasPrefix(cmd, windowsPathOnlyLookup+windowsEntireGuard) {
		t.Fatalf("direct wrapper must start with the bare Windows body, got %q", cmd)
	}
	if strings.HasPrefix(cmd, "cmd.exe") {
		t.Fatalf("direct wrapper must not nest a cmd.exe invocation, got %q", cmd)
	}
	if !IsManagedHookCommand(cmd) {
		t.Fatalf("IsManagedHookCommand must recognise the direct Windows wrapper: %q", cmd)
	}
	kept, dropped := DropStaleManagedHooks([]string{cmd}, func(s string) string { return s }, []string{cmd})
	if dropped || len(kept) != 1 {
		t.Fatalf("a wanted direct-wrapper command must survive DropStaleManagedHooks, kept=%v dropped=%v", kept, dropped)
	}
	_, dropped = DropStaleManagedHooks([]string{cmd}, func(s string) string { return s }, nil)
	if !dropped {
		t.Fatal("an unwanted direct-wrapper command must be dropped as Entire's own")
	}
}

// TestWindowsWrappers_ExactForms pins every Windows wrapper string, since the
// agents write them verbatim into config files and compare them byte for byte.
// Each body sets NoDefaultCurrentDirectoryInExePath before it names a program,
// so cmd.exe resolves `where.exe` and `entire` from PATH and never from the
// repository the hook runs in.
func TestWindowsWrappers_ExactForms(t *testing.T) {
	t.Parallel()

	const hook = "entire hooks x stop"
	const body = `set NoDefaultCurrentDirectoryInExePath=1& pushd %SystemRoot%& where.exe entire >nul 2>nul & if errorlevel 1 `
	cases := []struct {
		name, got, want string
	}{
		{"nested, bare cmd.exe", WrapWindowsProductionSilentHookCommand(hook),
			`cmd.exe /d /s /c "` + body + `(popd& ver>nul) else (popd& entire hooks x stop)"`},
		{"nested, cmd host", WrapWindowsProductionSilentHookCommandForCmdHost(hook),
			`%SystemRoot%\System32\cmd.exe /d /s /c "` + body + `(popd& ver>nul) else (popd& entire hooks x stop)"`},
		{"direct silent", WrapWindowsProductionSilentHookCommandDirect(hook),
			body + `(popd& ver>nul) else (popd& entire hooks x stop)`},
		{"plain-text warning", WrapWindowsProductionPlainTextWarningHookCommand(hook, WarningFormatSingleLine),
			body + `(popd& echo ` + escapeWindowsCMD(windowsPlainTextWarning(WarningFormatSingleLine)) + `) else (popd& entire hooks x stop)`},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, tc.got, tc.want)
		}
	}
	if json := WrapWindowsProductionJSONWarningHookCommand(hook, WarningFormatSingleLine); !strings.HasPrefix(json, body+`(popd& echo {^"systemMessage^":`) {
		t.Errorf("JSON warning wrapper must start with the path-only body, got %q", json)
	}
}

func TestWrapProductionSilentHookCommandForCmdHostOS(t *testing.T) {
	t.Parallel()

	const hook = "entire hooks cursor stop"
	if got, want := WrapProductionSilentHookCommandForCmdHostOS(hook, false), WrapProductionSilentHookCommand(hook); got != want {
		t.Errorf("useWindows=false = %q, want the sh wrapper %q", got, want)
	}
	if got, want := WrapProductionSilentHookCommandForCmdHostOS(hook, true), WrapWindowsProductionSilentHookCommandForCmdHost(hook); got != want {
		t.Errorf("useWindows=true = %q, want %q", got, want)
	}
}

// TestIsManagedHookCommand_WindowsWrapperMigration covers every Windows wrapper
// an older version installed: each must still read as Entire's own, differ from
// what is installed now, and be dropped by DropStaleManagedHooks, so the next
// `entire enable` replaces it and `entire disable` removes it.
func TestIsManagedHookCommand_WindowsWrapperMigration(t *testing.T) {
	t.Parallel()

	const hook = "entire hooks codex stop"
	legacy := []string{
		`cmd.exe /d /s /c "where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks codex stop)"`,
		`where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks codex stop)`,
		`where.exe entire >nul 2>nul & if errorlevel 1 (echo Entire CLI is enabled but not installed or not on PATH.) else (entire hooks codex stop)`,
		`cmd.exe /d /s /c "set NoDefaultCurrentDirectoryInExePath=1& where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks codex stop)"`,
		`%SystemRoot%\System32\cmd.exe /d /s /c "set NoDefaultCurrentDirectoryInExePath=1& where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks codex stop)"`,
		`set NoDefaultCurrentDirectoryInExePath=1& where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (entire hooks codex stop)`,
	}
	current := []string{
		WrapWindowsProductionSilentHookCommand(hook),
		WrapWindowsProductionSilentHookCommandForCmdHost(hook),
		WrapWindowsProductionSilentHookCommandDirect(hook),
		WrapWindowsProductionJSONWarningHookCommand(hook, WarningFormatSingleLine),
		WrapWindowsProductionPlainTextWarningHookCommand(hook, WarningFormatSingleLine),
	}
	id := func(s string) string { return s }
	for _, command := range append(slices.Clone(legacy), current...) {
		if !IsManagedHookCommand(command) {
			t.Errorf("IsManagedHookCommand(%q) = false, want true", command)
		}
	}
	for _, old := range legacy {
		if slices.Contains(current, old) {
			t.Errorf("legacy wrapper %q is still generated; installs would not replace it", old)
		}
	}
	kept, dropped := DropStaleManagedHooks(append(slices.Clone(legacy), current[0]), id, current[:1])
	if !dropped || !slices.Equal(kept, current[:1]) {
		t.Errorf("DropStaleManagedHooks kept %q (dropped=%v), want only the current wrapper", kept, dropped)
	}
	if kept, _ := DropStaleManagedHooks(current, id, nil); len(kept) != 0 {
		t.Errorf("disable must remove every current Windows wrapper, kept %q", kept)
	}
}

func TestIsManagedHookCommand_WindowsWrapperAroundForeignCommand(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		WrapWindowsProductionSilentHookCommand("entire status --json"),
		WrapWindowsProductionSilentHookCommandForCmdHost("my-tool hooks stop"),
		WrapWindowsProductionSilentHookCommandDirect("echo entire hooks"),
		`%SystemRoot%\System32\cmd.exe /d /s /c "echo hi"`,
	} {
		if IsManagedHookCommand(command) {
			t.Errorf("IsManagedHookCommand(%q) = true; it does not run an Entire hook", command)
		}
	}
}

// TestSHHookWrapperProbe_DoesNotSearchWorkingDirectory pins how the sh probe is
// started: outside the directory `entire` runs in, and with cmd.exe told not to
// search its own working directory, so a repository `sh.bat` neither runs nor
// decides which hook wrapper is installed. The Windows execution of the same
// probe is in hook_command_exec_windows_test.go.
func TestSHHookWrapperProbe_DoesNotSearchWorkingDirectory(t *testing.T) {
	t.Parallel()

	cmd := shHookWrapperProbe(t.Context(), shHookWrapperProbeCommand)
	if cmd.Dir != os.TempDir() {
		t.Errorf("probe Dir = %q, want the temp directory %q", cmd.Dir, os.TempDir())
	}
	if !slices.Contains(cmd.Env, "NoDefaultCurrentDirectoryInExePath=1") {
		t.Errorf("probe environment must set NoDefaultCurrentDirectoryInExePath=1, got %q", cmd.Env)
	}
	if got := cmd.Args; !slices.Equal(got, []string{systemCmdExe(), "/d", "/s", "/c", shHookWrapperProbeCommand}) {
		t.Errorf("probe args = %q", got)
	}
}
