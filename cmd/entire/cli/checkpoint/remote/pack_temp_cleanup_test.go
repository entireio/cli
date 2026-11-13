package remote

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// receiveCutAt is where the test upload-packs cut the stream: past the ref
// advertisement and negotiation, well inside the pack.
const receiveCutAt = 64 * 1024

// setupLargePackRemote returns a bare remote whose meta branch holds an
// incompressible blob several times receiveCutAt, and a clone of its main
// branch that does not have it yet. The clone sets fetch.unpackLimit=1 so git
// receives through index-pack, which is what writes tmp_pack_ (a fetch of
// fewer than 100 objects would otherwise unpack loose objects instead).
func setupLargePackRemote(t *testing.T) (bareDir, cloneDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	bareDir = filepath.Join(tmpDir, "bare.git")
	seedDir := filepath.Join(tmpDir, "seed")
	cloneDir = filepath.Join(tmpDir, "clone")

	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "f.txt", "init")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "init")
	runIsolatedGit(t, "", "init", "--bare", bareDir)
	runIsolatedGit(t, seedDir, "remote", "add", "origin", bareDir)
	runIsolatedGit(t, seedDir, "push", "origin", "HEAD:refs/heads/main")

	runIsolatedGit(t, seedDir, "checkout", "--orphan", "meta")
	runIsolatedGit(t, seedDir, "rm", "-rf", ".")
	blob := make([]byte, 8*receiveCutAt)
	_, err := rand.Read(blob)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "transcript.bin"), blob, 0o600))
	testutil.GitAdd(t, seedDir, "transcript.bin")
	testutil.GitCommit(t, seedDir, "checkpoint")
	runIsolatedGit(t, seedDir, "push", "origin", "HEAD:refs/heads/meta")

	runIsolatedGit(t, "", "clone", "--single-branch", "--branch", "main", "file://"+bareDir, cloneDir)
	runIsolatedGit(t, cloneDir, "config", "fetch.unpackLimit", "1")
	return bareDir, cloneDir
}

// cutStream is a shell command that passes the first receiveCutAt bytes of
// its input through and exits. dd with bs=1 rather than head -c: head
// buffers its output, which deadlocks the request/response exchange before
// the pack.
var cutStream = "dd bs=1 count=" + strconv.Itoa(receiveCutAt) + " 2>/dev/null"

// testFetchTimeout keeps a broken setup from hanging the suite.
const testFetchTimeout = time.Minute

// writeUploadPack writes an upload-pack replacement that runs the real one
// with its output piped through tail, and returns the --upload-pack argument
// that selects it.
func writeUploadPack(t *testing.T, tail string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "upload-pack.sh")
	body := "#!/bin/sh\ngit upload-pack \"$@\" | " + tail + "\n"
	require.NoError(t, os.WriteFile(script, []byte(body), 0o700))
	return "--upload-pack='" + script + "'"
}

func receiveTemps(t *testing.T, cloneDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cloneDir, ".git", "objects", "pack"))
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		if isReceiveTempName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func skipWithoutPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the upload-pack replacement is a POSIX shell script")
	}
}

