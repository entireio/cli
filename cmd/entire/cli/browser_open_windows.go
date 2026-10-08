//go:build windows

package cli

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// shellExecute is the Win32 entry point openBrowserPlatform launches through.
// It is a variable so the Windows test can capture the exact string handed to
// the shell association without launching a real browser.
var shellExecute = windows.ShellExecute

// openBrowserPlatform hands browserURL to the Windows shell association for
// its scheme, which is the user's default browser.
//
// This calls ShellExecute rather than spawning `cmd /c start` on purpose, and
// that choice is the fix for a login that opened
// `/authorize?client_id=entire-cli` and was rejected for a missing
// redirect_uri. An OAuth authorization URL carries `&` between query
// parameters and `%XX` inside the percent-encoded redirect_uri, and both are
// cmd.exe metacharacters: `&` ends the command, `%VAR%` expands. Go's argv
// escaping does not help, because syscall.EscapeArg only quotes arguments
// containing a space, tab, quote, or backslash — a URL has none of those, so
// it reached cmd.exe bare and the browser only ever saw the text before the
// first `&`. ShellExecute takes the URL as a single wide string, so there is
// no command line to parse and no escaping to get wrong.
//
// Escaping is the wrong tool whenever Entire owns the exec, so prefer this
// shape. Escaping (agent.escapeWindowsCMD) is only for the opposite case,
// where Entire writes a command into a third party's config file and that
// third party runs it through cmd.exe.
//
// hwnd is 0 (no owner window — this is a console app) and the verb is nil so
// the association's default verb ("open") applies. ShellExecute returns once
// the browser has been asked to open the URL; there is no child process for us
// to wait on or release.
func openBrowserPlatform(browserURL string) error {
	if err := shellExecute(0, nil, windows.StringToUTF16Ptr(browserURL), nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("open browser via ShellExecute: %w", err)
	}
	return nil
}
