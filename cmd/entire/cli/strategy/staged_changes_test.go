package strategy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestGetStagedChanges(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	staged, err := getStagedChanges(t.Context())
	require.NoError(t, err)
	require.NotNil(t, staged.paths)
	require.Empty(t, staged.paths)

	testutil.WriteFile(t, dir, "new file.txt", "staged content\n")
	testutil.GitAdd(t, dir, "new file.txt")
	wantHash := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", ":new file.txt"))
	testutil.WriteFile(t, dir, "new file.txt", "unstaged replacement\n")
	staged, err = getStagedChanges(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"new file.txt"}, staged.paths)
	require.Equal(t, wantHash, staged.hashes["new file.txt"].String())

	testutil.GitCommit(t, dir, "Initial file")
	testutil.RunGit(t, dir, "config", "diff.renames", "true")
	testutil.RunGit(t, dir, "mv", "new file.txt", "renamed file.txt")
	staged, err = getStagedChanges(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"renamed file.txt"}, staged.paths)
	require.Equal(t, wantHash, staged.hashes["renamed file.txt"].String())
}

func TestParseStagedChanges(t *testing.T) {
	t.Parallel()
	for _, hashLength := range []int{40, 64} {
		t.Run(fmt.Sprintf("hash%d", hashLength), func(t *testing.T) {
			t.Parallel()
			oldHash := strings.Repeat("1", hashLength)
			newHash := strings.Repeat("2", hashLength)
			zeroHash := strings.Repeat("0", hashLength)
			const unusualPath = " space\tquote\"\nback\\slash.txt "
			raw := ":100644 100644 " + oldHash + " " + newHash + " M\x00" + unusualPath + "\x00" +
				":100644 000000 " + oldHash + " " + zeroHash + " D\x00deleted.txt\x00" +
				":100644 100644 " + oldHash + " " + newHash + " R90\x00old.txt\x00renamed.txt\x00" +
				":100644 100644 " + oldHash + " " + newHash + " C100\x00source.txt\x00copy.txt\x00"
			staged, err := parseStagedChanges(raw)
			require.NoError(t, err)
			require.Equal(t, []string{unusualPath, "deleted.txt", "renamed.txt", "copy.txt"}, staged.paths)
			for _, path := range []string{unusualPath, "renamed.txt", "copy.txt"} {
				require.Equal(t, newHash, staged.hashes[path].String())
			}
			require.True(t, staged.hashes["deleted.txt"].IsZero())
		})
	}
}

func TestParseStagedChanges_RejectsIncompleteSnapshot(t *testing.T) {
	t.Parallel()
	header := ":000000 100644 " + strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " "
	for name, raw := range map[string]string{
		"header":      "invalid\x00file.txt\x00",
		"hash":        ":000000 100644 0000 invalid A\x00file.txt\x00",
		"path":        header + "A\x00",
		"terminator":  header + "A\x00file.txt",
		"destination": header + "R100\x00old.txt\x00",
		"tail":        header + "A\x00valid.txt\x00incomplete",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			staged, err := parseStagedChanges(raw)
			require.Error(t, err)
			require.Nil(t, staged.paths)
			require.Nil(t, staged.hashes)
		})
	}
}
