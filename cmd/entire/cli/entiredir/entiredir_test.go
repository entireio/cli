package entiredir_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// The root cache is process-global. These tests share it safely because each
// works in its own t.TempDir, which keys a distinct entry. Only the Reset test
// touches the whole cache, and it is deliberately not parallel so it runs before
// any parallel test has a handle to lose.

func TestOpenAt_CreatesEntireDir(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	root, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)
	require.NotNil(t, root)

	info, err := os.Stat(filepath.Join(worktree, paths.EntireDir))
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestOpenAtForRead_DoesNotCreate(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	_, err := entiredir.OpenAtForRead(worktree)
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, statErr := os.Stat(filepath.Join(worktree, paths.EntireDir))
	require.ErrorIs(t, statErr, fs.ErrNotExist,
		"a read-only open must leave an untouched repo untouched")
}

// The point of the package is that consumers share one handle, so a second
// caller must get the same *os.Root rather than a fresh open per call site.
func TestOpenAt_ReturnsTheSameRootPerWorktree(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	first, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)
	second, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)
	require.Same(t, first, second)

	// A read-only open of an existing directory joins the same entry.
	third, err := entiredir.OpenAtForRead(worktree)
	require.NoError(t, err)
	require.Same(t, first, third)
}

func TestOpenAt_DistinctWorktreesGetDistinctRoots(t *testing.T) {
	t.Parallel()

	a, err := entiredir.OpenAt(t.TempDir())
	require.NoError(t, err)
	b, err := entiredir.OpenAt(t.TempDir())
	require.NoError(t, err)
	require.NotSame(t, a, b)
}

func TestRoot_ConfinesWritesToEntire(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	root, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)

	// A name built from an agent-supplied session ID cannot climb out.
	err = root.WriteFile("../escaped.json", []byte("nope"), 0o600)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(worktree, "escaped.json"))
	require.ErrorIs(t, statErr, fs.ErrNotExist)
}

func TestRoot_RefusesToFollowSymlinkOutOfEntire(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	outside := filepath.Join(worktree, "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("SECRET"), 0o600))

	root, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)
	if err := os.Symlink(outside, filepath.Join(worktree, paths.EntireDir, "link.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err = root.Open("link.txt")
	require.Error(t, err, "a symlink escaping .entire must not resolve")
}

func TestOpenAt_RejectsSymlinkedEntireDirectory(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(worktree, paths.EntireDir)); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err := entiredir.OpenAt(worktree)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries, "opening .entire must not create files through its symlink")
}

func TestOpen_GlobalRuntimeRejectsSymlinkBelowGitCommonDir(t *testing.T) {
	// No t.Parallel: t.Chdir and process-global path/root caches.
	worktree := t.TempDir()
	testutil.InitRepo(t, worktree)
	t.Chdir(worktree)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(entiredir.Reset)

	resolvedWorktree, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	gitCommonDir := filepath.Join(resolvedWorktree, ".git")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(gitCommonDir, "entire")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	ctx := repopolicy.WithRepoPolicy(t.Context(), repopolicy.RepoPolicy{
		Active:           true,
		ActivationSource: repopolicy.ActivationGlobal,
		WorktreeRoot:     resolvedWorktree,
		GitCommonDir:     gitCommonDir,
		WorktreeKey:      "0123456789abcdef",
	})
	_, err = entiredir.Open(ctx)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries, "the routed runtime must not be created through a planted symlink")
}

func TestOpen_GlobalRuntimeCreatesBelowGitCommonDir(t *testing.T) {
	// No t.Parallel: t.Chdir and process-global path/root caches.
	worktree := t.TempDir()
	testutil.InitRepo(t, worktree)
	t.Chdir(worktree)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(entiredir.Reset)

	resolvedWorktree, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	gitCommonDir := filepath.Join(resolvedWorktree, ".git")
	ctx := repopolicy.WithRepoPolicy(t.Context(), repopolicy.RepoPolicy{
		Active:           true,
		ActivationSource: repopolicy.ActivationGlobal,
		WorktreeRoot:     resolvedWorktree,
		GitCommonDir:     gitCommonDir,
		WorktreeKey:      "0123456789abcdef",
	})

	root, err := entiredir.Open(ctx)
	require.NoError(t, err)
	require.NoError(t, entiredir.WriteFile(root, "marker", []byte("ok"), 0o600))
	got, err := os.ReadFile(filepath.Join(gitCommonDir, "entire", "worktree", "0123456789abcdef", "marker"))
	require.NoError(t, err)
	require.Equal(t, "ok", string(got))
}

