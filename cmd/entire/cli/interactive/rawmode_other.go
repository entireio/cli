//go:build !darwin && !linux && !windows

package interactive

import "os"

// ttyInRawMode cannot inspect terminal modes on these platforms, so it reports
// false (fail open — see rawmode_unix.go and rawmode_windows.go).
func ttyInRawMode(_ *os.File) bool {
	return false
}
