//go:build darwin || linux

package interactive

import "golang.org/x/sys/unix"

// ttyIsPrivateSession reports whether our controlling terminal was created for
// this command alone, rather than handed to us by a user's interactive shell.
//
// Output-capturing git clients don't merely inherit the user's terminal: lazygit
// runs `git commit` on a *fresh pty*, and making a pty into a controlling
// terminal requires setsid + TIOCSCTTY. The git process therefore becomes a
// session leader on a private terminal — one that opens cleanly and sits in
// canonical mode, so neither the /dev/tty probe nor the raw-mode check
// (rawmode_unix.go) can tell that nobody is reading it. lazygit only renders
// that pty's output; it does not forward keystrokes to it, so a prompt there
// never receives an answer: the commit blocks and the UI appears frozen.
//
// Job control is the discriminator. An interactive shell puts every foreground
// command in its own process group, distinct from the session leader's own, so a
// human's `git commit` always runs with pgrp != sid. A command handed a private
// session is both session and process-group leader, so every process in it (git,
// the hook script, us) sees pgrp == sid.
//
// Measured on Linux against lazygit 0.48 (sid == pgrp == the git pid, on a
// second pty distinct from lazygit's own) and against interactive bash and
// busybox sh (sid = the shell, pgrp = the git job). This shape follows from the
// setsid a private controlling terminal requires, so unlike the pty's window
// size — which lazygit presets to 80x24 in current master — it does not drift
// with a client's rendering details.
//
// The known trade-off is `ssh -t host git commit` and `script -c`, which also
// setsid onto a fresh pty but do forward input: those lose the prompt and fall
// back to the non-interactive path (for commit linking, that means auto-link —
// the prompt's own default).
func ttyIsPrivateSession() bool {
	sid, err := unix.Getsid(0)
	if err != nil {
		// Can't tell — fail open. This check may only ever suppress prompts we
		// positively know are unusable.
		return false
	}
	return sid == unix.Getpgrp()
}