func TestWriteFile_ReplacesLeafSymlinkWithoutFollowingIt(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	root, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)

	protected := filepath.Join(worktree, paths.EntireDir, "settings.local.json")
	require.NoError(t, os.WriteFile(protected, []byte("protected"), 0o600))
	link := filepath.Join(worktree, paths.EntireDir, "marker")
	if err := os.Symlink("settings.local.json", link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	require.NoError(t, entiredir.WriteFile(root, "marker", []byte("marker"), 0o600))

	got, err := os.ReadFile(protected)
	require.NoError(t, err)
	require.Equal(t, "protected", string(got))
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.Zero(t, info.Mode()&os.ModeSymlink)
	got, err = os.ReadFile(link)
	require.NoError(t, err)
	require.Equal(t, "marker", string(got))
}

// No t.Parallel: Reset clears the cache every other test in this package
// shares. Non-parallel tests run to completion before the parallel ones resume,
// so this cannot pull a root out from under one of them.
func TestReset_DropsCachedRoots(t *testing.T) {
	worktree := t.TempDir()
	first, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)

	entiredir.Reset()

	second, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)
	require.NotSame(t, first, second)
}

func TestName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: paths.EntireDir, want: "."},
		{in: paths.EntireTmpDir, want: "tmp"},
		{in: paths.EntireMetadataDir + "/2026-01-01-abc", want: "metadata/2026-01-01-abc"},
		{in: ".entire/settings.json", want: "settings.json"},
		{in: ".entire//tmp", want: "tmp"},
		{in: "settings.json", wantErr: true},
		{in: ".entirely/tmp", wantErr: true},
		{in: "src/.entire", wantErr: true},
	}
	for _, tc := range cases {
		got, err := entiredir.Name(tc.in)
		if tc.wantErr {
			require.Errorf(t, err, "Name(%q)", tc.in)
			continue
		}
		require.NoErrorf(t, err, "Name(%q)", tc.in)
		require.Equalf(t, tc.want, got, "Name(%q)", tc.in)
	}
}

func TestSplit(t *testing.T) {
	t.Parallel()

	base := filepath.Join(string(filepath.Separator), "repo")
	entire := filepath.Join(base, paths.EntireDir)

	dir, name, ok := entiredir.Split(filepath.Join(entire, "settings.json"))
	require.True(t, ok)
	require.Equal(t, entire, dir)
	require.Equal(t, "settings.json", name)

	dir, name, ok = entiredir.Split(filepath.Join(entire, "metadata", "s1", "full.jsonl"))
	require.True(t, ok)
	require.Equal(t, entire, dir)
	require.Equal(t, "metadata/s1/full.jsonl", name)

	// The clone-preferences file lives in .git and must not be claimed.
	_, _, ok = entiredir.Split(filepath.Join(base, ".git", "entire-clone-preferences.json"))
	require.False(t, ok)

	// The directory itself is not a file within it.
	_, _, ok = entiredir.Split(entire)
	require.False(t, ok)

	// A relative path is accepted: paths.AbsPath still falls back to one when it
	// cannot resolve a worktree root, and callers pass that value straight on.
	dir, name, ok = entiredir.Split(filepath.Join(paths.EntireDir, "settings.json"))
	require.True(t, ok)
	require.Equal(t, paths.EntireDir, dir)
	require.Equal(t, "settings.json", name)

	dir, name, ok = entiredir.Split(filepath.Join(paths.EntireTmpDir, "pre-prompt-x.json"))
	require.True(t, ok)
	require.Equal(t, paths.EntireDir, dir)
	require.Equal(t, "tmp/pre-prompt-x.json", name)

	runtimeDir := filepath.Join(base, repopolicy.WorktreeRegistryRelative, strings.Repeat("a", repopolicy.RuntimeKeyLength))
	dir, name, ok = entiredir.Split(filepath.Join(runtimeDir, "tmp", "transcript.jsonl"))
	require.True(t, ok)
	require.Equal(t, runtimeDir, dir)
	require.Equal(t, "tmp/transcript.jsonl", name)

	// A bare name with no .entire component is not claimed.
	_, _, ok = entiredir.Split("settings.json")
	require.False(t, ok)

	// The innermost .entire wins when one is nested inside another.
	nested := filepath.Join(entire, "worktrees", "w", paths.EntireDir)
	dir, name, ok = entiredir.Split(filepath.Join(nested, "settings.json"))
	require.True(t, ok)
	require.Equal(t, nested, dir)
	require.Equal(t, "settings.json", name)
}

func TestOpenPath_ResolvesThroughTheSharedRoot(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	shared, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)

	root, name, err := entiredir.OpenPath(filepath.Join(worktree, paths.EntireDir, "settings.json"))
	require.NoError(t, err)
	require.Same(t, shared, root, "a path-based open must join the shared root, not open a second one")
	require.Equal(t, "settings.json", name)
}

