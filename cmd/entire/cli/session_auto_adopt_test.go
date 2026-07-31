package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestAutoAdopt_PrepareCommitMsg_CrossCommonDirSibling(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	sessionID := "test-auto-adopt-sibling-001"
	// Distinctive path + matching owner: intentional multi-repo adopt.
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"services/billing/handler.go"}, true)

	testutil.WriteFile(t, targetRepo, "services/billing/handler.go", "agent change\n")
	testutil.GitAdd(t, targetRepo, "services/billing/handler.go")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected auto-adopted session state in target repo")
	}
	if adopted.WorktreePath != targetRepo {
		t.Fatalf("WorktreePath = %q, want %q", adopted.WorktreePath, targetRepo)
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("commit in B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer after auto-adopt", string(content))
	}
}

func TestAutoAdopt_SkipsUnrelatedSiblingSameRelativePath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	// Same relative filename as the steal case (README.md) but no owner match.
	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-steal-readme", []string{"README.md"}, false)

	testutil.WriteFile(t, targetRepo, "README.md", "unrelated human edit\n")
	testutil.GitAdd(t, targetRepo, "README.md")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), "test-auto-adopt-steal-readme")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("same relative path without owner match must not steal sibling session")
	}
}

func TestAutoAdopt_SkipsBoilerplateOverlapEvenWithOwner(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	// Owner matches + README.md overlap must NOT auto-adopt (boilerplate).
	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-boilerplate", []string{"README.md", "go.mod"}, true)

	testutil.WriteFile(t, targetRepo, "README.md", "unrelated human edit\n")
	testutil.GitAdd(t, targetRepo, "README.md")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), "test-auto-adopt-boilerplate")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("boilerplate-only FilesTouched overlap must not auto-adopt even with owner match")
	}
}

func TestFilesTouchedOverlap_IgnoresBoilerplate(t *testing.T) {
	t.Parallel()

	if filesTouchedOverlap([]string{"README.md"}, []string{"README.md"}) {
		t.Fatal("README.md alone must not count as overlap")
	}
	if filesTouchedOverlap([]string{"go.mod", "package.json"}, []string{"go.mod"}) {
		t.Fatal("go.mod alone must not count as overlap")
	}
	if !filesTouchedOverlap([]string{"README.md", "services/billing/handler.go"}, []string{"services/billing/handler.go"}) {
		t.Fatal("distinctive path overlap must count even alongside boilerplate")
	}
	if !filesTouchedOverlap([]string{"internal/foo.go"}, []string{"internal/foo.go"}) {
		t.Fatal("non-boilerplate path must count")
	}
}

func TestAutoAdopt_PrepareCommitMsg_ViaLiveRegistry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	// Sibling dirs under one parent: registry discover + proximity both apply.
	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	sessionID := "test-auto-adopt-registry-001"
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"feature.txt"}, true)

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetRepo, "feature.txt")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected registry-based auto-adopt into target repo")
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("commit in B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

// TestAutoAdopt_AdoptsDistantRegistryEntry reproduces issue #1439's own repro:
// the source session lives under one parent (…/src-nest/repo) while the commit
// happens in a repo under a completely unrelated parent (…/dst-nest/repo), so the
// two are NOT siblings. With a distinctive overlapping path and a matching owner,
// the registry discovery path now adopts across the parent boundary — proximity
// is no longer required for the registry path — and the checkpoint trailer lands,
// which is exactly what #1439 asked for.
//
// Uses t.Chdir(), so no t.Parallel().
func TestAutoAdopt_AdoptsDistantRegistryEntry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	// Nest under distinct parents — bare t.TempDir() siblings share a parent on macOS.
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(t.TempDir(), "src-nest", "repo"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(t.TempDir(), "dst-nest", "repo"))

	sessionID := "test-auto-adopt-distant-registry"
	// Distinctive path + matching owner: the registry path's stronger guards
	// (owner + non-boilerplate overlap + uniqueness) all PASS, so the adopt goes
	// through even though the repos are non-siblings.
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"services/billing/handler.go"}, true)

	testutil.WriteFile(t, targetRepo, "services/billing/handler.go", "agent change\n")
	testutil.GitAdd(t, targetRepo, "services/billing/handler.go")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("non-sibling registry entry with owner+distinctive overlap must auto-adopt (#1439 repro)")
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("commit in unrelated dir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer after non-sibling adopt", string(content))
	}
}

