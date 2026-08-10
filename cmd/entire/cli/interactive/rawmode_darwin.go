//go:build darwin

package interactive

import "golang.org/x/sys/unix"

// rawModeIoctl is the BSD/darwin termios read ioctl. See rawmode_unix.go.
const rawModeIoctl = unix.TIOCGETA
