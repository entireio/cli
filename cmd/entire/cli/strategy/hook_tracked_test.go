package strategy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// trackedProjectHook is a shell hook whose one observable line is a builtin,
// so the run still works when PATH cannot see the real entire binary.
const trackedProjectHook = "#!/bin/sh\nprintf '%s\\n' project-hook\n"

// newTrackedHookRepo commits one shell hook, then points core.hooksPath at
// that directory. The commit happens while hooks still live in .git/hooks, so
// it does not run the script under test.
func newTrackedHookRepo(t *testing.T, body string) (string, string, []byte) {
	t.Helper()

	repo, _ := initHooksTestRepo(t)
	testutil.RunGit(t, repo, "config", "user.email", "hook@example.com")
	testutil.RunGit(t, repo, "config", "user.name", "Hook Test")
	testutil.RunGit(t, repo, "config", "commit.gpgsign", "false")
	testutil.RunGit(t, repo, "config", "core.autocrlf", "false")

	hooks := filepath.Join(repo, "githooks")
	require.NoError(t, os.MkdirAll(hooks, 0o755))
	hookPath := filepath.Join(hooks, prePushHook)
	require.NoError(t, os.WriteFile(hookPath, []byte(body), 0o755))
	testutil.RunGit(t, repo, "add", "--", "githooks/"+prePushHook)
	testutil.RunGit(t, repo, "commit", "-m", "add hook")

	original, err := os.ReadFile(hookPath)
	require.NoError(t, err)

	testutil.RunGit(t, repo, "config", "core.hooksPath", "githooks")
	ClearHooksDirCache()
	t.Cleanup(ClearHooksDirCache)
	return repo, hookPath, original
}

func requireProjectAfter(t *testing.T, content, project string) {
	t.Helper()
	require.Contains(t, content, trackedSpliceBegin)
	require.Contains(t, content, trackedSpliceEnd)
	end := strings.Index(content, trackedSpliceEnd)
	require.NotContains(t, content[:end], project)
	require.Contains(t, content[end+len(trackedSpliceEnd):], project)
}

func runTrackedHook(t *testing.T, hookPath string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), shellForHook(t), hookPath)
	// Hide the real entire binary. The pre-push splice exits before the
	// project's script when that binary is found and then fails.
	cmd.Env = envWithPath(t.TempDir())
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%s", out)
	return string(out)
}

// shellForHook is a POSIX sh that can run a spliced hook. Git for Windows
// ships sh.exe beside git, but it is not on PATH for the test process.
func shellForHook(t *testing.T) string {
	t.Helper()
	if sh, err := exec.LookPath("sh"); err == nil {
		return sh
	}
	var candidates []string
	if gitExe, err := exec.LookPath("git"); err == nil {
		dir := filepath.Dir(gitExe)
		candidates = append(candidates,
			filepath.Join(dir, "sh.exe"),
			filepath.Join(dir, "..", "usr", "bin", "sh.exe"),
			filepath.Join(dir, "..", "bin", "sh.exe"),
		)
	}
	if out, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output(); err == nil {
		base := strings.TrimSpace(string(out))
		candidates = append(candidates,
			filepath.Join(base, "..", "..", "..", "usr", "bin", "sh.exe"),
			filepath.Join(base, "..", "..", "usr", "bin", "sh.exe"),
			filepath.Join(base, "..", "..", "bin", "sh.exe"),
		)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("sh not available")
	return ""
}

func hookSnapshot(t *testing.T, repo, name string) []byte {
	t.Helper()
	gitDir := strings.TrimSpace(testutil.RunGit(t, repo, "rev-parse", "--absolute-git-dir"))
	data, err := os.ReadFile(filepath.Join(gitDir, hookOriginalsDir, name))
	require.NoError(t, err)
	return data
}

func TestSpliceTrackedRoundTrip(t *testing.T) {
	t.Parallel()

	spec := findHookSpec(t, buildHookSpecs(bareEntireHookCmd), prePushHook)
	once, err := spliceTrackedHook(prePushHook, spec.content, []byte(trackedProjectHook))
	require.NoError(t, err)
	back, ok := stripTrackedSplice(once)
	require.True(t, ok)
	require.Equal(t, trackedProjectHook, back)

	twice, err := spliceTrackedHook(prePushHook, spec.content, []byte(once))
	require.NoError(t, err)
	require.Equal(t, once, twice)

	edited := once + "printf '%s\\n' edited\n"
	refreshed, err := spliceTrackedHook(prePushHook, spec.content, []byte(edited))
	require.NoError(t, err)
	requireProjectAfter(t, refreshed, "edited")
	back, ok = stripTrackedSplice(refreshed)
	require.True(t, ok)
	require.Equal(t, trackedProjectHook+"printf '%s\\n' edited\n", back)

	crlf := "#!/bin/sh\r\nprintf '%s\\n' project-hook\r\n"
	spliced, err := spliceTrackedHook(prePushHook, spec.content, []byte(crlf))
	require.NoError(t, err)
	require.Contains(t, spliced, "\r\n")
	back, ok = stripTrackedSplice(spliced)
	require.True(t, ok)
	require.Equal(t, crlf, back)

	withE := "#!/bin/sh -e\nprintf '%s\\n' keep\n"
	spliced, err = spliceTrackedHook(prePushHook, spec.content, []byte(withE))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(spliced, "#!/bin/sh -e\n"))
}

