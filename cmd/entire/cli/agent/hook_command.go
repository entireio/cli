package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// GeneratedHookFileState reports hook-config drift for agents whose entire
// Entire integration is one generated file in the repo (Pi's extension,
// OpenCode's plugin), for use by HookFreshness implementations.
//
// The file counts as ours only if it contains marker, and as current only if
// it matches one of renders — the outputs InstallHooks could have written.
// Pass both the production and local-dev renders: they differ only in the
// substituted entire command, and CheckHookConfig cannot know which mode
// installed the file. Matching on content rather than a stamped version keeps
// this honest for free — any edit to the embedded template makes installed
// copies read as outdated without anyone remembering to bump anything.
//
// Line endings are normalized before comparing. These files are generated with
// LF but are typically committed, so on Windows a checkout under the default
// core.autocrlf=true hands us CRLF and byte equality never holds. That would
// report permanent drift the user cannot clear: InstallHooks writes LF back,
// and the next checkout converts it to CRLF again.
//
// A file that exists but lacks marker reads as HooksAbsent, not HooksOutdated:
// InstallHooks refuses to overwrite a foreign file at our path, so there is no
// Entire config there for us to call stale.
func GeneratedHookFileState(path, marker string, renders ...string) HookConfigState {
	data, err := os.ReadFile(path) //nolint:gosec // path constructed from validated repo root
	if err != nil {
		return HooksAbsent
	}
	content := string(data)
	if !strings.Contains(content, marker) {
		return HooksAbsent
	}
	normalized := normalizeLineEndings(content)
	for _, render := range renders {
		if normalized == normalizeLineEndings(render) {
			return HooksCurrent
		}
	}
	return HooksOutdated
}

// normalizeLineEndings rewrites CRLF to LF so content comparison survives a
// Windows checkout. Lone CR is left alone: no generator here emits it, and
// touching it would rewrite content that is genuinely different.
func normalizeLineEndings(s string) string {
	if !strings.Contains(s, "\r\n") {
		return s
	}
	return strings.ReplaceAll(s, "\r\n", "\n")
}

type WarningFormat int

const (
	WarningFormatSingleLine WarningFormat = iota + 1
	WarningFormatMultiLine
)

func MissingEntireWarning(format WarningFormat) string {
	switch format {
	case WarningFormatSingleLine:
		return "Entire CLI is enabled but not installed or not on PATH. Installation guide: https://docs.entire.io/cli/installation#installation-methods"
	case WarningFormatMultiLine:
		return "\n\nEntire CLI is enabled but not installed or not on PATH.\nInstallation guide: https://docs.entire.io/cli/installation#installation-methods"
	default:
		return MissingEntireWarning(WarningFormatSingleLine)
	}
}

// LocalDevHookScript is the local-development hook launcher, with the repo root
// resolved at hook runtime via git. It points at scripts/entire-dev, which
// compiles the CLI on demand and falls back to the entire binary on PATH when
// the tree does not build (e.g. mid merge-conflict-fix). Agents that locate the
// repo root with `git rev-parse` build their local-dev command prefix from
// this; claude-code uses ${CLAUDE_PROJECT_DIR} and defines its own prefix.
const LocalDevHookScript = `"$(git rev-parse --show-toplevel)"/scripts/entire-dev`

// WrapProductionSilentHookCommand exits successfully without output when the
// Entire CLI is missing from PATH.
func WrapProductionSilentHookCommand(command string) string {
	return fmt.Sprintf(
		`sh -c 'if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec %s'`,
		command,
	)
}

// WrapProductionJSONWarningHookCommand emits a JSON hook response with a
// systemMessage field on stdout when the Entire CLI is missing from PATH.
func WrapProductionJSONWarningHookCommand(command string, format WarningFormat) string {
	payload, err := jsonutil.MarshalWithNoHTMLEscape(struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}{
		SystemMessage: MissingEntireWarning(format),
	})
	if err != nil {
		// Fallback to plain text on stdout if JSON payload construction somehow fails.
		return WrapProductionPlainTextWarningHookCommand(command, format)
	}

	return fmt.Sprintf(
		`sh -c 'if ! command -v entire >/dev/null 2>&1; then printf "%%s\n" %q; exit 0; fi; exec %s'`,
		string(payload),
		command,
	)
}

