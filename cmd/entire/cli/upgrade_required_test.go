package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	handled, _ := offerCLIUpgrade(context.Background(), &out, errors.New("boom"), loginArgv(), upgradeOfferDeps{
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
	handled, exitCode := offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { return false },
	})

	if !handled {
		t.Error("handled = false, want true so main.go skips the raw error")
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1 on the guidance-only leaf", exitCode)
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

func TestOfferCLIUpgrade_ConfirmYesRunsSharedUpdateFlow(t *testing.T) {
	t.Parallel()

	type updateCall struct {
		cmdStr string
		argv   []string
	}
	var calls []updateCall
	var out bytes.Buffer
	_, exitCode := offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { return true },
		confirm:   func(context.Context, string) (bool, error) { return true, nil },
		runUpdate: func(_ context.Context, _ io.Writer, cmdStr string, argv []string) (int, bool) {
			calls = append(calls, updateCall{cmdStr: cmdStr, argv: argv})
			return 3, true
		},
	})

	want := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)
	if len(calls) != 1 {
		t.Fatalf("runUpdate calls = %d, want exactly 1", len(calls))
	}
	if calls[0].cmdStr != want {
		t.Errorf("runUpdate command = %q, want %q", calls[0].cmdStr, want)
	}
	if strings.Join(calls[0].argv, " ") != strings.Join(loginArgv(), " ") {
		t.Errorf("runUpdate argv = %v, want the original invocation %v", calls[0].argv, loginArgv())
	}
	if exitCode != 3 {
		t.Errorf("exitCode = %d, want the rerun child's exit code 3", exitCode)
	}
}

func TestOfferCLIUpgrade_RerunGuardExplainsStaleBinary(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("ENTIRE_UPGRADE_RERUN", "1")

	var out bytes.Buffer
	handled, _ := offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { t.Error("prompt must be suppressed on a post-update rerun"); return true },
		runUpdate: func(context.Context, io.Writer, string, []string) (int, bool) {
			t.Error("update must not run again on a post-update rerun")
			return 0, false
		},
	})

	if !handled {
		t.Error("handled = false, want true")
	}
	got := out.String()
	if !strings.Contains(got, "still ran an unsupported binary (/usr/local/bin/entire)") {
		t.Errorf("missing stale-binary explanation naming the binary:\n%s", got)
	}
	if !strings.Contains(got, "entire login --device") {
		t.Errorf("missing rerun command:\n%s", got)
	}
	if strings.Contains(got, cliUnsupportedMsg) {
		t.Errorf("must not tell the user to update again right after updating:\n%s", got)
	}
}

func TestOfferCLIUpgrade_ConfirmNoPrintsCommands(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	_, _ = offerCLIUpgrade(context.Background(), &out, upgradeErr(), loginArgv(), upgradeOfferDeps{
		canPrompt: func() bool { return true },
		confirm:   func(context.Context, string) (bool, error) { return false, nil },
		runUpdate: func(context.Context, io.Writer, string, []string) (int, bool) {
			t.Error("update must not run when declined")
			return 0, false
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
