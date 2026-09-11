//go:build unix

package osroot

import "golang.org/x/sys/unix"

const noFollowOpenFlag = unix.O_NOFOLLOW
