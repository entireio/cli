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

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// envKillSwitch disables the interactive update prompt regardless of TTY.
const envKillSwitch = "ENTIRE_NO_AUTO_UPDATE"

// AutoUpdateAction describes the result of an update prompt.
type AutoUpdateAction string

const (
	autoUpdateActionSkip                 AutoUpdateAction = "skip"
	autoUpdateActionUpdate               AutoUpdateAction = "update"
	autoUpdateActionSkipUntilNextVersion AutoUpdateAction = "skip_until_next_version"
)

// chooseUpdateFn is the signature for the update-prompt seam. The
// concrete implementation renders a huh.Select with the installer
// command interpolated into option 1.
type chooseUpdateFn func(ctx context.Context, currentVersion, latestVersion, cmdStr string) (AutoUpdateAction, error)

// Test seams.
var (
	runInstaller                 = realRunInstaller
	chooseUpdate  chooseUpdateFn = realChooseUpdate
	isTerminalOut                = interactive.IsTerminalWriter
)

// MaybeAutoUpdate prints an update notification and offers an interactive
// upgrade. Silent on every failure path — it must never interrupt the CLI.
//
// The same 3-option prompt (update / skip / skip until next version) is
// shown for every install manager that supports auto-installation
// (brew, mise, scoop, curl-bash). The only thing that varies between
// installers is the shell command interpolated into option 1.
//
// If the installer command fails, a hint with the exact command is
// printed so the user can retry manually. The 24h version-check cache
// is not invalidated on failure: we don't want to re-prompt on every
// invocation while an upstream issue (network, auth, repo outage) is
// still in place.
//
// When the prompt cannot be shown (kill switch set, or non-interactive
// environment like CI / agent subprocess / no TTY) the installer
// command is printed so the user still learns what to run manually.
//
// On Windows + unknown install manager the POSIX curl-pipe-bash fallback
// can't auto-run and there's no native equivalent, so we point the user
// at the releases download page instead.
func MaybeAutoUpdate(ctx context.Context, w io.Writer, currentVersion, latestVersion string) AutoUpdateAction {
	if !canAutoInstall() {
		printNotification(w, currentVersion, latestVersion)
		fmt.Fprintf(w, "To update, download the latest release from:\n  %s\n", downloadsURL)
		return autoUpdateActionSkip
	}

	cmdStr := updateCommand(currentVersion)

	if os.Getenv(envKillSwitch) != "" || !interactive.CanPromptInteractively() || !isTerminalOut(w) {
		printNotification(w, currentVersion, latestVersion)
		fmt.Fprintf(w, "To update, run:\n  %s\n", cmdStr)
		return autoUpdateActionSkip
	}

	action, err := chooseUpdate(ctx, currentVersion, latestVersion, cmdStr)
	if err != nil {
		logging.Debug(ctx, "auto-update: prompt failed", "error", err.Error())
		return autoUpdateActionSkip
	}

	switch action {
	case autoUpdateActionUpdate:
		fmt.Fprintf(w, "\nUpdating Entire CLI: %s\n", cmdStr)
		if err := runInstaller(ctx, cmdStr); err != nil {
			fmt.Fprintf(w, "Update failed: %v\nTry again later running:\n  %s\n", err, cmdStr)
			return autoUpdateActionUpdate
		}
		fmt.Fprintln(w, "Update complete. Re-run entire to use the new version.")
		return autoUpdateActionUpdate
	case autoUpdateActionSkipUntilNextVersion:
		return autoUpdateActionSkipUntilNextVersion
	case autoUpdateActionSkip:
		return autoUpdateActionSkip
	default:
		return autoUpdateActionSkip
	}
}

// realChooseUpdate renders a huh.Select with the three update actions.
// In normal mode this is an arrow-key TUI; when ACCESSIBLE is set huh
// falls back to a plain numbered prompt readable by screen readers.
func realChooseUpdate(ctx context.Context, currentVersion, latestVersion, cmdStr string) (AutoUpdateAction, error) {
	action := autoUpdateActionUpdate
	sel := huh.NewSelect[AutoUpdateAction]().
		Title(fmt.Sprintf("Update available! %s -> %s",
			displayVersion(currentVersion), displayVersion(latestVersion))).
		Description("Release notes: "+releaseNotesURL(latestVersion)).
		Options(
			huh.NewOption(fmt.Sprintf("Update now (runs `%s`)", cmdStr), autoUpdateActionUpdate),
			huh.NewOption("Skip", autoUpdateActionSkip),
			huh.NewOption("Skip until next version", autoUpdateActionSkipUntilNextVersion),
		).
		Value(&action)
	form := huh.NewForm(huh.NewGroup(sel)).WithTheme(huh.ThemeDracula())
	if os.Getenv("ACCESSIBLE") != "" {
		form = form.WithAccessible(true)
	}
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, huh.ErrTimeout) {
			return autoUpdateActionSkip, nil
		}
		return autoUpdateActionSkip, fmt.Errorf("update prompt: %w", err)
	}
	return action, nil
}

// realRunInstaller shells out to the installer command, streaming stdin/stdout/stderr
// so password prompts and progress output reach the user.
func realRunInstaller(ctx context.Context, cmdStr string) error {
	var c *exec.Cmd
	if runtime.GOOS == goosWindows {
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
