package checkpoint

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/stretchr/testify/require"
)

func runGitOut(t *testing.T, repoRoot string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// TestMigrateShadowBranchRef_SerializesAgainstConcurrentCheckpointWrite is a
// genuine, real-git reproduction of the race this function exists to close:
// migrateShadowBranchToBaseCommit used to read the old branch's hash and
// SetReference/delete it with no lock at all, while writeCheckpoint's
// casUpdateShadowBranchRef advances the SAME branch under a real flock.
// Before the fix, a migration racing a concurrent checkpoint write could
// read a stale hash and migrate it, then delete the branch out from under
// the in-flight write -- silently orphaning the checkpoint the write was
// about to land.
//
// This test drives both paths concurrently with real synchronization (not
// sleeps): the "writer" goroutine holds the real shadow-branch flock,
// signals it has started, advances the branch to a new commit, then
// releases. MigrateShadowBranchRef is invoked while the writer holds the
// lock, so it can only proceed once the writer's advance has landed --
// proving the two now share a lock domain instead of racing.
func TestMigrateShadowBranchRef_SerializesAgainstConcurrentCheckpointWrite(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "f.txt", "init")
	testutil.GitAdd(t, repoRoot, "f.txt")
	testutil.GitCommit(t, repoRoot, "init")

	commonDir := filepath.Join(repoRoot, ".git")
	headHash := runGitOut(t, repoRoot, "rev-parse", "HEAD")

	const oldBranch = "entire/oldbranch"
	const newBranch = "entire/newbranch"
	runGitOut(t, repoRoot, "branch", oldBranch, headHash)

	writerStarted := make(chan struct{})
	writerMayFinish := make(chan struct{})
	var wg sync.WaitGroup
	var writerErr error
	var advancedHash string

	wg.Add(1)
	go func() {
		defer wg.Done()
		writerErr = withShadowBranchFlock(commonDir, oldBranch, func() error {
			close(writerStarted)
			// Simulate the writer's real work (building/redacting a
			// commit) by waiting for the test to tell it to proceed --
			// this is the exact window the pre-fix migration could race
			// into.
			<-writerMayFinish
			runGitOut(t, repoRoot, "commit", "--allow-empty", "-m", "concurrent checkpoint")
			advancedHash = runGitOut(t, repoRoot, "rev-parse", "HEAD")
			return casUpdateShadowBranchRef(context.Background(), repoRoot, oldBranch,
				plumbing.NewHash(advancedHash), plumbing.NewHash(headHash))
		})
	}()

	<-writerStarted // writer holds the flock now

	migrateDone := make(chan struct {
		migrated bool
		err      error
	})
	go func() {
		m, err := MigrateShadowBranchRef(context.Background(), repoRoot, commonDir, oldBranch, newBranch)
		migrateDone <- struct {
			migrated bool
			err      error
		}{m, err}
	}()

	// Give the migration goroutine a moment to reach (and block on) the
	// flock acquire before releasing the writer -- without this the
	// scheduler could run the migration to completion before the writer
	// even starts, which wouldn't exercise the race at all.
	time.Sleep(20 * time.Millisecond)
	close(writerMayFinish)

	result := <-migrateDone
	wg.Wait()

	require.NoError(t, writerErr)
	require.NoError(t, result.err)
	require.True(t, result.migrated)
	require.NotEmpty(t, advancedHash, "writer goroutine did not run its commit")

	// The new branch must carry the WRITER's latest commit -- not a stale
	// hash read before the writer's advance landed. This is the property
	// that was broken pre-fix: an unlocked migration could read headHash,
	// migrate that stale value, and the writer's CAS would then fail
	// against a branch the migration had already deleted.
	gotHash := runGitOut(t, repoRoot, "rev-parse", "--verify", "refs/heads/"+newBranch)
	require.Equal(t, advancedHash, gotHash,
		"migrated branch must carry the concurrent writer's commit, not a stale pre-race hash")

	// Old branch must be gone -- migration completed cleanly.
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+oldBranch)
	cmd.Dir = repoRoot
	require.Error(t, cmd.Run(), "old shadow branch should have been removed by the migration")
}

// TestMigrateShadowBranchRef_NoOldBranch confirms the ordinary case (no
// concurrent writer, old branch simply doesn't exist yet) still behaves as
// documented: not an error, migrated=false.
func TestMigrateShadowBranchRef_NoOldBranch(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "f.txt", "init")
	testutil.GitAdd(t, repoRoot, "f.txt")
	testutil.GitCommit(t, repoRoot, "init")

	commonDir := filepath.Join(repoRoot, ".git")
	migrated, err := MigrateShadowBranchRef(context.Background(), repoRoot, commonDir, "entire/doesnotexist", "entire/newbranch")
	require.NoError(t, err)
	require.False(t, migrated)
}
