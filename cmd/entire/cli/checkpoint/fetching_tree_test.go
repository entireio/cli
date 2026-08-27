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
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
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

	tracePath := traceGitCatFile(t)
	t.Chdir(repoDir)

	tree := &FetchingTree{ctx: t.Context()}
	file, err := tree.readFileViaGit("fixture.txt", &object.TreeEntry{Hash: plumbing.NewHash(blobHash)})
	require.NoError(t, err)
	contents, err := file.Contents()
	require.NoError(t, err)
	require.Equal(t, "checkpoint content", contents)
	require.Equal(t, []string{"-p\t1"}, readGitCatFileTrace(t, tracePath))
}

// TestFetchingTreeCollectMissingBlobsProbesOnce pins both halves of the
// missing-blob probe: one subprocess for the whole candidate set (not one per
// blob), with lazy fetching disabled so the probe reports absence instead of
// creating presence.
func TestFetchingTreeCollectMissingBlobsProbesOnce(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("fake git uses a POSIX shell script")
	}

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	hashes := make([]plumbing.Hash, 0, 3)
	entries := make([]object.TreeEntry, 0, 3)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		testutil.WriteFile(t, repoDir, name, "content of "+name)
		testutil.GitAdd(t, repoDir, name)
	}
	testutil.GitCommit(t, repoDir, "add fixtures")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		hash := plumbing.NewHash(strings.TrimSpace(testutil.RunGit(t, repoDir, "rev-parse", "HEAD:"+name)))
		hashes = append(hashes, hash)
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
	}

	// Delete b.txt's loose object so exactly one candidate is genuinely
	// absent from the object store while the other two remain readable.
	absent := hashes[1].String()
	require.NoError(t, os.Remove(filepath.Join(repoDir, ".git", "objects", absent[:2], absent[2:])))

	tracePath := traceGitCatFile(t)
	t.Chdir(repoDir)

	// A storer that sees nothing makes every entry a candidate — the
	// partial-clone shape, where go-git's index misses blobs that are on disk.
	tree := &FetchingTree{
		inner:  &object.Tree{Entries: entries},
		ctx:    t.Context(),
		storer: blindStorer{},
	}

	require.Equal(t, []plumbing.Hash{hashes[1]}, tree.CollectMissingBlobs())
	require.Equal(t, []string{"--batch-check\t1"}, readGitCatFileTrace(t, tracePath))
}

func TestFetchingTreeCollectMissingBlobsProbeFailureKeepsEveryCandidate(t *testing.T) {
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")
	tree := &FetchingTree{
		// Not a git repository, so the batch probe cannot answer.
		inner:  &object.Tree{Entries: []object.TreeEntry{{Name: "a.txt", Mode: filemode.Regular, Hash: hash}}},
		ctx:    t.Context(),
		storer: blindStorer{},
	}
	t.Chdir(t.TempDir())

	require.Equal(t, []plumbing.Hash{hash}, tree.CollectMissingBlobs())
}

// blindStorer reports every object as absent, standing in for go-git's storer
// in a partial clone.
type blindStorer struct {
	storer.EncodedObjectStorer
}

func (blindStorer) HasEncodedObject(plumbing.Hash) error {
	return plumbing.ErrObjectNotFound
}

// traceGitCatFile puts a git wrapper on PATH that records
// "<first argument>\t<GIT_NO_LAZY_FETCH>" for every cat-file invocation before
// exec'ing the real git. GIT_NO_LAZY_FETCH is seeded to 0 in this process so
// the trace shows what the FetchingTree set rather than an inherited value.
// Returns the trace path.
func traceGitCatFile(t *testing.T) string {
	t.Helper()

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	tracePath := filepath.Join(t.TempDir(), "cat-file-env")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "cat-file" ]; then
	printf '%%s\t%%s\n' "$2" "$GIT_NO_LAZY_FETCH" >> %q
fi
exec %q "$@"
`, tracePath, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tracePath
}

func readGitCatFileTrace(t *testing.T, tracePath string) []string {
	t.Helper()

	trace, err := os.ReadFile(tracePath)
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(trace), "\n"), "\n")
}
