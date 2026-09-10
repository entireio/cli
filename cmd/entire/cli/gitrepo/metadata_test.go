package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

const windowsOS = "windows"

func TestResolveWorktreeMetadata_RealGitLayouts(t *testing.T) {
	t.Parallel()

	t.Run("ordinary repository", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "ordinary")
		initMetadataRepo(t, root)
		assertMetadataMatchesGit(t, root, "")
	})

	t.Run("conventional linked worktree", func(t *testing.T) {
		t.Parallel()
		mainRoot, linkedRoot := conventionalMetadataWorktree(t, "linked")
		metadata := assertMetadataMatchesGit(t, linkedRoot, "linked")
		requireSameDirectory(t, filepath.Join(mainRoot, gitDir), metadata.CommonDir)
		require.Equal(t, "../..\n", readMetadataFile(t, filepath.Join(metadata.GitDir, "commondir")))
	})

	t.Run("dot-bare worktrees", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		seed := filepath.Join(tmp, "seed")
		layout := filepath.Join(tmp, "layout")
		bare := filepath.Join(layout, ".bare")
		linked := filepath.Join(layout, "dot-bare-linked")
		initMetadataRepo(t, seed)
		require.NoError(t, os.MkdirAll(layout, 0o750))
		runMetadataGit(t, tmp, "clone", "--bare", seed, bare)
		writeMetadataFile(t, filepath.Join(layout, gitDir), "gitdir: ./.bare\n")
		runMetadataGit(t, tmp, "--git-dir", bare, "worktree", "add", "-b", "dot-bare-linked", linked)

		metadata := assertMetadataMatchesGit(t, linked, "dot-bare-linked")
		requireSameDirectory(t, bare, metadata.CommonDir)
	})

	t.Run("bare-backed linked worktree", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		seed := filepath.Join(tmp, "seed")
		bare := filepath.Join(tmp, "arbitrary-storage")
		linked := filepath.Join(tmp, "bare-linked")
		initMetadataRepo(t, seed)
		runMetadataGit(t, tmp, "clone", "--bare", seed, bare)
		runMetadataGit(t, tmp, "--git-dir", bare, "worktree", "add", "-b", "bare-linked", linked)

		metadata := assertMetadataMatchesGit(t, linked, "bare-linked")
		requireSameDirectory(t, bare, metadata.CommonDir)
	})

	t.Run("separate Git directory", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		root := filepath.Join(tmp, "checkout")
		storage := filepath.Join(tmp, "storage")
		runMetadataGit(t, tmp, "init", "--separate-git-dir", storage, root)
		configureMetadataRepo(t, root)
		commitMetadataFile(t, root, "initial.txt", "initial\n")

		metadata := assertMetadataMatchesGit(t, root, "")
		requireSameDirectory(t, storage, metadata.GitDir)
		requireSameDirectory(t, storage, metadata.CommonDir)
	})

	t.Run("ordinary and linked submodules", func(t *testing.T) {
		t.Parallel()
		ordinary, linked := metadataSubmoduleWorktrees(t)
		assertMetadataMatchesGit(t, ordinary, "")
		assertMetadataMatchesGit(t, linked, "linked-submodule")
	})

	t.Run("submodule in a linked superproject", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		subject := filepath.Join(tmp, "subject")
		super := filepath.Join(tmp, "super")
		linkedSuper := filepath.Join(tmp, "linked-super")
		initMetadataRepo(t, subject)
		initMetadataRepo(t, super)
		runMetadataGit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", subject, "sub")
		runMetadataGit(t, super, "add", ".gitmodules", "sub")
		runMetadataGit(t, super, "commit", "--no-gpg-sign", "-m", "add submodule")
		runMetadataGit(t, super, "worktree", "add", "-b", "linked-super", linkedSuper)
		runMetadataGit(t, linkedSuper, "-c", "protocol.file.allow=always", "submodule", "update", "--init")

		assertMetadataMatchesGit(t, filepath.Join(linkedSuper, "sub"), "")
	})

	t.Run("relative gitdir pointer", func(t *testing.T) {
		t.Parallel()
		_, linked := conventionalMetadataWorktree(t, "relative-gitdir")
		metadata := assertMetadataMatchesGit(t, linked, "relative-gitdir")
		physicalLinked, err := filepath.EvalSymlinks(linked)
		require.NoError(t, err)
		relative, err := filepath.Rel(physicalLinked, metadata.GitDir)
		require.NoError(t, err)
		writeMetadataFile(t, filepath.Join(linked, gitDir), "gitdir: "+relative+"\n")

		got := assertMetadataMatchesGit(t, linked, "relative-gitdir")
		require.Equal(t, filepath.Clean(filepath.Join(linked, relative)), got.GitDir)
	})

	t.Run("symlink-aliased common directory", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == windowsOS {
			t.Skip("symlink creation requires privileges on some Windows builders")
		}
		mainRoot, linked := conventionalMetadataWorktree(t, "aliased")
		metadata := assertMetadataMatchesGit(t, linked, "aliased")
		alias := filepath.Join(t.TempDir(), "common-alias")
		require.NoError(t, os.Symlink(filepath.Join(mainRoot, gitDir), alias))
		writeMetadataFile(t, filepath.Join(metadata.GitDir, "commondir"), alias+"\n")

		got := assertMetadataMatchesGit(t, linked, "aliased")
		require.Equal(t, alias, got.CommonDir)
	})

	t.Run("symlink-aliased linked-worktree registration", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == windowsOS {
			t.Skip("symlink creation requires privileges on some Windows builders")
		}
		mainRoot, linked := conventionalMetadataWorktree(t, "registration-alias")
		metadata := assertMetadataMatchesGit(t, linked, "registration-alias")
		alias := filepath.Join(t.TempDir(), "registration")
		require.NoError(t, os.Symlink(metadata.GitDir, alias))
		writeMetadataFile(t, filepath.Join(linked, gitDir), "gitdir: "+alias+"\n")

		got := assertMetadataMatchesGit(t, linked, "registration-alias")
		require.Equal(t, alias, got.GitDir)
		requireSameDirectory(t, filepath.Join(mainRoot, gitDir), got.CommonDir)
		repo, err := OpenPath(linked)
		require.NoError(t, err)
		require.NoError(t, repo.Close())
	})

	t.Run("symlink-aliased worktree root", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == windowsOS {
			t.Skip("symlink creation requires privileges on some Windows builders")
		}
		root := filepath.Join(t.TempDir(), "physical-root")
		initMetadataRepo(t, root)
		alias := filepath.Join(t.TempDir(), "root-alias")
		require.NoError(t, os.Symlink(root, alias))

		metadata, err := ResolveWorktreeMetadata(alias)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(alias, gitDir), metadata.GitDir)
		require.Equal(t, filepath.Join(alias, gitDir), metadata.CommonDir)
	})

	t.Run("symlinked .git directory", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == windowsOS {
			t.Skip("symlink creation requires privileges on some Windows builders")
		}
		tmp := t.TempDir()
		root := filepath.Join(tmp, "checkout")
		storage := filepath.Join(tmp, "storage")
		initMetadataRepo(t, root)
		require.NoError(t, os.Rename(filepath.Join(root, gitDir), storage))
		require.NoError(t, os.Symlink(storage, filepath.Join(root, gitDir)))

		metadata := assertMetadataMatchesGit(t, root, "")
		require.Equal(t, filepath.Join(root, gitDir), metadata.GitDir)
		require.Equal(t, filepath.Join(root, gitDir), metadata.CommonDir)
	})
}