// The Windows wrappers use an `if errorlevel 1 (…) else (<command>)` form
// rather than a `where … || <else> & <command>` form. This keeps the wrapped
// command INSIDE the else branch, so there is no unconditional trailing
// command to fall through to and correctness does NOT depend on `exit /b`
// aborting the whole `cmd /c` line — behavior that is underspecified when the
// `exit /b` sits inside a parenthesized block. Semantics:
//   - entire present  → `where.exe` succeeds (errorlevel 0) → else branch runs
//     the wrapped command and its exit code propagates (parity with the POSIX
//     `exec entire …` form).
//   - entire absent   → `where.exe` fails (errorlevel ≥ 1) → the if branch runs
//     (silently via `ver>nul`, or echoing the warning) and the line exits 0.

// WrapWindowsProductionSilentHookCommand exits successfully without output when
// the Entire CLI is missing from PATH. It avoids sh so Codex hooks still work
// from native Windows shells.
func WrapWindowsProductionSilentHookCommand(command string) string {
	return fmt.Sprintf(
		`cmd.exe /d /s /c "where.exe entire >nul 2>nul & if errorlevel 1 (ver>nul) else (%s)"`,
		command,
	)
}

// WrapWindowsProductionJSONWarningHookCommand emits a JSON hook response with a
// systemMessage field on stdout when the Entire CLI is missing from PATH. It
// avoids sh so Codex hooks still work from native Windows shells. Codex already
// runs hook commands through cmd.exe /C, so this JSON-bearing command uses that
// shell directly instead of adding a second quote-parsing layer.
func WrapWindowsProductionJSONWarningHookCommand(command string, format WarningFormat) string {
	payload, err := jsonutil.MarshalWithNoHTMLEscape(struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}{
		SystemMessage: MissingEntireWarning(format),
	})
	if err != nil {
		return WrapWindowsProductionPlainTextWarningHookCommand(command, format)
	}

	return fmt.Sprintf(
		`where.exe entire >nul 2>nul & if errorlevel 1 (echo %s) else (%s)`,
		escapeWindowsCMD(string(payload)),
		command,
	)
}

// WrapWindowsProductionPlainTextWarningHookCommand is the direct-shell fallback
// for WrapWindowsProductionJSONWarningHookCommand when JSON marshaling fails.
func WrapWindowsProductionPlainTextWarningHookCommand(command string, format WarningFormat) string {
	return fmt.Sprintf(
		`where.exe entire >nul 2>nul & if errorlevel 1 (echo %s) else (%s)`,
		escapeWindowsCMD(windowsPlainTextWarning(format)),
		command,
	)
}

// WrapProductionPlainTextWarningHookCommand emits the warning as plain
// text to stdout when the Entire CLI is missing from PATH.
func WrapProductionPlainTextWarningHookCommand(command string, format WarningFormat) string {
	return fmt.Sprintf(
		`sh -c 'if ! command -v entire >/dev/null 2>&1; then printf "%%s\n" %q; exit 0; fi; exec %s'`,
		MissingEntireWarning(format),
		command,
	)
}

const productionHookWrapperPrefix = `sh -c 'if ! command -v entire >/dev/null 2>&1; then `
const windowsProductionHookWrapperPrefix = `where.exe entire >nul 2>nul & if errorlevel 1 `
const nestedWindowsProductionHookWrapperPrefix = `cmd.exe /d /s /c "where.exe entire >nul 2>nul & if errorlevel 1 `

// IsManagedHookCommand reports whether command is either a direct Entire hook
// command or one of Entire's production wrapper forms that exec that command.
func IsManagedHookCommand(command string, prefixes []string) bool {
	if hasManagedHookPrefix(command, prefixes) {
		return true
	}
	if strings.HasPrefix(command, productionHookWrapperPrefix) {
		_, wrappedCommand, ok := strings.Cut(command, "; fi; exec ")
		if !ok {
			return false
		}

		return hasManagedHookPrefix(wrappedCommand, prefixes)
	}
	if strings.HasPrefix(command, windowsProductionHookWrapperPrefix) ||
		strings.HasPrefix(command, nestedWindowsProductionHookWrapperPrefix) {
		// The wrapped command lives in the `else (<command>)` branch. Take the
		// last ` else (` so a warning string containing the marker can't fool us.
		const elseMarker = " else ("
		idx := strings.LastIndex(command, elseMarker)
		if idx < 0 {
			return false
		}
		wrappedCommand := command[idx+len(elseMarker):]
		wrappedCommand = strings.TrimSuffix(wrappedCommand, `"`)
		wrappedCommand = strings.TrimSuffix(wrappedCommand, `)`)
		return hasManagedHookPrefix(wrappedCommand, prefixes)
	}
	return false
}

