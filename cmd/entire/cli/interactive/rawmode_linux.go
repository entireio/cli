//go:build linux

package interactive

import "golang.org/x/sys/unix"

// rawModeIoctl is the Linux termios read ioctl. See rawmode_unix.go.
const rawModeIoctl = unix.TCGETS
