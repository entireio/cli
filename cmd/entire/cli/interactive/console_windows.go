//go:build windows

package interactive

import "golang.org/x/sys/windows"

var procGetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

// consoleHasNoWindow reports whether this process's console has no window.
// A desktop app (Claude Code, Codex) that starts its shell with
// CREATE_NO_WINDOW gives it a headless console: CONIN$ still opens and stays
// in line mode, so the device and raw-mode probes both pass, but nothing
// written to it is ever shown and a read waits for keys no one can type.
//
// Only a missing window counts. A window that exists but is not visible is not
// a signal: pseudoconsole hosts (Windows Terminal, VS Code) report an invisible
// placeholder window for consoles a person is using.
func consoleHasNoWindow() bool {
	if procGetConsoleWindow.Find() != nil {
		return false // can't tell — fail open, matching the raw-mode probe
	}
	hwnd, _, _ := procGetConsoleWindow.Call() //nolint:errcheck // GetConsoleWindow sets no error; NULL is the answer
	return hwnd == 0
}
