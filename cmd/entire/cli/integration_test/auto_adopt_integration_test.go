//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// These integration tests exercise the REAL hook binary wiring for
// cross-common-dir auto-adopt (#1439) — specifically the prepare/post-commit
// split introduced so an aborted commit never strands the source session
// (finding #74a). They drive `entire hooks git post-commit` as a spawned
// subprocess after a real `git commit`, so they cover the actual wiring in
// hooks_git_cmd.go, not just the in-process helpers the unit tests call.
//
// The owner-gated cross-repo DISCOVERY path (prepare-commit-msg →
// tryAutoAdoptCrossCommonDirSession) is intentionally covered in-process by the
// unit tests in cmd/entire/cli/session_auto_adopt_test.go: proclive.ResolveOwner
// fingerprints a NON-transient process-tree ancestor, and git/sh/entire are all
// transient, so a hook spawned by a real `git commit` resolves its owner to the
// go-test process itself — which the external `integration` package cannot
// fingerprint to seed a matching source owner. The post-commit finalize path
// tested here has no owner dependency, so it is driven end-to-end through the
// real binary.
//
// XDG_CACHE_HOME and ENTIRE_CONFIG_DIR are already isolated to a throwaway temp
// dir by setup_test.go's TestMain (process-wide), so no test here touches the
// developer's real ~/.cache/entire live-session registry. These tests are
// marker-driven (they never rely on the shared registry), so they are immune to
// cross-test registry collisions.

// seedSourceSession writes an ACTIVE session into repoDir's session store — the
// "source" a cross-common-dir adopt would retire.
func seedSourceSession(t *testing.T, repoDir, sessionID string) {
	t.Helper()
	store := session.NewStateStoreWithDir(filepath.Join(repoDir, ".git", session.SessionStateDirName))
	last := time.Now().Add(-1 * time.Minute)
	if err := store.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &last,
		Phase:               session.PhaseActive,
		WorktreePath:        repoDir,
	}); err != nil {
		t.Fatalf("seed source session: %v", err)
	}
}

// seedAdoptedTargetWithMarker writes an ACTIVE adopted session into targetRepo
// carrying a PendingSourceRetire marker pointing at sourceRepo — the exact state
// prepare-commit-msg leaves behind before post-commit completes the retire.
func seedAdoptedTargetWithMarker(t *testing.T, targetRepo, sourceRepo, sessionID, baseCommit string) {
	t.Helper()
	store := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	last := time.Now().Add(-1 * time.Minute)
	if err := store.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &last,
		Phase:                 session.PhaseActive,
		BaseCommit:            baseCommit,
		AttributionBaseCommit: baseCommit,
		WorktreePath:          targetRepo,
		FilesTouched:          []string{"feature.txt"},
		PendingSourceRetire: &session.PendingSourceRetire{
			SourceCommonDir:    filepath.Join(sourceRepo, ".git"),
			SourceWorktreePath: sourceRepo,
		},
	}); err != nil {
		t.Fatalf("seed adopted target session: %v", err)
	}
}

// runPostCommitHook spawns the real `entire hooks git post-commit` binary in
// env.RepoDir with the isolated CLI environment.
func runPostCommitHook(t *testing.T, env *TestEnv) {
	t.Helper()
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "hooks", "git", "post-commit")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	t.Logf("post-commit hook: %s (err: %v)", out, err)
}

// realCommit makes a real `git commit` (git CLI) touching feature.txt in
// env.RepoDir. The message carries no Entire-Checkpoint trailer, so post-commit
// takes its no-trailer path and then runs finalizePendingSourceRetires.
func realCommit(t *testing.T, env *TestEnv, content string) {
	t.Helper()
	testutil.WriteFile(t, env.RepoDir, "feature.txt", content)
	testutil.GitAdd(t, env.RepoDir, "feature.txt")
	testutil.GitCommit(t, env.RepoDir, "plain commit (no trailer)")
}

func loadState(t *testing.T, repoDir, sessionID string) *session.State {
	t.Helper()
	store := session.NewStateStoreWithDir(filepath.Join(repoDir, ".git", session.SessionStateDirName))
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load state %s: %v", sessionID, err)
	}
	return state
}

func stateIsAdoptable(state *session.State) bool {
	return state != nil &&
		state.Phase != session.PhaseEnded &&
		state.EndedAt == nil &&
		!state.FullyCondensed
}

