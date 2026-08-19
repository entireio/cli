package agent

import (
	"context"
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
