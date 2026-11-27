package agent

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

const testEntireLine = "entire-hook\n"

// testMergedFile is a MergedUserFile whose merge appends one Entire line.
// Entire's own state for it goes in dir's parent, standing in for the Entire
// user config directory.
func testMergedFile(dir string) MergedUserFile {
	return MergedUserFile{
		state:     testState(filepath.Dir(dir)),
		Path:      filepath.Join(dir, "settings.txt"),
		Perm:      0o600,
		HasEntire: func(data []byte) bool { return strings.Contains(string(data), testEntireLine) },
		Merge: func(data []byte, _ bool) ([]byte, int, error) {
			if strings.Contains(string(data), testEntireLine) {
				return data, 0, nil
			}
			return append(append([]byte{}, data...), testEntireLine...), 1, nil
		},
		Strip: func(data []byte) ([]byte, bool, error) {
			out := strings.Replace(string(data), testEntireLine, "", 1)
			return []byte(out), out != string(data), nil
		},
	}
}

func TestMergedUserFile_InstallInspectRemove(t *testing.T) {
	t.Parallel()
	file := testMergedFile(t.TempDir())
	ctx := context.Background()
	require.NoError(t, os.WriteFile(file.Path, []byte("user\n"), 0o600))
	assert.Equal(t, UserHooksAbsent, file.Inspect().State)

	result, err := file.Ensure(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Changed)
	assert.Equal(t, UserHooksCurrent, file.Inspect().State)
	result, err = file.Ensure(ctx, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Changed)

	removed, err := file.Remove(ctx)
	require.NoError(t, err)
	assert.True(t, removed)
	data, err := os.ReadFile(file.Path)
	require.NoError(t, err)
	assert.Equal(t, "user\n", string(data))
	removed, err = file.Remove(ctx)
	require.NoError(t, err)
	assert.False(t, removed)
}

// No config directory means the agent is not in use: nothing is created.
func TestMergedUserFile_NoConfigDirCreatesNothing(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "absent")
	file := testMergedFile(dir)
	_, err := file.Ensure(context.Background(), nil)
	require.ErrorIs(t, err, ErrUserHooksNotInUse)
	removed, err := file.Remove(context.Background())
	require.NoError(t, err)
	assert.False(t, removed)
	assert.Equal(t, UserHooksNotInUse, file.Inspect().State)
	assert.NoDirExists(t, dir)
}

// The install re-checks, under the lock, that it is still wanted: a disable
// that turned the flag off while it waited must not be undone.
func TestMergedUserFile_InstallsNothingOnceUnwanted(t *testing.T) {
	t.Parallel()
	file := testMergedFile(t.TempDir())
	result, err := file.Ensure(context.Background(), func(context.Context) bool { return false })
	require.NoError(t, err)
	assert.Zero(t, result.Changed)
	_, err = os.Stat(file.Path)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

// A config kept in a dotfiles checkout is common; the write lands on the
// link's target, and a link that does not resolve is refused, not replaced.
func TestMergedUserFile_SymlinkWriteThroughAndDanglingRefusal(t *testing.T) {
	t.Parallel()
	file := testMergedFile(t.TempDir())
	target := filepath.Join(t.TempDir(), "dotfile")
	require.NoError(t, os.WriteFile(target, []byte("user\n"), 0o600))
	require.NoError(t, os.Symlink(target, file.Path))

	_, err := file.Ensure(context.Background(), nil)
	require.NoError(t, err)
	info, err := os.Lstat(file.Path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the symlink was replaced")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Contains(t, string(data), testEntireLine)

	dangling := testMergedFile(t.TempDir())
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "not-there"), dangling.Path))
	_, err = dangling.Ensure(context.Background(), nil)
	require.Error(t, err)
	info, err = os.Lstat(dangling.Path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the dangling symlink was replaced")
}

func TestMergedUserFile_KeepsTheFileMode(t *testing.T) {
	t.Parallel()
	file := testMergedFile(t.TempDir())
	require.NoError(t, os.WriteFile(file.Path, []byte("user\n"), 0o644))
	require.NoError(t, os.Chmod(file.Path, 0o644))
	_, err := file.Ensure(context.Background(), nil)
	require.NoError(t, err)
	info, err := os.Stat(file.Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestMergedUserFile_ConcurrentCallsInstallOnce(t *testing.T) {
	t.Parallel()
	file := testMergedFile(t.TempDir())
	const callers = 8
	added := make(chan int, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			result, err := file.Ensure(context.Background(), nil)
			assert.NoError(t, err)
			added <- result.Changed
		})
	}
	wg.Wait()
	close(added)
	installs := 0
	for n := range added {
		installs += n
	}
	assert.Equal(t, 1, installs)
}

