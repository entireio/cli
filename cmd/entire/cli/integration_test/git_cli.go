//go:build integration

package integration

import (
	"fmt"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// gitVersionAtLeast reports whether the git on PATH is at least
// wantMajor.wantMinor.
func gitVersionAtLeast(t *testing.T, wantMajor, wantMinor int) bool {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), "git", "--version")
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	var major, minor int
	_, err = fmt.Sscanf(string(out), "git version %d.%d", &major, &minor)
	require.NoError(t, err, "git --version: %q", out)
	return major > wantMajor || major == wantMajor && minor >= wantMinor
}
