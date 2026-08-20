package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// GeneratedHookFileState reports hook-config drift for agents whose entire
// Entire integration is one generated file in the repo (Pi's extension,
// OpenCode's plugin), for use by HookFreshness implementations.
//
// The file counts as ours only if it contains marker, and as current only if it
// matches render — what InstallHooks writes today. Pass exactly that one render:
// anything else still carrying the marker (a template edit, or a file left by an
// older version) must read as outdated so the next install replaces it. Passing a
// legacy render here would mark it current and strand it on disk forever, which
// is what TestCheckHookConfig_LegacyLocalDevIsDrift pins against. Matching on
// content rather than a stamped version keeps this honest for free — any edit to
// the embedded template makes installed copies read as outdated without anyone
// remembering to bump anything.
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
func GeneratedHookFileState(path, marker, render string) HookConfigState {
	data, err := os.ReadFile(path) //nolint:gosec // path constructed from validated repo root
	if err != nil {
		return HooksAbsent
	}
	content := string(data)
	if !strings.Contains(content, marker) {
		return HooksAbsent
	}
	if normalizeLineEndings(content) == normalizeLineEndings(render) {
		return HooksCurrent
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

// currentHookCommandPrefix is what every hook command Entire generates begins
// with today: the entire binary resolved through PATH, then the `hooks`
// subcommand.
//
// Scoped to `hooks ` rather than just `entire `, because this predicate decides
// what gets DELETED. Every command Entire writes is `entire hooks <target>
// <verb>`, so anything else invoking the binary — a user's own hook running
// `entire search …` or `entire status --json` — is theirs, not ours to remove.
// That distinction did not matter while removal only happened under --force; it
// matters now that stale hooks are dropped on every install.
const currentHookCommandPrefix = "entire hooks "

// legacyHookCommandPrefixes are the command shapes Entire wrote in the past and
// must still recognize as its own, so hooks installed by older versions are
// replaced rather than left in place beside a current one.
//
// These are DETECTION-ONLY, and unexported so that is enforced rather than
// documented: production code outside this file cannot name them, so it cannot
// splice one into a generated hook command. Every one of them runs Entire from
// the working tree — pointing a hook at a path inside the checkout meant that
// whatever the branch contained ran on every agent turn, and because local-dev
// mode was honored from the committed .entire/settings.json, any repository could
// opt its cloners into that. Tests that need to *build* one of these (to seed a
// config an older version would have written) go through
// agent/testutil, which is test-only by construction.
//
// Both path styles appear because agents resolved the repo root differently:
// Claude Code has ${CLAUDE_PROJECT_DIR}, everyone else shelled out to git.
// The whole set is matched for every agent — a hook naming any of them is ours
// regardless of which agent's config it turned up in.
// Each ends with `hooks ` for the same reason as currentHookCommandPrefix: a
// launcher invoking the CLI for something other than a generated hook is not
// ours to delete.
var legacyHookCommandPrefixes = []string{
	`"$(git rev-parse --show-toplevel)"/scripts/entire-dev hooks `,
	"${CLAUDE_PROJECT_DIR}/scripts/entire-dev hooks ",
	`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks `,
	"go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks ",
}

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

// DropStaleManagedHooks removes Entire-owned entries whose command is not one of
// want, leaving foreign entries and the wanted commands untouched. commandOf
// reads the command string off an entry, which is the only per-agent knowledge
// this needs — agents whose config stores it under a different field (e.g.
// Copilot CLI's `bash`) just return that field. want is a set because one hook
// list can legitimately carry several Entire commands.
//
// It reports whether anything was dropped, because the caller must persist the
// config even when no hook was *added*: a config can hold both a stale and a
// current hook at once, and skipping the write would leave the stale one on disk.
//
// Every agent must run this on every install, not only under --force. Without it
// a hook written by an older version survives alongside the freshly added one and
// keeps firing — which for the removed local-dev mode means a script inside the
// working tree still executes on every agent turn. This lives here rather than in
// each agent because it was independently re-derived six times and two of those
// copies got it wrong, and because `dupl` cannot see duplication across packages.
func DropStaleManagedHooks[E any](entries []E, commandOf func(E) string, want []string) ([]E, bool) {
	dropped := false
	kept := make([]E, 0, len(entries))
	for _, entry := range entries {
		cmd := commandOf(entry)
		if IsManagedHookCommand(cmd) && !slices.Contains(want, cmd) {
			dropped = true
			continue
		}
		kept = append(kept, entry)
	}
	if !dropped {
		// Nothing to change: hand back the original slice so the common
		// idempotent install does not reallocate every hook list.
		return entries, false
	}
	return kept, true
}

// IsManagedHookCommand reports whether command is one Entire wrote: the current
// form or any shape an older version wrote, either bare or inside one of Entire's
// production wrapper forms.
//
// The recognized set is owned here rather than passed in. Each agent previously
// declared its own copy, which meant six near-identical literals that a new agent
// could silently omit — and omitting the legacy entries is precisely what leaves
// an old hook installed forever.
func IsManagedHookCommand(command string) bool {
	if hasManagedHookPrefix(command) {
		return true
	}
	if strings.HasPrefix(command, productionHookWrapperPrefix) {
		_, wrappedCommand, ok := strings.Cut(command, "; fi; exec ")
		if !ok {
			return false
		}

		return hasManagedHookPrefix(wrappedCommand)
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
		return hasManagedHookPrefix(wrappedCommand)
	}
	return false
}

func hasManagedHookPrefix(command string) bool {
	if strings.HasPrefix(command, currentHookCommandPrefix) {
		return true
	}
	for _, prefix := range legacyHookCommandPrefixes {
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
//
// Escaping is only correct here because a third party owns the exec: Entire
// writes these commands into an agent's config file and the agent runs them
// through cmd.exe, so there is no shell to avoid. Do NOT reach for this when
// Entire launches something itself — bypass the shell instead, the way
// cli.openBrowserPlatform (browser_open_windows.go) does.
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
func UseWindowsProductionHooks(ctx context.Context) bool {
	if hookCommandOS != hookWrapperOSWindows {
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
