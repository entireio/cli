//go:build windows

package cli

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// wrappedTitleCommand runs a preserved agy title command the way agy itself
// runs the slot on this host: through cmd.exe. The whole line is handed over
// verbatim via CmdLine — Go's argv quoting would rewrite the user's own quotes
// and carets — with /d to skip AutoRun and /s so cmd strips exactly the one
// pair of outer quotes added here. Nothing else is interpolated: original is
// the string agy would have run through cmd.exe itself had the tee not taken
// the slot (see newAntigravityTitleTeeCmd). cmd.exe is started by full path:
// os/exec checks the working directory, the folder agy was opened in, before
// PATH and refuses a match there with ErrDot, so a repository cmd.exe would
// otherwise make the user's own title command fail.
//
// Unlike agy, the cmd.exe started here has NoDefaultCurrentDirectoryInExePath
// set, so the programs the user's command names resolve from PATH, never from
// the folder agy was opened in: that folder may be any repository, and the
// title slot runs in all of them.
func wrappedTitleCommand(ctx context.Context, original string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, agent.SystemCmdExe())
	cmd.Env = agent.AppendWindowsPathOnlyLookupEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + original + `"`}
	return cmd
}
