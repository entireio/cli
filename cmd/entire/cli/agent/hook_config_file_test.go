package agent_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/stretchr/testify/require"
)

// skipWithoutSymlinks skips a test that needs to create one. On Windows that
// takes elevation or developer mode, neither of which CI has.
func skipWithoutSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
}

func TestHookConfig_ReadWriteRemoveRoundTrip(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	cfg, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	require.NoError(t, err)

	_, err = cfg.Read()
	require.True(t, os.IsNotExist(err), "a missing file must classify with os.IsNotExist: %v", err)
	require.False(t, cfg.Exists())

	require.NoError(t, cfg.Write([]byte(`{"hooks":{}}`), 0o600))
	require.True(t, cfg.Exists())

	got, err := cfg.Read()
	require.NoError(t, err)
	require.JSONEq(t, `{"hooks":{}}`, string(got))
	require.Equal(t, filepath.Join(worktree, ".claude", "settings.json"), cfg.Path())

	require.NoError(t, cfg.Remove())
	require.False(t, cfg.Exists())
	require.NoError(t, cfg.Remove(), "removing an absent file is not an error")
}

// The case the type exists for. A repository can carry a checked-in symlink at
// `.claude`, and `entire enable` used to create directories and write JSON
// through it — to wherever it pointed, outside the repository included.
func TestHookConfig_WriteRefusesASymlinkedConfigDirectory(t *testing.T) {
	t.Parallel()
	skipWithoutSymlinks(t)

	worktree := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(worktree, ".claude")))

	cfg, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	require.NoError(t, err, "opening is fine; it is the write that must refuse")

	err = cfg.Write([]byte(`{"hooks":{}}`), 0o600)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)

	_, statErr := os.Stat(filepath.Join(outside, "settings.json"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "nothing may be written through the link")
}

// A symlinked config FILE is not refused by this type — only a symlinked parent
// directory is. Pointing .claude/settings.json at another file in the checkout
// is a real setup and Entire has no business breaking it.
func TestHookConfig_ReadFollowsARelativeSymlinkInsideTheWorktree(t *testing.T) {
	t.Parallel()
	skipWithoutSymlinks(t)

	worktree := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, ".claude"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "real.json"), []byte(`{"a":1}`), 0o600))
	require.NoError(t, os.Symlink("../real.json", filepath.Join(worktree, ".claude", "settings.json")))

	cfg, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	require.NoError(t, err)
	got, err := cfg.Read()
	require.NoError(t, err)
	require.JSONEq(t, `{"a":1}`, string(got))
}

// An ABSOLUTE symlink at the leaf is refused, and that is a deliberate
// behaviour change rather than a side effect worth glossing over: os.Root
// documents that "symbolic links must not be absolute" unconditionally, so a
// `.claude/settings.json -> ~/dotfiles/claude/settings.json` created by a
// dotfile manager stops resolving.
//
// It is kept because the alternative is a root that can be stepped out of by
// anything that plants an absolute link, which is the whole property this type
// exists for, and because the same rule already governs `.entire`. What it costs
// is a legitimate setup, so the failure has to be legible: the error names the
// path, and read failures surface as "could not tell" rather than as "no hooks
// installed".
func TestHookConfig_ReadRefusesAnAbsoluteSymlinkAtTheLeaf(t *testing.T) {
	t.Parallel()
	skipWithoutSymlinks(t)

	worktree := t.TempDir()
	outside := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"a":1}`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, ".claude"), 0o750))
	require.NoError(t, os.Symlink(outside, filepath.Join(worktree, ".claude", "settings.json")))

	cfg, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	require.NoError(t, err)

	_, err = cfg.Read()
	require.Error(t, err)
	require.False(t, os.IsNotExist(err),
		"an escaping link must not read as an absent file, which callers answer with default settings")
}

// The root is anchored on the worktree, so a relPath climbing out of it is
// rejected while the name is still being resolved rather than at the open.
func TestHookConfig_RejectsAPathOutsideTheWorktree(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	for _, rel := range []string{"../escape.json", "..", "."} {
		_, err := agent.OpenHookConfig(worktree, rel)
		require.Error(t, err, "relPath %q must not resolve", rel)
	}
}

func TestHookConfig_RequiresAWorktreeRoot(t *testing.T) {
	t.Parallel()

	_, err := agent.OpenHookConfig("", ".claude/settings.json")
	require.Error(t, err)
}

// os.Root blocks a symlink that escapes the worktree but follows one pointing
// elsewhere inside it. Only Write checked, so a repository shipping
// ".claude -> vendor/x" had its planted settings read, reported present, and had
// Remove delete the file at the far end.
func TestHookConfigFile_RefusesASymlinkedDirectoryOnEveryOperation(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	planted := filepath.Join(worktree, "vendor")
	require.NoError(t, os.MkdirAll(planted, 0o750))
	plantedFile := filepath.Join(planted, "settings.json")
	require.NoError(t, os.WriteFile(plantedFile, []byte(`{"planted":true}`), 0o600))
	if err := os.Symlink("vendor", filepath.Join(worktree, ".claude")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	f, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	require.NoError(t, err)

	_, readErr := f.Read()
	require.ErrorIs(t, readErr, osroot.ErrSymlinkedPath)

	require.False(t, f.Exists(), "a file behind a symlinked directory is not present at a path Entire will read")

	require.ErrorIs(t, f.Write([]byte(`{"entire":true}`), 0o600), osroot.ErrSymlinkedPath)

	require.ErrorIs(t, f.Remove(), osroot.ErrSymlinkedPath)
	got, err := os.ReadFile(plantedFile)
	require.NoError(t, err, "Remove must not delete the file at the far end of the link")
	require.Equal(t, `{"planted":true}`, string(got))

	require.Equal(t, agent.HooksAbsent, f.GeneratedState("marker", "render"))
}

// The documented dotfile-repo setup still works: only the DIRECTORY components
// are refused, never the leaf.
func TestHookConfigFile_StillFollowsASymlinkedFileAtTheLeaf(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, ".claude"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "dotfiles.json"), []byte(`{"from":"dotfiles"}`), 0o600))
	if err := os.Symlink("../dotfiles.json", filepath.Join(worktree, ".claude", "settings.json")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	f, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	require.NoError(t, err)

	data, err := f.Read()
	require.NoError(t, err)
	require.JSONEq(t, `{"from":"dotfiles"}`, string(data))
	require.True(t, f.Exists())
}
