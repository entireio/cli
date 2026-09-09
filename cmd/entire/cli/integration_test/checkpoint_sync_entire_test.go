//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// =============================================================================
// Entire remote election tier
//
// When exactly one configured remote is an entire:// remote, it is elected as
// the checkpoint sync remote and EVERY push carries checkpoints there — to any
// other remote, or to a raw URL — the way the dedicated checkpoint_remote URL
// mode already bypasses the single-remote gate. The first delivery is
// announced once per clone (persisted in .git/entire-checkpoint-sync-entire.json).
//
// An entire:// remote cannot be reached from a test, so the harness fakes it:
// the remote's RAW config URL is entire:// (which is what the election reads),
// and a url.<file>.insteadOf rewrite sends every fetch/push/ls-remote to a bare
// repo on disk. `git remote -v` therefore shows the rewritten file:// URL while
// `git config --get remote.<name>.url` shows entire://.
// =============================================================================

// entireTestCluster is the fake cluster host used in test entire:// URLs.
const entireTestCluster = "entire://cluster.test/gh/acme/"

// originRemoteName is the conventional first remote, the legacy checkpoint
// destination these scenarios migrate away from.
const originRemoteName = "origin"

// entireStateFileName mirrors strategy.entireSyncStateFileName.
const entireStateFileName = "entire-checkpoint-sync-entire.json"

// setupEntireRemote adds remoteName with a raw entire:// URL and redirects its
// transport to a fresh bare repo via url.<file>.insteadOf. It does NOT push, so
// the remote has no tracking refs until a test creates some. Returns the bare
// repo path.
func setupEntireRemote(t *testing.T, env *TestEnv, remoteName string) string {
	t.Helper()
	bare := initUnregisteredBareRepo(t)
	url := entireTestCluster + remoteName
	testutil.AddRemote(t, env.RepoDir, remoteName, url)
	testutil.RunGit(t, env.RepoDir, "config", "--add", "url.file://"+bare+".insteadOf", url)
	// Deliberate test setup, not drift — refresh the teardown baseline.
	env.setGitConfigBaseline()
	return bare
}

func entireStateFileExists(t *testing.T, env *TestEnv) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(env.RepoDir, ".git", entireStateFileName))
	return err == nil
}

// TestCheckpointSyncRemote_EntireRemoteElected_PushToOriginCarriesCheckpoints
// is the headline behavior: origin (GitHub) is the user's habitual push
// destination, "entire" is the Entire remote. A pre-push for origin carries the
// checkpoint to the Entire remote, origin never receives checkpoint data, the
// first delivery announces itself exactly once, and status reports the tier.
func TestCheckpointSyncRemote_EntireRemoteElected_PushToOriginCarriesCheckpoints(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		bareEntire := setupEntireRemote(t, env, "entire")
		env.GitPush("entire", "HEAD")
		setBranchTrackingRemote(t, env, originRemoteName)

		checkpointID := createCheckpointedCommit(t, env, "Add entire module", "entire.go", "package entire", "Add entire module")

		out := env.RunPrePushOutput(originRemoteName)
		if !env.CheckpointExistsOnRemote(bareEntire, checkpointID) {
			t.Errorf("checkpoint %s should land on the Entire remote when pushing origin; output:\n%s", checkpointID, out)
		}
		if env.CheckpointsPresentOnRemote(bareOrigin) {
			t.Error("origin must not receive checkpoint data while the Entire remote is elected")
		}
		if env.usingGitRefs() && queuedCheckpointRefCount(t, env) != 0 {
			t.Error("push queue should drain into the Entire remote")
		}
		if !strings.Contains(out, `Checkpoints now sync to "entire"`) {
			t.Errorf("first delivery should announce the Entire remote; output:\n%s", out)
		}
		if !strings.Contains(out, `Earlier checkpoints may still be on "origin". Run `+"`entire checkpoint migrate`"+` to move them.`) {
			t.Errorf("first delivery should name the displaced remote and the command; output:\n%s", out)
		}
		if !entireStateFileExists(t, env) {
			t.Error("the announcement must be persisted so it happens once per clone")
		}
		if got := capturedSyncRemotesOnDisk(t, env); got != nil {
			t.Errorf("the entire tier must not capture, got %v", got)
		}

		// Second push: no repeated announcement.
		out2 := env.RunPrePushOutput(originRemoteName)
		if strings.Contains(out2, "Checkpoints now sync to") {
			t.Errorf("announcement must happen once; output:\n%s", out2)
		}

		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != "entire" || st.CheckpointSyncRemoteSource != "entire" {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				"entire", "entire", st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
	})
}