func TestResolveWorktreeMetadata_MovedRegistrationPreservesFacts(t *testing.T) {
	t.Parallel()
	mainRoot, linked := conventionalMetadataWorktree(t, "moved")
	before := assertMetadataMatchesGit(t, linked, "moved")
	moved := filepath.Join(filepath.Dir(linked), "moved-checkout")
	require.NoError(t, os.Rename(linked, moved))

	metadata, err := ResolveWorktreeMetadata(moved)
	require.NoError(t, err)
	require.Equal(t, before.GitDir, metadata.GitDir)
	requireSameDirectory(t, filepath.Join(mainRoot, gitDir), metadata.CommonDir)
	require.Equal(t, "moved", metadata.WorktreeID)
}

func TestResolveWorktreeMetadata_RejectsRegistrationSymlinkEscapingCommonDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("symlink creation requires privileges on some Windows builders")
	}

	tmp := t.TempDir()
	root := filepath.Join(tmp, "worktree")
	commonDir := filepath.Join(tmp, "common")
	registrationRoot := filepath.Join(commonDir, "worktrees")
	plantedRegistration := filepath.Join(tmp, "planted-registration")
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.MkdirAll(registrationRoot, 0o750))
	require.NoError(t, os.MkdirAll(plantedRegistration, 0o750))
	require.NoError(t, os.Symlink(plantedRegistration, filepath.Join(registrationRoot, "escaped")))
	writeMetadataFile(t, filepath.Join(root, gitDir), "gitdir: "+filepath.Join(registrationRoot, "escaped")+"\n")
	writeMetadataFile(t, filepath.Join(plantedRegistration, "commondir"), commonDir+"\n")

	metadata, err := ResolveWorktreeMetadata(root)
	require.ErrorContains(t, err, "not an immediate child")
	require.Equal(t, WorktreeMetadata{}, metadata)

	repo, err := OpenPath(root)
	require.ErrorContains(t, err, "resolve worktree metadata")
	require.ErrorContains(t, err, "not an immediate child")
	require.Nil(t, repo)
}