func TestOpenPath_RejectsPathsOutsideEntire(t *testing.T) {
	t.Parallel()

	_, _, err := entiredir.OpenPath(filepath.Join(t.TempDir(), ".git", "prefs.json"))
	require.Error(t, err)
}

func TestOpenPathForRead_DoesNotCreate(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	_, _, err := entiredir.OpenPathForRead(filepath.Join(worktree, paths.EntireDir, "settings.json"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, statErr := os.Stat(filepath.Join(worktree, paths.EntireDir))
	require.ErrorIs(t, statErr, fs.ErrNotExist)
}

// The anchoring guarantee: .entire is located from the git worktree root, never
// from wherever the process happens to be. An agent running in a subdirectory
// must write to the repo's one .entire, not create a second one beside itself.
func TestOpen_AnchorsOnWorktreeRootNotCurrentDirectory(t *testing.T) {
	// No t.Parallel: t.Chdir.
	worktree := t.TempDir()
	testutil.InitRepo(t, worktree)
	sub := filepath.Join(worktree, "frontend", "src")
	require.NoError(t, os.MkdirAll(sub, 0o750))

	t.Chdir(sub)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(entiredir.Reset)

	root, err := entiredir.Open(t.Context())
	require.NoError(t, err)
	require.NoError(t, root.WriteFile("marker", []byte("x"), 0o600))

	_, err = os.Stat(filepath.Join(worktree, paths.EntireDir, "marker"))
	require.NoError(t, err, "the write must land in the worktree root's .entire")

	_, err = os.Stat(filepath.Join(sub, paths.EntireDir))
	require.ErrorIs(t, err, fs.ErrNotExist,
		"no .entire may be created in the subdirectory the process is sitting in")
}

// Outside a repository the anchor falls back to the current directory, because
// `entire enable` legitimately runs before `git init`. What makes that safe is
// that reaching the fallback needs git's positive "not a repository" verdict —
// not merely a subprocess that failed.
func TestOpen_WithoutRepositoryUsesCurrentDirectory(t *testing.T) {
	// No t.Parallel: t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(entiredir.Reset)

	root, err := entiredir.Open(t.Context())
	require.NoError(t, err)
	require.NoError(t, root.WriteFile("marker", []byte("x"), 0o600))

	_, err = os.Stat(filepath.Join(dir, paths.EntireDir, "marker"))
	require.NoError(t, err)
}

// The cwd fallback must not be reachable when git could not answer, only when
// git answered "there is no repository here". That is the case the old
// paths.AbsPath fallback got wrong: it fired on any resolve failure, so an agent
// working in a subdirectory of a real repo got its own .entire beside itself.
func TestOpen_UnresolvedRepositoryDoesNotFallBackToCurrentDirectory(t *testing.T) {
	// No t.Parallel: t.Chdir and t.Setenv.
	worktree := t.TempDir()
	testutil.InitRepo(t, worktree)
	sub := filepath.Join(worktree, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o750))

	// An empty PATH: `git` cannot be found, so nothing is known about where we
	// are. Neither directory may be guessed at.
	t.Setenv("PATH", t.TempDir())

	t.Chdir(sub)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(entiredir.Reset)

	_, err := entiredir.Open(t.Context())
	require.Error(t, err, "an unresolved worktree root must not produce a directory")
	require.NotErrorIs(t, err, paths.ErrNotARepository)

	_, statErr := os.Stat(filepath.Join(sub, paths.EntireDir))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "must not create one beside the process")
	_, statErr = os.Stat(filepath.Join(worktree, paths.EntireDir))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "must not guess the repository root either")
}

// Git's own fatals are verdicts about this directory, and none of them is the
// benign one. Dubious ownership fires INSIDE a repository, so it must surface
// rather than be answered with a directory.
func TestOpen_GitsFatalIsNotAnswerdWithADirectory(t *testing.T) {
	// No t.Parallel: t.Chdir and t.Setenv.
	worktree := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, ".git"), 0o750))

	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"),
		[]byte("#!/bin/sh\necho 'fatal: detected dubious ownership in repository' >&2\nexit 128\n"),
		0o700))
	t.Setenv("PATH", binDir)

	t.Chdir(worktree)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(entiredir.Reset)

	_, err := entiredir.Open(t.Context())
	require.Error(t, err, "a git fatal must not be answered with a directory")
	require.Contains(t, err.Error(), "dubious ownership")
}

// A relative path is the same cwd anchor by another route, so the path-based
// entry points reject it instead of resolving it.
func TestOpenPath_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, _, err := entiredir.OpenPath(filepath.Join(paths.EntireDir, "settings.json"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "absolute")
}
