//go:build darwin || linux

package gitrepo

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/stretchr/testify/require"
)

// A pipe cannot reproduce the hang: git only opens this pager when its output
// is a terminal. The helper owns a controlling PTY while CommitPaths starts git
// in a separate process group, just as the interactive setup command does.
func TestCommitPaths_HookPager(t *testing.T) {
	// Not parallel: isolateGitConfig and the pager overrides use t.Setenv.
	isolateGitConfig(t)
	const helper = "ENTIRE_TEST_COMMIT_PAGER_REPO"
	if dir := os.Getenv(helper); dir != "" {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		require.NoError(t, CommitPaths(ctx, dir, []string{"settings"}, "setup", os.Stdout))
		return
	}
	if _, err := exec.LookPath("less"); err != nil {
		t.Skip("less is needed to reproduce the hook pager")
	}
	dir := initGitlinkRepo(t)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings"), []byte("enabled\n"), 0o600))
	hooks := filepath.Join(dir, ".git", "hooks")
	require.NoError(t, os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nset -eu\necho HOOK_STARTED\ngit diff --cached --check\necho HOOK_FINISHED\n"), 0o755))
	gitIn(t, dir, "config", "core.pager", "less")
	gitIn(t, dir, "config", "pager.diff", "true")
	t.Setenv("GIT_PAGER", "less")
	t.Setenv("PAGER", "less")
	t.Setenv("LESS", "-R")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommitPaths_HookPager$")
	cmd.Env = append(EnvWithoutRepoOverrides(), helper+"="+dir, "TERM=xterm")
	terminal, err := pty.Start(cmd)
	require.NoError(t, err)
	defer terminal.Close()
	output := make(chan string, 1)
	go func() {
		var transcript strings.Builder
		// PTYs can end with EIO instead of EOF; successful completion and the
		// hook's final marker below determine whether the operation succeeded.
		_, _ = io.Copy(&transcript, terminal) //nolint:errcheck // see above
		output <- transcript.String()
	}()
	err = cmd.Wait()
	var transcript string
	select {
	case transcript = <-output:
	case <-ctx.Done():
		require.NoError(t, terminal.Close())
		transcript = <-output
		t.Fatalf("terminal output did not close: %s", transcript)
	}
	require.NoError(t, err, "%s", transcript)
	require.Contains(t, transcript, "HOOK_FINISHED")
	require.Equal(t, "setup\n", gitOut(t, dir, "log", "-1", "--format=%s"))
	_, err = os.Stat(filepath.Join(dir, ".git", "index.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	// Disabling the pager for this operation must not rewrite repo config.
	require.Equal(t, "less\n", gitOut(t, dir, "config", "core.pager"))
	require.Equal(t, "true\n", gitOut(t, dir, "config", "pager.diff"))
}

func TestCommitPaths_CancelsHookProcessGroup(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings"), []byte("enabled\n"), 0o600))
	hook := "#!/bin/sh\nsleep 60 &\necho HOOK_READY\nwait\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(hook), 0o755))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, w := io.Pipe()
	readerDone := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			if scanner.Text() == "HOOK_READY" {
				close(ready)
			}
		}
	}()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		err := CommitPaths(ctx, dir, []string{"settings"}, "setup", w)
		_ = w.Close() // release the test's output reader
		done <- err
	}()
	defer func() {
		cancel()
		_ = r.Close() // release the test reader on failure too
		<-readerDone
		<-finished
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not start its pipe-holding child")
	}
	cancel()
	// If only git dies, sleep keeps the exec output pipe open. Complete before
	// WaitDelay can force-close it, proving the descendant was terminated too.
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(execx.KillWaitDelay / 2):
		t.Fatal("hook child outlived cancellation")
	}
}