// TestAutoAdopt_SkipsDistantRegistryOwnerMismatch proves that dropping the
// registry proximity gate did not weaken cross-parent safety: a non-sibling
// source with a distinctive overlapping path but NO matching owner must still be
// rejected. Owner match is the primary steal guard for the registry path once
// proximity is gone.
//
// Uses t.Chdir(), so no t.Parallel().
func TestAutoAdopt_SkipsDistantRegistryOwnerMismatch(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	sourceRepo := setupAdoptRepoAt(t, filepath.Join(t.TempDir(), "src-nest", "repo"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(t.TempDir(), "dst-nest", "repo"))

	sessionID := "test-auto-adopt-distant-owner-mismatch"
	// Distinctive path overlaps and proximity is no longer checked, so the owner
	// guard is the ONLY reason to reject — the source records no owner.
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"services/billing/handler.go"}, false)

	testutil.WriteFile(t, targetRepo, "services/billing/handler.go", "agent change\n")
	testutil.GitAdd(t, targetRepo, "services/billing/handler.go")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("non-sibling registry entry without owner match must not auto-adopt")
	}
}

func TestAutoAdopt_SkipsIdleSibling(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	sessionID := "test-auto-adopt-idle-sibling"
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"feature.txt"}, true)

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	state, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil || state == nil {
		t.Fatalf("load source: %v", err)
	}
	state.Phase = session.PhaseIdle
	if err := sourceStore.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetRepo, "feature.txt")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("Idle sibling must not be auto-adopted")
	}
}

func TestOwnerMatches_RejectsBootMismatch(t *testing.T) {
	t.Parallel()

	recorded := &proclive.Identity{PID: 42, Start: "tick", Boot: "boot-a", Host: "host"}
	current := proclive.Identity{PID: 42, Start: "tick", Boot: "boot-b", Host: "host"}
	if ownerMatches(recorded, current) {
		t.Fatal("Boot mismatch must reject owner match (post-reboot PID reuse)")
	}
	current.Boot = "boot-a"
	if !ownerMatches(recorded, current) {
		t.Fatal("matching Boot should allow owner match")
	}
	// Empty Boot on either side is best-effort (legacy / unknown); don't fail closed.
	if !ownerMatches(recorded, proclive.Identity{PID: 42, Start: "tick", Host: "host"}) {
		t.Fatal("empty current Boot should not reject when PID+Start match")
	}
}