func TestResolveWorktreeMetadata_RequiresExplicitRoot(t *testing.T) {
	t.Parallel()

	metadata, err := ResolveWorktreeMetadata("")
	require.EqualError(t, err, "worktree root is required")
	require.Equal(t, WorktreeMetadata{}, metadata)
}

func TestResolveWorktreeMetadata_WhitespaceOnlyRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "   ")
	initMetadataRepo(t, root)
	t.Chdir(parent)

	metadata, err := ResolveWorktreeMetadata("   ")
	require.NoError(t, err)
	requireSameDirectory(t, filepath.Join(root, gitDir), metadata.GitDir)
}

func TestResolveWorktreeMetadata_IgnoresGitEnvironment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	other := filepath.Join(t.TempDir(), "other")
	initMetadataRepo(t, root)
	initMetadataRepo(t, other)
	t.Setenv("GIT_DIR", filepath.Join(other, gitDir))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, gitDir))

	metadata, err := ResolveWorktreeMetadata(root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, gitDir), metadata.GitDir)
	require.Equal(t, filepath.Join(root, gitDir), metadata.CommonDir)
}

func TestResolveWorktreeMetadata_StartsNoGitSubprocess(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("fake git uses a POSIX shell script")
	}
	root := filepath.Join(t.TempDir(), "root")
	initMetadataRepo(t, root)
	fakeBin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "git-started")
	writeMetadataFile(t, filepath.Join(fakeBin, "git"), "#!/bin/sh\n: > \"$GIT_METADATA_PROBE\"\nexit 99\n")
	require.NoError(t, os.Chmod(filepath.Join(fakeBin, "git"), 0o750))
	t.Setenv("GIT_METADATA_PROBE", marker)
	t.Setenv("PATH", fakeBin)

	_, err := ResolveWorktreeMetadata(root)
	require.NoError(t, err)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestResolveWorktreeMetadata_ObservesMetadataRepair(t *testing.T) {
	t.Parallel()
	_, linked := conventionalMetadataWorktree(t, "repair")
	metadata := assertMetadataMatchesGit(t, linked, "repair")
	commonFile := filepath.Join(metadata.GitDir, "commondir")
	writeMetadataFile(t, commonFile, "../missing\n")

	got, err := ResolveWorktreeMetadata(linked)
	require.Error(t, err)
	require.Equal(t, WorktreeMetadata{}, got)
	writeMetadataFile(t, commonFile, "../..\n")

	got, err = ResolveWorktreeMetadata(linked)
	require.NoError(t, err)
	require.Equal(t, metadata, got)
}

