package testutil

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// MigrateToReftable migrates a temporary fixture repository, skipping the test
// when installed Git lacks ref-format migration support.
func MigrateToReftable(t *testing.T, root string) {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), "git", "refs", "migrate", "--ref-format=reftable")
	cmd.Dir = root
	cmd.Env = gitenv.Isolated()
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := string(out)
		if strings.Contains(message, "not a git command") || strings.Contains(message, "unknown subcommand") || strings.Contains(message, "unknown option") || strings.Contains(message, "unknown ref storage format") {
			t.Skipf("Git cannot migrate fixture to reftable: %v\n%s", err, out)
		}
		t.Fatalf("migrate fixture to reftable: %v\n%s", err, out)
	}
}