func TestSiblingLooksLikeGitWorktree(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	plain := filepath.Join(base, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if siblingLooksLikeGitWorktree(plain) {
		t.Fatal("dir without .git must not look like a worktree")
	}

	withDir := filepath.Join(base, "with-dir")
	if err := os.MkdirAll(filepath.Join(withDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !siblingLooksLikeGitWorktree(withDir) {
		t.Fatal(".git directory must look like a worktree")
	}

	withFile := filepath.Join(base, "with-file")
	if err := os.Mkdir(withFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withFile, ".git"), []byte("gitdir: ../with-dir/.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !siblingLooksLikeGitWorktree(withFile) {
		t.Fatal(".git gitfile must look like a worktree")
	}
}

func TestSiblingLooksLikeEntireRepo(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	plain := filepath.Join(base, "plain")
	if err := os.MkdirAll(filepath.Join(plain, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if siblingLooksLikeEntireRepo(plain) {
		t.Fatal("git repo without .entire must not look Entire-enabled")
	}
	if err := os.Mkdir(filepath.Join(plain, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !siblingLooksLikeEntireRepo(plain) {
		t.Fatal(".entire directory must look Entire-enabled")
	}
}

func TestAutoAdopt_SkipsWhenAmbiguous(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceA := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	sourceC := setupAdoptRepoAt(t, filepath.Join(base, "repo-c"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	seedAutoAdoptSourceSession(t, sourceA, "test-auto-adopt-ambig-a", []string{"feature.txt"}, true)
	seedAutoAdoptSourceSession(t, sourceC, "test-auto-adopt-ambig-c", []string{"feature.txt"}, true)

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetRepo, "feature.txt")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	states, err := targetStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("ambiguous sources must not auto-adopt; got %d states", len(states))
	}
}

func TestAutoAdopt_SkipsWithoutFilesTouchedOverlap(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-no-overlap", []string{"other.txt"}, true)

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetRepo, "feature.txt")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), "test-auto-adopt-no-overlap")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("must not auto-adopt without FilesTouched overlap")
	}
}

func TestAutoAdopt_SkipsWhenLocalActiveSessionExists(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-remote", []string{"feature.txt"}, true)

	localID := "test-auto-adopt-local"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	if err := targetStore.Save(context.Background(), &session.State{
		SessionID:             localID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, targetRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, targetRepo),
		WorktreePath:          targetRepo,
		FilesTouched:          []string{"feature.txt"},
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetRepo, "feature.txt")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	remote, err := targetStore.Load(context.Background(), "test-auto-adopt-remote")
	if err != nil {
		t.Fatal(err)
	}
	if remote != nil {
		t.Fatal("must not auto-adopt when target already has an active session")
	}
}

// TestAutoAdopt_AbortedCommitLeavesSourceActive proves the prepare/post-commit
// split: prepare-commit-msg registers the adopted session in the target (so a
// trailer can land) and stamps a PendingSourceRetire marker, but does NOT retire
// the source. When the commit is aborted (post-commit never runs), the source
// session must remain ACTIVE — the whole point of deferring the destructive
// retire until the commit is a fact.
//
// Uses t.Chdir(), so no t.Parallel().
func TestAutoAdopt_AbortedCommitLeavesSourceActive(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	sessionID := "test-auto-adopt-abort-001"
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"services/billing/handler.go"}, true)

	testutil.WriteFile(t, targetRepo, "services/billing/handler.go", "agent change\n")
	testutil.GitAdd(t, targetRepo, "services/billing/handler.go")
	t.Chdir(targetRepo)

	// prepare-commit-msg phase only (simulating an aborted commit: post-commit
	// never fires).
	tryAutoAdoptCrossCommonDirSession(context.Background())

	// Target registered the adopted session with a pending-retire marker.
	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target after prepare phase")
	}
	if adopted.PendingSourceRetire == nil {
		t.Fatal("expected PendingSourceRetire marker recorded at prepare phase")
	}

	// Source must still be ACTIVE — not tombstoned — because no commit happened.
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	sourceAfter, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter == nil {
		t.Fatal("source session vanished after aborted commit")
	}
	if !isAdoptableSourceSession(sourceAfter) {
		t.Fatalf("aborted commit must leave source ACTIVE; phase=%s endedAt=%v fullyCondensed=%v",
			sourceAfter.Phase, sourceAfter.EndedAt, sourceAfter.FullyCondensed)
	}
}

// TestAutoAdopt_PostCommitFinalizeRetiresSource proves the companion of the
// aborted-commit case: once the commit is a fact, the post-commit finalize step
// tombstones the source and clears the marker.
//
// Uses t.Chdir(), so no t.Parallel().
func TestAutoAdopt_PostCommitFinalizeRetiresSource(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	sessionID := "test-auto-adopt-finalize-001"
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"services/billing/handler.go"}, true)

	testutil.WriteFile(t, targetRepo, "services/billing/handler.go", "agent change\n")
	testutil.GitAdd(t, targetRepo, "services/billing/handler.go")
	t.Chdir(targetRepo)

	// prepare-commit-msg phase: register target, defer source retire.
	tryAutoAdoptCrossCommonDirSession(context.Background())
	// post-commit phase: commit is a fact, so complete the retire.
	finalizePendingSourceRetires(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target")
	}
	if adopted.PendingSourceRetire != nil {
		t.Fatal("post-commit finalize must clear the PendingSourceRetire marker")
	}

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	sourceAfter, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter == nil {
		t.Fatal("source session vanished")
	}
	if isAdoptableSourceSession(sourceAfter) {
		t.Fatalf("post-commit finalize must tombstone the source; phase=%s", sourceAfter.Phase)
	}
	if sourceAfter.Phase != session.PhaseEnded {
		t.Fatalf("source phase = %s, want %s after finalize", sourceAfter.Phase, session.PhaseEnded)
	}
	if !sameAdoptPath(sourceAfter.AdoptedIntoWorktreePath, targetRepo) {
		t.Fatalf("source AdoptedIntoWorktreePath = %q, want %q", sourceAfter.AdoptedIntoWorktreePath, targetRepo)
	}
}