func TestSpliceShellShebang(t *testing.T) {
	t.Parallel()

	cases := []struct {
		line string
		want bool
	}{
		{line: "#!/bin/sh", want: true},
		{line: "#!/bin/sh -e", want: true},
		{line: "#!/usr/bin/env bash", want: true},
		{line: "#!/bin/bash -e", want: true},
		{line: "#!/bin/dash", want: true},
		{line: "#!/bin/ash", want: true},
		{line: "#!/usr/bin/env python3", want: false},
		{line: "#!/bin/python3", want: false},
		{line: "#!/bin/ship", want: false},
		{line: "printf hi", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, isShellHook([]byte(tc.line+"\n")))
		})
	}
	require.True(t, isShellHook([]byte("#!/bin/sh\r\nprintf hi\n")))
	require.False(t, isShellHook(append([]byte("#!/bin/sh\n"), 0)))

	project := "printf '%s\\n' project-hook\n"
	original := "#!/bin/sh\n" + project
	pre := findHookSpec(t, buildHookSpecs(bareEntireHookCmd), prePushHook)
	spliced, err := spliceTrackedHook(prePushHook, pre.content, []byte(original))
	require.NoError(t, err)
	require.Contains(t, spliced, "exit")
	require.Contains(t, spliced, PrePushStdinRefsEnv)
	requireProjectAfter(t, spliced, "project-hook")

	post := findHookSpec(t, buildHookSpecs(bareEntireHookCmd), postRewriteHook)
	spliced, err = spliceTrackedHook(postRewriteHook, post.content, []byte(original))
	require.NoError(t, err)
	require.Contains(t, spliced, "_entire_stdin")
	require.Contains(t, spliced, postRewriteInvocationWithStdin)
	require.NotContains(t, spliced, "trap")
	requireProjectAfter(t, spliced, "project-hook")
}

