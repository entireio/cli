package cli

import (
	"context"
	"fmt"
	"io"
	"os"
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

// cliUnsupportedMsg leads every cli_upgrade_required guidance block; the
// two indented lines that follow are the update command and the command
// that failed, in the order the user should run them.
const cliUnsupportedMsg = "This Entire CLI version is no longer supported. Update it, then rerun the command:"

// IsCLIUpgradeRequired reports whether err carries the server's
// cli_upgrade_required OAuth error code. auth-go surfaces the code as an
// unrecognised-code error whose message carries it verbatim — there is no
// sentinel to errors.Is against, so match on the code string.
func IsCLIUpgradeRequired(err error) bool {
	return err != nil && strings.Contains(err.Error(), cliUpgradeRequiredCode)
}

// upgradeOfferDeps carries the environment-dependent pieces of
// offerCLIUpgrade so tests can drive both branches without a TTY or a
// real installer. Zero-value fields are safe: nil confirm declines and
// nil runInstaller is never reached without confirm.
type upgradeOfferDeps struct {
	canPrompt    func() bool
	confirm      func(ctx context.Context, updateCmd string) (bool, error)
	runInstaller func(ctx context.Context, cmdStr string) error
}

// OfferCLIUpgradeIfRequired handles a cli_upgrade_required failure after
// main.go has printed the error itself. On an interactive terminal it
// offers to run the updater right away (same installer the version-check
// prompt uses); otherwise — no TTY, ENTIRE_NO_AUTO_UPDATE set, declined,
// or no runnable installer for this platform — it prints the update
// command and the failed command so the user can run both. argv is the
// failed invocation's os.Args. No-op when err doesn't carry the code.
//
// The code can surface from any auth-server OAuth flow — login (device
// and browser), the refresh grant under every authenticated command, and
// the RFC 8693 exchanges — which is why this hangs off main.go's central
// error-print arm rather than any single command.
func OfferCLIUpgradeIfRequired(ctx context.Context, w io.Writer, err error, argv []string) {
	offerCLIUpgrade(ctx, w, err, argv, upgradeOfferDeps{
		canPrompt: func() bool {
			return os.Getenv(versioncheck.EnvNoAutoUpdate) == "" &&
				versioncheck.CanAutoInstall() &&
				interactive.CanPromptInteractively() &&
				interactive.IsTerminalWriter(w)
		},
		confirm:      confirmCLIUpgrade,
		runInstaller: versioncheck.RunUpdateInstaller,
	})
}

func offerCLIUpgrade(ctx context.Context, w io.Writer, err error, argv []string, deps upgradeOfferDeps) {
	if !IsCLIUpgradeRequired(err) {
		return
	}
	updateCmd := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)
	rerun := rerunCommandLine(argv)

	if !deps.canPrompt() {
		printCLIUpgradeCommands(w, updateCmd, rerun)
		return
	}

	confirmed, confirmErr := deps.confirm(ctx, updateCmd)
	if confirmErr != nil || !confirmed {
		// Declined or the prompt itself failed/was aborted — leave the
		// copyable commands either way.
		printCLIUpgradeCommands(w, updateCmd, rerun)
		return
	}

	fmt.Fprintf(w, "\nUpdating Entire CLI: %s\n", updateCmd)
	if installErr := deps.runInstaller(ctx, updateCmd); installErr != nil {
		fmt.Fprintf(w, "Update failed: %v\nTry again later running:\n  %s\n", installErr, updateCmd)
		return
	}
	fmt.Fprintf(w, "Update complete. Rerun the command:\n\n  %s\n", rerun)
}

// printCLIUpgradeCommands prints the non-interactive guidance block: the
// update command followed by the command that failed, ready to copy.
func printCLIUpgradeCommands(w io.Writer, updateCmd, rerun string) {
	fmt.Fprintf(w, "\n%s\n\n  %s\n", cliUnsupportedMsg, updateCmd)
	if rerun != "" {
		fmt.Fprintf(w, "  %s\n", rerun)
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