// saveActiveSourceSession writes a minimal ACTIVE source session directly into a
// repo's session store for the deferred cross-common-dir adopt/claim tests. The
// deferred adopt path uses SkipTranscriptValidation, so no transcript is needed.
func saveActiveSourceSession(t *testing.T, repo, sessionID string, claim *session.AdoptClaim) {
	t.Helper()
	lastInteraction := time.Now().Add(-1 * time.Minute)
	store := session.NewStateStoreWithDir(filepath.Join(repo, ".git", session.SessionStateDirName))
	if err := store.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, repo),
		AttributionBaseCommit: testutil.GetHeadHash(t, repo),
		WorktreePath:          repo,
		LastPrompt:            "cross-repo work",
		AdoptClaim:            claim,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAdoptClaimBlocks pins the source-claim recency + ownership semantics: only
// a FRESH claim held by a DIFFERENT target blocks a new deferred adoption.
func TestAdoptClaimBlocks(t *testing.T) {
	t.Parallel()
	const target = "/repos/target/.git"
	const other = "/repos/other/.git"
	now := time.Now()
	tests := []struct {
		name  string
		claim *session.AdoptClaim
		want  bool
	}{
		{"nil claim", nil, false},
		{"zero time", &session.AdoptClaim{ByCommonDir: other, At: time.Time{}}, false},
		{"stale different target", &session.AdoptClaim{ByCommonDir: other, At: now.Add(-2 * adoptRecentWindow)}, false},
		{"fresh same target idempotent", &session.AdoptClaim{ByCommonDir: target, At: now}, false},
		{"fresh different target blocks", &session.AdoptClaim{ByCommonDir: other, At: now}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := adoptClaimBlocks(tt.claim, target); got != tt.want {
				t.Fatalf("adoptClaimBlocks(%+v) = %v, want %v", tt.claim, got, tt.want)
			}
		})
	}
}

// TestAutoAdopt_ConcurrentClaimAdoptsExactlyOnce proves the cross-process mutual
// exclusion the deferred-retire regression dropped: two targets that concurrently
// discover the SAME unique live source session must not both register it. The
// shared sourceCommonDir lock serializes the two deferred adopts; the winner
// stamps a non-destructive claim on the source and the loser, re-reading the
// source under the lock, observes the claim and refuses — so the session is
// adopted exactly once instead of double-adopted into two repos.
//
// Uses t.Chdir(), so no t.Parallel().
func TestAutoAdopt_ConcurrentClaimAdoptsExactlyOnce(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	target1 := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))
	target2 := setupAdoptRepoAt(t, filepath.Join(base, "repo-c"))

	sessionID := "test-auto-adopt-concurrent-claim"
	saveActiveSourceSession(t, sourceRepo, sessionID, nil)

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	_, _, sourceCommonDir, err := stateStoreForWorktree(context.Background(), sourceRepo)
	if err != nil {
		t.Fatal(err)
	}

	// First target adopts and wins the claim.
	t.Chdir(target1)
	target1Store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, target1CommonDir, err := stateStoreForWorktree(context.Background(), target1)
	if err != nil {
		t.Fatal(err)
	}
	deferOpts := adoptOptions{Force: true, SkipTranscriptValidation: true, DeferSourceRetire: true}
	adopted, _, err := adoptFromExternalSessionStore(
		context.Background(), sourceStore, sourceRepo, sourceCommonDir,
		target1Store, target1CommonDir, sessionID, deferOpts,
	)
	if err != nil {
		t.Fatalf("first adopt failed: %v", err)
	}
	if adopted == nil || adopted.PendingSourceRetire == nil {
		t.Fatal("first adopt must register the target with a PendingSourceRetire marker")
	}

	sourceAfter1, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter1.AdoptClaim == nil {
		t.Fatal("winning adopt must stamp an AdoptClaim on the source")
	}
	if !sameAdoptStore(sourceAfter1.AdoptClaim.ByCommonDir, target1CommonDir) {
		t.Fatalf("claim ByCommonDir = %q, want target1 %q", sourceAfter1.AdoptClaim.ByCommonDir, target1CommonDir)
	}
	if !isAdoptableSourceSession(sourceAfter1) {
		t.Fatal("deferred adopt must leave the source ACTIVE (the claim is non-destructive)")
	}

	// Second target, discovering the SAME unique source, must observe the claim
	// under the lock and refuse.
	t.Chdir(target2)
	target2Store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, target2CommonDir, err := stateStoreForWorktree(context.Background(), target2)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = adoptFromExternalSessionStore(
		context.Background(), sourceStore, sourceRepo, sourceCommonDir,
		target2Store, target2CommonDir, sessionID, deferOpts,
	)
	if err == nil {
		t.Fatal("second concurrent adopt succeeded; want a claim refusal (would be a double-adopt)")
	}
	var claimed *sourceClaimedError
	if !errors.As(err, &claimed) {
		t.Fatalf("second adopt error = %v, want sourceClaimedError", err)
	}
	if got, loadErr := target2Store.Load(context.Background(), sessionID); loadErr != nil {
		t.Fatal(loadErr)
	} else if got != nil {
		t.Fatalf("second target registered the session %#v; want no adoption", got)
	}

	// The loser must not mutate the winner's claim.
	sourceAfter2, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter2.AdoptClaim == nil || !sameAdoptStore(sourceAfter2.AdoptClaim.ByCommonDir, target1CommonDir) {
		t.Fatalf("loser mutated the source claim: %#v", sourceAfter2.AdoptClaim)
	}
}