func TestResolveWorktreeMetadata_MalformedMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  string
	}{
		{
			name: "missing worktree root",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "missing")
			},
			want: "inspect worktree root",
		},
		{
			name: "worktree root is a file",
			setup: func(t *testing.T) string {
				root := filepath.Join(t.TempDir(), "root")
				writeMetadataFile(t, root, "file\n")
				return root
			},
			want: "worktree root at",
		},
		{
			name: "absent .git",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			want: "worktree Git metadata not found",
		},
		{
			name: "malformed gitdir pointer",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeMetadataFile(t, filepath.Join(root, gitDir), "not-a-pointer\n")
				return root
			},
			want: "missing gitdir prefix",
		},
		{
			name: "empty gitdir pointer",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeMetadataFile(t, filepath.Join(root, gitDir), "gitdir: \n")
				return root
			},
			want: "empty gitdir value",
		},
		{
			name: "gitdir prefix without space",
			setup: func(t *testing.T) string {
				return malformedGitDirMetadata(t, "gitdir:target\n")
			},
			want: "missing gitdir prefix",
		},
		{
			name: "gitdir leading whitespace",
			setup: func(t *testing.T) string {
				return malformedGitDirMetadata(t, " gitdir: target\n")
			},
			want: "missing gitdir prefix",
		},
		{
			name: "gitdir tab separator",
			setup: func(t *testing.T) string {
				return malformedGitDirMetadata(t, "gitdir:\ttarget\n")
			},
			want: "missing gitdir prefix",
		},
		{
			name: "gitdir trailing whitespace",
			setup: func(t *testing.T) string {
				return malformedGitDirMetadata(t, "gitdir: target \n")
			},
			want: "inspect Git directory",
		},
		{
			name: "gitdir extra content",
			setup: func(t *testing.T) string {
				return malformedGitDirMetadata(t, "gitdir: target\nignored\n")
			},
			want: "inspect Git directory",
		},
		{
			name: "dangling .git symlink",
			setup: func(t *testing.T) string {
				if runtime.GOOS == windowsOS {
					t.Skip("symlink creation requires privileges on some Windows builders")
				}
				root := t.TempDir()
				require.NoError(t, os.Symlink("missing", filepath.Join(root, gitDir)))
				return root
			},
			want: "inspect .git entry",
		},
		{
			name: ".git symlink loop",
			setup: func(t *testing.T) string {
				if runtime.GOOS == windowsOS {
					t.Skip("symlink creation requires privileges on some Windows builders")
				}
				root := t.TempDir()
				require.NoError(t, os.Symlink(gitDir, filepath.Join(root, gitDir)))
				return root
			},
			want: "inspect .git entry",
		},
		{
			name: "missing Git directory",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeMetadataFile(t, filepath.Join(root, gitDir), "gitdir: missing\n")
				return root
			},
			want: "inspect Git directory",
		},
		{
			name: "Git directory target is a file",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeMetadataFile(t, filepath.Join(root, "target"), "file\n")
				writeMetadataFile(t, filepath.Join(root, gitDir), "gitdir: target\n")
				return root
			},
			want: "Git directory at",
		},
		{
			name: "empty commondir",
			setup: func(t *testing.T) string {
				root, perWorktree := malformedLinkedMetadata(t)
				writeMetadataFile(t, filepath.Join(perWorktree, "commondir"), "\n")
				return root
			},
			want: "empty value",
		},
		{
			name: "commondir leading whitespace",
			setup: func(t *testing.T) string {
				return malformedCommonDirMetadata(t, " ../..\n")
			},
			want: "inspect common Git directory",
		},
		{
			name: "commondir trailing whitespace",
			setup: func(t *testing.T) string {
				return malformedCommonDirMetadata(t, "../.. \n")
			},
			want: "inspect common Git directory",
		},
		{
			name: "commondir extra content",
			setup: func(t *testing.T) string {
				return malformedCommonDirMetadata(t, "../..\nignored\n")
			},
			want: "inspect common Git directory",
		},
		{
			name: "missing common directory",
			setup: func(t *testing.T) string {
				root, perWorktree := malformedLinkedMetadata(t)
				writeMetadataFile(t, filepath.Join(perWorktree, "commondir"), "../missing\n")
				return root
			},
			want: "inspect common Git directory",
		},
		{
			name: "common directory target is a file",
			setup: func(t *testing.T) string {
				root, perWorktree := malformedLinkedMetadata(t)
				writeMetadataFile(t, filepath.Join(filepath.Dir(perWorktree), "common"), "file\n")
				writeMetadataFile(t, filepath.Join(perWorktree, "commondir"), "../common\n")
				return root
			},
			want: "common Git directory at",
		},
		{
			name: "commondir entry is a directory",
			setup: func(t *testing.T) string {
				root, perWorktree := malformedLinkedMetadata(t)
				require.NoError(t, os.Mkdir(filepath.Join(perWorktree, "commondir"), 0o750))
				return root
			},
			want: "commondir entry",
		},
		{
			name: "symlinked commondir",
			setup: func(t *testing.T) string {
				if runtime.GOOS == windowsOS {
					t.Skip("symlink creation requires privileges on some Windows builders")
				}
				root, perWorktree := malformedLinkedMetadata(t)
				require.NoError(t, os.Symlink("../..", filepath.Join(perWorktree, "commondir")))
				return root
			},
			want: "commondir entry",
		},
		{
			name: "nested registration",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				common := filepath.Join(root, "store")
				perWorktree := filepath.Join(common, "worktrees", "nested", "id")
				require.NoError(t, os.MkdirAll(perWorktree, 0o750))
				writeMetadataFile(t, filepath.Join(root, gitDir), "gitdir: "+perWorktree+"\n")
				writeMetadataFile(t, filepath.Join(perWorktree, "commondir"), "../../..\n")
				return root
			},
			want: "not an immediate child",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := tt.setup(t)
			metadata, err := ResolveWorktreeMetadata(root)
			require.ErrorContains(t, err, tt.want)
			if tt.name != "absent .git" {
				require.NotErrorIs(t, err, ErrWorktreeMetadataNotFound)
			}
			require.Equal(t, WorktreeMetadata{}, metadata)
		})
	}
}

