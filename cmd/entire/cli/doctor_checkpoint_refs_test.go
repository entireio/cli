package cli

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The ULID from #2402, whose shard ("6B") differs from a legacy hex shard only
// by case, and the legacy checkpoint that names the same bucket in lowercase.
const (
	shardCaseULID      = "01M2DCHJCHTR9T9MZTSB7WV76B"
	shardCaseULIDRef   = "refs/entire/checkpoints/6B/01M2DCHJCHTR9T9MZTSB7WV76B"
	shardCaseFoldedRef = "refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B"
)

func shardCaseRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := setupGitRepoForPhaseTest(t)
	testutil.WriteFile(t, dir, "README.md", "# test")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	head := testutil.RunGit(t, dir, "rev-parse", "HEAD")
	return dir, head[:40]
}

func TestCheckCheckpointRefShardCase_ReportsFoldedRef(t *testing.T) {
	dir, head := shardCaseRepo(t)
	testutil.GitUpdateRef(t, dir, shardCaseFoldedRef, head)

	cmd, stdout := newTestCmd(t)
	checkCheckpointRefShardCase(cmd)

	output := stdout.String()
	assert.Contains(t, output, "FOLDED SHARD DIRECTORY")
	assert.Contains(t, output, shardCaseFoldedRef, "the report must name the ref")
	// The rename is what a reader reaches for and what silently deletes the ref
	// on the filesystem that produced the condition, so it must not be offered.
	assert.NotContains(t, output, "update-ref -d "+shardCaseFoldedRef)
}

func TestCheckCheckpointRefShardCase_ReportsSplitCheckpoint(t *testing.T) {
	dir, head := shardCaseRepo(t)
	// Packed folded ref plus a loose canonical one: the only way both spellings
	// coexist on a case-insensitive filesystem, and how the split really forms.
	testutil.GitUpdateRef(t, dir, shardCaseFoldedRef, head)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	testutil.GitUpdateRef(t, dir, shardCaseULIDRef, head)

	cmd, stdout := newTestCmd(t)
	checkCheckpointRefShardCase(cmd)

	output := stdout.String()
	assert.Contains(t, output, "SPLIT ACROSS SHARD SPELLINGS")
	assert.Contains(t, output, shardCaseULID, "the report must name the checkpoint")
	assert.Contains(t, output, shardCaseFoldedRef)
	assert.Contains(t, output, shardCaseULIDRef)
	assert.Contains(t, output, "git pack-refs --all", "the delete is unsafe without packing first")
}

// When one ref continues the other, the report names the earlier one as the
// ref to remove, whichever spelling it has. A user told to pick could delete
// the version Entire reads.
func TestCheckCheckpointRefShardCase_NamesTheEarlierRefOfASplit(t *testing.T) {
	for _, tc := range []struct {
		name, earlier, later string
	}{
		{"folded is earlier", shardCaseFoldedRef, shardCaseULIDRef},
		{"canonical is earlier", shardCaseULIDRef, shardCaseFoldedRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, head := shardCaseRepo(t)
			testutil.GitUpdateRef(t, dir, tc.earlier, head)
			testutil.RunGit(t, dir, "pack-refs", "--all")
			testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "later version")
			testutil.GitUpdateRef(t, dir, tc.later, "HEAD")
			testutil.RunGit(t, dir, "pack-refs", "--all")

			cmd, stdout := newTestCmd(t)
			checkCheckpointRefShardCase(cmd)

			output := stdout.String()
			assert.Contains(t, output, "git update-ref -d "+tc.earlier+" "+head,
				"the delete must name the commit it expects to remove")
			assert.NotContains(t, output, "update-ref -d "+tc.later)
			assert.Less(t, strings.Index(output, "git pack-refs --all"), strings.Index(output, "git update-ref -d"),
				"the pack step comes before the delete it makes safe")
		})
	}
}

// The delete is run exactly as printed, before any pack. While the later version
// is a loose ref, a case-insensitive filesystem resolves the earlier ref's name
// to that same file, so an unguarded delete removes the later version too.
func TestCheckCheckpointRefShardCase_PrintedDeleteKeepsTheLaterVersion(t *testing.T) {
	dir, head := shardCaseRepo(t)
	testutil.GitUpdateRef(t, dir, shardCaseFoldedRef, head)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "later version")
	later := testutil.RunGit(t, dir, "rev-parse", "HEAD")[:40]
	testutil.GitUpdateRef(t, dir, shardCaseULIDRef, later)

	cmd, stdout := newTestCmd(t)
	checkCheckpointRefShardCase(cmd)

	var printed []string
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		if fields := strings.Fields(line); len(fields) > 2 && fields[1] == "update-ref" {
			printed = fields[1:]
		}
	}
	require.NotEmpty(t, printed, "the report must offer a delete for the earlier ref")
	del := exec.CommandContext(t.Context(), "git", printed...)
	del.Dir = dir
	_ = del.Run() //nolint:errcheck // git refuses it while the refs are loose on a case-insensitive filesystem

	assert.Equal(t, later, testutil.RunGit(t, dir, "rev-parse", shardCaseULIDRef)[:40],
		"the later version must survive the printed delete")
}

// Refs whose histories diverged each hold sessions the other lacks, so the
// report offers no delete for either.
func TestCheckCheckpointRefShardCase_OffersNoDeleteForDivergedRefs(t *testing.T) {
	dir, head := shardCaseRepo(t)
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "one side")
	testutil.GitUpdateRef(t, dir, shardCaseFoldedRef, "HEAD")
	testutil.RunGit(t, dir, "pack-refs", "--all")
	testutil.GitReset(t, dir, head)
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "other side")
	testutil.GitUpdateRef(t, dir, shardCaseULIDRef, "HEAD")
	testutil.RunGit(t, dir, "pack-refs", "--all")

	cmd, stdout := newTestCmd(t)
	checkCheckpointRefShardCase(cmd)

	output := stdout.String()
	assert.Contains(t, output, shardCaseFoldedRef)
	assert.Contains(t, output, shardCaseULIDRef)
	assert.NotContains(t, output, "update-ref -d")
}

func TestCheckCheckpointRefShardCase_SilentOnCanonicalRefs(t *testing.T) {
	dir, head := shardCaseRepo(t)
	testutil.GitUpdateRef(t, dir, shardCaseULIDRef, head)
	testutil.GitUpdateRef(t, dir, "refs/entire/checkpoints/f6/a1b2c3d4e5f6", head)

	cmd, stdout := newTestCmd(t)
	checkCheckpointRefShardCase(cmd)

	assert.Empty(t, stdout.String(), "a repo whose checkpoint refs all sit at their canonical spelling says nothing")
}
