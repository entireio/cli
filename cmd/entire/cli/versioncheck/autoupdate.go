package versioncheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
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
// On Windows the installer is never auto-run (a running entire.exe cannot
// replace itself). Scoop, mise, and install.ps1 commands are printed for
// the user to run after entire has exited.
func MaybeAutoUpdate(ctx context.Context, w io.Writer, currentVersion, latestVersion string) AutoUpdateAction {
	cmdStr := updateCommand(currentVersion)

	// Windows can't replace a running executable, so no installer can update
	// entire in place while it runs. For Scoop this is acute: the live
	// entire.exe holds its own shim open, so scoop can't relink or uninstall it
	// mid-run (install leaves the shim on the old package, and uninstall fails
	// with "still running"). Never auto-run on Windows — print the command(s)
	// to run once entire has exited.
	//
	// This returns plain skip, not skipUntilNextVersion, so Windows users can't
	// suppress a specific version: the nudge returns every 24h until they
	// update. That is deliberate while the Scoop rename migration is live —
	// there is no prompt to choose from, so there is no choice to remember.
	if goos == goosWindows {
		printNotification(w, currentVersion, latestVersion)
		fmt.Fprintf(w, "To update, run the following when entire is not running:\n  %s\n", cmdStr)
		return autoUpdateActionSkip
	}

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
	form := uiform.New(huh.NewGroup(sel))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, huh.ErrTimeout) {
			return autoUpdateActionSkip, nil
		}
		return autoUpdateActionSkip, fmt.Errorf("update prompt: %w", err)
	}
	return action, nil
}

// realRunInstaller shells out to the installer command, streaming stdin/stdout/stderr
// so password prompts and progress output reach the user. In practice it only
// ever runs the POSIX installers (brew, mise, curl): MaybeAutoUpdate returns
// before reaching here on Windows, so the cmd.exe branch below is unreachable
// today. It is kept as cover for Windows print-only being lifted later, and
// reads the same `goos` seam MaybeAutoUpdate does so a test can drive both
// consistently.
func realRunInstaller(ctx context.Context, cmdStr string) error {
	var c *exec.Cmd
	if goos == goosWindows {
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