func TestResolveWorktreeMetadata_RejectsBareRepository(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "bare.git")
	runMetadataGit(t, filepath.Dir(root), "init", "--bare", root)

	metadata, err := ResolveWorktreeMetadata(root)
	require.ErrorIs(t, err, ErrWorktreeMetadataNotFound)
	require.Equal(t, WorktreeMetadata{}, metadata)
}

// Git refuses an oversized .git file outright ("too large to be a .git file").
// Reading one unbounded turns a sparse file of near-zero disk size into an
// allocation the size of its apparent length, and echoing it into the error
// doubles that, so the cap and the elision are tested together.
func TestResolveWorktreeMetadata_BoundsPointerFiles(t *testing.T) {
	t.Parallel()

	t.Run("oversized .git file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeSparsePointerFile(t, filepath.Join(root, gitDir), 64<<20)

		_, err := ResolveWorktreeMetadata(root)
		require.ErrorContains(t, err, "too large to be a .git file")
		require.Less(t, len(err.Error()), 4096, "error must not carry the payload")
	})

	t.Run("oversized commondir file", func(t *testing.T) {
		t.Parallel()
		root, perWorktree := malformedLinkedMetadata(t)
		writeSparsePointerFile(t, filepath.Join(perWorktree, "commondir"), 64<<20)

		_, err := ResolveWorktreeMetadata(root)
		require.ErrorContains(t, err, "too large to be a commondir file")
		require.Less(t, len(err.Error()), 4096, "error must not carry the payload")
	})

	t.Run("pointer at the cap is still parsed", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		pointer := "gitdir: " + strings.Repeat("a", maxMetadataPointerSize-len("gitdir: ")-1) + "\n"
		require.Len(t, pointer, maxMetadataPointerSize)
		writeMetadataFile(t, filepath.Join(root, gitDir), pointer)

		_, err := ResolveWorktreeMetadata(root)
		// The target does not exist, which is the point: the file was read and
		// parsed rather than refused for its size.
		require.ErrorContains(t, err, "inspect Git directory")
		require.NotErrorIs(t, err, ErrWorktreeMetadataNotFound)
	})

	t.Run("long pointer path is elided in the error", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeMetadataFile(t, filepath.Join(root, gitDir), "gitdir: "+strings.Repeat("b", 2048)+"\n")

		_, err := ResolveWorktreeMetadata(root)
		require.ErrorContains(t, err, "bytes elided")
		require.True(t, utf8.ValidString(err.Error()), "elided error must stay valid UTF-8")
	})
}

func TestElideMetadataPath(t *testing.T) {
	t.Parallel()

	short := strings.Repeat("a", maxMetadataErrorPathLen)
	require.Equal(t, short, elideMetadataPath(short))

	// Cutting mid-rune would produce an invalid replacement character in the
	// message, so elision backs up to a boundary.
	multibyte := strings.Repeat("é", maxMetadataErrorPathLen)
	elided := elideMetadataPath(multibyte)
	require.NotEqual(t, multibyte, elided)
	require.True(t, utf8.ValidString(elided))
	require.Contains(t, elided, "bytes elided")
}

