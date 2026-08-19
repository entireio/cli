package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// divergenceRepo returns a clone whose local primary metadata ref and origin's
// remote-tracking ref both exist and agree, ready for a test to move either
// side. run executes git in the clone.
func divergenceRepo(t *testing.T) (string, func(args ...string)) {
	t.Helper()

	bareDir := initBareWithMetadataBranch(t)
	cloneDir, run := cloneWithConfig(t, bareDir)

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	require.NoError(t, EnsurePrimaryRef(t.Context(), repo))

	return cloneDir, run
}

// commitOnMetadataBranch adds one commit to the checked-out metadata branch and
// returns nothing — callers read the resulting hashes off the refs.
func commitOnMetadataBranch(t *testing.T, cloneDir string, run func(...string), dir, content string) {
	t.Helper()

	path := filepath.Join(cloneDir, dir)
	require.NoError(t, os.MkdirAll(path, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(path, "metadata.json"), []byte(content), 0o644))
	run("add", ".")
	run("commit", "-m", "checkpoint "+dir)
}

func divergence(t *testing.T, cloneDir string) MetadataComparison {
	t.Helper()

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	c, err := CompareMetadataWithRemote(context.Background(), repo, metadataOriginRemoteRef())
	require.NoError(t, err)
	return c
}

// diverged is the assertion these tests are about; the other relations are
// asserted by name where they matter.
func diverged(c MetadataComparison) bool { return c.Relation == MetadataRelationDiverged }

// TestMetadataDivergence_BothAdvanced is the case no existing check reported:
// local and remote each gained a commit since their merge base, so the next
// fetch rewrites the local ref by replaying local commits onto the remote tip.
func TestMetadataDivergence_BothAdvanced(t *testing.T) {
	t.Parallel()

	cloneDir, run := divergenceRepo(t)

	// Remember the agreed tip, add a local commit, then build a DIFFERENT
	// commit on the same base and point origin's tracking ref at it.
	run("checkout", paths.MetadataBranchName)
	base := electionHeadHash(t, cloneDir)

	commitOnMetadataBranch(t, cloneDir, run, filepath.Join("aa", "1111111111"), `{"side":"local"}`)
	localTip := electionHeadHash(t, cloneDir)

	run("checkout", "-b", "remote-side", base)
	commitOnMetadataBranch(t, cloneDir, run, filepath.Join("bb", "2222222222"), `{"side":"remote"}`)
	remoteTip := electionHeadHash(t, cloneDir)
	run("update-ref", metadataOriginRemoteRef().String(), remoteTip)
	run("checkout", "main")

	require.NotEqual(t, localTip, remoteTip, "setup: the two sides must differ")

	c := divergence(t, cloneDir)
	assert.True(t, diverged(c), "both sides advanced past the merge base; that is a divergence")
	assert.Equal(t, localTip, c.Local.String())
	assert.Equal(t, remoteTip, c.Remote.String())
}

// TestMetadataDivergence_LocalAhead: a fast-forward is not a divergence — the
// fetch advances the ref without replaying anything, so there is nothing to say.
func TestMetadataDivergence_LocalAhead(t *testing.T) {
	t.Parallel()

	cloneDir, run := divergenceRepo(t)
	run("checkout", paths.MetadataBranchName)
	commitOnMetadataBranch(t, cloneDir, run, filepath.Join("cc", "3333333333"), `{"side":"local"}`)
	run("checkout", "main")

	assert.False(t, diverged(divergence(t, cloneDir)),
		"local strictly ahead of the tracking ref is a fast-forward, not a divergence")
}

// TestMetadataDivergence_RemoteAhead is the mirror image.
func TestMetadataDivergence_RemoteAhead(t *testing.T) {
	t.Parallel()

	cloneDir, run := divergenceRepo(t)
	run("checkout", paths.MetadataBranchName)
	base := electionHeadHash(t, cloneDir)
	commitOnMetadataBranch(t, cloneDir, run, filepath.Join("dd", "4444444444"), `{"side":"remote"}`)
	aheadTip := electionHeadHash(t, cloneDir)

	// Local back to the base, tracking ref left ahead.
	run("update-ref", "refs/heads/"+paths.MetadataBranchName, base)
	run("update-ref", metadataOriginRemoteRef().String(), aheadTip)
	run("checkout", "main")

	assert.False(t, diverged(divergence(t, cloneDir)),
		"remote strictly ahead is a fast-forward, not a divergence")
}

func TestMetadataDivergence_SameTip(t *testing.T) {
	t.Parallel()

	cloneDir, _ := divergenceRepo(t)
	assert.False(t, diverged(divergence(t, cloneDir)), "identical tips cannot diverge")
}