// TestAutoAdopt_Integration_PostCommitFinalizeRetiresSource drives the finding
// #74a wiring end-to-end: after a real `git commit`, the real post-commit hook
// binary must tombstone the deferred source session and clear the marker.
func TestAutoAdopt_Integration_PostCommitFinalizeRetiresSource(t *testing.T) {
	t.Parallel()

	sourceEnv := NewFeatureBranchEnv(t)
	targetEnv := NewFeatureBranchEnv(t)

	sessionID := "test-auto-adopt-integration-finalize"
	seedSourceSession(t, sourceEnv.RepoDir, sessionID)
	seedAdoptedTargetWithMarker(t, targetEnv.RepoDir, sourceEnv.RepoDir, sessionID, targetEnv.GetHeadHash())

	realCommit(t, targetEnv, "agent change\n")
	runPostCommitHook(t, targetEnv)

	// Source must now be tombstoned.
	src := loadState(t, sourceEnv.RepoDir, sessionID)
	if src == nil {
		t.Fatal("source session vanished")
	}
	if stateIsAdoptable(src) {
		t.Fatalf("post-commit must tombstone the source; phase=%s endedAt=%v fullyCondensed=%v",
			src.Phase, src.EndedAt, src.FullyCondensed)
	}
	if src.Phase != session.PhaseEnded {
		t.Fatalf("source phase = %s, want %s", src.Phase, session.PhaseEnded)
	}

	// Target marker must be cleared.
	tgt := loadState(t, targetEnv.RepoDir, sessionID)
	if tgt == nil {
		t.Fatal("target session vanished")
	}
	if tgt.PendingSourceRetire != nil {
		t.Fatal("post-commit must clear the PendingSourceRetire marker")
	}
}

// TestAutoAdopt_Integration_ContendedSourceLockLeavesSourceActive holds the
// source session's cross-process flock while the real post-commit hook runs. The
// finalize step must give up within its timeout WITHOUT corrupting state: the
// source stays ACTIVE and the marker survives so a later commit retries.
func TestAutoAdopt_Integration_ContendedSourceLockLeavesSourceActive(t *testing.T) {
	t.Parallel()

	sourceEnv := NewFeatureBranchEnv(t)
	targetEnv := NewFeatureBranchEnv(t)

	sessionID := "test-auto-adopt-integration-contended"
	seedSourceSession(t, sourceEnv.RepoDir, sessionID)
	seedAdoptedTargetWithMarker(t, targetEnv.RepoDir, sourceEnv.RepoDir, sessionID, targetEnv.GetHeadHash())

	realCommit(t, targetEnv, "agent change\n")

	// Hold the SOURCE session lock (OS flock, cross-process) so the spawned
	// post-commit's finalize contends and must time out.
	lockPath := filepath.Join(sourceEnv.RepoDir, ".git", "entire-session-locks", sessionID+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(lockPath)
	if err != nil {
		t.Fatalf("acquire source lock: %v", err)
	}

	runPostCommitHook(t, targetEnv)
	release()

	// Source must be untouched (still ACTIVE) — finalize gave up under contention.
	src := loadState(t, sourceEnv.RepoDir, sessionID)
	if src == nil {
		t.Fatal("source session vanished under lock contention")
	}
	if !stateIsAdoptable(src) {
		t.Fatalf("contended finalize must leave source ACTIVE; phase=%s endedAt=%v fullyCondensed=%v",
			src.Phase, src.EndedAt, src.FullyCondensed)
	}

	// Marker must survive so the retire is retried on the next commit.
	tgt := loadState(t, targetEnv.RepoDir, sessionID)
	if tgt == nil {
		t.Fatal("target session vanished")
	}
	if tgt.PendingSourceRetire == nil {
		t.Fatal("contended finalize must NOT clear the marker (retire is deferred, not lost)")
	}
}

// TestAutoAdopt_Integration_DisabledTargetDoesNotFinalize proves the enablement
// gate holds end-to-end: when the target repo is disabled, the post-commit hook
// short-circuits and does no cross-common-dir work — the source is not retired.
func TestAutoAdopt_Integration_DisabledTargetDoesNotFinalize(t *testing.T) {
	t.Parallel()

	sourceEnv := NewFeatureBranchEnv(t)
	targetEnv := NewFeatureBranchEnv(t)

	sessionID := "test-auto-adopt-integration-disabled"
	seedSourceSession(t, sourceEnv.RepoDir, sessionID)
	seedAdoptedTargetWithMarker(t, targetEnv.RepoDir, sourceEnv.RepoDir, sessionID, targetEnv.GetHeadHash())

	realCommit(t, targetEnv, "agent change\n")

	// Disable Entire in the target: its git hooks must become inert.
	targetEnv.RunCLI("disable")

	runPostCommitHook(t, targetEnv)

	// Disabled target ran no finalize — source stays ACTIVE.
	src := loadState(t, sourceEnv.RepoDir, sessionID)
	if src == nil {
		t.Fatal("source session vanished")
	}
	if !stateIsAdoptable(src) {
		t.Fatalf("disabled target must not retire the source; phase=%s", src.Phase)
	}
}