func writeSparsePointerFile(t *testing.T, path string, size int64) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	_, err = file.WriteString("gitdir: ")
	require.NoError(t, err)
	require.NoError(t, file.Truncate(size))
}

func TestErrWorktreeMetadataNotFoundClassification(t *testing.T) {
	t.Parallel()

	_, err := ResolveWorktreeMetadata(t.TempDir())
	require.ErrorIs(t, err, ErrWorktreeMetadataNotFound)

	_, err = ResolveWorktreeMetadata(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrWorktreeMetadataNotFound)
}

func assertMetadataMatchesGit(t *testing.T, root, wantID string) WorktreeMetadata {
	t.Helper()
	metadata, err := ResolveWorktreeMetadata(root)
	require.NoError(t, err)
	require.Equal(t, wantID, metadata.WorktreeID)
	require.True(t, filepath.IsAbs(metadata.GitDir))
	require.True(t, filepath.IsAbs(metadata.CommonDir))
	requireSameDirectory(t, metadata.GitDir, runMetadataGit(t, root, "rev-parse", "--absolute-git-dir"))
	requireSameDirectory(t, metadata.CommonDir, runMetadataGit(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	return metadata
}

func conventionalMetadataWorktree(t *testing.T, name string) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, name)
	initMetadataRepo(t, mainRoot)
	runMetadataGit(t, mainRoot, "worktree", "add", "-b", name, linkedRoot)
	return mainRoot, linkedRoot
}

func metadataSubmoduleWorktrees(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	subject := filepath.Join(tmp, "subject")
	super := filepath.Join(tmp, "super")
	ordinary := filepath.Join(super, "sub")
	linked := filepath.Join(tmp, "linked-submodule")
	initMetadataRepo(t, subject)
	initMetadataRepo(t, super)
	runMetadataGit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", subject, "sub")
	runMetadataGit(t, super, "add", ".gitmodules", "sub")
	runMetadataGit(t, super, "commit", "--no-gpg-sign", "-m", "add submodule")
	runMetadataGit(t, ordinary, "worktree", "add", "-b", "linked-submodule", linked)
	return ordinary, linked
}

func malformedLinkedMetadata(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	perWorktree := filepath.Join(root, "store", "worktrees", "id")
	require.NoError(t, os.MkdirAll(perWorktree, 0o750))
	writeMetadataFile(t, filepath.Join(root, gitDir), "gitdir: "+perWorktree+"\n")
	return root, perWorktree
}

func malformedGitDirMetadata(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "target"), 0o750))
	writeMetadataFile(t, filepath.Join(root, gitDir), content)
	return root
}

func malformedCommonDirMetadata(t *testing.T, content string) string {
	t.Helper()
	root, perWorktree := malformedLinkedMetadata(t)
	writeMetadataFile(t, filepath.Join(perWorktree, "commondir"), content)
	return root
}

func initMetadataRepo(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o750))
	runMetadataGit(t, root, "init", "-b", "main")
	configureMetadataRepo(t, root)
	commitMetadataFile(t, root, "initial.txt", "initial\n")
}

func configureMetadataRepo(t *testing.T, root string) {
	t.Helper()
	runMetadataGit(t, root, "config", "user.name", "Metadata Test")
	runMetadataGit(t, root, "config", "user.email", "metadata@example.com")
	runMetadataGit(t, root, "config", "commit.gpgsign", "false")
}

func commitMetadataFile(t *testing.T, root, name, content string) {
	t.Helper()
	writeMetadataFile(t, filepath.Join(root, name), content)
	runMetadataGit(t, root, "add", name)
	runMetadataGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")
}

func runMetadataGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "global.gitconfig"),
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), output)
	return strings.TrimSpace(string(output))
}

func requireSameDirectory(t *testing.T, got, want string) {
	t.Helper()
	gotInfo, err := os.Stat(got)
	require.NoError(t, err)
	wantInfo, err := os.Stat(want)
	require.NoError(t, err)
	require.Truef(t, os.SameFile(gotInfo, wantInfo), "%s and %s identify different directories", got, want)
}

func readMetadataFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func writeMetadataFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