// TestAutoAdopt_StaleClaimDoesNotBlockAdoption proves the recency bound: a claim
// older than the adopt-recency window (an abandoned/aborted-commit claim) does
// not pin the source — a later legitimate deferred adopt proceeds and replaces
// the stale claim with its own fresh one.
//
// Uses t.Chdir(), so no t.Parallel().
func TestAutoAdopt_StaleClaimDoesNotBlockAdoption(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	sessionID := "test-auto-adopt-stale-claim"
	staleClaim := &session.AdoptClaim{
		ByCommonDir:    filepath.Join(base, "repo-abandoned", ".git"),
		ByWorktreePath: filepath.Join(base, "repo-abandoned"),
		At:             time.Now().Add(-2 * adoptRecentWindow),
	}
	saveActiveSourceSession(t, sourceRepo, sessionID, staleClaim)

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	_, _, sourceCommonDir, err := stateStoreForWorktree(context.Background(), sourceRepo)
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(targetRepo)
	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, targetCommonDir, err := stateStoreForWorktree(context.Background(), targetRepo)
	if err != nil {
		t.Fatal(err)
	}

	adopted, _, err := adoptFromExternalSessionStore(
		context.Background(), sourceStore, sourceRepo, sourceCommonDir,
		targetStore, targetCommonDir, sessionID,
		adoptOptions{Force: true, SkipTranscriptValidation: true, DeferSourceRetire: true},
	)
	if err != nil {
		t.Fatalf("adopt blocked by a STALE claim: %v", err)
	}
	if adopted == nil || adopted.PendingSourceRetire == nil {
		t.Fatal("adopt over a stale claim must still register the target with a PendingSourceRetire marker")
	}

	sourceAfter, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter.AdoptClaim == nil {
		t.Fatal("adopt must stamp a fresh claim on the source")
	}
	if !sameAdoptStore(sourceAfter.AdoptClaim.ByCommonDir, targetCommonDir) {
		t.Fatalf("claim ByCommonDir = %q, want target %q (stale claim not replaced)", sourceAfter.AdoptClaim.ByCommonDir, targetCommonDir)
	}
	if time.Since(sourceAfter.AdoptClaim.At) > adoptRecentWindow {
		t.Fatal("refreshed claim must be within the adopt-recency window")
	}
}

func setupAdoptRepoAt(t *testing.T, repoDir string) string {
	t.Helper()
	if err := os.MkdirAll(repoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "init.txt", "init\n")
	testutil.GitAdd(t, repoDir, "init.txt")
	testutil.GitCommit(t, repoDir, "init")
	enableEntire(t, repoDir)
	realRepoDir, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return realRepoDir
}

func seedAutoAdoptSourceSession(t *testing.T, sourceRepo, sessionID string, filesTouched []string, withOwner bool) {
	t.Helper()

	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"cross-repo work"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	var owner *proclive.Identity
	if withOwner {
		id, ok := proclive.ResolveOwner()
		if !ok {
			t.Fatal("withOwner requested but ResolveOwner failed")
		}
		owner = &id
	}
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		TranscriptPath:        transcriptPath,
		LastPrompt:            "cross-repo work",
		FilesTouched:          filesTouched,
		StepCount:             1,
		Owner:                 owner,
	}); err != nil {
		t.Fatal(err)
	}
}

func requireResolveOwner(t *testing.T) {
	t.Helper()
	if _, ok := proclive.ResolveOwner(); !ok {
		t.Skip("ResolveOwner unavailable on this platform")
	}
}