// Every command checks the file while global enable is on, so hooks already
// in are settled without the lock; only a needed write waits for it.
func TestMergedUserFile_TakesTheLockOnlyToWrite(t *testing.T) {
	t.Parallel()
	file := testMergedFile(t.TempDir())
	release, err := flock.Acquire(filepath.Join(filepath.Dir(filepath.Dir(file.Path)), userHookLockFile))
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = file.Ensure(ctx, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	require.NoError(t, os.WriteFile(file.Path, []byte(testEntireLine), 0o600))
	result, err := file.Ensure(context.Background(), nil)
	require.NoError(t, err)
	assert.Zero(t, result.Changed)
}

const testMarker = "ENTIRE-GENERATED"

func testGeneratedFile(home string) GeneratedUserFile {
	return GeneratedUserFile{
		state:   testState(filepath.Dir(home)),
		Home:    home,
		Path:    filepath.Join(home, "ext", "entire.ts"),
		Marker:  testMarker,
		Content: "// " + testMarker + " version 2\nbody\n",
		Version: regexp.MustCompile(`version (\d+)`),
	}
}

// An older render of Entire's file is rewritten; a file that is not Entire's
// is refused and kept.
func TestGeneratedUserFile_UpgradesOursAndRefusesForeign(t *testing.T) {
	t.Parallel()
	file := testGeneratedFile(t.TempDir())
	ctx := context.Background()
	require.NoError(t, os.MkdirAll(filepath.Dir(file.Path), 0o750))
	require.NoError(t, os.WriteFile(file.Path, []byte("// "+testMarker+" version 1\nold\n"), 0o644))
	report := file.Inspect()
	assert.Equal(t, UserHooksOutdated, report.State)
	assert.Equal(t, 1, report.Version)
	result, err := file.Ensure(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Changed)
	assert.Equal(t, UserHooksCurrent, file.Inspect().State)

	require.NoError(t, os.WriteFile(file.Path, []byte("theirs\n"), 0o644))
	_, err = file.Ensure(ctx, nil)
	require.Error(t, err)
	removed, err := file.Remove(ctx)
	require.NoError(t, err)
	assert.False(t, removed)
	assert.FileExists(t, file.Path)
}

// A CRLF copy (a dotfiles checkout on Windows) is current: rewriting it with
// LF would only be converted back, on every run.
func TestGeneratedUserFile_CRLFCopyIsCurrent(t *testing.T) {
	t.Parallel()
	file := testGeneratedFile(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(file.Path), 0o750))
	crlf := strings.ReplaceAll(file.Content, "\n", "\r\n")
	require.NoError(t, os.WriteFile(file.Path, []byte(crlf), 0o644))

	assert.Equal(t, UserHooksCurrent, file.Inspect().State)
	result, err := file.Ensure(context.Background(), nil)
	require.NoError(t, err)
	assert.Zero(t, result.Changed)
	data, err := os.ReadFile(file.Path)
	require.NoError(t, err)
	assert.Equal(t, crlf, string(data))
}

func TestGeneratedUserFile_NoHomeCreatesNothing(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "absent")
	file := testGeneratedFile(home)
	_, err := file.Ensure(context.Background(), nil)
	require.ErrorIs(t, err, ErrUserHooksNotInUse)
	assert.Equal(t, UserHooksNotInUse, file.Inspect().State)
	assert.NoDirExists(t, home)
}

// Removal takes away the empty directories Entire created for the file, and
// keeps a directory the user already had, empty or not.
func TestGeneratedUserFile_RemovesOnlyDirectoriesItCreated(t *testing.T) {
	t.Parallel()
	for name, preExisting := range map[string]bool{"created by Entire": false, "the user's own": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			file := testGeneratedFile(home)
			dir := filepath.Dir(file.Path)
			if preExisting {
				require.NoError(t, os.MkdirAll(dir, 0o750))
			}
			ctx := context.Background()
			_, err := file.Ensure(ctx, nil)
			require.NoError(t, err)
			removed, err := file.Remove(ctx)
			require.NoError(t, err)
			assert.True(t, removed)
			if preExisting {
				assert.DirExists(t, dir)
			} else {
				assert.NoDirExists(t, dir)
			}
			assertNoEntireFilesLeft(t, home)
			assert.DirExists(t, home)
		})
	}
}

