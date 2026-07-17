package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

func TestWithCLIUpgradeHint_AppendsUpdateCommand(t *testing.T) {
	t.Parallel()

	base := errors.New("start login: start device auth: oauth error: cli_upgrade_required")
	err := WithCLIUpgradeHint(base)
	if !errors.Is(err, base) {
		t.Fatalf("hinted error must wrap the original, got %v", err)
	}
	want := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %q, want it to contain update command %q", err, want)
	}
}

func TestWithCLIUpgradeHint_RefreshFlowError(t *testing.T) {
	t.Parallel()

	base := errors.New("refresh login token: oauth error: cli_upgrade_required")
	err := WithCLIUpgradeHint(base)
	want := versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %q, want it to contain update command %q", err, want)
	}
}

func TestWithCLIUpgradeHint_PassesThroughUnrelatedErrors(t *testing.T) {
	t.Parallel()

	base := errors.New("start login: connection refused")
	got := WithCLIUpgradeHint(base)
	if !errors.Is(got, base) {
		t.Fatalf("got %v, want it to wrap the original error", got)
	}
	if got.Error() != base.Error() {
		t.Fatalf("got %q, want message unchanged %q", got, base)
	}
}

func TestWithCLIUpgradeHint_NilStaysNil(t *testing.T) {
	t.Parallel()

	if got := WithCLIUpgradeHint(nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