func TestAutoAdopt_SkipsOwnerMatchWithoutOverlap(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	// Owner matches but FilesTouched does not overlap staged feature.txt.
	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-owner-only", []string{"other.txt"}, true)

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetRepo, "feature.txt")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), "test-auto-adopt-owner-only")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("owner match without FilesTouched overlap must not auto-adopt")
	}
}

func TestAutoAdopt_SkipsOwnerMismatchDistinctivePath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	// Distinctive path overlaps AND the repos are siblings, so overlap and
	// proximity both PASS. The source session records no owner, so the owner
	// guard is the only reason to reject — proving it, not a boilerplate overlap.
	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-owner-mismatch", []string{"services/billing/handler.go"}, false)

	testutil.WriteFile(t, targetRepo, "services/billing/handler.go", "agent change\n")
	testutil.GitAdd(t, targetRepo, "services/billing/handler.go")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), "test-auto-adopt-owner-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("distinctive-path overlap without owner match must not auto-adopt")
	}
}

func TestCandidateFromLoaded_OwnerMismatchRejectsDistinctivePath(t *testing.T) {
	t.Parallel()

	current, ok := proclive.ResolveOwner()
	if !ok {
		t.Skip("ResolveOwner unavailable on this platform")
	}

	now := time.Now()
	staged := []string{"services/billing/handler.go"}
	newState := func(owner *proclive.Identity) *session.State {
		return &session.State{
			SessionID:           "cand-owner-unit",
			Phase:               session.PhaseActive,
			StartedAt:           now.Add(-time.Minute),
			LastInteractionTime: &now,
			FilesTouched:        []string{"services/billing/handler.go"},
			Owner:               owner,
		}
	}

	// Distinctive overlap present, but the recorded owner differs → rejected by
	// the owner guard even though overlap passes.
	mismatch := &proclive.Identity{PID: current.PID + 1, Start: "different", Host: current.Host, Boot: current.Boot}
	if _, ok := candidateFromLoaded(nil, "", "", "/target", newState(mismatch), staged, current, true); ok {
		t.Fatal("owner mismatch must reject a distinctive-path candidate")
	}

	// Positive control: only the owner changed — a matching owner is accepted.
	matching := current
	if _, ok := candidateFromLoaded(nil, "", "", "/target", newState(&matching), staged, current, true); !ok {
		t.Fatal("matching owner with distinctive overlap must be accepted")
	}
}

