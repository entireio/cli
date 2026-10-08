//go:build windows

package interactive

import (
	"os"

	"golang.org/x/sys/windows"
)

// ttyInRawMode reports whether console line input is disabled. Full-screen
// Windows TUIs disable ENABLE_LINE_INPUT while they own the console, just as
// Unix TUIs disable ICANON; prompting from a child in that state would race the
// parent for keys and draw over its screen.
func ttyInRawMode(f *os.File) bool {
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(f.Fd()), &mode); err != nil {
		// Can't tell — fail open, matching the Unix mode probe.
		return false
	}
	return consoleModeInRawMode(mode)
}

func consoleModeInRawMode(mode uint32) bool {
	return mode&windows.ENABLE_LINE_INPUT == 0
}
