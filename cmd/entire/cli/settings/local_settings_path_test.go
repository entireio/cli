package settings

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// worktreePair returns a main worktree with one commit and a linked worktree
// of it. Neither has .entire/settings.local.json.
func worktreePair(t *testing.T) (mainRoot, linked string) {
	t.Helper()
	mainRoot = t.TempDir()
	testutil.InitRepo(t, mainRoot)
	require.NoError(t, os.MkdirAll(filepath.Join(mainRoot, ".entire"), 0o755))
	// Mirror the shipped .entire/.gitignore: the local file is never committed.
	require.NoError(t, os.WriteFile(filepath.Join(mainRoot, ".entire", ".gitignore"),
		[]byte("settings.local.json\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mainRoot, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, mainRoot, "add", ".")
	testutil.RunGit(t, mainRoot, "commit", "-q", "-m", "init")
	linked = filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainRoot, "worktree", "add", "-q", "-b", "feature", linked)

	var err error
	mainRoot, err = filepath.EvalSymlinks(mainRoot)
	require.NoError(t, err)
	linked, err = filepath.EvalSymlinks(linked)
	require.NoError(t, err)
	return mainRoot, linked
}

// A linked worktree is created without the gitignored local file, so every
// developer-only setting silently stopped applying in it (an external agent's
// hooks failed with "unknown agent" and its commits linked to another
// worktree's session).
func TestLocalSettingsPathIn(t *testing.T) {
	t.Parallel()

	t.Run("linked worktree without its own file uses the main worktree's", func(t *testing.T) {
		t.Parallel()
		mainRoot, linked := worktreePair(t)
		writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
		assert.True(t, inherited)
	})

	t.Run("linked worktree with its own file keeps it", func(t *testing.T) {
		t.Parallel()
		mainRoot, linked := worktreePair(t)
		writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)
		writeSettingsFile(t, filepath.Join(linked, EntireSettingsLocalFile), `{"log_level":"debug"}`)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(linked, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("neither has one: the worktree's own path, so a writer creates it there", func(t *testing.T) {
		t.Parallel()
		_, linked := worktreePair(t)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(linked, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("the main worktree uses its own", func(t *testing.T) {
		t.Parallel()
		mainRoot, _ := worktreePair(t)

		path, inherited := localSettingsPathIn(mainRoot)
		assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("worktree of a bare repository has no main worktree to inherit from", func(t *testing.T) {
		t.Parallel()
		src, _ := worktreePair(t)
		bare := filepath.Join(t.TempDir(), "bare.git")
		testutil.RunGit(t, src, "clone", "-q", "--bare", src, bare)
		linked := filepath.Join(t.TempDir(), "wt")
		testutil.RunGit(t, bare, "worktree", "add", "-q", "-b", "other", linked)
		linked, err := filepath.EvalSymlinks(linked)
		require.NoError(t, err)

		path, inherited := localSettingsPathIn(linked)
		assert.Equal(t, filepath.Join(linked, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})

	t.Run("not a repository: own path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		path, inherited := localSettingsPathIn(dir)
		assert.Equal(t, filepath.Join(dir, EntireSettingsLocalFile), path)
		assert.False(t, inherited)
	})
}

// Not parallel: t.Chdir.
func TestLocalSettingsPath_FromLinkedWorktreeCwd(t *testing.T) {
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)
	t.Chdir(linked)

	path, inherited, err := LocalSettingsPath(t.Context())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
	assert.True(t, inherited)
}

func TestLocalSettingsPath_FromExplicitWorktreeRoot(t *testing.T) {
	t.Parallel()
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)

	path, inherited, err := LocalSettingsPath(WithWorktreeRoot(t.Context(), linked))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mainRoot, EntireSettingsLocalFile), path)
	assert.True(t, inherited)
}

func TestLoad_LinkedWorktreeInheritsMainLocalSettings(t *testing.T) {
	t.Parallel()
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile),
		`{"enabled":true,"external_agents":true,"commit_linking":"always"}`)

	s, err := Load(WithWorktreeRoot(t.Context(), linked))
	require.NoError(t, err)
	assert.True(t, s.Enabled)
	assert.Equal(t, CommitLinkingAlways, s.CommitLinking)
	assert.True(t, s.ExternalAgents, "an untracked main-worktree file still grants external_agents")
	_, rejected := s.ExternalAgentsRejection()
	assert.False(t, rejected)
}

// Trust follows the file: a local file tracked in the main worktree is
// rejected in its linked worktrees exactly as it is in the main one.
func TestLoad_InheritedFileTrackedInMainIsDropped(t *testing.T) {
	t.Parallel()
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile),
		`{"commit_linking":"always","external_agents":true}`)
	testutil.RunGit(t, mainRoot, "add", "-f", EntireSettingsLocalFile)

	s, err := Load(WithWorktreeRoot(t.Context(), linked))
	require.NoError(t, err)
	assert.NotEqual(t, CommitLinkingAlways, s.CommitLinking)
	assert.False(t, s.ExternalAgents)
	assert.NotEmpty(t, s.LocalLayerRejection(), "the same rejection as a tracked file in the worktree itself")
}

func TestLoad_LinkedWorktreeOwnFileWins(t *testing.T) {
	t.Parallel()
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)
	writeSettingsFile(t, filepath.Join(linked, EntireSettingsLocalFile), `{"log_level":"debug"}`)

	s, err := Load(WithWorktreeRoot(t.Context(), linked))
	require.NoError(t, err)
	assert.False(t, s.ExternalAgents, "no merging: the worktree's own file is the whole local layer")
	assert.Equal(t, "debug", s.LogLevel)
}

// Not parallel: t.Chdir. Hooks resolve settings from the process directory.
func TestLoad_FromLinkedWorktreeCwdInheritsMainLocalSettings(t *testing.T) {
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"external_agents":true}`)
	t.Chdir(linked)

	s, err := Load(t.Context())
	require.NoError(t, err)
	assert.True(t, s.ExternalAgents)
}

// Not parallel: t.Chdir. A write from a linked worktree (answering "always"
// at the commit-link prompt, picking a summary provider) must land in the
// file the worktree reads; a new worktree-only file would hide the inherited
// grants and bring the original failure back.
func TestLocalWritesFromLinkedWorktreeLandInTheInheritedFile(t *testing.T) {
	mainRoot, linked := worktreePair(t)
	mainLocal := filepath.Join(mainRoot, EntireSettingsLocalFile)
	writeSettingsFile(t, mainLocal, `{"external_agents":true}`)
	t.Chdir(linked)

	path, raw, exists, err := LoadLocalRaw(t.Context())
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, mainLocal, path)
	raw["commit_linking"] = json.RawMessage(`"always"`)
	require.NoError(t, SaveLocalRaw(path, raw))

	_, statErr := os.Lstat(filepath.Join(linked, EntireSettingsLocalFile))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "no worktree-only file that would hide the inherited one")
	s, err := Load(t.Context())
	require.NoError(t, err)
	assert.True(t, s.ExternalAgents, "the grant survives the write")
	assert.Equal(t, CommitLinkingAlways, s.CommitLinking)

	data, err := LoadLocalBytes(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(data), "external_agents")
}

// Not parallel: t.Chdir.
func TestSaveLocal_FromLinkedWorktreeWritesInheritedFile(t *testing.T) {
	mainRoot, linked := worktreePair(t)
	mainLocal := filepath.Join(mainRoot, EntireSettingsLocalFile)
	writeSettingsFile(t, mainLocal, `{"enabled":true}`)
	t.Chdir(linked)

	require.NoError(t, SaveLocal(t.Context(), &EntireSettings{Enabled: true, LogLevel: "debug"}))

	_, statErr := os.Lstat(filepath.Join(linked, EntireSettingsLocalFile))
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	data, err := os.ReadFile(mainLocal)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"debug"`)
}

// Not parallel: t.Chdir. `entire enable --local` in the main tree must count
// as set up in its worktrees, or they report "not set up" and capture nothing.
func TestFilesPresent_CountsInheritedLocalFile(t *testing.T) {
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"enabled":true}`)
	t.Chdir(linked)

	project, local, err := FilesPresent(t.Context())
	require.NoError(t, err)
	assert.False(t, project)
	assert.True(t, local)
	assert.True(t, IsSetUpLocal(t.Context()))
	assert.True(t, IsSetUpAny(t.Context()))
}

// Not parallel: t.Chdir. A worktree checked out from a commit with no
// .entire at all is still governed by the main worktree's file.
func TestFilesPresent_InheritedLocalFileWithoutWorktreeEntireDir(t *testing.T) {
	mainRoot, linked := worktreePair(t)
	writeSettingsFile(t, filepath.Join(mainRoot, EntireSettingsLocalFile), `{"enabled":true}`)
	require.NoError(t, os.RemoveAll(filepath.Join(linked, ".entire")))
	t.Chdir(linked)

	project, local, err := FilesPresent(t.Context())
	require.NoError(t, err)
	assert.False(t, project)
	assert.True(t, local)
}
