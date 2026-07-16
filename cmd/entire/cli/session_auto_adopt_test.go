package cli

import (
	"context"
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

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	sessionID := "test-auto-adopt-sibling-001"
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"feature.txt"})

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

func TestAutoAdopt_PrepareCommitMsg_ViaLiveRegistry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// Non-sibling temp dirs: only the live registry can discover the source.
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-auto-adopt-registry-001"
	seedAutoAdoptSourceSession(t, sourceRepo, sessionID, []string{"feature.txt"})

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

func TestAutoAdopt_SkipsWhenAmbiguous(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	base := t.TempDir()
	sourceA := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	sourceC := setupAdoptRepoAt(t, filepath.Join(base, "repo-c"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	seedAutoAdoptSourceSession(t, sourceA, "test-auto-adopt-ambig-a", []string{"feature.txt"})
	seedAutoAdoptSourceSession(t, sourceC, "test-auto-adopt-ambig-c", []string{"feature.txt"})

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

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-no-overlap", []string{"other.txt"})

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
		t.Fatal("must not auto-adopt without FilesTouched overlap or owner match")
	}
}

func TestAutoAdopt_SkipsWhenLocalActiveSessionExists(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-remote", []string{"feature.txt"})

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

func seedAutoAdoptSourceSession(t *testing.T, sourceRepo, sessionID string, filesTouched []string) {
	t.Helper()

	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"cross-repo work"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
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
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAutoAdopt_SkipsOwnerMatchWithoutOverlap(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	base := t.TempDir()
	sourceRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-a"))
	targetRepo := setupAdoptRepoAt(t, filepath.Join(base, "repo-b"))

	// FilesTouched does not overlap staged feature.txt.
	seedAutoAdoptSourceSession(t, sourceRepo, "test-auto-adopt-owner-only", []string{"other.txt"})

	// Inject a fake owner matching ResolveOwner if available; overlap still required.
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	state, err := sourceStore.Load(context.Background(), "test-auto-adopt-owner-only")
	if err != nil || state == nil {
		t.Fatal(err)
	}
	if owner, ok := proclive.ResolveOwner(); ok {
		state.Owner = &owner
		if err := sourceStore.Save(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}

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
