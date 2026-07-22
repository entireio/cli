package cli

import (
	"context"
	"io"
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

func TestAutoAdopt_SkipsDistantRegistryEntry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	requireResolveOwner(t)

	// Nest under distinct parents — bare t.TempDir() siblings share a parent on macOS.
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(t.TempDir(), "src-nest", "repo"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(t.TempDir(), "dst-nest", "repo"))

	sessionID := "test-auto-adopt-distant-registry"
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"README.md"}, true)

	testutil.WriteFile(t, targetRepo, "README.md", "unrelated human edit\n")
	testutil.GitAdd(t, targetRepo, "README.md")
	t.Chdir(targetRepo)

	tryAutoAdoptCrossCommonDirSession(context.Background())

	targetStore := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != nil {
		t.Fatal("distant registry entry must not auto-adopt even with owner+overlap")
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

func TestAutoAdoptSiblingProximity(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	if !autoAdoptSiblingProximity(a, b) {
		t.Fatal("siblings under same parent should match")
	}
	if autoAdoptSiblingProximity(a, filepath.Join(t.TempDir(), "other")) {
		t.Fatal("distinct parents must not match")
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
	_, ok := candidateFromLoaded(nil, "/scanned/worktree", "/common", state, []string{"feature.txt"}, proclive.Identity{}, false)
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
	_, ok := candidateFromLoaded(nil, "/scanned/worktree", "/common", state, []string{"feature.txt"}, owner, true)
	if ok {
		t.Fatal("Idle session must not be an auto-adopt candidate")
	}
}

func TestClearInvalidAdoptTranscript_WarnsAndClears(t *testing.T) {
	// Redirects os.Stderr; not parallel-safe.
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = w.Close()
		_ = r.Close()
	})

	state := &session.State{
		SessionID:      "test-clear-invalid-transcript",
		AgentType:      agent.AgentTypeClaudeCode,
		TranscriptPath: "/tmp/not-an-agent-transcript.jsonl",
	}
	clearInvalidAdoptTranscript(context.Background(), state, t.TempDir())
	_ = w.Close()
	os.Stderr = oldStderr

	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()

	if state.TranscriptPath != "" {
		t.Fatalf("TranscriptPath = %q, want cleared", state.TranscriptPath)
	}
	if !strings.Contains(buf.String(), "lost its transcript pointer") {
		t.Fatalf("stderr = %q, want transcript-pointer warning", buf.String())
	}
}