// shouldTryAutoAdoptOnPrepareCommitMsg uses t.Chdir(), so no t.Parallel().
func TestShouldTryAutoAdoptOnPrepareCommitMsg(t *testing.T) {
	repo := setupAdoptRepoAt(t, filepath.Join(t.TempDir(), "repo"))
	t.Chdir(repo)

	if !shouldTryAutoAdoptOnPrepareCommitMsg(context.Background(), "") {
		t.Fatal("empty source in clean repo should allow auto-adopt")
	}
	if !shouldTryAutoAdoptOnPrepareCommitMsg(context.Background(), "message") {
		t.Fatal("message source in clean repo should allow auto-adopt")
	}
	if shouldTryAutoAdoptOnPrepareCommitMsg(context.Background(), "merge") {
		t.Fatal("merge source must skip auto-adopt")
	}
	if shouldTryAutoAdoptOnPrepareCommitMsg(context.Background(), "squash") {
		t.Fatal("squash source must skip auto-adopt")
	}
	if shouldTryAutoAdoptOnPrepareCommitMsg(context.Background(), prepareCommitMsgSourceAmend) {
		t.Fatal("amend (source=commit) must skip auto-adopt")
	}

	if err := os.MkdirAll(filepath.Join(repo, ".git", "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if shouldTryAutoAdoptOnPrepareCommitMsg(context.Background(), "") {
		t.Fatal("rebase sequence must skip auto-adopt even for empty source")
	}
	if shouldTryAutoAdoptOnPrepareCommitMsg(context.Background(), "message") {
		t.Fatal("rebase sequence must skip auto-adopt for message source")
	}
}

func TestCandidateFromLoaded_RejectsWorktreePathMismatch(t *testing.T) {
	t.Parallel()

	now := time.Now()
	state := &session.State{
		SessionID:           "mismatch",
		Phase:               session.PhaseActive,
		LastInteractionTime: &now,
		WorktreePath:        "/other/worktree",
		FilesTouched:        []string{"feature.txt"},
	}
	_, ok := candidateFromLoaded(nil, "/scanned/worktree", "/common", "/target", state, []string{"feature.txt"}, proclive.Identity{}, false)
	if ok {
		t.Fatal("stale WorktreePath mismatch must reject candidate")
	}
}

func TestCandidateFromLoaded_RejectsIdle(t *testing.T) {
	t.Parallel()

	now := time.Now()
	owner := proclive.Identity{PID: 1, Start: "s"}
	state := &session.State{
		SessionID:           "idle",
		Phase:               session.PhaseIdle,
		LastInteractionTime: &now,
		WorktreePath:        "/scanned/worktree",
		FilesTouched:        []string{"feature.txt"},
		Owner:               &owner,
	}
	_, ok := candidateFromLoaded(nil, "/scanned/worktree", "/common", "/target", state, []string{"feature.txt"}, owner, true)
	if ok {
		t.Fatal("Idle session must not be an auto-adopt candidate")
	}
}

func TestCandidateFromLoaded_SkipsSourceClaimedByOtherTarget(t *testing.T) {
	t.Parallel()

	now := time.Now()
	owner := proclive.Identity{PID: 1, Start: "s"}
	newState := func(claim *session.AdoptClaim) *session.State {
		return &session.State{
			SessionID:           "claimed",
			Phase:               session.PhaseActive,
			LastInteractionTime: &now,
			WorktreePath:        "/scanned/worktree",
			FilesTouched:        []string{"feature.txt"},
			Owner:               &owner,
			AdoptClaim:          claim,
		}
	}

	// A fresh claim by a DIFFERENT target drops the source from discovery.
	other := &session.AdoptClaim{ByCommonDir: "/other/.git", At: now}
	if _, ok := candidateFromLoaded(nil, "/scanned/worktree", "/common", "/target/.git", newState(other), []string{"feature.txt"}, owner, true); ok {
		t.Fatal("source claimed by a different target must be skipped")
	}

	// A claim by THIS target is idempotent — still adoptable (re-adopt).
	same := &session.AdoptClaim{ByCommonDir: "/target/.git", At: now}
	if _, ok := candidateFromLoaded(nil, "/scanned/worktree", "/common", "/target/.git", newState(same), []string{"feature.txt"}, owner, true); !ok {
		t.Fatal("source claimed by this same target must remain adoptable")
	}
}

func TestClearInvalidAdoptTranscript_WarnsAndClears(t *testing.T) {
	// Overrides the package-level adoptWarningWriter rather than swapping the
	// process-global os.Stderr; not parallel-safe (shared package var).
	var buf bytes.Buffer
	oldWriter := adoptWarningWriter
	adoptWarningWriter = &buf
	t.Cleanup(func() { adoptWarningWriter = oldWriter })

	state := &session.State{
		SessionID:      "test-clear-invalid-transcript",
		AgentType:      agent.AgentTypeClaudeCode,
		TranscriptPath: "/tmp/not-an-agent-transcript.jsonl",
	}
	clearInvalidAdoptTranscript(context.Background(), state, t.TempDir())

	if state.TranscriptPath != "" {
		t.Fatalf("TranscriptPath = %q, want cleared", state.TranscriptPath)
	}
	if !strings.Contains(buf.String(), "lost its transcript pointer") {
		t.Fatalf("warning = %q, want transcript-pointer warning", buf.String())
	}
}

func TestStagedFilesForAutoAdopt_NonASCIIAndSpacedPaths(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "init.txt", "init\n")
	testutil.GitAdd(t, repoDir, "init.txt")
	testutil.GitCommit(t, repoDir, "init")

	// A non-ASCII name and a name with spaces: both are C-quoted by
	// `git diff --cached --name-only` (core.quotepath default on) unless -z is
	// used, so they would never match the UTF-8 paths in FilesTouched.
	nonASCII := "services/café/handler.go"
	spaced := "my dir/some file.go"
	testutil.WriteFile(t, repoDir, nonASCII, "x\n")
	testutil.WriteFile(t, repoDir, spaced, "y\n")
	testutil.GitAdd(t, repoDir, nonASCII, spaced)

	staged, err := stagedFilesForAutoAdopt(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("stagedFilesForAutoAdopt: %v", err)
	}
	got := make(map[string]bool, len(staged))
	for _, s := range staged {
		got[s] = true
	}
	for _, want := range []string{nonASCII, spaced} {
		if !got[want] {
			t.Fatalf("staged = %v, want to contain %q (unquoted UTF-8)", staged, want)
		}
	}
}