func TestInstallGitHook_TrackedShellHookStaysInTheFile(t *testing.T) {
	repo, hookPath, original := newTrackedHookRepo(t, trackedProjectHook)
	notice := captureHintStderr(t)

	n, err := InstallGitHook(t.Context(), true, false)
	require.NoError(t, err)
	require.Equal(t, len(gitHookNames), n)

	status := testutil.RunGit(t, repo, "status", "--porcelain")
	require.NotContains(t, status, backupSuffix)
	diff := testutil.RunGit(t, repo, "diff", "--", "githooks/"+prePushHook)
	require.NotContains(t, diff, "-printf")
	require.NotContains(t, diff, backupSuffix)
	require.Contains(t, diff, trackedSpliceBegin)

	got, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	requireProjectAfter(t, string(got), "project-hook")
	require.Contains(t, string(got), "hooks git pre-push")
	require.Equal(t, original, hookSnapshot(t, repo, prePushHook))

	text := notice.String()
	require.Contains(t, text, "githooks/"+prePushHook)
	require.Contains(t, text, hookOriginalsDir)
	require.Contains(t, text, "is tracked")
	require.NotContains(t, text, repo)
	require.NotContains(t, text, filepath.ToSlash(repo))

	if runtime.GOOS != "windows" {
		gitDir := strings.TrimSpace(testutil.RunGit(t, repo, "rev-parse", "--absolute-git-dir"))
		info, statErr := os.Stat(filepath.Join(gitDir, hookOriginalsDir, prePushHook))
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	again, err := InstallGitHook(t.Context(), true, false)
	require.NoError(t, err)
	require.Equal(t, 0, again)
	second, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	require.Equal(t, got, second)
	require.Equal(t, original, hookSnapshot(t, repo, prePushHook))

	require.Contains(t, runTrackedHook(t, hookPath), "project-hook")

	res, err := RemoveGitHookDetailed(t.Context())
	require.NoError(t, err)
	require.Contains(t, res.Restored, prePushHook)
	restored, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	require.Equal(t, original, restored)
	require.Empty(t, strings.TrimSpace(testutil.RunGit(t, repo, "diff", "HEAD")))
}

func TestInstallGitHook_TrackedSpliceRunsInAFreshClone(t *testing.T) {
	repo, _, _ := newTrackedHookRepo(t, trackedProjectHook)
	_, err := InstallGitHook(t.Context(), true, false)
	require.NoError(t, err)

	// The commit records the splice. Point this one command at .git/hooks so
	// the wrappers just installed in githooks/ do not run the real entire binary.
	idle := filepath.ToSlash(filepath.Join(repo, ".git", "hooks"))
	testutil.RunGit(t, repo, "add", "--", "githooks")
	testutil.RunGit(t, repo, "-c", "core.hooksPath="+idle, "commit", "-m", "splice hook")

	clone := t.TempDir()
	testutil.RunGit(t, repo, "clone", "-c", "core.autocrlf=false", repo, clone)
	cloned := filepath.Join(clone, "githooks", prePushHook)
	body, err := os.ReadFile(cloned)
	require.NoError(t, err)
	requireProjectAfter(t, string(body), "project-hook")
	_, err = os.Stat(cloned + backupSuffix)
	require.ErrorIs(t, err, os.ErrNotExist)

	// core.hooksPath is local config and is not cloned. Run the script by path.
	require.Contains(t, runTrackedHook(t, cloned), "project-hook")
}

func TestInstallGitHook_TrackedNonShellHookIsRefused(t *testing.T) {
	repo, hookPath, original := newTrackedHookRepo(t, "#!/usr/bin/env python3\nprint('nope')\n")
	_, err := InstallGitHook(t.Context(), true, false)
	require.ErrorIs(t, err, errTrackedHookNotShell)
	require.Contains(t, err.Error(), "githooks/"+prePushHook)
	require.NotContains(t, err.Error(), repo)

	got, readErr := os.ReadFile(hookPath)
	require.NoError(t, readErr)
	require.Equal(t, original, got)
	entries, readErr := os.ReadDir(filepath.Dir(hookPath))
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
	require.Equal(t, prePushHook, entries[0].Name())
}

func TestInstallGitHook_TrackedSymlinkIsRefused(t *testing.T) {
	repo, hookPath, _ := newTrackedHookRepo(t, trackedProjectHook)
	target := filepath.Join(repo, "target.sh")
	require.NoError(t, os.WriteFile(target, []byte(trackedProjectHook), 0o755))
	require.NoError(t, os.Remove(hookPath))
	if err := os.Symlink(target, hookPath); err != nil {
		t.Skipf("symlink: %v", err)
	}

	_, err := InstallGitHook(t.Context(), true, false)
	require.ErrorIs(t, err, errTrackedHookSymlink)
	require.NotContains(t, err.Error(), repo)

	entries, readErr := os.ReadDir(filepath.Dir(hookPath))
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
	info, statErr := os.Lstat(hookPath)
	require.NoError(t, statErr)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestInstallGitHook_GitignoredHooksPathStillBacksUp(t *testing.T) {
	// Detection is index membership, not ignore status. A force-added ignored
	// file is tracked and would be spliced; this file is not in the index.
	repo, _ := initHooksTestRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".husky/\n"), 0o644))
	dir := filepath.Join(repo, ".husky", "_")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	hookPath := filepath.Join(dir, prePushHook)
	require.NoError(t, os.WriteFile(hookPath, []byte(trackedProjectHook), 0o755))
	testutil.RunGit(t, repo, "config", "core.hooksPath", ".husky/_")
	ClearHooksDirCache()
	t.Cleanup(ClearHooksDirCache)

	_, err := InstallGitHook(t.Context(), true, false)
	require.NoError(t, err)
	got, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	require.Contains(t, string(got), entireHookMarker)
	require.NotContains(t, string(got), trackedSpliceBegin)
	_, err = os.Stat(hookPath + backupSuffix)
	require.NoError(t, err)
}

func overwriteWithEntireWrapper(t *testing.T, hookPath string, sidecar []byte) {
	t.Helper()
	spec := findHookSpec(t, buildHookSpecs(bareEntireHookCmd), prePushHook)
	require.NoError(t, os.WriteFile(hookPath, []byte(spec.content), 0o755))
	if sidecar != nil {
		require.NoError(t, os.WriteFile(hookPath+backupSuffix, sidecar, 0o755))
	}
}

func TestInstallGitHook_RepairsWrapperAndUntrackedSidecar(t *testing.T) {
	_, hookPath, original := newTrackedHookRepo(t, trackedProjectHook)
	notice := captureHintStderr(t)
	overwriteWithEntireWrapper(t, hookPath, original)

	_, err := InstallGitHook(t.Context(), true, false)
	require.NoError(t, err)
	got, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	requireProjectAfter(t, string(got), "project-hook")
	_, err = os.Stat(hookPath + backupSuffix)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Contains(t, notice.String(), "Removed the untracked")
}

func TestInstallGitHook_RepairsTrackedWrapperFromIndex(t *testing.T) {
	_, hookPath, _ := newTrackedHookRepo(t, trackedProjectHook)
	overwriteWithEntireWrapper(t, hookPath, nil)

	_, err := InstallGitHook(t.Context(), true, false)
	require.NoError(t, err)
	got, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	requireProjectAfter(t, string(got), "project-hook")
	_, err = os.Stat(hookPath + backupSuffix)
	require.ErrorIs(t, err, os.ErrNotExist)
}
