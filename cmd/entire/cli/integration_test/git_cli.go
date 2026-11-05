//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/stretchr/testify/require"
)

// runGit runs the git CLI in dir and returns its trimmed combined output,
// failing the test when git does.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// gitVersionAtLeast reports whether the installed git is at least
// wantMajor.wantMinor.
func gitVersionAtLeast(t *testing.T, wantMajor, wantMinor int) bool {
	t.Helper()
	out, err := execx.NonInteractive(t.Context(), "git", "--version").Output()
	require.NoError(t, err)
	var major, minor int
	_, err = fmt.Sscanf(string(out), "git version %d.%d", &major, &minor)
	require.NoError(t, err, "git --version: %q", out)
	return major > wantMajor || major == wantMajor && minor >= wantMinor
}
