package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

func TestLiveRegistry_RegisterListUnregister(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	now := time.Now()
	state := &State{
		SessionID:           "live-reg-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        "/tmp/repo-a",
		LastInteractionTime: &now,
		FilesTouched:        []string{"feature.txt"},
		Owner:               &proclive.Identity{PID: 42, Start: "start", Host: "host"},
	}
	commonDir := filepath.Join(t.TempDir(), ".git")
	if err := RegisterLiveSession(state, commonDir); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}

	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListLiveSessions len=%d, want 1", len(entries))
	}
	if entries[0].SessionID != state.SessionID || entries[0].CommonDir != filepath.Clean(commonDir) {
		t.Fatalf("entry = %+v", entries[0])
	}

	if err := UnregisterLiveSession(state.SessionID, commonDir); err != nil {
		t.Fatal(err)
	}
	entries, err = ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("after unregister len=%d, want 0", len(entries))
	}
}

func TestListLiveSessionsContext_FailsClosedAtCapAndSkipsOversizedFiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	for _, sessionID := range []string{"bounded-reg-001", "bounded-reg-002"} {
		if err := RegisterLiveSession(&State{
			SessionID: sessionID, Phase: PhaseActive, WorktreePath: "/tmp/" + sessionID,
			LastInteractionTime: &now,
		}, "/tmp/.git"); err != nil {
			t.Fatal(err)
		}
	}

	if entries, complete, err := ListLiveSessionsContext(context.Background(), 1); err != nil {
		t.Fatal(err)
	} else if complete || len(entries) != 1 {
		t.Fatalf("cap=1 got len=%d complete=%v, want len=1 complete=false", len(entries), complete)
	}
	if entries, complete, err := ListLiveSessionsContext(context.Background(), 2); err != nil {
		t.Fatal(err)
	} else if !complete || len(entries) != 2 {
		t.Fatalf("cap=2 got len=%d complete=%v, want len=2 complete=true", len(entries), complete)
	}

	oversizedID := "bounded-reg-oversized"
	if err := os.WriteFile(filepath.Join(liveSessionsDir(), oversizedID+".json"),
		[]byte(strings.Repeat("x", maxLiveSessionEntryBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := readLiveSessionEntry(context.Background(), oversizedID); err == nil || found {
		t.Fatalf("oversized entry read = found %v, err %v; want bounded rejection", found, err)
	}
}

func TestLiveRegistry_ClaimOwnershipIncludesWorktreeAndAttempt(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	sessionID := "claim-owner-001"
	first := AdoptClaim{ByCommonDir: "/repo/.git", ByWorktreePath: "/repo/wt-a", ByWorktreeID: "a", AttemptID: "one", At: now}
	if claimed, err := ClaimLiveSessionContext(context.Background(), sessionID, first); err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	for _, contender := range []AdoptClaim{
		{ByCommonDir: first.ByCommonDir, ByWorktreePath: "/repo/wt-b", ByWorktreeID: "b", AttemptID: "two", At: now},
		{ByCommonDir: first.ByCommonDir, ByWorktreePath: first.ByWorktreePath, ByWorktreeID: first.ByWorktreeID, AttemptID: "two", At: now},
	} {
		if claimed, err := ClaimLiveSessionContext(context.Background(), sessionID, contender); err != nil || claimed {
			t.Fatalf("contender %+v claim = %v, %v; want refused", contender, claimed, err)
		}
	}
	if released, err := ReleaseLiveSessionClaimIfOwned(context.Background(), sessionID, AdoptClaim{
		ByCommonDir: first.ByCommonDir, ByWorktreePath: first.ByWorktreePath,
		ByWorktreeID: first.ByWorktreeID, AttemptID: "wrong",
	}); err != nil || released {
		t.Fatalf("wrong-attempt release = %v, %v; want no-op", released, err)
	}
	if released, err := ReleaseLiveSessionClaimIfOwned(context.Background(), sessionID, first); err != nil || !released {
		t.Fatalf("exact release = %v, %v", released, err)
	}
}

func TestLiveRegistry_ConcurrentRegisterCannotEraseClaim(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	state := &State{SessionID: "claim-register-race", Phase: PhaseActive, WorktreePath: "/source/wt", LastInteractionTime: &now}
	if err := RegisterLiveSession(state, "/source/.git"); err != nil {
		t.Fatal(err)
	}
	claim := AdoptClaim{ByCommonDir: "/target/.git", ByWorktreePath: "/target/wt", AttemptID: "attempt", At: now}
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 50 {
			if err := RegisterLiveSession(state, "/source/.git"); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		claimed, err := ClaimLiveSessionContext(context.Background(), state.SessionID, claim)
		if err != nil {
			errCh <- err
			return
		}
		if !claimed {
			errCh <- errors.New("initial claim was unexpectedly refused")
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	got, err := LiveSessionClaim(state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.AttemptID != claim.AttemptID {
		t.Fatalf("concurrent register erased claim: %+v", got)
	}
}

func TestLiveRegistry_NonLiveSavePreservesFreshAdoptionClaim(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	commonDir := filepath.Join(t.TempDir(), ".git")
	store := NewStateStoreWithDir(filepath.Join(commonDir, SessionStateDirName))
	state := &State{SessionID: "claimed-target-condensed", Phase: PhaseActive, WorktreePath: "/target/wt", LastInteractionTime: &now}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	claim := AdoptClaim{ByCommonDir: commonDir, ByWorktreePath: state.WorktreePath, AttemptID: "attempt", At: now}
	if claimed, err := ClaimLiveSessionContext(context.Background(), state.SessionID, claim); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	ended := now
	state.Phase, state.EndedAt, state.FullyCondensed = PhaseEnded, &ended, true
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if got, err := LiveSessionClaim(state.SessionID); err != nil || got == nil || got.AttemptID != claim.AttemptID {
		t.Fatalf("post-commit-like non-live save erased claim: %+v, %v", got, err)
	}
	if released, err := ReleaseLiveSessionClaimIfOwned(context.Background(), state.SessionID, claim); err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if entries, err := ListLiveSessions(); err != nil || len(entries) != 0 {
		t.Fatalf("ended state remained registered after claim release: %+v, %v", entries, err)
	}
}

func TestLiveRegistry_SaveHooksRegisterAndClearUnregisters(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "entire-sessions")
	store := NewStateStoreWithDir(dir)
	now := time.Now()
	state := &State{
		SessionID:           "live-reg-save-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		StartedAt:           now,
		LastInteractionTime: &now,
		BaseCommit:          "abc123",
		WorktreePath:        "/tmp/repo-a",
		FilesTouched:        []string{"a.txt"},
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != state.SessionID {
		t.Fatalf("expected registry entry after Save, got %+v", entries)
	}

	if err := store.Clear(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}
	entries, err = ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty registry after Clear, got %+v", entries)
	}
}

func TestLiveRegistry_CrossRepoRetireKeepsTargetEntry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	sessionID := "live-reg-adopt-001"
	now := time.Now()

	// Target repo store: a cross-repo adopt writes the ACTIVE session here first.
	targetCommon := filepath.Join(t.TempDir(), ".git")
	targetStore := NewStateStoreWithDir(filepath.Join(targetCommon, "entire-sessions"))
	if err := targetStore.Save(context.Background(), &State{
		SessionID:           sessionID,
		Phase:               PhaseActive,
		StartedAt:           now,
		LastInteractionTime: &now,
		WorktreePath:        "/tmp/target-wt",
	}); err != nil {
		t.Fatal(err)
	}

	// Source repo store: the adopt then retires the SAME session id here. Because
	// the registry is keyed by session id alone, an unscoped unregister would
	// delete the entry the target just wrote. It must survive.
	sourceCommon := filepath.Join(t.TempDir(), ".git")
	sourceStore := NewStateStoreWithDir(filepath.Join(sourceCommon, "entire-sessions"))
	ended := now
	if err := sourceStore.Save(context.Background(), &State{
		SessionID:      sessionID,
		Phase:          PhaseEnded,
		EndedAt:        &ended,
		FullyCondensed: true,
		WorktreePath:   "/tmp/source-wt",
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != sessionID {
		t.Fatalf("source retire erased the target registry entry: %+v", entries)
	}
	if entries[0].CommonDir != normalizeCommonDir(targetCommon) {
		t.Fatalf("entry CommonDir = %q, want target %q", entries[0].CommonDir, normalizeCommonDir(targetCommon))
	}

	// A retire from the OWNING common dir still clears the entry.
	if err := UnregisterLiveSession(sessionID, targetCommon); err != nil {
		t.Fatal(err)
	}
	entries, err = ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("owning-common-dir unregister left entry behind: %+v", entries)
	}
}

func TestShouldRegisterLive_RejectsTombstone(t *testing.T) {
	state := &State{
		SessionID:               "x",
		Phase:                   PhaseActive,
		AdoptedIntoWorktreePath: "/other",
	}
	if ShouldRegisterLive(state) {
		t.Fatal("tombstoned session must not register")
	}
}

func TestRegisterLiveSession_NilStateNoPanic(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := RegisterLiveSession(nil, "/tmp/git"); err != nil {
		t.Fatalf("RegisterLiveSession(nil) = %v, want nil", err)
	}
}

func TestRegisterLiveSession_AbsolutizesRelativeCommonDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	now := time.Now()
	state := &State{
		SessionID:           "live-reg-rel-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        repo,
		LastInteractionTime: &now,
		FilesTouched:        []string{"feature.txt"},
	}
	if err := RegisterLiveSession(state, ".git"); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}

	// Reinterpret from a different CWD — persisted CommonDir must stay absolute.
	t.Chdir(t.TempDir())
	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("len=%d, want 1", len(entries))
	}
	want, err := filepath.Abs(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Clean(want)
	if entries[0].CommonDir != want {
		t.Fatalf("CommonDir = %q, want absolute %q", entries[0].CommonDir, want)
	}
	if !filepath.IsAbs(entries[0].CommonDir) {
		t.Fatalf("CommonDir must be absolute, got %q", entries[0].CommonDir)
	}
}

func TestListLiveSessions_SweepsExpiredEntries(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	stale := time.Now().Add(-LiveSessionMaxAge - time.Hour)
	fresh := time.Now()
	staleState := &State{
		SessionID:           "live-reg-stale-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        "/tmp/repo-stale",
		LastInteractionTime: &stale,
		FilesTouched:        []string{"a.txt"},
	}
	freshState := &State{
		SessionID:           "live-reg-fresh-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        "/tmp/repo-fresh",
		LastInteractionTime: &fresh,
		FilesTouched:        []string{"b.txt"},
	}
	commonDir := filepath.Join(t.TempDir(), ".git")
	if err := RegisterLiveSession(staleState, commonDir); err != nil {
		t.Fatal(err)
	}
	if err := RegisterLiveSession(freshState, commonDir); err != nil {
		t.Fatal(err)
	}

	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != freshState.SessionID {
		t.Fatalf("expected only fresh entry after TTL sweep, got %+v", entries)
	}
}
