package versioncheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/charmbracelet/huh"
	"golang.org/x/term"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// envKillSwitch disables the interactive update prompt regardless of TTY.
const envKillSwitch = "ENTIRE_NO_AUTO_UPDATE"

// Test seams.
var (
	runInstaller     = realRunInstaller
	stdoutIsTerminal = defaultStdoutIsTerminal
	confirmUpdate    = realConfirmUpdate
)

func defaultStdoutIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // G115: uintptr->int is safe for fd
}

// MaybeAutoUpdate offers an interactive upgrade after the standard
// "version available" notification has been printed. Silent on every
// failure path — it must never interrupt the CLI.
func MaybeAutoUpdate(ctx context.Context, w io.Writer, currentVersion string) {
	if os.Getenv(envKillSwitch) != "" {
		return
	}
	if os.Getenv("CI") != "" {
		return
	}
	if !stdoutIsTerminal() {
		return
	}

	confirmed, err := confirmUpdate()
	if err != nil {
		logging.Debug(ctx, "auto-update: prompt failed", "error", err.Error())
		return
	}
	if !confirmed {
		return
	}

	cmdStr := updateCommand(currentVersion)
	fmt.Fprintf(w, "\nUpdating Entire CLI: %s\n", cmdStr)
	if err := runInstaller(ctx, cmdStr); err != nil {
		fmt.Fprintf(w, "Update failed: %v\n", err)
		return
	}
	fmt.Fprintln(w, "Update complete. Re-run entire to use the new version.")
}

func realConfirmUpdate() (bool, error) {
	var confirmed bool
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Install the new version now?").
				Affirmative("Yes").
				Negative("No").
				Value(&confirmed),
		),
	).WithTheme(huh.ThemeDracula())
	if os.Getenv("ACCESSIBLE") != "" {
		form = form.WithAccessible(true)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, huh.ErrTimeout) {
			return false, nil
		}
		return false, fmt.Errorf("confirm form: %w", err)
	}
	return confirmed, nil
}

// realRunInstaller shells out to the installer command, streaming stdin/stdout/stderr
// so password prompts and progress output reach the user.
func realRunInstaller(ctx context.Context, cmdStr string) error {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmdStr)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("installer exited: %w", err)
	}
	return nil
}