// TestFetch_CutOffReceiveLeavesNoTempPack covers issue 2895: a checkpoint
// fetch whose transfer ends early made git leave a read-only tmp_pack_* of
// everything received so far in .git/objects/pack, and every later attempt
// added another.
//
// Not parallel: Fetch runs git with the process environment, so the test
// isolates git config and repository selectors through t.Setenv.
func TestFetch_CutOffReceiveLeavesNoTempPack(t *testing.T) {
	skipWithoutPOSIXShell(t)
	// A GIT_DIR or GIT_INDEX_FILE inherited from a hook outranks cmd.Dir, so
	// without this the setup's git commands would add a remote, push and switch
	// branches in the repository running the tests instead of its temp repos.
	gitenv.IsolateRepository(t)

	t.Run("plain git leaves the partial pack", func(t *testing.T) {
		// Control: shows the setup really makes git leave a temp pack, so the
		// cases below test the cleanup rather than a receive that never
		// started. Git up to at least 2.53 leaves it; should a later git
		// clean up after itself, this skips and the cleanup is redundant.
		bareDir, cloneDir := setupLargePackRemote(t)
		ctx, cancel := context.WithTimeout(context.Background(), testFetchTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "fetch", "--no-auto-gc", "--no-tags",
			writeUploadPack(t, cutStream),
			"file://"+bareDir, "+refs/heads/meta:refs/entire-test/meta")
		cmd.Dir = cloneDir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		require.Error(t, err, "the cut-off fetch should fail: %s", out)
		if len(receiveTemps(t, cloneDir)) == 0 {
			t.Skipf("this git removed its own temp pack after the failed receive: %s", out)
		}
	})

	t.Run("early EOF from the remote", func(t *testing.T) {
		bareDir, cloneDir := setupLargePackRemote(t)
		ctx, cancel := context.WithTimeout(context.Background(), testFetchTimeout)
		defer cancel()
		out, err := Fetch(ctx, FetchOptions{
			Remote:    "file://" + bareDir,
			RefSpecs:  []string{"+refs/heads/meta:refs/entire-test/meta"},
			NoTags:    true,
			NoFilter:  true,
			Dir:       cloneDir,
			ExtraArgs: []string{writeUploadPack(t, cutStream)},
		})
		require.Error(t, err, "the cut-off fetch should fail: %s", out)
		assert.Empty(t, receiveTemps(t, cloneDir), "a failed fetch must not leave its temp pack: %s", out)
	})

	t.Run("killed at its deadline", func(t *testing.T) {
		bareDir, cloneDir := setupLargePackRemote(t)
		// Send part of the pack, then hold the connection open, the way a
		// stalled remote does, so the fetch ends by its deadline's kill.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := Fetch(ctx, FetchOptions{
			Remote:    "file://" + bareDir,
			RefSpecs:  []string{"+refs/heads/meta:refs/entire-test/meta"},
			NoTags:    true,
			NoFilter:  true,
			Dir:       cloneDir,
			ExtraArgs: []string{writeUploadPack(t, "{ "+cutStream+"; sleep 60; }")},
		})
		require.Error(t, err, "the stalled fetch should be killed: %s", out)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		assert.Empty(t, receiveTemps(t, cloneDir), "a killed fetch must not leave its temp pack: %s", out)
	})

	t.Run("a temp pack from before the fetch stays", func(t *testing.T) {
		bareDir, cloneDir := setupLargePackRemote(t)
		// It may belong to another git process still receiving.
		earlier := filepath.Join(cloneDir, ".git", "objects", "pack", "tmp_pack_earlier")
		require.NoError(t, os.WriteFile(earlier, []byte("PACK"), 0o444))
		ctx, cancel := context.WithTimeout(context.Background(), testFetchTimeout)
		defer cancel()
		out, err := Fetch(ctx, FetchOptions{
			Remote:    "file://" + bareDir,
			RefSpecs:  []string{"+refs/heads/meta:refs/entire-test/meta"},
			NoTags:    true,
			NoFilter:  true,
			Dir:       cloneDir,
			ExtraArgs: []string{writeUploadPack(t, cutStream)},
		})
		require.Error(t, err, "the cut-off fetch should fail: %s", out)
		assert.Equal(t, []string{"tmp_pack_earlier"}, receiveTemps(t, cloneDir))
	})

	t.Run("a successful fetch keeps its pack", func(t *testing.T) {
		bareDir, cloneDir := setupLargePackRemote(t)
		ctx, cancel := context.WithTimeout(context.Background(), testFetchTimeout)
		defer cancel()
		out, err := Fetch(ctx, FetchOptions{
			Remote:   "file://" + bareDir,
			RefSpecs: []string{"+refs/heads/meta:refs/entire-test/meta"},
			NoTags:   true,
			NoFilter: true,
			Dir:      cloneDir,
		})
		require.NoError(t, err, "fetch output: %s", out)
		assert.Empty(t, receiveTemps(t, cloneDir))
		runIsolatedGit(t, cloneDir, "cat-file", "-e", "refs/entire-test/meta:transcript.bin")
	})
}

// Not parallel: the snapshot runs git with the process environment, so the
// test strips inherited repository selectors through t.Setenv; an inherited
// GIT_DIR would point it at the repository running the tests.
func TestPackTemps_RemovesOnlyNewReceiveTemps(t *testing.T) {
	gitenv.IsolateRepository(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	packDir := filepath.Join(repoDir, ".git", "objects", "pack")
	require.NoError(t, os.MkdirAll(packDir, 0o755))
	write := func(name string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(packDir, name), []byte("x"), 0o444))
	}

	write("tmp_pack_before")
	temps := snapshotPackTemps(context.Background(), repoDir)
	require.NotNil(t, temps)

	for _, name := range []string{"tmp_pack_new", "tmp_idx_new", "tmp_rev_new", "pack-new.pack", "tmp_obj_new", "other_tmp_pack_new"} {
		write(name)
	}

	temps.removeLeftovers(context.Background())

	assert.ElementsMatch(t, []string{"tmp_pack_before", "pack-new.pack", "tmp_obj_new", "other_tmp_pack_new"}, packDirNames(t, packDir),
		"only the receive temps that appeared after the snapshot are git's leftovers")
}

// Not parallel: repository selectors, as in TestPackTemps_RemovesOnlyNewReceiveTemps.
func TestPackTemps_LeavesSymlinks(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	gitenv.IsolateRepository(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	packDir := filepath.Join(repoDir, ".git", "objects", "pack")
	require.NoError(t, os.MkdirAll(packDir, 0o755))
	temps := snapshotPackTemps(context.Background(), repoDir)
	require.NotNil(t, temps)

	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(packDir, "tmp_pack_link")))

	temps.removeLeftovers(context.Background())

	assert.Equal(t, []string{"tmp_pack_link"}, packDirNames(t, packDir), "a symlink is not git's temp file")
	data, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data), "a symlink's target is never touched")
}

func packDirNames(t *testing.T, packDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(packDir)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// Not parallel: repository selectors, as in TestPackTemps_RemovesOnlyNewReceiveTemps.
func TestPackTemps_NilIsNoOp(t *testing.T) {
	gitenv.IsolateRepository(t)
	var temps *packTemps
	temps.removeLeftovers(context.Background())
	assert.Nil(t, snapshotPackTemps(context.Background(), filepath.Join(t.TempDir(), "not-a-repo")))
}

// Not parallel: t.Setenv.
func TestPackTemps_SkipsRedirectedObjectDirectory(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(t.TempDir(), "objects"))
	assert.Nil(t, snapshotPackTemps(context.Background(), repoDir),
		"the fetch writes outside the common dir, so there is nothing of its own to find there")
}
