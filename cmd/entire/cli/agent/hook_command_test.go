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
		name     string
		goos     string
		localDev bool
		probe    func(context.Context, string) bool
		want     bool
	}{
		{"non-windows never uses windows wrappers", "linux", false, shBroken, false},
		{"localDev never uses windows wrappers", windowsOS, true, shBroken, false},
		{"windows with working sh keeps sh wrappers", windowsOS, false, shWorks, false},
		{"windows without working sh uses windows wrappers", windowsOS, false, shBroken, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := SetWindowsHookProbeForTesting(tc.goos, tc.probe)
			defer restore()
			if got := UseWindowsProductionHooks(context.Background(), tc.localDev); got != tc.want {
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

	prefixes := []string{"entire ", `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go `}

	if !IsManagedHookCommand("entire hooks codex stop", prefixes) {
		t.Fatal("expected direct entire command to match")
	}
	if !IsManagedHookCommand(`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks codex stop`, prefixes) {
		t.Fatal("expected local-dev command to match")
	}
}

func TestIsManagedHookCommand_WrappedPrefix(t *testing.T) {
	t.Parallel()

	prefixes := []string{"entire "}

	if !IsManagedHookCommand(
		WrapProductionSilentHookCommand("entire hooks cursor stop"),
		prefixes,
	) {
		t.Fatal("expected wrapped silent command to match")
	}
	if !IsManagedHookCommand(
		WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", WarningFormatSingleLine),
		prefixes,
	) {
		t.Fatal("expected wrapped json warning command to match")
	}
	if !IsManagedHookCommand(
		WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid stop", WarningFormatSingleLine),
		prefixes,
	) {
		t.Fatal("expected wrapped plain text warning command to match")
	}
	if !IsManagedHookCommand(
		WrapWindowsProductionSilentHookCommand("entire hooks codex stop"),
		prefixes,
	) {
		t.Fatal("expected windows wrapped silent command to match")
	}
	if !IsManagedHookCommand(
		WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", WarningFormatSingleLine),
		prefixes,
	) {
		t.Fatal("expected windows wrapped json warning command to match")
	}
}

func TestIsManagedHookCommand_DoesNotMatchSubstring(t *testing.T) {
	t.Parallel()

	prefixes := []string{"entire ", `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go `}

	if IsManagedHookCommand(`echo "the entire workflow finished"`, prefixes) {
		t.Fatal("unexpected match for unrelated substring command")
	}
	if IsManagedHookCommand(`sh -c 'echo "the entire workflow finished"; exit 0'`, prefixes) {
		t.Fatal("unexpected match for unrelated wrapped shell command")
	}
	if IsManagedHookCommand(`sh -c 'if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec echo "the entire workflow finished"'`, prefixes) {
		t.Fatal("unexpected match for wrapper that does not exec an Entire hook")
	}
}
