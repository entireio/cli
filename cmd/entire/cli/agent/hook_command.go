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

// generatedStateFromContent reports hook-config drift for agents whose entire
// Entire integration is one generated file in the repo (Pi's extension,
// OpenCode's plugin), for use by HookFreshness implementations. Reached through
// HookConfigFile.GeneratedState, which reads the bytes through the worktree's
// root.
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
func generatedStateFromContent(content, marker, render string) HookConfigState {
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

// FailedEntireWarning is shown when `entire` is on PATH but cannot run, such as
// a version-manager shim with no version selected. It is spliced into a
// single-quoted sh -c string, so it must not contain a single quote, backtick,
// or dollar sign.
func FailedEntireWarning(format WarningFormat) string {
	switch format {
	case WarningFormatSingleLine:
		return "Entire CLI is enabled, but the entire command on PATH failed to run, so this session may not be captured. Check that entire --version works in this shell. Installation guide: https://docs.entire.io/cli/installation#installation-methods"
	case WarningFormatMultiLine:
		return "\n\nEntire CLI is enabled, but the entire command on PATH failed to run, so this session may not be captured.\nCheck that entire --version works in this shell.\nInstallation guide: https://docs.entire.io/cli/installation#installation-methods"
	default:
		return FailedEntireWarning(WarningFormatSingleLine)
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
// systemMessage field on stdout when the Entire CLI is missing from PATH or
// fails to run.
func WrapProductionJSONWarningHookCommand(command string, format WarningFormat) string {
	missing, missingErr := systemMessageJSON(MissingEntireWarning(format))
	failed, failedErr := systemMessageJSON(FailedEntireWarning(format))
	if missingErr != nil || failedErr != nil {
		// Fallback to plain text on stdout if JSON payload construction somehow fails.
		return WrapProductionPlainTextWarningHookCommand(command, format)
	}
	return warningHookCommand(command, missing, failed)
}

func systemMessageJSON(message string) (string, error) {
	payload, err := jsonutil.MarshalWithNoHTMLEscape(struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}{
		SystemMessage: message,
	})
	if err != nil {
		return "", fmt.Errorf("marshal hook system message: %w", err)
	}
	return string(payload), nil
}

// warningHookCommand prints missing when `entire` is not on PATH and failed when
// it is but `entire --version` fails, exiting 0 either way. A version-manager
// shim with no version selected passes `command -v` and then fails, and agents
// surface a failing hook's stderr only in verbose views, so without the probe
// capture stops unnoticed.
//
// The probe runs before the hook so the hook can still be exec'd: agents enforce
// hook timeouts by signalling the process they started, and only exec makes
// that process entire itself. The probe reads /dev/null so the hook payload on
// stdin reaches the hook.
func warningHookCommand(command, missing, failed string) string {
	return fmt.Sprintf(
		`sh -c 'if ! command -v entire >/dev/null 2>&1; then printf "%%s\n" %q; exit 0; fi; if ! entire --version </dev/null >/dev/null 2>&1; then printf "%%s\n" %q; exit 0; fi; exec %s'`,
		missing,
		failed,
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
//
// Every Windows wrapper body starts with windowsPathOnlyLookup. cmd.exe looks
// for a bare program name in the current directory before PATH unless
// NoDefaultCurrentDirectoryInExePath is set (see NeedCurrentDirectoryForExePathW),
// and agents run hooks with the repository root as the working directory. Without
// it, an `entire.cmd`, `entire.bat`, `entire.exe` or `where.exe` at the root of
// whatever checkout the agent has open replaces the installed CLI on every hook.
// The variable is set in the same cmd.exe that then resolves `where.exe` and
// `entire`, and `entire` inherits it.
//
// `where.exe` itself also searches its working directory for its argument, and
// NoDefaultCurrentDirectoryInExePath does not change that. windowsEntireGuard
// therefore runs it from %SystemRoot% (pushd), and each branch returns to the
// hook's directory (popd) before doing anything else, so a repository file named
// `entire.*` cannot make the check report a CLI that `entire` then fails to find
// on PATH. The usual way to keep `where` to PATH, `$PATH:entire`, is not usable:
// PowerShell would expand it inside the Codex form's double-quoted block before
// cmd.exe ever saw it. %SystemRoot% is left alone by PowerShell and expanded by
// whichever cmd.exe parses the line.
const windowsPathOnlyLookup = `set NoDefaultCurrentDirectoryInExePath=1& `

// WindowsPathOnlyLookupPrefix is windowsPathOnlyLookup for a cmd.exe line Entire
// writes outside the hook wrappers, such as Antigravity's global title command:
// prepended to the line, it makes cmd.exe resolve every program after it from
// PATH.
const WindowsPathOnlyLookupPrefix = windowsPathOnlyLookup

// windowsEntireGuard is the PATH check every Windows wrapper body runs after
// windowsPathOnlyLookup, up to the branch that handles a missing CLI. Both
// branches must start with windowsBranchStart. The errorlevel test has to come
// before popd, which is why popd sits inside the branches.
const windowsEntireGuard = `pushd %SystemRoot%& where.exe entire >nul 2>nul & if errorlevel 1 `

// windowsBranchStart opens both branches after windowsEntireGuard, returning to
// the hook's working directory.
const windowsBranchStart = `popd& `

// legacyWindowsEntireGuard is the guard written before windowsEntireGuard ran
// where.exe from %SystemRoot%. Hooks carrying it must still be recognised so
// the next install replaces them and disable removes them.
const legacyWindowsEntireGuard = `where.exe entire >nul 2>nul & if errorlevel 1 `

// windowsBareCmdLauncher and windowsSystemCmdLauncher open the nested form's
// quoted cmd.exe block.
//
// The first program of a hook line is resolved by the agent's own hook shell
// before anything in the line runs, and Entire cannot control how that shell
// looks it up; the wrappers control everything after it. Where Entire starts
// the agent itself (review runs), AppendWindowsPathOnlyLookupEnv puts
// NoDefaultCurrentDirectoryInExePath in the agent's environment, which its hook
// shell inherits. The two launchers differ in what the first program is:
//   - windowsSystemCmdLauncher names cmd.exe by its full path. The agent's
//     cmd.exe expands %SystemRoot% (so it works when Windows is not on C:),
//     and nothing is looked up in the working directory. Use it for hosts that
//     hand the whole line to cmd.exe: Factory Droid.
//   - windowsBareCmdLauncher is for hosts whose hook shell is or may be
//     PowerShell. Cursor's desktop app runs hook commands through PowerShell
//     on Windows (https://cursor.com/docs/hooks, "Command-Based Hooks"). Codex's
//     hook shell is the session's
//     shell (hooks/src/engine/command_runner.rs and core/src/shell.rs in
//     openai/codex): `powershell -NoProfile -Command <line>` for the default
//     PowerShell user, `cmd.exe /c "<line>"` for a cmd.exe user, and
//     `%COMSPEC% /C "<line>"` only when no shell was detected. PowerShell does
//     not expand %SystemRoot% and never runs a program from the current
//     directory, so the bare token is the one that works under both.
const (
	windowsBareCmdLauncher   = `cmd.exe /d /s /c "`
	windowsSystemCmdLauncher = `%SystemRoot%\System32\cmd.exe /d /s /c "`
)

// windowsSilentBody is the silent wrapper's cmd.exe line around command.
func windowsSilentBody(command string) string {
	return windowsPathOnlyLookup + windowsEntireGuard + `(` + windowsBranchStart + `ver>nul) else (` + windowsBranchStart + command + `)`
}

// windowsWarningBody is the warning wrapper's cmd.exe line around command; the
// warning must already be escaped with escapeWindowsCMD.
func windowsWarningBody(command, escapedWarning string) string {
	return windowsPathOnlyLookup + windowsEntireGuard + `(` + windowsBranchStart + `echo ` + escapedWarning + `) else (` + windowsBranchStart + command + `)`
}

// Choosing a Windows silent wrapper. Two properties of the agent's runner
// decide it; read the runner, because each wrong choice stops hooks firing
// without an error the user sees.
//
//  1. How the hook line reaches the shell. The nested forms carry a
//     `"…"` block that the nested `cmd.exe /d /s /c` unwraps (/s strips the
//     first and last quote and parses the rest as typed). The block survives
//     only when the host puts the line on the shell's command line as it is:
//     droid 0.178+ appends it unquoted after `cmd.exe /d /s /c`, Codex's
//     cmd.exe shell writes `cmd /c "<line>"` by hand, and PowerShell receives
//     it as one -Command argument and passes the block to cmd.exe as one
//     argument. A host that instead passes the line as a single argument
//     through standard Windows argument quoting (Go's os/exec, libuv without
//     windowsVerbatimArguments: agy, droid 0.109.1) wraps it in quotes and
//     backslash-escapes the inner ones. cmd.exe does not treat a backslash as
//     an escape, so the block reaches it as one unrecognised program name.
//     Such a host needs WrapWindowsProductionSilentHookCommandDirect, which
//     contains no quotes.
//  2. Which shell parses the line. The direct form is cmd.exe syntax and only
//     works under cmd.exe; under PowerShell `set` and `&` mean something else,
//     so a host that may use PowerShell needs a nested form. Of the nested
//     forms, WrapWindowsProductionSilentHookCommandForCmdHost names the inner
//     cmd.exe as %SystemRoot%\System32\cmd.exe, which only cmd.exe expands;
//     WrapWindowsProductionSilentHookCommand keeps a bare `cmd.exe` that works
//     under both shells. See windowsBareCmdLauncher.
//
// Hence: Codex (PowerShell or cmd.exe, verbatim) and Cursor (PowerShell) use
// the bare nested form; droid (cmd.exe, verbatim) uses the cmd-host nested
// form; agy (cmd.exe, argument-quoted) uses the direct form.
//
// WrapWindowsProductionSilentHookCommand exits successfully without output when
// the Entire CLI is missing from PATH. It avoids sh so Codex hooks still work
// from native Windows shells. It nests a bare `cmd.exe` so the line also runs
// under Codex's PowerShell hook shell; see windowsBareCmdLauncher. Hosts that
// always run hooks through cmd.exe use
// WrapWindowsProductionSilentHookCommandForCmdHost instead.
func WrapWindowsProductionSilentHookCommand(command string) string {
	return windowsBareCmdLauncher + windowsSilentBody(command) + `"`
}

// WrapWindowsProductionSilentHookCommandForCmdHost is the nested silent wrapper
// for a host that always hands the whole hook line to cmd.exe (Factory Droid).
// It must not be used for a host that may run hooks through PowerShell, which
// does not expand %SystemRoot%: the hook would never start.
// It names the nested cmd.exe as %SystemRoot%\System32\cmd.exe, so the host's
// cmd.exe never looks for it in the working directory.
func WrapWindowsProductionSilentHookCommandForCmdHost(command string) string {
	return windowsSystemCmdLauncher + windowsSilentBody(command) + `"`
}

// WrapWindowsProductionSilentHookCommandDirect is the silent wrapper for a host
// that already runs every hook command through cmd.exe /C on Windows (agy does;
// see HookHostIsWindows): the bare windowsSilentBody line, with no cmd.exe
// prefix and no quoted block. Handing such a
// host WrapWindowsProductionSilentHookCommand's nested form fails outright —
// cmd.exe /C takes the quoted block as one program name ('"where.exe entire
// >nul 2>nul & …"' is not recognized as an internal or external command) — and
// the sh wrapper fails too, because cmd.exe reads its `>/dev/null` as a redirect
// to a nonexistent path. Both were observed on Windows 11 with agy 1.2.7; only
// this shape produced a tracked session. Same distinction
// WrapWindowsProductionJSONWarningHookCommand draws for Codex.
func WrapWindowsProductionSilentHookCommandDirect(command string) string {
	return windowsSilentBody(command)
}

// WrapWindowsProductionJSONWarningHookCommand emits a JSON hook response with a
// systemMessage field on stdout when the Entire CLI is missing from PATH. It
// avoids sh so Codex hooks still work from native Windows shells. It is the
// direct form, with no nested cmd.exe, so it only runs when the hook shell is
// cmd.exe (Codex's cmd.exe and %COMSPEC% hook shells; see
// windowsBareCmdLauncher).
func WrapWindowsProductionJSONWarningHookCommand(command string, format WarningFormat) string {
	payload, err := jsonutil.MarshalWithNoHTMLEscape(struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}{
		SystemMessage: MissingEntireWarning(format),
	})
	if err != nil {
		return WrapWindowsProductionPlainTextWarningHookCommand(command, format)
	}

	return windowsWarningBody(command, escapeWindowsCMD(string(payload)))
}

// WrapWindowsProductionPlainTextWarningHookCommand is the direct-shell fallback
// for WrapWindowsProductionJSONWarningHookCommand when JSON marshaling fails.
func WrapWindowsProductionPlainTextWarningHookCommand(command string, format WarningFormat) string {
	return windowsWarningBody(command, escapeWindowsCMD(windowsPlainTextWarning(format)))
}

// WrapProductionPlainTextWarningHookCommand emits the warning as plain
// text to stdout when the Entire CLI is missing from PATH or fails to run.
func WrapProductionPlainTextWarningHookCommand(command string, format WarningFormat) string {
	return warningHookCommand(command, MissingEntireWarning(format), FailedEntireWarning(format))
}

const productionHookWrapperPrefix = `sh -c 'if ! command -v entire >/dev/null 2>&1; then `

// isWindowsProductionHookWrapper reports whether command has the shape of one
// of Entire's Windows wrappers: an optional nested cmd.exe launcher, an
// optional windowsPathOnlyLookup, and then windowsEntireGuard or
// legacyWindowsEntireGuard. Wrappers written by older versions lack the lookup
// prefix or carry the legacy guard; they must still be recognised so the next
// install replaces them.
func isWindowsProductionHookWrapper(command string) bool {
	body := command
	for _, launcher := range []string{windowsBareCmdLauncher, windowsSystemCmdLauncher} {
		if rest, ok := strings.CutPrefix(body, launcher); ok {
			body = rest
			break
		}
	}
	body = strings.TrimPrefix(body, windowsPathOnlyLookup)
	return strings.HasPrefix(body, windowsEntireGuard) || strings.HasPrefix(body, legacyWindowsEntireGuard)
}

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
	if isWindowsProductionHookWrapper(command) {
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
		wrappedCommand = strings.TrimPrefix(wrappedCommand, windowsBranchStart)
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

	return shHookWrapperProbe(probeCtx, command).Run() == nil
}

// shHookWrapperProbe builds the probe process. The probe must find sh where a
// hook would, on PATH, and never in the directory `entire` was started from:
// `entire enable` and Codex review setup run from the repository, and a
// repository `sh.bat` or `sh.cmd` would otherwise both run during the probe and
// decide which wrapper form gets installed. NoDefaultCurrentDirectoryInExePath
// stops cmd.exe searching its working directory; running from the temp
// directory keeps the checkout out of the picture even if it is ignored.
// cmd.exe itself is started by full path: see SystemCmdExe.
func shHookWrapperProbe(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, SystemCmdExe(), "/d", "/s", "/c", command)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), windowsNoCurrentDirLookupEnv)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd
}

