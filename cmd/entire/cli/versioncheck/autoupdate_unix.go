//go:build unix

package versioncheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// installerAutoRuns: the installer may replace the running binary in place.
const installerAutoRuns = true

// realRunInstaller shells out to the installer command, streaming stdin/stdout/stderr
// so password prompts and progress output reach the user.
//
// The shell stays, and this is the reason it is allowed to. CLAUDE.md's rule is
// "never put a dynamic value on a cmd.exe line", and the argument generalises to
// `sh -c`: Go's argv escaping does not protect a string a shell then re-parses.
// What makes this call site different is that there IS no dynamic value.
// UpdateCommandForCurrentBinary returns one of the unix commands' compile-time
// literals; the running binary's path and version choose BETWEEN them and never
// appear IN them. And the shell is load-bearing for the fallback, which is a
// pipeline (`curl … | bash`) — rewriting it as argv would mean re-implementing
// the pipe here, which is more moving parts than the risk being removed.
//
// That reasoning holds only as long as the literals stay literal, and the
// tempting next change is exactly the one that breaks it: interpolating the
// channel or the version into the command. So the invariant is enforced rather
// than asserted — TestUpdateCommandIsAlwaysALiteral drives every unix install
// manager and channel, including adversarial exec paths and versions, and fails
// when the result is not one of the known strings. If a future command
// genuinely needs a runtime value in it, that command must be built and run as
// argv, not added to the literal set.
func realRunInstaller(ctx context.Context, cmdStr string) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("installer exited: %w", err)
	}
	return nil
}
