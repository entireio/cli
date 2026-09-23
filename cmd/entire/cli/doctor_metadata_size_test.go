package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// withTinyOversizeThreshold lowers the doctor check's threshold so a few
// kilobytes count as oversized; the real threshold is 50 MiB.
func withTinyOversizeThreshold(t *testing.T) {
	t.Helper()
	old := oversizedMetadataThreshold
	oversizedMetadataThreshold = 1024
	t.Cleanup(func() { oversizedMetadataThreshold = old })
}

const bloatedDoctorMetadataPath = "ab/cdef000001/0/" + paths.MetadataFileName

// writeBloatedCheckpointBranch puts one checkpoint on entire/checkpoints/v1
// whose session metadata.json carries a prompt_attributions record far over
// the test threshold, and returns the branch tip.
func writeBloatedCheckpointBranch(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	perFile := make(map[string]int, 200)
	for i := range 200 {
		perFile[fmt.Sprintf(".claude/worktrees/agent/pkg/file%d.go", i)] = 2
	}
	meta, err := json.Marshal(map[string]any{
		"session_id":          "s1",
		"attribution":         map[string]any{"agent_lines": 5},
		"prompt_attributions": []map[string]any{{"checkpoint_number": 1, "user_added_per_file": perFile}},
	})
	require.NoError(t, err)
	tip := testutil.CommitFiles(t, repo, nil, map[string][]byte{
		paths.MetadataFileName:                        []byte("{}\n"),
		bloatedDoctorMetadataPath:                     meta,
		"ab/cdef000001/0/" + paths.TranscriptFileName: []byte("{}\n"),
	}, "Checkpoint: abcdef000001")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), tip)))
	return tip
}

func newDoctorTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	return cmd, &stdout
}

func v1Tip(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	return ref.Hash()
}

func TestCheckOversizedCheckpointMetadata_CleanRepoIsQuiet(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir modifies process-global state.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	cmd, stdout := newDoctorTestCmd()

	require.NoError(t, checkOversizedCheckpointMetadata(cmd))
	assert.Contains(t, stdout.String(), "✓ Checkpoint metadata size: OK")
}

func TestCheckOversizedCheckpointMetadata_NonInteractive_ReportsAndNamesSubcommand(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and the threshold override are process-global.
	withTinyOversizeThreshold(t)
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	tip := writeBloatedCheckpointBranch(t, repo)
	cmd, stdout := newDoctorTestCmd()

	require.NoError(t, checkOversizedCheckpointMetadata(cmd))

	out := stdout.String()
	assert.Contains(t, out, "Checkpoint metadata size: OVERSIZED")
	assert.Contains(t, out, bloatedDoctorMetadataPath)
	assert.Contains(t, out, "Run `"+shrinkMetadataCommand+"` to apply it.")
	assert.NotContains(t, out, "Fixed")
	assert.Equal(t, tip, v1Tip(t, repo), "report-only: the branch is untouched")
}

// `doctor --force` applies every other fix without asking; this one rewrites
// history and pushes, so it must stay report-only under --force.
func TestRunSessionsFix_Force_DoesNotRewriteCheckpointBranch(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and the threshold override are process-global.
	withTinyOversizeThreshold(t)
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	tip := writeBloatedCheckpointBranch(t, repo)
	cmd, stdout := newDoctorTestCmd()

	require.NoError(t, runSessionsFix(cmd, true))

	out := stdout.String()
	assert.Contains(t, out, "Checkpoint metadata size: OVERSIZED")
	assert.Contains(t, out, shrinkMetadataCommand)
	assert.NotContains(t, out, "Fixed: rewrote")
	assert.Equal(t, tip, v1Tip(t, repo), "--force must not rewrite the checkpoint branch")
}

func TestDoctorShrinkCheckpointMetadata_Yes_RewritesBranch(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and the threshold override are process-global.
	withTinyOversizeThreshold(t)
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	tip := writeBloatedCheckpointBranch(t, repo)

	run := func(args ...string) string {
		cmd := newDoctorShrinkCheckpointMetadataCmd()
		cmd.SetContext(context.Background())
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute())
		return stdout.String()
	}

	// Without --yes and without a terminal it reports and stops.
	out := run()
	assert.Contains(t, out, "Checkpoint metadata size: OVERSIZED")
	assert.Contains(t, out, "Pass --yes to apply it.")
	assert.Equal(t, tip, v1Tip(t, repo))

	out = run("--yes")
	assert.Contains(t, out, "✓ Fixed: rewrote 1 commit(s), shrank 1 file(s)")
	assert.Contains(t, out, "Not pushed: no checkpoint sync remote")
	newTip := v1Tip(t, repo)
	assert.NotEqual(t, tip, newTip)

	c, err := repo.CommitObject(newTip)
	require.NoError(t, err)
	f, err := c.File(bloatedDoctorMetadataPath)
	require.NoError(t, err)
	content, err := f.Contents()
	require.NoError(t, err)
	assert.NotContains(t, content, "prompt_attributions")
	assert.Contains(t, content, `"session_id"`)

	// A second run finds nothing.
	assert.Contains(t, run("--yes"), "nothing to repair")
	cmd, stdout := newDoctorTestCmd()
	require.NoError(t, checkOversizedCheckpointMetadata(cmd))
	assert.Contains(t, stdout.String(), "✓ Checkpoint metadata size: OK")
}
