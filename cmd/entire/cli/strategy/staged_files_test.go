package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStagedRaw(t *testing.T) {
	t.Parallel()
	z := "\x00"
	raw := ":100644 100644 1111111 2222222 M" + z + "modified.go" + z +
		":000000 100644 0000000 3333333 A" + z + "added name.go" + z +
		":100644 000000 4444444 0000000 D" + z + "deleted.go" + z +
		":100644 100644 5555555 6666666 R087" + z + "old.go" + z + "renamed.go" + z +
		":000000 160000 0000000 7777777 A" + z + "newsub" + z +
		":160000 160000 8888888 9999999 M" + z + "sub" + z +
		":160000 000000 aaaaaaa 0000000 D" + z + "gonesub" + z
	assert.Equal(t, []string{"modified.go", "added name.go", "deleted.go", "renamed.go"}, parseStagedRaw([]byte(raw)),
		"gitlinks are skipped; a rename reports its new path")
	assert.Empty(t, parseStagedRaw(nil))
}

// A staged `git submodule add` stages .gitmodules and a gitlink; only the
// file may reach the overlap check. Uses t.Chdir — do NOT add t.Parallel().
func TestGetStagedFiles_SkipsSubmoduleGitlink(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	subSrc := t.TempDir()
	testutil.InitRepo(t, subSrc)
	testutil.WriteFile(t, subSrc, "lib.txt", "v1\n")
	testutil.GitAdd(t, subSrc, "lib.txt")
	testutil.GitCommit(t, subSrc, "lib v1")
	testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")

	staged, err := getStagedFiles(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{".gitmodules"}, staged)
}