// testJSONMergedFile merges {"hooks":{"entire":1}} into a JSON object; its
// strip leaves {} when that was all.
func testJSONMergedFile(dir string) MergedUserFile {
	const entire = `{"hooks":{"entire":1}}`
	return MergedUserFile{
		state:     testState(filepath.Dir(dir)),
		Path:      filepath.Join(dir, "settings.json"),
		Perm:      0o600,
		HasEntire: func(data []byte) bool { return string(data) == entire },
		Merge: func(data []byte, _ bool) ([]byte, int, error) {
			if string(data) == entire {
				return data, 0, nil
			}
			return []byte(entire), 1, nil
		},
		Strip: func(data []byte) ([]byte, bool, error) {
			return []byte(`{}`), string(data) == entire, nil
		},
	}
}

func TestMergedUserFile_RemovesAnEmptiedFileOnlyIfEntireCreatedIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("created by Entire", func(t *testing.T) {
		t.Parallel()
		file := testJSONMergedFile(t.TempDir())
		_, err := file.Ensure(ctx, nil)
		require.NoError(t, err)
		removed, err := file.Remove(ctx)
		require.NoError(t, err)
		assert.True(t, removed)
		assert.NoFileExists(t, file.Path)
		assertNoEntireFilesLeft(t, filepath.Dir(file.Path))
	})

	t.Run("the user's own file", func(t *testing.T) {
		t.Parallel()
		file := testJSONMergedFile(t.TempDir())
		require.NoError(t, os.WriteFile(file.Path, []byte(`{}`), 0o600))
		_, err := file.Ensure(ctx, nil)
		require.NoError(t, err)
		removed, err := file.Remove(ctx)
		require.NoError(t, err)
		assert.True(t, removed)
		data, err := os.ReadFile(file.Path)
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(data))
	})
}

// assertNoEntireFilesLeft checks that removal left none of Entire's own files
// (state, lock) in the agent's directory or in the state directory beside it.
func assertNoEntireFilesLeft(t *testing.T, agentDir string) {
	t.Helper()
	entries, err := os.ReadDir(agentDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "entire", "left in the agent's directory")
		assert.NotContains(t, e.Name(), ".lock", "left in the agent's directory")
	}
	assert.NoFileExists(t, filepath.Join(filepath.Dir(agentDir), userHookLockFile))
	assert.NoFileExists(t, filepath.Join(filepath.Dir(agentDir), userHookStateFile))
}

// Entire keeps its lock and its record of what it created outside the
// agent's directory, and removal leaves neither behind.
func TestUserFiles_LeaveNoLockOrRecordInTheAgentDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	merged := testMergedFile(t.TempDir())
	require.NoError(t, os.WriteFile(merged.Path, []byte("user\n"), 0o600))
	_, err := merged.Ensure(ctx, nil)
	require.NoError(t, err)
	_, err = merged.Remove(ctx)
	require.NoError(t, err)
	assertNoEntireFilesLeft(t, filepath.Dir(merged.Path))

	home := t.TempDir()
	generated := testGeneratedFile(home)
	_, err = generated.Ensure(ctx, nil)
	require.NoError(t, err)
	assertNoUnexpected := func() {
		entries, err := os.ReadDir(home)
		require.NoError(t, err)
		for _, e := range entries {
			assert.Equal(t, "ext", e.Name(), "Entire wrote its own file into the agent's directory")
		}
	}
	assertNoUnexpected()
	_, err = generated.Remove(ctx)
	require.NoError(t, err)
	assertNoEntireFilesLeft(t, home)
}

// A directory on the file's path that is a symlink (a dotfiles checkout) is
// the user's: removal never deletes the link, even one RemoveDir names.
func TestGeneratedUserFile_KeepsASymlinkedDirectory(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	file := testGeneratedFile(home)
	file.RemoveDir = true
	dotfiles := t.TempDir()
	require.NoError(t, os.Symlink(dotfiles, filepath.Dir(file.Path)))
	ctx := context.Background()
	_, err := file.Ensure(ctx, nil)
	require.NoError(t, err)
	removed, err := file.Remove(ctx)
	require.NoError(t, err)
	assert.True(t, removed)
	info, err := os.Lstat(filepath.Dir(file.Path))
	require.NoError(t, err, "the symlinked directory was removed")
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	assert.DirExists(t, dotfiles)
}

// testState keeps Entire's user-level state in dir instead of the Entire
// user config directory.
func testState(dir string) userHookState {
	return userHookState{open: func() (*os.Root, error) { return osroot.Shared(dir) }}
}
