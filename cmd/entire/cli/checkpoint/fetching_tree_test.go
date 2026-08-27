package checkpoint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

const windowsOS = "windows"

func TestFetchingTreeReadFileViaGitDisablesLazyFetch(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("fake git uses a POSIX shell script")
	}

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "fixture.txt", "checkpoint content")
	testutil.GitAdd(t, repoDir, "fixture.txt")
	testutil.GitCommit(t, repoDir, "add fixture")
	blobHash := strings.TrimSpace(testutil.RunGit(t, repoDir, "rev-parse", "HEAD:fixture.txt"))

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	tracePath := filepath.Join(t.TempDir(), "cat-file-env")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\t%%s\n' "$2" "$GIT_NO_LAZY_FETCH" > %q
exec %q "$@"
`, tracePath, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(repoDir)

	tree := &FetchingTree{ctx: t.Context()}
	file, err := tree.readFileViaGit("fixture.txt", &object.TreeEntry{Hash: plumbing.NewHash(blobHash)})
	require.NoError(t, err)
	contents, err := file.Contents()
	require.NoError(t, err)
	require.Equal(t, "checkpoint content", contents)
	trace, err := os.ReadFile(tracePath)
	require.NoError(t, err)
	require.Equal(t, "-p\t1\n", string(trace))
}
