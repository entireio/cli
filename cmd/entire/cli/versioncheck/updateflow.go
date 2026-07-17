package versioncheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// This file is the single update flow shared by both update triggers:
// the version-check nag (MaybeAutoUpdate) and the server's
// cli_upgrade_required rejection (package cli). The triggers differ only
// in how they detect the condition and which prompt they show — the gate
// (PromptAllowed) and everything after the user accepts (RunUpdate) live
// here once.

// envUpgradeRerun marks a process re-executed after a successful update.
// If that rerun still fails or still looks outdated (the installer updated
// a different binary than the one running), prompts are suppressed so an
// ineffective update can't loop forever.
const envUpgradeRerun = "ENTIRE_UPGRADE_RERUN"

// reexec is a test seam for re-executing the original command.
var reexec = realReexecCommand

// IsPostUpdateRerun reports whether this process was re-executed by
// RunUpdate after an update. Callers use it to replace "update it" advice
// with a stale-binary explanation.
func IsPostUpdateRerun() bool {
	return os.Getenv(envUpgradeRerun) != ""
}

// PromptAllowed is the single gate for showing an interactive update
// prompt, shared by both triggers: never with the kill switch set or on a
// post-update rerun (loop guard), and only with a runnable installer on
// an interactive terminal.
func PromptAllowed(w io.Writer) bool {
	return os.Getenv(envKillSwitch) == "" &&
		!IsPostUpdateRerun() &&
		canAutoInstall() &&
		interactive.CanPromptInteractively() &&
		isTerminalOut(w)
}

// RunUpdate is the shared execution tail after the user accepts an update
// offer: it announces the installer command, runs it, and reports failure
// with a copyable retry command. On success, a nil argv just tells the
// user to re-run entire (the version-check trigger fires after the
// command already completed — re-executing would run it twice); a non-nil
// argv is re-executed with the freshly installed binary (the
// cli_upgrade_required trigger fires after the command failed, so the
// rerun is a retry). On a successful re-exec the process exits with the
// child's exit code and this function does not return.
func RunUpdate(ctx context.Context, w io.Writer, cmdStr string, argv []string) {
	fmt.Fprintf(w, "\nUpdating Entire CLI: %s\n", cmdStr)
	if err := runInstaller(ctx, cmdStr); err != nil {
		fmt.Fprintf(w, "Update failed: %v\nTry again later running:\n  %s\n", err, cmdStr)
		return
	}
	if len(argv) == 0 {
		fmt.Fprintln(w, "Update complete. Re-run entire to use the new version.")
		return
	}
	rerun := RerunCommandLine(argv)
	fmt.Fprintf(w, "Update complete. Rerunning the command:\n\n  %s\n\n", rerun)
	if err := reexec(ctx, argv); err != nil {
		fmt.Fprintf(w, "Could not rerun automatically (%v). Rerun the command:\n\n  %s\n", err, rerun)
	}
}

// realReexecCommand reruns the original invocation with the freshly
// installed binary: argv[0] is re-resolved (through PATH when bare, or
// the same path the installer just replaced) and the child inherits the
// terminal, so interactive flows like the login prompts still work. The
// envUpgradeRerun marker suppresses further update prompts if the rerun
// fails the same way. On success this does not return — the process
// exits with the child's exit code. A spawn-and-exit is used instead of
// syscall.Exec so the one code path covers Windows too.
func realReexecCommand(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("original command unknown")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("locate %s: %w", argv[0], err)
	}
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), envUpgradeRerun+"=1")
	if runErr := cmd.Run(); runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("rerun command: %w", runErr)
	}
	os.Exit(0)
	return nil
}

// RerunCommandLine reconstructs an invocation for display: argv[0]
// reduced to its base name, arguments with shell-significant whitespace
// or quotes quoted so the line can be pasted back verbatim.
func RerunCommandLine(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(argv))
	parts = append(parts, filepath.Base(argv[0]))
	for _, a := range argv[1:] {
		if strings.ContainsAny(a, " \t\"'") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}
