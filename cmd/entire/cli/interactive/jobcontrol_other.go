//go:build !darwin && !linux

package interactive

// ttyIsPrivateSession needs POSIX sessions and process groups to say anything,
// so it reports false (fail open — see jobcontrol_unix.go). Those platforms have
// no /dev/tty for CanPromptInteractively to open, so this check is not reached
// in practice.
func ttyIsPrivateSession() bool {
	return false
}