// windowsNoCurrentDirLookupEnv is the environment entry windowsPathOnlyLookup
// sets inside a hook line, for processes Entire starts through cmd.exe itself.
const windowsNoCurrentDirLookupEnv = "NoDefaultCurrentDirectoryInExePath=1"

// AppendWindowsPathOnlyLookupEnv returns env with
// NoDefaultCurrentDirectoryInExePath=1 appended when hooks run on a Windows
// host, and env unchanged elsewhere. It is for an agent process Entire starts
// itself, such as a review run: the agent's hook shell inherits the session
// environment, so cmd.exe there resolves every program in a hook line from
// PATH, including the line's first program, which no wrapper text can reach.
// os/exec keeps the last of duplicate keys, so an inherited value is
// overridden.
func AppendWindowsPathOnlyLookupEnv(env []string) []string {
	if !HookHostIsWindows() {
		return env
	}
	return append(env, windowsNoCurrentDirLookupEnv)
}

// HookHostIsWindows reports whether hook commands will run on a Windows host,
// without asking whether a POSIX sh is reachable there.
//
// It is the predicate for an agent that hands every hook command to cmd.exe on
// Windows whatever else is installed. Factory Droid is one: its Windows build
// runs each hook command as an argument of cmd.exe, while its macOS/Linux
// build runs the same string under sh. For such an agent
// UseWindowsProductionHooks answers the wrong question — its probe only
// establishes that `sh -c 'exit 0'` runs, and that command carries no cmd.exe
// metacharacters, so a host with Git Bash reports success while the real sh
// wrapper, which is full of `>` and `&`, is still cut apart by cmd.exe before
// any sh sees it.
//
// Codex and Cursor keep UseWindowsProductionHooks because a Windows host may
// hand their hooks to a real sh, not because their runners are known to avoid
// cmd.exe. Codex runs a hook in the session's shell (hooks/src/engine/
// command_runner.rs and core/src/shell.rs in openai/codex): PowerShell by
// default, cmd.exe for a cmd.exe user, %COMSPEC% when no shell was detected,
// and a POSIX sh when the session runs under one — and Codex demonstrably
// passes the Windows nightly with the sh wrapper installed. Cursor's desktop
// app runs hook commands through PowerShell on Windows (https://cursor.com/
// docs/hooks); its Windows build is excluded from the Windows matrix (no
// tmux), so nothing here has run under a real Cursor. The choice of Windows
// wrapper for each runner is described above
// WrapWindowsProductionSilentHookCommand.
func HookHostIsWindows() bool {
	return hookCommandOS == hookWrapperOSWindows
}

