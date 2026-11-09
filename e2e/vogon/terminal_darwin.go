//go:build darwin

package main

import "golang.org/x/sys/unix"

const (
	getTermiosReq = unix.TIOCGETA
	setTermiosReq = unix.TIOCSETA
)
