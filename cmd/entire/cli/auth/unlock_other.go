//go:build !darwin

package auth

import "net"

// lookupProc is unavailable: no unlock server or client off macOS.
func lookupProc(int) (procInfo, error) { return procInfo{}, ErrUnlockUnsupported }

// lookupPeer is unavailable off macOS.
func lookupPeer(*net.UnixConn) (int, int, error) { return 0, 0, ErrUnlockUnsupported }