// WrapProductionSilentHookCommandForOS picks the sh-based or native Windows
// silent wrapper based on useWindows (typically from UseWindowsProductionHooks).
func WrapProductionSilentHookCommandForOS(command string, useWindows bool) string {
	if useWindows {
		return WrapWindowsProductionSilentHookCommand(command)
	}
	return WrapProductionSilentHookCommand(command)
}

// WrapProductionSilentHookCommandForCmdHostOS is
// WrapProductionSilentHookCommandForOS for a host that hands every hook line to
// cmd.exe on Windows (Factory Droid): its Windows form names the nested
// cmd.exe by full path. See windowsSystemCmdLauncher.
func WrapProductionSilentHookCommandForCmdHostOS(command string, useWindows bool) string {
	if useWindows {
		return WrapWindowsProductionSilentHookCommandForCmdHost(command)
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

// WrapProductionPlainTextWarningHookCommandForOS picks the sh-based or native
// Windows plain-text-warning wrapper based on useWindows.
func WrapProductionPlainTextWarningHookCommandForOS(command string, format WarningFormat, useWindows bool) string {
	if useWindows {
		return WrapWindowsProductionPlainTextWarningHookCommand(command, format)
	}
	return WrapProductionPlainTextWarningHookCommand(command, format)
}

// SetWindowsHookProbeForTesting overrides the OS and sh-wrapper probe used by
// UseWindowsProductionHooks and HookHostIsWindows, and returns a restore
// function. Test-only.
func SetWindowsHookProbeForTesting(goos string, works func(ctx context.Context, command string) bool) func() {
	oldOS, oldProbe := hookCommandOS, shHookWrapperWorks
	hookCommandOS = goos
	shHookWrapperWorks = works
	return func() {
		hookCommandOS = oldOS
		shHookWrapperWorks = oldProbe
	}
}
