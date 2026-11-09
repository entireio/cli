//go:build linux

package main

import "golang.org/x/sys/unix"

const (
	getTermiosReq = unix.TCGETS
	setTermiosReq = unix.TCSETS
)
