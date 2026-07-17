package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

func upgradeErr() error {
	return errors.New("start login: start device auth: oauth error: cli_upgrade_required")
}

func loginArgv() []string {
	return []string{"/usr/local/bin/entire", "login", "--device"}
}

func TestIsCLIUpgradeRequired(t *testing.T) {
	t.Parallel()

	if !IsCLIUpgradeRequired(upgradeErr()) {
		t.Error("want true for an error carrying cli_upgrade_required")
	}
	if IsCLIUpgradeRequired(errors.New("connection refused")) {
		t.Error("want false for an unrelated error")
	}
	if IsCLIUpgradeRequired(nil) {
		t.Error("want false for nil")
	}
}

func TestOfferCLIUpgrade_UnrelatedErrorPrintsNothing(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	handled := offerCLIUpgrade(context.Background(), &out, errors.New("boom"), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { t.Error("canPrompt must not be consulted"); return false },
	})
	if handled {
		t.Error("handled = true, want false so main.go prints the error itself")
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output, got:\n%s", out.String())
	}
}

func TestOfferCLIUpgrade_NonInteractivePrintsUpdateAndRerunCommands(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	handled := offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { return false },
	})

	if !handled {
		t.Error("handled = false, want true so main.go skips the raw error")
	}
	got := out.String()
	if !strings.Contains(got, "This Entire CLI version is no longer supported. Update it:") {
		t.Errorf("missing unsupported-version message:\n%s", got)
	}
	if want := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version); !strings.Contains(got, want) {
		t.Errorf("missing update command %q:\n%s", want, got)
	}
	if !strings.Contains(got, "Then rerun the command:") {
		t.Errorf("missing rerun label:\n%s", got)
	}
	if !strings.Contains(got, "entire login --device") {
		t.Errorf("missing failed command to rerun:\n%s", got)
	}
}

func TestOfferCLIUpgrade_ConfirmYesRunsInstallerAndRerunsCommand(t *testing.T) {
	t.Parallel()

	var installed []string
	var rerunArgv [][]string
	var out bytes.Buffer
	offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { return true },
		confirm:   func(context.Context, string) (bool, error) { return true, nil },
		runInstaller: func(_ context.Context, cmdStr string) error {
			installed = append(installed, cmdStr)
			return nil
		},
		reexec: func(_ context.Context, argv []string) error {
			rerunArgv = append(rerunArgv, argv)
			return nil
		},
	})

	want := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)
	if len(installed) != 1 || installed[0] != want {
		t.Fatalf("installer calls = %v, want exactly [%q]", installed, want)
	}
	if len(rerunArgv) != 1 || strings.Join(rerunArgv[0], " ") != strings.Join(loginArgv(), " ") {
		t.Fatalf("reexec argv = %v, want the original invocation %v", rerunArgv, loginArgv())
	}
	got := out.String()
	if !strings.Contains(got, "Update complete") {
		t.Errorf("missing completion message:\n%s", got)
	}
	if !strings.Contains(got, "entire login --device") {
		t.Errorf("missing rerun command line:\n%s", got)
	}
}

func TestOfferCLIUpgrade_ReexecFailurePrintsRerunCommand(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt:    func() bool { return true },
		confirm:      func(context.Context, string) (bool, error) { return true, nil },
		runInstaller: func(context.Context, string) error { return nil },
		reexec:       func(context.Context, []string) error { return errors.New("exec format error") },
	})

	got := out.String()
	if !strings.Contains(got, "entire login --device") {
		t.Errorf("missing manual rerun command after failed re-exec:\n%s", got)
	}
}

func TestUpgradePromptAllowed_RerunGuardBlocksSecondPrompt(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv(envUpgradeRerun, "1")
	t.Setenv("ENTIRE_TEST_TTY", "1") // force the interactive probe on; the guard must still win

	if upgradePromptAllowed(&bytes.Buffer{}) {
		t.Error("prompt must be suppressed on a post-update rerun to avoid an update loop")
	}
}

func TestOfferCLIUpgrade_ConfirmNoPrintsCommands(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { return true },
		confirm:   func(context.Context, string) (bool, error) { return false, nil },
		runInstaller: func(context.Context, string) error {
			t.Error("installer must not run when declined")
			return nil
		},
	})

	got := out.String()
	if !strings.Contains(got, "This Entire CLI version is no longer supported. Update it:") {
		t.Errorf("missing unsupported-version message:\n%s", got)
	}
	if !strings.Contains(got, "entire login --device") {
		t.Errorf("missing failed command to rerun:\n%s", got)
	}
}

func TestOfferCLIUpgrade_InstallerFailurePrintsRetryCommand(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt:    func() bool { return true },
		confirm:      func(context.Context, string) (bool, error) { return true, nil },
		runInstaller: func(context.Context, string) error { return errors.New("brew exploded") },
	})

	got := out.String()
	if !strings.Contains(got, "Update failed") {
		t.Errorf("missing failure message:\n%s", got)
	}
	if want := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version); !strings.Contains(got, want) {
		t.Errorf("missing retry command %q:\n%s", want, got)
	}
}

func TestRerunCommandLine(t *testing.T) {
	t.Parallel()

	got := rerunCommandLine([]string{"/usr/local/bin/entire", "api", "/me", "--to", "cell"})
	if got != "entire api /me --to cell" {
		t.Errorf("got %q, want %q", got, "entire api /me --to cell")
	}
	if got := rerunCommandLine([]string{"entire", "dispatch", "fix the thing"}); got != `entire dispatch "fix the thing"` {
		t.Errorf("got %q, want quoted arg", got)
	}
	if got := rerunCommandLine(nil); got != "" {
		t.Errorf("got %q, want empty for nil argv", got)
	}
}
