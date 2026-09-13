package strategy

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetStagedFiles_PreservesNonASCIIPaths verifies that getStagedFiles
// returns literal (unquoted) paths for non-ASCII filenames. With git's default
// core.quotePath=true, newline-delimited --name-only output C-escapes such
// names (café.go becomes "caf\303\251.go"), which never matches the literal
// SessionState.FilesTouched entry. Parsing -z NUL-delimited output keeps them
// literal.
func TestGetStagedFiles_PreservesNonASCIIPaths(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	cmd := exec.Command("git", "config", "core.quotePath", "true")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())

	testutil.WriteFile(t, dir, "café.go", "package main\n")
	testutil.WriteFile(t, dir, "control.go", "package main\n")
	testutil.GitAdd(t, dir, "café.go", "control.go")

	staged, err := getStagedFiles(context.Background())
	require.NoError(t, err)
	require.NotNil(t, staged)
	assert.Contains(t, staged, "café.go")
	assert.Contains(t, staged, "control.go")
	for _, entry := range staged {
		assert.NotContains(t, entry, `"`, "staged entry must not be git-quoted: %q", entry)
		assert.False(t, strings.Contains(entry, `caf\303\251.go`),
			"staged entry must not be C-escaped: %q", entry)
	}
}
