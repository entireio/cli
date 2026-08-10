//go:build !darwin && !linux

package interactive

import "os"

// ttyInRawMode cannot inspect terminal modes on platforms without the unix
// termios ioctls, so it reports false (fail open — see rawmode_unix.go). Those
// platforms don't have a /dev/tty for CanPromptInteractively to open either, so
// this path is not reached in practice.
func ttyInRawMode(_ *os.File) bool {
	return false
}
