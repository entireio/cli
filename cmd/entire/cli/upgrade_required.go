package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"

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

// upgradeOfferDeps carries the environment-dependent pieces of
// offerCLIUpgrade so tests can drive both branches without a TTY, a
// real installer, or replacing the test process. Zero-value fields are
// safe: nil confirm declines, and nil runUpdate is never reached
// without confirm.
type upgradeOfferDeps struct {
	canPrompt func() bool
	confirm   func(ctx context.Context, updateCmd string) (bool, error)
	runUpdate func(ctx context.Context, w io.Writer, cmdStr string, argv []string)
}

// OfferCLIUpgradeIfRequired handles a cli_upgrade_required failure in
// place of the raw error print: it reports true when it took over the
// messaging (main.go then skips printing err — the raw OAuth chain adds
// nothing the guidance doesn't say), false when err doesn't carry the
// code. On an interactive terminal it offers to run the updater right
// away and, on success, re-executes the original command with the
// freshly installed binary (the shared versioncheck.RunUpdate tail, also
// used by the version-check prompt); otherwise — no TTY,
// ENTIRE_NO_AUTO_UPDATE set, declined, or no runnable installer for this
// platform — it prints the update command and the failed command so the
// user can run both. argv is the failed invocation's os.Args.
//
// The code can surface from any auth-server OAuth flow — login (device
// and browser), the refresh grant under every authenticated command, and
// the RFC 8693 exchanges — which is why this hangs off main.go's central
// error-print arm rather than any single command.
func OfferCLIUpgradeIfRequired(ctx context.Context, w io.Writer, err error, argv []string) bool {
	return offerCLIUpgrade(ctx, w, err, argv, upgradeOfferDeps{
		canPrompt: func() bool { return versioncheck.PromptAllowed(w) },
		confirm:   confirmCLIUpgrade,
		runUpdate: versioncheck.RunUpdate,
	})
}

func offerCLIUpgrade(ctx context.Context, w io.Writer, err error, argv []string, deps upgradeOfferDeps) bool {
	if !IsCLIUpgradeRequired(err) {
		return false
	}
	updateCmd := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)
	rerun := versioncheck.RerunCommandLine(argv)

	// Post-update rerun still rejected: the installer wrote somewhere
	// other than the binary this invocation ran (e.g. a dev build outside
	// the install manager's path). Saying "update it" again would be
	// wrong — the update already happened; name the actual problem.
	// (PromptAllowed also blocks on this marker, but this branch replaces
	// the whole guidance block, not just the prompt.)
	if versioncheck.IsPostUpdateRerun() {
		binary := "this binary"
		if len(argv) > 0 {
			binary = argv[0]
		}
		fmt.Fprintf(w, "The update installed, but this command still ran an unsupported binary (%s).\nCheck that `entire` on your PATH points at the updated install, then rerun:\n\n  %s\n", binary, rerun)
		return true
	}

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

	deps.runUpdate(ctx, w, updateCmd, argv)
	return true
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
