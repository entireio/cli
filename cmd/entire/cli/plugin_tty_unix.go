//go:build unix

package cli

import "golang.org/x/sys/unix"

// terminalSentInterrupt reports whether a SIGINT this process received came
// from its controlling terminal and has reached the plugin pid too: this
// process and the plugin are both in the terminal's foreground process group,
// which is where the terminal sends a typed Ctrl-C. Forwarding it as well
// would hand the plugin a second SIGINT for one key press, and a plugin that
// force-quits on a second Ctrl-C would do so on the first, dropping what it
// had not flushed. A `kill -INT` sent to this process alone while it runs in
// the foreground is indistinguishable and is not forwarded either; the plugin
// is then killed after pluginCancelGrace. False when there is no controlling
// terminal or it cannot be queried.
func terminalSentInterrupt(pid int) bool {
	tty, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(tty)
	fg, err := unix.IoctlGetInt(tty, unix.TIOCGPGRP)
	if err != nil {
		return false
	}
	pluginGroup, err := unix.Getpgid(pid)
	if err != nil {
		return false
	}
	return fg == unix.Getpgrp() && fg == pluginGroup
}