// TestMetadataDivergence_DisconnectedIsNotDiverged keeps the two classifications
// disjoint: no merge base at all is IsMetadataDisconnected's case, and doctor
// reports it separately with a repair. Reporting it here too would double up.
func TestMetadataDivergence_DisconnectedIsNotDiverged(t *testing.T) {
	t.Parallel()

	cloneDir, run := divergenceRepo(t)

	run("checkout", "--orphan", "orphan-side")
	run("rm", "-rf", ".")
	commitOnMetadataBranch(t, cloneDir, run, filepath.Join("ee", "5555555555"), `{"side":"orphan"}`)
	orphanTip := electionHeadHash(t, cloneDir)
	run("update-ref", "refs/heads/"+paths.MetadataBranchName, orphanTip)
	run("checkout", "main")

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	disconnected, err := IsMetadataDisconnected(context.Background(), repo, metadataOriginRemoteRef())
	require.NoError(t, err)
	require.True(t, disconnected, "setup: the orphan branch must be disconnected")

	assert.False(t, diverged(divergence(t, cloneDir)),
		"disconnected is reported by IsMetadataDisconnected, not as a divergence")
}

func TestMetadataDivergence_MissingRefs(t *testing.T) {
	t.Parallel()

	t.Run("no remote-tracking ref", func(t *testing.T) {
		t.Parallel()
		cloneDir, run := divergenceRepo(t)
		run("update-ref", "-d", metadataOriginRemoteRef().String())
		assert.False(t, diverged(divergence(t, cloneDir)), "nothing to diverge from")
	})

	t.Run("no local ref", func(t *testing.T) {
		t.Parallel()
		cloneDir, run := divergenceRepo(t)
		run("update-ref", "-d", "refs/heads/"+paths.MetadataBranchName)
		assert.False(t, diverged(divergence(t, cloneDir)), "nothing to diverge")
	})
}

// TestSafelyAdvanceLocalRef_LogsTheReplay pins the trace on the one path where a
// fetch reaches past remote-tracking refs and rewrites local state. The whole
// advance/replay chain used to be silent, which made an after-the-fact
// investigation impossible: the ref had moved, the local commits had new hashes,
// and .entire/logs/ said nothing about it.
//
// Not parallel: t.Chdir plus the process-global logger.
func TestSafelyAdvanceLocalRef_LogsTheReplay(t *testing.T) {
	cloneDir, run := divergenceRepo(t)

	run("checkout", paths.MetadataBranchName)
	base := electionHeadHash(t, cloneDir)
	commitOnMetadataBranch(t, cloneDir, run, filepath.Join("aa", "6666666666"), `{"side":"local"}`)
	localTip := electionHeadHash(t, cloneDir)
	run("checkout", "-b", "remote-side", base)
	commitOnMetadataBranch(t, cloneDir, run, filepath.Join("bb", "7777777777"), `{"side":"remote"}`)
	remoteTip := electionHeadHash(t, cloneDir)
	run("update-ref", "refs/heads/"+paths.MetadataBranchName, localTip)
	run("checkout", "main")

	t.Chdir(cloneDir)
	require.NoError(t, logging.Init(t.Context(), ""))
	// Registered immediately: an assertion failing before the explicit flush below
	// would otherwise leave the process-global logger holding the file handle.
	// Close is safe to call twice.
	t.Cleanup(logging.Close)

	repo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	localRefName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	require.NoError(t, SafelyAdvanceLocalRef(t.Context(), repo, localRefName, plumbing.NewHash(remoteTip)))
	logging.Close() // flush the buffered writer

	data, err := os.ReadFile(filepath.Join(cloneDir, logging.LogsDir, "entire.log"))
	require.NoError(t, err)
	logged := string(data)

	assert.Contains(t, logged, "local ref will be rewritten", "the rewrite must be announced in the log")
	assert.Contains(t, logged, "diverged", "the reason must distinguish diverged from disconnected")
	assert.Contains(t, logged, localTip, "the pre-rewrite local tip must be recoverable from the log")
	assert.Contains(t, logged, remoteTip, "the target tip must be recorded")
	assert.Contains(t, logged, "commits_replayed", "the replayed-commit count must be recorded")

	// The replay itself must still preserve the local checkpoint.
	after, err := repo.Reference(localRefName, true)
	require.NoError(t, err)
	assert.NotEqual(t, plumbing.NewHash(remoteTip), after.Hash(),
		"the local commit is replayed on top, so the ref is not merely reset to the remote tip")
}