// TestCheckpointSyncRemote_EntireRemote_RawURLPushAlsoCarries: the gate blocks
// raw-URL pushes because no election vouches for the URL; under the entire tier
// the destination is the Entire remote regardless of where the code went.
func TestCheckpointSyncRemote_EntireRemote_RawURLPushAlsoCarries(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		env.SetupBareRemote()
		bareEntire := setupEntireRemote(t, env, "entire")
		env.GitPush("entire", "HEAD")
		rawTarget := initUnregisteredBareRepo(t)
		env.GitPush(rawTarget, "HEAD")

		checkpointID := createCheckpointedCommit(t, env, "Add raw module", "raw.go", "package raw", "Add raw module")

		env.RunPrePush(rawTarget)
		if !env.CheckpointExistsOnRemote(bareEntire, checkpointID) {
			t.Errorf("checkpoint %s should land on the Entire remote for a raw-URL push", checkpointID)
		}
		if env.CheckpointsPresentOnRemote(rawTarget) {
			t.Error("the raw URL target must not receive checkpoint data")
		}
	})
}

// TestCheckpointSyncRemote_TwoEntireRemotes_TierDoesNotApply: with several
// Entire remotes the tier declines to guess, origin stays elected, and a push
// to one of them is gated with a hint naming `entire checkpoint migrate --to`.
func TestCheckpointSyncRemote_TwoEntireRemotes_TierDoesNotApply(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		bareA := setupEntireRemote(t, env, "entire-a")
		setupEntireRemote(t, env, "entire-b")
		env.GitPush("entire-a", "HEAD")

		checkpointID := createCheckpointedCommit(t, env, "Add pair module", "pair.go", "package pair", "Add pair module")

		out := env.RunPrePushOutput("entire-a")
		if env.CheckpointsPresentOnRemote(bareA) {
			t.Error("a non-elected Entire remote must not receive checkpoint data")
		}
		if !strings.Contains(out, `entire checkpoint migrate --to "entire-a"`) {
			t.Errorf("gated push to one of several Entire remotes should name the command; output:\n%s", out)
		}
		if !strings.Contains(out, "2 Entire remotes") {
			t.Errorf("hint should explain why the tier did not apply; output:\n%s", out)
		}

		env.RunPrePush(originRemoteName)
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Errorf("checkpoint %s should reach origin, the default election", checkpointID)
		}

		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != originRemoteName || st.CheckpointSyncRemoteSource != "default" {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				originRemoteName, "default", st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
	})
}

// TestCheckpointSyncRemote_EntireRemote_GitBranchNoDeferOnEmptyEntireRemote pins
// that the empty-remote defer is skipped for a redirected push: the Entire remote
// has never been pushed to (no tracking refs), and the user's origin pushes will
// never create any, so deferring would strand v1 forever. git-branch only: the
// defer protects a refs/heads branch.
func TestCheckpointSyncRemote_EntireRemote_GitBranchNoDeferOnEmptyEntireRemote(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	env.SetupBareRemote()
	bareEntire := setupEntireRemote(t, env, "entire") // never pushed: no refs/remotes/entire/*

	checkpointID := createCheckpointedCommit(t, env, "Add defer module", "defer.go", "package defer", "Add defer module")

	env.RunPrePush(originRemoteName)
	if !env.BranchExistsOnRemote(bareEntire, paths.MetadataBranchName) {
		t.Error("v1 must publish to the Entire remote even though it has no tracking refs")
	}
	if !env.CheckpointExistsOnRemote(bareEntire, checkpointID) {
		t.Errorf("checkpoint %s should be on the Entire remote", checkpointID)
	}
}

// TestCheckpointSyncRemote_CapturedRemoteStillBeatsEntire documents the product
// order: a capture already in force outranks the entire tier. Re-routing is
// `entire checkpoint migrate --to entire`, which writes the explicit setting.
func TestCheckpointSyncRemote_CapturedRemoteStillBeatsEntire(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		env.SetupBareRemote()
		bareFork := env.SetupNamedBareRemote(forkRemote) // `-u`: branch declares fork

		checkpointID := createCheckpointedCommit(t, env, "Add order module", "order.go", "package order", "Add order module")
		env.RunPrePush(forkRemote)
		if got := capturedSyncRemotesOnDisk(t, env); len(got) != 1 || got[0] != forkRemote {
			t.Fatalf("precondition: fork should be captured, got %v", got)
		}
		if !env.CheckpointExistsOnRemote(bareFork, checkpointID) {
			t.Fatalf("precondition: checkpoint %s should be on fork", checkpointID)
		}

		bareEntire := setupEntireRemote(t, env, "entire")
		env.GitPush("entire", "HEAD")

		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != forkRemote || st.CheckpointSyncRemoteSource != "observed" {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				forkRemote, "observed", st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
		env.RunPrePush(originRemoteName)
		if env.CheckpointsPresentOnRemote(bareEntire) {
			t.Error("the Entire remote must not receive checkpoint data while a capture is in force")
		}
	})
}
