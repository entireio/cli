package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStagedRaw(t *testing.T) {
	t.Parallel()
	z := "\x00"
	h := func(c string) string { return strings.Repeat(c, 40) }
	raw := ":100644 100644 " + h("1") + " " + h("2") + " M" + z + "modified.go" + z +
		":000000 100644 " + h("0") + " " + h("3") + " A" + z + "added name.go" + z +
		":100644 000000 " + h("4") + " " + h("0") + " D" + z + "deleted.go" + z +
		":100644 100644 " + h("5") + " " + h("6") + " R087" + z + "old.go" + z + "renamed.go" + z +
		":100644 100755 " + h("b") + " " + h("b") + " M" + z + "script.sh" + z +
		":000000 160000 " + h("0") + " " + h("7") + " A" + z + "newsub" + z +
		":160000 160000 " + h("8") + " " + h("9") + " M" + z + "sub" + z +
		":160000 000000 " + h("a") + " " + h("0") + " D" + z + "gonesub" + z
	assert.Equal(t, []stagedChange{
		{path: "modified.go", stagedEntry: stagedEntry{blob: plumbing.NewHash(h("2")), mode: filemode.Regular}},
		{path: "added name.go", stagedEntry: stagedEntry{blob: plumbing.NewHash(h("3")), mode: filemode.Regular}},
		{path: "deleted.go", stagedEntry: stagedEntry{deleted: true}},
		{path: "renamed.go", stagedEntry: stagedEntry{blob: plumbing.NewHash(h("6")), mode: filemode.Regular}},
		{path: "script.sh", stagedEntry: stagedEntry{blob: plumbing.NewHash(h("b")), mode: filemode.Executable}},
	}, parseStagedRaw([]byte(raw)),
		"gitlinks are skipped; a rename reports its new path; each path keeps the blob and mode the index stages")
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
