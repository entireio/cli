//go:build windows

package cli

import (
	"context"
	"os/exec"
	"syscall"
)

// wrappedTitleCommand runs a preserved agy title command the way agy itself
// runs the slot on this host: through cmd.exe. The whole line is handed over
// verbatim via CmdLine — Go's argv quoting would rewrite the user's own quotes
// and carets — with /d to skip AutoRun and /s so cmd strips exactly the one
// pair of outer quotes added here. Nothing else is interpolated: original is
// the string agy would have run through cmd.exe itself had the tee not taken
// the slot (see newAntigravityTitleTeeCmd).
func wrappedTitleCommand(ctx context.Context, original string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + original + `"`}
	return cmd
}