func hasManagedHookPrefix(command string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	return false
}

// escapeWindowsCMD caret-escapes the cmd.exe block metacharacters that would
// otherwise terminate the `(echo …)` warning block or redirect its output.
//
// `%` is deliberately NOT escaped. Hook runners pass these strings to a cmd /c
// command line, not a batch script, so batch's `%%` doubling does not apply,
// and caret-escaping `%` would leak the caret. The fixed warning constants are
// %-free; if that changes, percent expansion needs separate handling.
func escapeWindowsCMD(s string) string {
	replacer := strings.NewReplacer(
		`^`, `^^`,
		`&`, `^&`,
		`|`, `^|`,
		`<`, `^<`,
		`>`, `^>`,
		`"`, `^"`,
		`(`, `^(`,
		`)`, `^)`,
	)
	return replacer.Replace(s)
}

func windowsPlainTextWarning(format WarningFormat) string {
	return strings.Join(strings.Fields(MissingEntireWarning(format)), " ")
}

const hookWrapperOSWindows = "windows"

// shHookWrapperProbeCommand is run through cmd.exe to decide whether the
// sh-based production wrappers actually work on this host. It must be a no-op
// that succeeds iff a working POSIX sh is reachable.
const shHookWrapperProbeCommand = `sh -c 'exit 0'`

// hookCommandOS and shHookWrapperWorks are package vars so tests can simulate a
// Windows host and a present/absent sh without spawning cmd.exe. Override them
// via SetWindowsHookProbeForTesting.
var (
	hookCommandOS      = runtime.GOOS
	shHookWrapperWorks = defaultSHHookWrapperWorks
)

// UseWindowsProductionHooks reports whether an agent should install the native
// Windows (cmd.exe) production hook wrappers instead of the sh-based ones. It
// is true only on Windows when the sh-based wrapper does not actually run on
// this host. This lives in the shared agent layer so every agent that wraps
// hooks for production inherits the same Windows fallback decision rather than
// re-implementing the probe and selection (codex is the first adopter; the
// other agents can switch to these helpers without new logic).
//
// The probe runs once per InstallHooks call (not memoized): InstallHooks is
// invoked once per `entire enable`, and not caching is what lets a host that
// gains or loses a working sh migrate its hooks on the next install. The 2s
// timeout is deliberately generous so a momentarily slow sh isn't misread as
// absent; if it ever is, the next install simply re-migrates — the outcome is
// self-correcting, never wedged.
func UseWindowsProductionHooks(ctx context.Context, localDev bool) bool {
	if localDev || hookCommandOS != hookWrapperOSWindows {
		return false
	}
	return !shHookWrapperWorks(ctx, shHookWrapperProbeCommand)
}

func defaultSHHookWrapperWorks(ctx context.Context, command string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, "cmd.exe", "/d", "/s", "/c", command)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// WrapProductionSilentHookCommandForOS picks the sh-based or native Windows
// silent wrapper based on useWindows (typically from UseWindowsProductionHooks).
func WrapProductionSilentHookCommandForOS(command string, useWindows bool) string {
	if useWindows {
		return WrapWindowsProductionSilentHookCommand(command)
	}
	return WrapProductionSilentHookCommand(command)
}

// WrapProductionJSONWarningHookCommandForOS picks the sh-based or native Windows
// JSON-warning wrapper based on useWindows.
func WrapProductionJSONWarningHookCommandForOS(command string, format WarningFormat, useWindows bool) string {
	if useWindows {
		return WrapWindowsProductionJSONWarningHookCommand(command, format)
	}
	return WrapProductionJSONWarningHookCommand(command, format)
}

// SetWindowsHookProbeForTesting overrides the OS and sh-wrapper probe used by
// UseWindowsProductionHooks and returns a restore function. Test-only.
func SetWindowsHookProbeForTesting(goos string, works func(ctx context.Context, command string) bool) func() {
	oldOS, oldProbe := hookCommandOS, shHookWrapperWorks
	hookCommandOS = goos
	shHookWrapperWorks = works
	return func() {
		hookCommandOS = oldOS
		shHookWrapperWorks = oldProbe
	}
}
