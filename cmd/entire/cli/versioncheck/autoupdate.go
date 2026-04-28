package versioncheck

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

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

// Test seams.
var (
	runInstaller     = realRunInstaller
	confirmUpdate    = realConfirmUpdate
	chooseBrewUpdate = realChooseBrewUpdate
	isTerminalOut    = interactive.IsTerminalWriter
)

// MaybeAutoUpdate prints an update notification and offers an interactive
// upgrade. Silent on every failure path — it must never interrupt the CLI.
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
func MaybeAutoUpdate(ctx context.Context, w io.Writer, currentVersion, latestVersion string) AutoUpdateAction {
	if installManagerForCurrentBinary() == installManagerBrew {
		return maybeBrewAutoUpdate(ctx, w, currentVersion, latestVersion)
	}

	printNotification(w, currentVersion, latestVersion)

	// Windows + unknown install manager: the POSIX curl-pipe-bash fallback
	// would error if auto-run, and there's no safe native equivalent. Point
	// the user at the releases page so they can download manually.
	if !canAutoInstall() {
		fmt.Fprintf(w, "To update, download the latest release from:\n  %s\n", downloadsURL)
		return autoUpdateActionSkip
	}
	if os.Getenv(envKillSwitch) != "" || !interactive.CanPromptInteractively() || !isTerminalOut(w) {
		fmt.Fprintf(w, "To update, run:\n  %s\n", updateCommand(currentVersion))
		return autoUpdateActionSkip
	}

	confirmed, err := confirmUpdate()
	if err != nil {
		logging.Debug(ctx, "auto-update: prompt failed", "error", err.Error())
		return autoUpdateActionSkip
	}
	if !confirmed {
		return autoUpdateActionSkip
	}

	cmdStr := updateCommand(currentVersion)
	fmt.Fprintf(w, "\nUpdating Entire CLI: %s\n", cmdStr)
	if err := runInstaller(ctx, cmdStr); err != nil {
		fmt.Fprintf(w, "Update failed: %v\nTry again later running:\n  %s\n", err, cmdStr)
		return autoUpdateActionUpdate
	}
	fmt.Fprintln(w, "Update complete. Re-run entire to use the new version.")
	return autoUpdateActionUpdate
}

func maybeBrewAutoUpdate(ctx context.Context, w io.Writer, currentVersion, latestVersion string) AutoUpdateAction {
	cmdStr := updateCommand(currentVersion)

	if os.Getenv(envKillSwitch) != "" || !interactive.CanPromptInteractively() || !isTerminalOut(w) {
		printNotification(w, currentVersion, latestVersion)
		fmt.Fprintf(w, "To update, run:\n  %s\n", cmdStr)
		return autoUpdateActionSkip
	}

	printBrewUpdateMessage(w, currentVersion, latestVersion, cmdStr)

	action, err := chooseBrewUpdate(w)
	if err != nil {
		logging.Debug(ctx, "auto-update: brew prompt failed", "error", err.Error())
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

func printBrewUpdateMessage(w io.Writer, currentVersion, latestVersion, cmdStr string) {
	fmt.Fprintf(w, "\nUpdate available! %s -> %s\nRelease notes: %s\n1. Update now (runs `%s`)\n2. Skip\n3. Skip until next version\n\nPress enter to continue\n",
		displayVersion(currentVersion), displayVersion(latestVersion), releaseNotesURL(latestVersion), cmdStr)
}

func realChooseBrewUpdate(w io.Writer) (AutoUpdateAction, error) {
	return chooseBrewUpdateFromReader(w, os.Stdin)
}

func chooseBrewUpdateFromReader(w io.Writer, input io.Reader) (AutoUpdateAction, error) {
	reader := bufio.NewReader(input)
	for {
		fmt.Fprint(w, "Choose an option [1]: ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return autoUpdateActionSkip, fmt.Errorf("read update choice: %w", err)
		}
		if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
			return autoUpdateActionSkip, nil
		}

		action, ok := parseBrewUpdateChoice(line)
		if ok {
			return action, nil
		}
		if errors.Is(err, io.EOF) {
			return autoUpdateActionSkip, nil
		}
		fmt.Fprintln(w, "Please choose 1, 2, or 3.")
	}
}

func parseBrewUpdateChoice(input string) (AutoUpdateAction, bool) {
	switch strings.TrimSpace(input) {
	case "", "1":
		return autoUpdateActionUpdate, true
	case "2":
		return autoUpdateActionSkip, true
	case "3":
		return autoUpdateActionSkipUntilNextVersion, true
	default:
		return autoUpdateActionSkip, false
	}
}

func realConfirmUpdate() (bool, error) {
	// Pre-select "Yes" so pressing Enter accepts — matches the (Y/n) UX.
	confirmed := true
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
