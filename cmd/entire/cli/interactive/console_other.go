//go:build !windows

package interactive

// consoleHasNoWindow reports false: only Windows consoles can exist without a
// window. See console_windows.go.
func consoleHasNoWindow() bool {
	return false
}
