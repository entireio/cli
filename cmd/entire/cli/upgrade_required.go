package cli

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

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// cliUpgradeRequiredCode is the OAuth error code the server returns when
// this CLI build is older than the minimum version it accepts.
const cliUpgradeRequiredCode = "cli_upgrade_required"

// cliUnsupportedMsg leads every cli_upgrade_required guidance block,
// followed by the update command, then a "Then rerun the command:" block
// with the command that failed.
const cliUnsupportedMsg = "This Entire CLI version is no longer supported. Update it:"

// IsCLIUpgradeRequired reports whether err carries the server's
// cli_upgrade_required OAuth error code. auth-go surfaces the code as an
// unrecognised-code error whose message carries it verbatim — there is no
// sentinel to errors.Is against, so match on the code string.
func IsCLIUpgradeRequired(err error) bool {
	return err != nil && strings.Contains(err.Error(), cliUpgradeRequiredCode)
}

// envUpgradeRerun marks a process re-executed after a successful
// cli_upgrade_required update. If that rerun fails the same way (the
// installer updated a different binary than the one running), the prompt
// is suppressed and the commands are printed instead — otherwise an
// ineffective update would loop forever.
const envUpgradeRerun = "ENTIRE_UPGRADE_RERUN"

// upgradeOfferDeps carries the environment-dependent pieces of
// offerCLIUpgrade so tests can drive both branches without a TTY, a
// real installer, or replacing the test process. Zero-value fields are
// safe: nil confirm declines, and nil runInstaller/reexec are never
// reached without confirm.
type upgradeOfferDeps struct {
	canPrompt    func() bool
	confirm      func(ctx context.Context, updateCmd string) (bool, error)
	runInstaller func(ctx context.Context, cmdStr string) error
	reexec       func(ctx context.Context, argv []string) error
}

// OfferCLIUpgradeIfRequired handles a cli_upgrade_required failure in
// place of the raw error print: it reports true when it took over the
// messaging (main.go then skips printing err — the raw OAuth chain adds
// nothing the guidance doesn't say), false when err doesn't carry the
// code. On an interactive terminal it offers to run the updater right
// away (same installer the version-check prompt uses) and, on success,
// re-executes the original command with the freshly installed binary;
// otherwise — no TTY, ENTIRE_NO_AUTO_UPDATE set, declined, or no
// runnable installer for this platform — it prints the update command
// and the failed command so the user can run both. argv is the failed
// invocation's os.Args.
//
// The code can surface from any auth-server OAuth flow — login (device
// and browser), the refresh grant under every authenticated command, and
// the RFC 8693 exchanges — which is why this hangs off main.go's central
// error-print arm rather than any single command.
func OfferCLIUpgradeIfRequired(ctx context.Context, w io.Writer, err error, argv []string) bool {
	return offerCLIUpgrade(ctx, w, err, argv, upgradeOfferDeps{
		canPrompt:    func() bool { return upgradePromptAllowed(w) },
		confirm:      confirmCLIUpgrade,
		runInstaller: versioncheck.RunUpdateInstaller,
		reexec:       reexecCommand,
	})
}

// upgradePromptAllowed reports whether the interactive update offer may
// be shown: never on a post-update rerun (loop guard), never with the
// auto-update kill switch set, and only with a runnable installer on an
// interactive terminal.
func upgradePromptAllowed(w io.Writer) bool {
	return os.Getenv(envUpgradeRerun) == "" &&
		os.Getenv(versioncheck.EnvNoAutoUpdate) == "" &&
		versioncheck.CanAutoInstall() &&
		interactive.CanPromptInteractively() &&
		interactive.IsTerminalWriter(w)
}

func offerCLIUpgrade(ctx context.Context, w io.Writer, err error, argv []string, deps upgradeOfferDeps) bool {
	if !IsCLIUpgradeRequired(err) {
		return false
	}
	updateCmd := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)
	rerun := rerunCommandLine(argv)

	if !deps.canPrompt() {
		printCLIUpgradeCommands(w, updateCmd, rerun)
		return true
	}

	confirmed, confirmErr := deps.confirm(ctx, updateCmd)
	if confirmErr != nil || !confirmed {
		// Declined or the prompt itself failed/was aborted — leave the
		// copyable commands either way.
		printCLIUpgradeCommands(w, updateCmd, rerun)
		return true
	}

	fmt.Fprintf(w, "Updating Entire CLI: %s\n", updateCmd)
	if installErr := deps.runInstaller(ctx, updateCmd); installErr != nil {
		fmt.Fprintf(w, "Update failed: %v\nTry again later running:\n  %s\n", installErr, updateCmd)
		return true
	}
	fmt.Fprintf(w, "Update complete. Rerunning the command:\n\n  %s\n\n", rerun)
	if execErr := deps.reexec(ctx, argv); execErr != nil {
		fmt.Fprintf(w, "Could not rerun automatically (%v). Rerun the command:\n\n  %s\n", execErr, rerun)
	}
	return true
}

// reexecCommand reruns the original invocation with the freshly
// installed binary: argv[0] is re-resolved (through PATH when bare, or
// the same path the installer just replaced) and the child inherits the
// terminal, so interactive flows like the login prompts still work. The
// envUpgradeRerun marker suppresses a second update offer if the rerun
// fails the same way. On success this does not return — the process
// exits with the child's exit code. A spawn-and-exit is used instead of
// syscall.Exec so the one code path covers Windows too.
func reexecCommand(ctx context.Context, argv []string) error {
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
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("rerun command: %w", runErr)
	}
	os.Exit(0)
	return nil
}

// printCLIUpgradeCommands prints the non-interactive guidance block: the
// update command, then the command that failed, each ready to copy.
func printCLIUpgradeCommands(w io.Writer, updateCmd, rerun string) {
	fmt.Fprintf(w, "%s\n\n  %s\n", cliUnsupportedMsg, updateCmd)
	if rerun != "" {
		fmt.Fprintf(w, "\nThen rerun the command:\n\n  %s\n", rerun)
	}
}

// confirmCLIUpgrade renders the Yes/No update prompt. Abort (Ctrl-C,
// timeout) surfaces as an error and the caller falls back to printing
// the commands.
func confirmCLIUpgrade(ctx context.Context, updateCmd string) (bool, error) {
	confirmed := true
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title("This Entire CLI version is no longer supported.").
			Description(fmt.Sprintf("Update now? (runs `%s`)", updateCmd)).
			Value(&confirmed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return false, fmt.Errorf("update prompt: %w", err)
	}
	return confirmed, nil
}

// rerunCommandLine reconstructs the failed invocation for display:
// argv[0] reduced to its base name, arguments with shell-significant
// whitespace or quotes quoted so the line can be pasted back verbatim.
func rerunCommandLine(argv []string) string {
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
