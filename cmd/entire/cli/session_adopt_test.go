package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestSessionAdopt_HelpDistinguishesForceAndYes(t *testing.T) {
	cmd := newAdoptCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("expected help to render without error, got: %v", err)
	}

	out := stdout.String()
	for _, want := range []string{
		"--force",
		"replace an existing local state file for the same session",
		"--yes",
		"confirm same-store adoption and replacement without prompting",
		// NOTE: this only proves the flag is DOCUMENTED. The name also
		// appears in the command's Long prose, so this assertion passes even
		// with the flag misregistered — verified by renaming it and watching
		// this test stay green. Registration is pinned by
		// TestSessionAdopt_ForceDoesNotSetAllowForeignSession instead.
		"--allow-foreign-session",
		"adopt a session that is not this command's own caller",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "replace an existing local state file for the same session") != 1 {
		t.Fatalf("--force and --yes should not share replacement help text:\n%s", out)
	}
}

// Three flags, three meanings — asserted at the FLAG layer, because every
// behavioural test in this file constructs adoptOptions directly and so would
// pass with the flag misregistered or not registered at all.
func TestSessionAdopt_ForceDoesNotSetAllowForeignSession(t *testing.T) {
	cmd := newAdoptCmd()
	if err := cmd.Flags().Parse([]string{"--force", "--from", "../somewhere"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	const foreignFlag = "allow-foreign-session"
	if cmd.Flags().Lookup(foreignFlag) == nil {
		t.Fatalf("--%s is not registered on the command", foreignFlag)
	}
	granted, err := cmd.Flags().GetBool(foreignFlag)
	if err != nil {
		t.Fatalf("GetBool(%q): %v", foreignFlag, err)
	}
	if granted {
		t.Errorf("--force granted %s; the ownership waiver must be its own flag", foreignFlag)
	}

	// And it does take effect on its own.
	cmd = newAdoptCmd()
	if err := cmd.Flags().Parse([]string{"--" + foreignFlag}); err != nil {
		t.Fatalf("parse --%s: %v", foreignFlag, err)
	}
	granted, err = cmd.Flags().GetBool(foreignFlag)
	if err != nil {
		t.Fatalf("GetBool(%q): %v", foreignFlag, err)
	}
	if !granted {
		t.Errorf("--%s did not take effect when passed", foreignFlag)
	}
}

// adoptAsCaller publishes sessionID as the caller's own, the way the agent
// that owns it would, so a test exercises adoption's MECHANICS through the
// supported path instead of the --allow-foreign-session escape hatch.
//
// Needed because adoption now refuses a session it cannot confirm is the
// caller's, and `go test` inherits the developer's real agent variable — which
// names a session that is emphatically not the fixture.
func adoptAsCaller(t *testing.T, sessionID string) {
	t.Helper()
	clearCallerSessionEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessionID)
}

func TestSessionAdopt_MovesExternalSessionIntoCurrentWorktree(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-session-001"
	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"update target file"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	lastInteraction := time.Now().Add(-1 * time.Minute)
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
		LastPrompt:            "update target file",
		FilesTouched:          []string{"source-only.txt"},
		TurnCheckpointIDs:     []string{"abc123def456"},
		AttachedManually:      true,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target repo")
	}
	if adopted.WorktreePath != targetRepo {
		t.Fatalf("WorktreePath = %q, want %q", adopted.WorktreePath, targetRepo)
	}
	if adopted.BaseCommit != testutil.GetHeadHash(t, targetRepo) {
		t.Fatalf("BaseCommit = %q, want target HEAD", adopted.BaseCommit)
	}
	if adopted.TranscriptPath != transcriptPath {
		t.Fatalf("TranscriptPath = %q, want %q", adopted.TranscriptPath, transcriptPath)
	}
	if adopted.AttachedManually {
		t.Fatal("adopted active sessions should not be marked manually attached")
	}
	if len(adopted.FilesTouched) != 1 || adopted.FilesTouched[0] != "feature.txt" {
		t.Fatalf("FilesTouched = %v, want [feature.txt]", adopted.FilesTouched)
	}
	if len(adopted.TurnCheckpointIDs) != 0 {
		t.Fatalf("TurnCheckpointIDs = %v, want empty target-local checkpoint bookkeeping", adopted.TurnCheckpointIDs)
	}
	if !bytes.Contains(out.Bytes(), []byte("Adopted session")) {
		t.Fatalf("output = %q, want adoption confirmation", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("Review tracked files before committing")) {
		t.Fatalf("output = %q, want tracked-file attribution warning", out.String())
	}
}

func TestSessionAdopt_ExternalStoreRetiresSourceSession(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-external-retire-source"
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
		LastPrompt:            "continue work in target repo",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted target session state")
	}
	if adopted.Phase != session.PhaseActive || adopted.EndedAt != nil {
		t.Fatalf("target state Phase/EndedAt = %q/%v, want active/nil", adopted.Phase, adopted.EndedAt)
	}

	sourceAfter, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter == nil {
		t.Fatal("expected source session state to remain as a retired record")
	}
	if sourceAfter.Phase != session.PhaseEnded {
		t.Fatalf("source Phase = %q, want ended", sourceAfter.Phase)
	}
	if sourceAfter.EndedAt == nil {
		t.Fatal("source EndedAt = nil, want retirement timestamp")
	}
	if isAdoptableSourceSession(sourceAfter) {
		t.Fatalf("source state remains adoptable after external adoption: %#v", sourceAfter)
	}

	t.Chdir(sourceRepo)
	sourceAgent := &mockLifecycleAgent{name: agent.AgentNameClaudeCode, agentType: agent.AgentTypeClaudeCode}
	if err := handleLifecycleSessionStart(context.Background(), sourceAgent, &agent.Event{
		Type:      agent.SessionStart,
		SessionID: sessionID,
	}); err != nil {
		t.Fatalf("SessionStart in the adopted-away source repo should no-op without disrupting the hook, got: %v", err)
	}
	sourceAfterSessionStart, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfterSessionStart == nil {
		entries, readErr := os.ReadDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
		if readErr != nil {
			t.Fatalf("source state disappeared after SessionStart; read state dir: %v", readErr)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("source state disappeared after SessionStart; state dir contains %v", names)
	}
	if sourceAfterSessionStart.Phase != session.PhaseEnded {
		t.Fatalf("source Phase after SessionStart = %q, want ended", sourceAfterSessionStart.Phase)
	}
	if sourceAfterSessionStart.EndedAt == nil {
		t.Fatal("source EndedAt after SessionStart = nil, want retirement timestamp")
	}

	err = strategy.NewManualCommitStrategy().InitializeSession(
		context.Background(),
		sessionID,
		agent.AgentTypeClaudeCode,
		"",
		"source prompt after adoption",
		"",
	)
	if err != nil {
		t.Fatalf("InitializeSession in the adopted-away source repo should no-op without disrupting the hook, got: %v", err)
	}

	sourceAfterTurnStart, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfterTurnStart.Phase != session.PhaseEnded {
		t.Fatalf("source Phase after rejected TurnStart = %q, want ended", sourceAfterTurnStart.Phase)
	}
	if sourceAfterTurnStart.EndedAt == nil {
		t.Fatal("source EndedAt after rejected TurnStart = nil, want retirement timestamp")
	}
}

func TestSessionAdopt_ExternalStoreRollsBackTargetWhenSourceRetireFails(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("uses POSIX directory permissions to force source save failure")
	}

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-retire-rollback"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStateDir := filepath.Join(sourceRepo, ".git", session.SessionStateDirName)
	sourceStore := session.NewStateStoreWithDir(sourceStateDir)
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		LastPrompt:            "move this session",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-10 * time.Minute),
		Phase:                 session.PhaseIdle,
		BaseCommit:            testutil.GetHeadHash(t, targetRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, targetRepo),
		WorktreePath:          targetRepo,
		LastPrompt:            "preexisting target state",
	}); err != nil {
		t.Fatal(err)
	}

	_, _, sourceCommonDir, err := stateStoreForWorktree(context.Background(), sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	_, _, targetCommonDir, err := stateStoreForWorktree(context.Background(), targetRepo)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(sourceStateDir)
	if err != nil {
		t.Fatal(err)
	}
	restoreSourceStateDir := func() error {
		return os.Chmod(sourceStateDir, info.Mode().Perm())
	}
	if err := os.Chmod(sourceStateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreSourceStateDir(); err != nil {
			t.Logf("restore source state dir permissions: %v", err)
		}
	})

	_, _, err = adoptFromExternalSessionStore(
		context.Background(),
		sourceStore,
		sourceRepo,
		sourceCommonDir,
		targetStore,
		targetCommonDir,
		sessionID,
		adoptOptions{Force: true},
		true, // ownership already authorized; this test covers mutation mechanics
	)
	if err := restoreSourceStateDir(); err != nil {
		t.Fatalf("restore source state dir permissions: %v", err)
	}
	if err == nil {
		t.Fatal("adoptFromExternalSessionStore succeeded, want source-retire failure")
	}
	if !strings.Contains(err.Error(), "retire source session state") {
		t.Fatalf("adoptFromExternalSessionStore error = %v, want source-retire failure", err)
	}

	loadedTarget, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedTarget == nil {
		t.Fatal("target rollback removed preexisting state, want restore")
	}
	if loadedTarget.LastPrompt != "preexisting target state" {
		t.Fatalf("target LastPrompt after rollback = %q, want preexisting target state", loadedTarget.LastPrompt)
	}
	if loadedTarget.Phase != session.PhaseIdle {
		t.Fatalf("target Phase after rollback = %q, want idle", loadedTarget.Phase)
	}

	sourceAfter, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter == nil || sourceAfter.Phase != session.PhaseActive {
		t.Fatalf("source state after failed adoption = %#v, want original active state", sourceAfter)
	}
}

func TestSessionAdopt_ExternalStoreClearsNewTargetWhenSourceRetireFails(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("uses POSIX directory permissions to force source save failure")
	}

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-retire-clear-target"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStateDir := filepath.Join(sourceRepo, ".git", session.SessionStateDirName)
	sourceStore := session.NewStateStoreWithDir(sourceStateDir)
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		LastPrompt:            "move this session",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, sourceCommonDir, err := stateStoreForWorktree(context.Background(), sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	_, _, targetCommonDir, err := stateStoreForWorktree(context.Background(), targetRepo)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(sourceStateDir)
	if err != nil {
		t.Fatal(err)
	}
	restoreSourceStateDir := func() error {
		return os.Chmod(sourceStateDir, info.Mode().Perm())
	}
	if err := os.Chmod(sourceStateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreSourceStateDir(); err != nil {
			t.Logf("restore source state dir permissions: %v", err)
		}
	})

	_, _, err = adoptFromExternalSessionStore(
		context.Background(),
		sourceStore,
		sourceRepo,
		sourceCommonDir,
		targetStore,
		targetCommonDir,
		sessionID,
		adoptOptions{Force: true},
		true, // ownership already authorized; this test covers mutation mechanics
	)
	if err := restoreSourceStateDir(); err != nil {
		t.Fatalf("restore source state dir permissions: %v", err)
	}
	if err == nil {
		t.Fatal("adoptFromExternalSessionStore succeeded, want source-retire failure")
	}
	if !strings.Contains(err.Error(), "retire source session state") {
		t.Fatalf("adoptFromExternalSessionStore error = %v, want source-retire failure", err)
	}

	loadedTarget, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedTarget != nil {
		t.Fatalf("target state after rollback = %#v, want nil", loadedTarget)
	}
}

func TestSessionAdopt_ClearsSourceOwner(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-clear-owner"
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
		Owner:                 &proclive.Identity{PID: os.Getpid(), Start: "source-owner"},
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target repo")
	}
	if adopted.Owner != nil {
		t.Fatalf("Owner = %#v, want nil so source process liveness cannot finalize adopted session", adopted.Owner)
	}
}

func TestSessionAdopt_RejectsUnexpectedSourceTranscriptPath(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-reject-transcript"
	transcriptPath := filepath.Join(t.TempDir(), sessionID+".jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	lastInteraction := time.Now().Add(-1 * time.Minute)
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
		LastPrompt:            "update target file",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded, want transcript-path refusal")
	}
	if !strings.Contains(err.Error(), "unexpected transcript path") {
		t.Fatalf("runAdopt error = %v, want unexpected transcript path", err)
	}

	targetStore, storeErr := session.NewStateStore(context.Background())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	adopted, loadErr := targetStore.Load(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if adopted != nil {
		t.Fatalf("target state was written despite transcript-path refusal: %#v", adopted)
	}
}

func TestSessionAdopt_ExternalStoreRejectsSourceEndedAfterInitialSelection(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-external-source-stale"
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
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := selectAdoptSourceSession(context.Background(), sourceStore, sourceRepo, sessionID); err != nil {
		t.Fatalf("initial source selection failed: %v", err)
	}

	endedAt := time.Now()
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		EndedAt:               &endedAt,
		Phase:                 session.PhaseIdle,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, sourceCommonDir, err := stateStoreForWorktree(context.Background(), sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	_, _, targetCommonDir, err := stateStoreForWorktree(context.Background(), targetRepo)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = adoptFromExternalSessionStore(
		context.Background(),
		sourceStore,
		sourceRepo,
		sourceCommonDir,
		targetStore,
		targetCommonDir,
		sessionID,
		adoptOptions{Force: true},
		true, // ownership already authorized; this test covers mutation mechanics
	)
	if err == nil {
		t.Fatal("adoptFromExternalSessionStore succeeded from stale ended source, want refusal")
	}
	if !strings.Contains(err.Error(), "ended or fully condensed") {
		t.Fatalf("adoptFromExternalSessionStore error = %v, want ended-session refusal", err)
	}
}

func TestSessionAdopt_ExternalStoreChecksTargetStateAfterLockWait(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-external-target-race"
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
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, sourceCommonDir, err := stateStoreForWorktree(context.Background(), sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	_, _, targetCommonDir, err := stateStoreForWorktree(context.Background(), targetRepo)
	if err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(targetCommonDir, "entire-session-locks", sessionID+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, adoptErr := adoptFromExternalSessionStore(
			context.Background(),
			sourceStore,
			sourceRepo,
			sourceCommonDir,
			targetStore,
			targetCommonDir,
			sessionID,
			adoptOptions{},
			true, // ownership already authorized; this test covers the lock wait
		)
		done <- adoptErr
	}()

	select {
	case err := <-done:
		release()
		t.Fatalf("adoptFromExternalSessionStore finished before target lock released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := targetStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now(),
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, targetRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, targetRepo),
		WorktreePath:          targetRepo,
		LastPrompt:            "concurrent target state",
	}); err != nil {
		release()
		t.Fatal(err)
	}
	release()

	err = <-done
	if err == nil {
		t.Fatal("adoptFromExternalSessionStore succeeded, want existing target refusal")
	}
	if !strings.Contains(err.Error(), "already tracked in this repo") {
		t.Fatalf("adoptFromExternalSessionStore error = %v, want existing-state refusal", err)
	}

	loaded, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastPrompt != "concurrent target state" {
		t.Fatalf("target state LastPrompt = %q, want concurrent target state", loaded.LastPrompt)
	}
}

func TestSessionAdopt_EnablesPrepareCommitMsgTrailer(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-trailer-001"
	targetRelPath := "src/feature.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)

	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"human","message":{"content":"write feature.go"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` + targetAbsPath + `","content":"package src\n"}}]}}
`
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(transcriptPath, stale, stale); err != nil {
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
		LastPrompt:            "write feature.go",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, targetRelPath, "package src\n")
	testutil.GitAdd(t, targetRepo, targetRelPath)
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}

	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func TestSessionAdopt_IdleSourceSurvivesPrepareCommitMsgTrailer(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-idle-source"
	targetRelPath := "src/idle.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)
	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"human","message":{"content":"write idle.go"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` + targetAbsPath + `","content":"package src\n"}}]}}
`
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseIdle,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		TranscriptPath:        transcriptPath,
		LastPrompt:            "write idle.go",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, targetRelPath, "package src\n")
	testutil.GitAdd(t, targetRepo, targetRelPath)
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state")
	}
	if adopted.Phase != session.PhaseActive {
		t.Fatalf("Phase = %q, want active so commit hooks do not sweep adopted state", adopted.Phase)
	}
	if adopted.EndedAt != nil {
		t.Fatalf("EndedAt = %v, want nil", adopted.EndedAt)
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add idle feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func TestSessionAdopt_RejectsEndedAtSourceSession(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-ended-at"
	endedAt := time.Now().Add(-30 * time.Second)
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		EndedAt:               &endedAt,
		Phase:                 session.PhaseIdle,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded, want ended-session refusal")
	}
	if !strings.Contains(err.Error(), "ended or fully condensed") {
		t.Fatalf("runAdopt error = %v, want ended-session refusal", err)
	}

	_, err = selectAdoptSourceSession(context.Background(), sourceStore, sourceRepo, "")
	if err == nil {
		t.Fatal("selectAdoptSourceSession succeeded, want no recent active sessions")
	}
	if !strings.Contains(err.Error(), "no recent active sessions") {
		t.Fatalf("selectAdoptSourceSession error = %v, want no recent active sessions", err)
	}
}

func TestSessionAdopt_ResetsSourceCheckpointWindow(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-reset-window"
	targetRelPath := "src/feature.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)

	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"human","message":{"content":"first source prompt"},"uuid":"source-user"}
{"type":"assistant","message":{"content":"source response"},"uuid":"source-assistant"}
{"type":"human","message":{"content":"write target feature"},"uuid":"target-user"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` + targetAbsPath + `","content":"package src\n"}}]},"uuid":"target-assistant"}
`
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:                   sessionID,
		AgentType:                   agent.AgentTypeClaudeCode,
		StartedAt:                   time.Now().Add(-5 * time.Minute),
		LastInteractionTime:         &lastInteraction,
		Phase:                       session.PhaseActive,
		BaseCommit:                  testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit:       testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:                sourceRepo,
		TranscriptPath:              transcriptPath,
		LastPrompt:                  "write target feature",
		StepCount:                   4,
		SessionDurationMs:           120_000,
		SessionTurnCount:            7,
		ContextTokens:               42_000,
		ContextWindowSize:           200_000,
		CheckpointTranscriptStart:   2,
		CheckpointTranscriptSize:    1234,
		CondensedTranscriptLines:    2,
		TranscriptLinesAtStart:      2,
		TranscriptIdentifierAtStart: "source-assistant",
		TurnID:                      "source-turn",
		TurnCheckpointIDs:           []string{"abc123def456"},
		LastCheckpointID:            id.MustCheckpointID("abc123def456"),
		CondensationAttempt: &session.CondensationAttempt{
			CheckpointID:    id.MustCheckpointID("fedcba987654"),
			RecoveryPending: true,
		},
		LastCheckpointCommitHash: "source-commit",
		CheckpointTokenUsage:     &agent.TokenUsage{InputTokens: 100, OutputTokens: 25, APICallCount: 1},
		UntrackedFilesAtStart:    []string{"source-only.txt"},
		PromptWindowBase:         3,
		PromptWindowResetPending: true,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, targetRelPath, "package src\n")
	testutil.GitAdd(t, targetRepo, targetRelPath)
	testutil.WriteFile(t, targetRepo, "target-notes.txt", "user notes\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target repo")
	}
	if adopted.StepCount != 0 {
		t.Fatalf("StepCount = %d, want 0 for first target checkpoint", adopted.StepCount)
	}
	if adopted.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want 0", adopted.CheckpointTranscriptStart)
	}
	if adopted.CheckpointTranscriptSize != 0 {
		t.Fatalf("CheckpointTranscriptSize = %d, want 0", adopted.CheckpointTranscriptSize)
	}
	if adopted.TranscriptIdentifierAtStart != "" {
		t.Fatalf("TranscriptIdentifierAtStart = %q, want empty", adopted.TranscriptIdentifierAtStart)
	}
	if adopted.SessionDurationMs != 120_000 {
		t.Fatalf("SessionDurationMs = %d, want preserved source duration", adopted.SessionDurationMs)
	}
	if adopted.SessionTurnCount != 7 {
		t.Fatalf("SessionTurnCount = %d, want preserved source turn count", adopted.SessionTurnCount)
	}
	if adopted.ContextTokens != 42_000 {
		t.Fatalf("ContextTokens = %d, want preserved source context tokens", adopted.ContextTokens)
	}
	if adopted.ContextWindowSize != 200_000 {
		t.Fatalf("ContextWindowSize = %d, want preserved source context window size", adopted.ContextWindowSize)
	}
	if adopted.PromptWindowBase != adopted.SessionTurnCount {
		t.Fatalf("PromptWindowBase = %d, want current SessionTurnCount %d", adopted.PromptWindowBase, adopted.SessionTurnCount)
	}
	if adopted.PromptWindowResetPending {
		t.Fatal("PromptWindowResetPending = true, want false for adopted target window")
	}
	if len(adopted.TurnCheckpointIDs) != 0 {
		t.Fatalf("TurnCheckpointIDs = %v, want empty", adopted.TurnCheckpointIDs)
	}
	if adopted.TurnID != "" {
		t.Fatalf("TurnID = %q, want empty target-local turn ID", adopted.TurnID)
	}
	if len(adopted.UntrackedFilesAtStart) != 1 || adopted.UntrackedFilesAtStart[0] != "target-notes.txt" {
		t.Fatalf("UntrackedFilesAtStart = %v, want target worktree snapshot [target-notes.txt]", adopted.UntrackedFilesAtStart)
	}
	if !adopted.LastCheckpointID.IsEmpty() {
		t.Fatalf("LastCheckpointID = %s, want empty", adopted.LastCheckpointID.String())
	}
	if adopted.CondensationAttempt != nil {
		t.Fatalf("CondensationAttempt = %#v, want nil", adopted.CondensationAttempt)
	}
	if adopted.LastCheckpointCommitHash != "" {
		t.Fatalf("LastCheckpointCommitHash = %q, want empty", adopted.LastCheckpointCommitHash)
	}
	if adopted.CheckpointTokenUsage != nil {
		t.Fatalf("CheckpointTokenUsage = %#v, want nil for first target checkpoint", adopted.CheckpointTokenUsage)
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add target feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func TestSessionAdopt_ClearsLegacyTranscriptOffsets(t *testing.T) {
	targetRepo := setupAdoptRepo(t)
	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	adopted, _, err := buildAdoptedSessionState(context.Background(), &session.State{
		SessionID:                 "test-adopt-legacy-offsets",
		AgentType:                 agent.AgentTypeClaudeCode,
		StartedAt:                 time.Now().Add(-5 * time.Minute),
		Phase:                     session.PhaseActive,
		BaseCommit:                "source-head",
		WorktreePath:              "/source/repo",
		CheckpointTranscriptStart: 9,
		CondensedTranscriptLines:  9,
		TranscriptLinesAtStart:    9,
	})
	if err != nil {
		t.Fatalf("buildAdoptedSessionState failed: %v", err)
	}
	if adopted.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want 0", adopted.CheckpointTranscriptStart)
	}

	encoded, err := json.Marshal(adopted)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("condensed_transcript_lines")) {
		t.Fatalf("adopted state JSON contains condensed_transcript_lines: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("transcript_lines_at_start")) {
		t.Fatalf("adopted state JSON contains transcript_lines_at_start: %s", encoded)
	}
}

// TestSessionAdopt_RebaselinesSubagentTokens pins finding 019f5ebf-dc42: cross-repo
// adoption opens a fresh target-local checkpoint window (StepCount=0,
// CheckpointTokenUsage=nil), but the cloned TokenUsage carries the SOURCE
// session's full cumulative subagent total. If SubagentTokensBaseline is not
// re-baselined to that cumulative, the first post-adopt checkpoint subtracts a
// stale/nil baseline and over-reports the source session's subagent usage.
func TestSessionAdopt_RebaselinesSubagentTokens(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sourceBaseline *agent.TokenUsage
	}{
		// Source never condensed: baseline is nil, so the first adopted
		// checkpoint would report the entire cumulative subagent total.
		{name: "never-condensed-source", sourceBaseline: nil},
		// Source condensed at an earlier window: its baseline is stale relative
		// to the current cumulative and must not carry into the target window.
		{name: "previously-condensed-source", sourceBaseline: &agent.TokenUsage{InputTokens: 200, OutputTokens: 100, APICallCount: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targetRepo := setupAdoptRepo(t)
			testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
			t.Chdir(targetRepo)

			adopted, _, err := buildAdoptedSessionState(context.Background(), &session.State{
				SessionID:    "test-adopt-subagent-baseline-" + tc.name,
				AgentType:    agent.AgentTypeClaudeCode,
				StartedAt:    time.Now().Add(-5 * time.Minute),
				Phase:        session.PhaseActive,
				BaseCommit:   "source-head",
				WorktreePath: "/source/repo",
				TokenUsage: &agent.TokenUsage{
					InputTokens:    1000,
					OutputTokens:   500,
					APICallCount:   10,
					SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5},
				},
				SubagentTokensBaseline: tc.sourceBaseline,
			})
			if err != nil {
				t.Fatalf("buildAdoptedSessionState failed: %v", err)
			}

			if adopted.SubagentTokensBaseline == nil {
				t.Fatal("adopted SubagentTokensBaseline = nil, want re-baselined to the cumulative subagent total")
			}
			if adopted.SubagentTokensBaseline.InputTokens != 500 || adopted.SubagentTokensBaseline.OutputTokens != 250 {
				t.Fatalf("adopted SubagentTokensBaseline = %#v, want cumulative subagent total 500/250",
					adopted.SubagentTokensBaseline)
			}

			// The first post-adopt checkpoint delta (cumulative - baseline) must be
			// zero: adoption should count only target-side subagent growth.
			delta := types.SubtractTokenUsage(adopted.TokenUsage.SubagentTokens, adopted.SubagentTokensBaseline)
			if delta.InputTokens != 0 || delta.OutputTokens != 0 || delta.APICallCount != 0 {
				t.Fatalf("first post-adopt subagent delta = %#v, want zero", delta)
			}
		})
	}
}

func TestSessionAdopt_PreservesReviewAndInvestigateMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind session.Kind
	}{
		{name: "review", kind: session.KindAgentReview},
		{name: "investigate", kind: session.KindAgentInvestigate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targetRepo := setupAdoptRepo(t)
			testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
			t.Chdir(targetRepo)

			adopted, _, err := buildAdoptedSessionState(context.Background(), &session.State{
				SessionID:         "test-adopt-kind-" + tc.name,
				AgentType:         agent.AgentTypeClaudeCode,
				StartedAt:         time.Now().Add(-5 * time.Minute),
				Phase:             session.PhaseActive,
				Kind:              tc.kind,
				ReviewSkills:      []string{"/review"},
				ReviewPrompt:      "review this branch",
				InvestigateRunID:  "abcdef012345",
				InvestigateTopic:  "Why is adoption misclassified?",
				BaseCommit:        "source-head",
				WorktreePath:      "/source/repo",
				LastCheckpointID:  id.MustCheckpointID("abc123def456"),
				TurnCheckpointIDs: []string{"abc123def456"},
				PromptWindowBase:  3,
				SessionTurnCount:  7,
				AttachedManually:  true,
			})
			if err != nil {
				t.Fatalf("buildAdoptedSessionState failed: %v", err)
			}

			if adopted.Kind != tc.kind {
				t.Fatalf("Kind = %q, want %q", adopted.Kind, tc.kind)
			}
			if len(adopted.ReviewSkills) != 1 || adopted.ReviewSkills[0] != "/review" {
				t.Fatalf("ReviewSkills = %v, want [/review]", adopted.ReviewSkills)
			}
			if adopted.ReviewPrompt != "review this branch" {
				t.Fatalf("ReviewPrompt = %q, want review prompt", adopted.ReviewPrompt)
			}
			if adopted.InvestigateRunID != "abcdef012345" {
				t.Fatalf("InvestigateRunID = %q, want source run ID", adopted.InvestigateRunID)
			}
			if adopted.InvestigateTopic != "Why is adoption misclassified?" {
				t.Fatalf("InvestigateTopic = %q, want source topic", adopted.InvestigateTopic)
			}
		})
	}
}

func TestSessionAdopt_CloneSourceStateDoesNotShareMutableFields(t *testing.T) {
	lastInteraction := time.Now().Add(-1 * time.Minute)
	endedAt := time.Now()
	source := &session.State{
		SessionID:             "test-adopt-deep-copy",
		StartedAt:             time.Now().Add(-5 * time.Minute),
		EndedAt:               &endedAt,
		LastInteractionTime:   &lastInteraction,
		ReviewSkills:          []string{"/review"},
		TurnCheckpointIDs:     []string{"source-checkpoint"},
		UntrackedFilesAtStart: []string{"untracked.txt"},
		FilesTouched:          []string{"source.txt"},
		TokenUsage: &agent.TokenUsage{
			InputTokens: 1,
			SubagentTokens: &agent.TokenUsage{
				OutputTokens: 2,
			},
		},
		SkillEvents: []agent.SkillEvent{
			{
				ID: "skill-event",
				TranscriptAnchor: &agent.SkillEventTranscriptAnchor{
					EntryIDs: []string{"entry-1"},
				},
				Native: map[string]string{"tool": "skill"},
			},
		},
		PromptAttributions: []session.PromptAttribution{
			{
				UserAddedPerFile:   map[string]int{"source.txt": 1},
				UserRemovedPerFile: map[string]int{"source.txt": 2},
			},
		},
		PendingPromptAttribution: &session.PromptAttribution{
			UserAddedPerFile:   map[string]int{"pending.txt": 3},
			UserRemovedPerFile: map[string]int{"pending.txt": 4},
		},
	}

	adopted := cloneAdoptSourceState(source)
	*adopted.EndedAt = endedAt.Add(1 * time.Hour)
	*adopted.LastInteractionTime = lastInteraction.Add(1 * time.Hour)
	adopted.ReviewSkills[0] = "/changed"
	adopted.TurnCheckpointIDs[0] = "changed-checkpoint"
	adopted.UntrackedFilesAtStart[0] = "changed-untracked.txt"
	adopted.FilesTouched[0] = "changed-source.txt"
	adopted.TokenUsage.SubagentTokens.OutputTokens = 99
	adopted.SkillEvents[0].TranscriptAnchor.EntryIDs[0] = "changed-entry"
	adopted.SkillEvents[0].Native["tool"] = "changed-skill"
	adopted.PromptAttributions[0].UserAddedPerFile["source.txt"] = 99
	adopted.PromptAttributions[0].UserRemovedPerFile["source.txt"] = 99
	adopted.PendingPromptAttribution.UserAddedPerFile["pending.txt"] = 99
	adopted.PendingPromptAttribution.UserRemovedPerFile["pending.txt"] = 99

	if !source.EndedAt.Equal(endedAt) {
		t.Fatalf("source EndedAt was mutated: %v", source.EndedAt)
	}
	if !source.LastInteractionTime.Equal(lastInteraction) {
		t.Fatalf("source LastInteractionTime was mutated: %v", source.LastInteractionTime)
	}
	if source.ReviewSkills[0] != "/review" {
		t.Fatalf("source ReviewSkills = %v, want unchanged", source.ReviewSkills)
	}
	if source.TurnCheckpointIDs[0] != "source-checkpoint" {
		t.Fatalf("source TurnCheckpointIDs = %v, want unchanged", source.TurnCheckpointIDs)
	}
	if source.UntrackedFilesAtStart[0] != "untracked.txt" {
		t.Fatalf("source UntrackedFilesAtStart = %v, want unchanged", source.UntrackedFilesAtStart)
	}
	if source.FilesTouched[0] != "source.txt" {
		t.Fatalf("source FilesTouched = %v, want unchanged", source.FilesTouched)
	}
	if source.TokenUsage.SubagentTokens.OutputTokens != 2 {
		t.Fatalf("source TokenUsage.SubagentTokens.OutputTokens = %d, want unchanged", source.TokenUsage.SubagentTokens.OutputTokens)
	}
	if source.SkillEvents[0].TranscriptAnchor.EntryIDs[0] != "entry-1" {
		t.Fatalf("source SkillEvents entry IDs = %v, want unchanged", source.SkillEvents[0].TranscriptAnchor.EntryIDs)
	}
	if source.SkillEvents[0].Native["tool"] != "skill" {
		t.Fatalf("source SkillEvents native = %v, want unchanged", source.SkillEvents[0].Native)
	}
	if source.PromptAttributions[0].UserAddedPerFile["source.txt"] != 1 {
		t.Fatalf("source PromptAttributions user added = %v, want unchanged", source.PromptAttributions[0].UserAddedPerFile)
	}
	if source.PromptAttributions[0].UserRemovedPerFile["source.txt"] != 2 {
		t.Fatalf("source PromptAttributions user removed = %v, want unchanged", source.PromptAttributions[0].UserRemovedPerFile)
	}
	if source.PendingPromptAttribution.UserAddedPerFile["pending.txt"] != 3 {
		t.Fatalf("source PendingPromptAttribution user added = %v, want unchanged", source.PendingPromptAttribution.UserAddedPerFile)
	}
	if source.PendingPromptAttribution.UserRemovedPerFile["pending.txt"] != 4 {
		t.Fatalf("source PendingPromptAttribution user removed = %v, want unchanged", source.PendingPromptAttribution.UserRemovedPerFile)
	}
}

func TestSessionAdopt_FromSubdirectoryReadsSourceStore(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sourceSubdir := filepath.Join(sourceRepo, "nested", "dir")
	if err := os.MkdirAll(sourceSubdir, 0o750); err != nil {
		t.Fatal(err)
	}

	sessionID := "test-adopt-from-subdir"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceSubdir,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed from source subdir: %v", err)
	}
}

func TestSessionAdopt_FiltersSharedSourceStoreByFromWorktree(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	siblingWorktree := filepath.Join(t.TempDir(), "sibling-worktree")
	runAdoptGit(t, sourceRepo, "worktree", "add", siblingWorktree, "-b", "sibling-worktree")
	resolvedSiblingWorktree, err := filepath.EvalSymlinks(siblingWorktree)
	if err != nil {
		t.Fatal(err)
	}
	siblingWorktree = resolvedSiblingWorktree
	t.Cleanup(func() {
		runAdoptGit(t, sourceRepo, "worktree", "remove", siblingWorktree, "--force")
	})
	targetRepo := setupAdoptRepo(t)

	sourceWorktreeID, err := paths.GetWorktreeID(sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	siblingWorktreeID, err := paths.GetWorktreeID(siblingWorktree)
	if err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           "source-worktree-session",
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		WorktreeID:          sourceWorktreeID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           "sibling-worktree-session",
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, siblingWorktree),
		WorktreePath:        siblingWorktree,
		WorktreeID:          siblingWorktreeID,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	// Auto-selection is guarded too: whatever --from resolves to still has to
	// be the caller's own session, which is what closes the
	// `adopt --from <path>` shape that needs no session ID at all.
	adoptAsCaller(t, "source-worktree-session")
	err = runAdopt(context.Background(), &out, "", adoptOptions{
		FromWorktree: sourceRepo,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), "source-worktree-session")
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected source worktree session to be adopted")
	}
	if wrong, err := targetStore.Load(context.Background(), "sibling-worktree-session"); err != nil {
		t.Fatal(err)
	} else if wrong != nil {
		t.Fatalf("adopted sibling worktree session unexpectedly: %#v", wrong)
	}
}

func TestSessionAdopt_RejectsSourceSessionWithoutWorktreeMetadata(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)

	sessionID := "missing-worktree-metadata"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := selectAdoptSourceSession(context.Background(), sourceStore, sourceRepo, sessionID)
	if err == nil {
		t.Fatal("selectAdoptSourceSession succeeded for explicit session without worktree metadata, want refusal")
	}
	if !strings.Contains(err.Error(), "belongs to") || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("selectAdoptSourceSession error = %v, want missing-worktree ownership refusal", err)
	}

	_, err = selectAdoptSourceSession(context.Background(), sourceStore, sourceRepo, "")
	if err == nil {
		t.Fatal("selectAdoptSourceSession auto-selected session without worktree metadata, want no candidate")
	}
	if !strings.Contains(err.Error(), "no recent active sessions") {
		t.Fatalf("selectAdoptSourceSession error = %v, want no recent active sessions", err)
	}
}

func TestStateStoreForWorktreeIgnoresGitStderrOnSuccess(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("uses a POSIX shell script fake git")
	}

	fakeBin := t.TempDir()
	fakeGit := filepath.Join(fakeBin, "git")
	script := `#!/bin/sh
printf 'advice: noisy git warning\n' >&2
printf '%s\n%s\n' "$FAKE_WORKTREE_ROOT" "$FAKE_GIT_COMMON_DIR"
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sourceRoot := filepath.Join(t.TempDir(), "source")
	commonDir := filepath.Join(t.TempDir(), "common.git")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_WORKTREE_ROOT", sourceRoot)
	t.Setenv("FAKE_GIT_COMMON_DIR", commonDir)

	_, gotSourceRoot, gotCommonDir, err := stateStoreForWorktree(context.Background(), ".")
	if err != nil {
		t.Fatalf("stateStoreForWorktree failed: %v", err)
	}
	if gotSourceRoot != sourceRoot {
		t.Fatalf("sourceRoot = %q, want %q", gotSourceRoot, sourceRoot)
	}
	if gotCommonDir != filepath.Clean(commonDir) {
		t.Fatalf("commonDir = %q, want %q", gotCommonDir, filepath.Clean(commonDir))
	}
}

func TestStateStoreForWorktreePreservesGitCommonDirSymlink(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("uses a POSIX shell script fake git")
	}

	fakeBin := t.TempDir()
	fakeGit := filepath.Join(fakeBin, "git")
	script := `#!/bin/sh
printf '%s\n%s\n' "$FAKE_WORKTREE_ROOT" "$FAKE_GIT_COMMON_DIR"
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sourceRoot := filepath.Join(t.TempDir(), "source")
	realCommonDir := filepath.Join(t.TempDir(), "real-common.git")
	if err := os.MkdirAll(realCommonDir, 0o750); err != nil {
		t.Fatal(err)
	}
	commonDirLink := filepath.Join(t.TempDir(), "common-link.git")
	if err := os.Symlink(realCommonDir, commonDirLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_WORKTREE_ROOT", sourceRoot)
	t.Setenv("FAKE_GIT_COMMON_DIR", commonDirLink)

	_, _, gotCommonDir, err := stateStoreForWorktree(context.Background(), ".")
	if err != nil {
		t.Fatalf("stateStoreForWorktree failed: %v", err)
	}
	if gotCommonDir != filepath.Clean(commonDirLink) {
		t.Fatalf("commonDir = %q, want git-reported symlink path %q", gotCommonDir, filepath.Clean(commonDirLink))
	}
}

func TestSameAdoptStoreCanonicalizesGitCommonDirSymlinks(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink path canonicalization is POSIX-only in this test")
	}

	realCommonDir := filepath.Join(t.TempDir(), "real-common.git")
	if err := os.MkdirAll(realCommonDir, 0o750); err != nil {
		t.Fatal(err)
	}
	commonDirLink := filepath.Join(t.TempDir(), "common-link.git")
	if err := os.Symlink(realCommonDir, commonDirLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if !sameAdoptStore(commonDirLink, realCommonDir) {
		t.Fatalf("sameAdoptStore(%q, %q) = false, want true", commonDirLink, realCommonDir)
	}
}

func TestSessionAdopt_SameStoreReloadsSourceStateUnderLock(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetWorktree := filepath.Join(t.TempDir(), "target-worktree")
	runAdoptGit(t, sourceRepo, "worktree", "add", targetWorktree, "-b", "target-worktree")
	resolvedTargetWorktree, err := filepath.EvalSymlinks(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktree = resolvedTargetWorktree
	t.Cleanup(func() {
		runAdoptGit(t, sourceRepo, "worktree", "remove", targetWorktree, "--force")
	})

	sourceWorktreeID, err := paths.GetWorktreeID(sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktreeID, err := paths.GetWorktreeID(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "test-adopt-same-store-reload"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		WorktreeID:          sourceWorktreeID,
		LastPrompt:          "stale prompt",
		SessionTurnCount:    1,
	}); err != nil {
		t.Fatal(err)
	}
	staleSelected, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		WorktreeID:          sourceWorktreeID,
		LastPrompt:          "fresh hook prompt",
		SessionTurnCount:    9,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetWorktree, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetWorktree, "feature.txt")
	t.Chdir(targetWorktree)

	adopted, _, err := adoptFromSameSessionStore(context.Background(), sourceStore, sourceRepo, staleSelected, adoptOptions{
		Force: true,
	}, true) // ownership already authorized; this test covers the locked reload
	if err != nil {
		t.Fatalf("adoptFromSameSessionStore failed: %v", err)
	}
	if adopted.LastPrompt != "fresh hook prompt" {
		t.Fatalf("adopted LastPrompt = %q, want fresh hook prompt", adopted.LastPrompt)
	}
	if adopted.SessionTurnCount != 9 {
		t.Fatalf("adopted SessionTurnCount = %d, want fresh source value", adopted.SessionTurnCount)
	}

	loaded, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorktreePath != targetWorktree {
		t.Fatalf("WorktreePath = %q, want %q", loaded.WorktreePath, targetWorktree)
	}
	if loaded.WorktreeID != targetWorktreeID {
		t.Fatalf("WorktreeID = %q, want %q", loaded.WorktreeID, targetWorktreeID)
	}
	if loaded.LastPrompt != "fresh hook prompt" {
		t.Fatalf("loaded LastPrompt = %q, want fresh hook prompt", loaded.LastPrompt)
	}
	if loaded.SessionTurnCount != 9 {
		t.Fatalf("loaded SessionTurnCount = %d, want fresh source value", loaded.SessionTurnCount)
	}
}

func TestSessionAdopt_MovesSameStoreSessionIntoCurrentWorktree(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetWorktree := filepath.Join(t.TempDir(), "target-worktree")
	runAdoptGit(t, sourceRepo, "worktree", "add", targetWorktree, "-b", "target-worktree")
	resolvedTargetWorktree, err := filepath.EvalSymlinks(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktree = resolvedTargetWorktree
	t.Cleanup(func() {
		runAdoptGit(t, sourceRepo, "worktree", "remove", targetWorktree, "--force")
	})

	sourceWorktreeID, err := paths.GetWorktreeID(sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktreeID, err := paths.GetWorktreeID(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "test-adopt-same-store"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:                 sessionID,
		AgentType:                 agent.AgentTypeClaudeCode,
		StartedAt:                 time.Now().Add(-5 * time.Minute),
		LastInteractionTime:       &lastInteraction,
		Phase:                     session.PhaseActive,
		BaseCommit:                testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:              sourceRepo,
		WorktreeID:                sourceWorktreeID,
		StepCount:                 4,
		CheckpointTranscriptStart: 2,
		LastCheckpointID:          id.MustCheckpointID("abc123def456"),
		LastCheckpointCommitHash:  "source-commit",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetWorktree, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetWorktree, "feature.txt")
	t.Chdir(targetWorktree)

	var out bytes.Buffer
	adoptAsCaller(t, sessionID)
	err = runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded without --force, want existing same-store state refusal")
	}
	if !strings.Contains(err.Error(), "already tracked in this repo") {
		t.Fatalf("runAdopt error = %v, want existing-state refusal", err)
	}

	loaded, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorktreePath != sourceRepo {
		t.Fatalf("WorktreePath changed without --force: %q", loaded.WorktreePath)
	}

	err = runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	loaded, err = sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorktreePath != targetWorktree {
		t.Fatalf("WorktreePath = %q, want %q", loaded.WorktreePath, targetWorktree)
	}
	if loaded.WorktreeID != targetWorktreeID {
		t.Fatalf("WorktreeID = %q, want %q", loaded.WorktreeID, targetWorktreeID)
	}
	if loaded.BaseCommit != testutil.GetHeadHash(t, targetWorktree) {
		t.Fatalf("BaseCommit = %q, want target HEAD", loaded.BaseCommit)
	}
	if loaded.StepCount != 0 {
		t.Fatalf("StepCount = %d, want reset target-local checkpoint state", loaded.StepCount)
	}
	if loaded.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want reset target-local transcript window", loaded.CheckpointTranscriptStart)
	}
	if !loaded.LastCheckpointID.IsEmpty() {
		t.Fatalf("LastCheckpointID = %s, want empty target-local checkpoint ID", loaded.LastCheckpointID.String())
	}
	if loaded.LastCheckpointCommitHash != "" {
		t.Fatalf("LastCheckpointCommitHash = %q, want empty target-local commit hash", loaded.LastCheckpointCommitHash)
	}

	commitMsgFile := filepath.Join(targetWorktree, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add same-store feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func setupAdoptRepo(t *testing.T) string {
	t.Helper()

	repoDir := t.TempDir()
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

func claudeAdoptTranscriptPath(t *testing.T, sourceRepo, sessionID string) string {
	t.Helper()

	transcriptDir := filepath.Join(sourceRepo, ".claude", "projects", "adopt-test")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", transcriptDir)
	return filepath.Join(transcriptDir, sessionID+".jsonl")
}

func runAdoptGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	testutil.RunGit(t, dir, args...)
}

// saveAdoptableSession writes a live, adoptable session into a worktree's
// store, with a transcript so validateAdoptSourceTranscript is satisfied and
// the ownership gate is what the test actually exercises.
func saveAdoptableSession(t *testing.T, repo, sessionID string) {
	t.Helper()
	transcriptPath := claudeAdoptTranscriptPath(t, repo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lastInteraction := time.Now().Add(-1 * time.Minute)
	store := session.NewStateStoreWithDir(filepath.Join(repo, ".git", session.SessionStateDirName))
	if err := store.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, repo),
		WorktreePath:        repo,
		TranscriptPath:      transcriptPath,
	}); err != nil {
		t.Fatal(err)
	}
}

// The reported incident, as a test. An agent read a session ID out of
// `entire session current` — which used to answer with a foreign worktree's
// live session — and adopted it, moving a third party's running session and
// resetting its checkpoint bookkeeping. The pre-existing checks all pass here:
// the ID and the worktree agree with each other, and the session is live.
func TestSessionAdopt_RefusesASessionThatIsNotTheCallers(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "someone-elses-session")

	t.Chdir(targetRepo)
	adoptAsCaller(t, "my-own-session")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "someone-elses-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded on a session belonging to another caller")
	}
	if !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("error should name the refusal, got: %v", err)
	}
	// The message must say which session is ours, or the operator cannot tell
	// a mistake from a legitimate cross-session action. Compared through
	// shortSessionID so the assertion tracks the command's own rendering
	// instead of a hardcoded prefix.
	for _, want := range []string{shortSessionID("my-own-session"), shortSessionID("someone-elses-session")} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output should name %q, got: %q", want, out.String())
		}
	}
	if store := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName)); true {
		state, err := store.Load(context.Background(), "someone-elses-session")
		if err != nil {
			t.Fatal(err)
		}
		if state == nil || state.WorktreePath != sourceRepo || state.Phase != session.PhaseActive {
			t.Fatalf("refused adoption still mutated the source session: %+v", state)
		}
	}
}

// --force must not grant this. It already means two other things (replace
// local state; confirm same-store adoption), and it is the flag an agent
// reaches for on any refusal — so waiving a check on someone else's running
// session needs a name that says that.
func TestSessionAdopt_ForceDoesNotWaiveTheOwnershipCheck(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "someone-elses-session")

	t.Chdir(targetRepo)
	adoptAsCaller(t, "my-own-session")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "someone-elses-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("--force waived the ownership check")
	}
	// Assert on the OUTPUT, not the error. The refusal is a SilentError and
	// main.go prints nothing for those, so an error string containing the
	// flag name is invisible to the caller — this assertion used to pass
	// while the remedy never reached any output.
	if !strings.Contains(out.String(), "--allow-foreign-session") {
		t.Fatalf("refusal should print the flag that does grant it, got: %q (err: %v)", out.String(), err)
	}
}

// The escape hatch works, because deliberately adopting another session is a
// real (if rare) operator action — it just has to be stated.
func TestSessionAdopt_AllowForeignSessionOverridesTheCheck(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "someone-elses-session")

	testutil.WriteFile(t, targetRepo, "feature.txt", "change\n")
	t.Chdir(targetRepo)
	adoptAsCaller(t, "my-own-session")

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, "someone-elses-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
		AllowForeign: true,
	}); err != nil {
		t.Fatalf("runAdopt failed with --allow-foreign-session: %v", err)
	}
	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), "someone-elses-session")
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("session was not adopted despite the override")
	}
}

// No caller could be identified at all — no agent variable, no owner of ours
// in the ancestry. That covers every cross-machine --from, where ancestry
// cannot speak to another host. Unverifiable is refused, not waved through:
// a prompt nobody can see must never count as consent.
func TestSessionAdopt_RefusesWhenTheCallerCannotBeIdentified(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "unverifiable-session")

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "unverifiable-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded without being able to identify its caller")
	}
	if !strings.Contains(out.String(), "could not be identified") {
		t.Fatalf("output should say the caller is unknown, got: %q", out.String())
	}
}

// An ambiguous identification does not satisfy IsCaller, so it must not
// satisfy adoption either — being inside *some* agent session is not knowing
// which, and the reported session is explicitly a guess.
func TestSessionAdopt_RefusesOnAnAmbiguousCaller(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "contested-session")

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)
	// Two claims, neither placeable in our ancestry: caller-ambiguous.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "contested-session")
	t.Setenv("CODEX_SESSION_ID", "other-claim")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "contested-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded on an ambiguous caller identification")
	}
	// The reason must reach the output, not just exist: the refusal renders
	// it as "Cannot confirm it is yours: <reason>.", so an empty reason —
	// which is what asking for the verdict and the reason in two separate
	// calls could produce — shows up as a bare full stop.
	if !strings.Contains(out.String(), "none could be ordered") {
		t.Errorf("refusal lost its reason, got: %q", out.String())
	}
	if strings.Contains(out.String(), "Cannot confirm it is yours: .") {
		t.Errorf("refusal rendered an empty reason: %q", out.String())
	}
}

// The command's primary use case for the agents that publish no session ID.
// Gemini CLI and opencode are recognised only by process ancestry, and
// ResolveCallerSession searches the CURRENT repository's store — so in a
// cross-repository adoption the source state, and the owner recorded on it,
// was never in the listing. Without checking that owner directly, such an
// agent adopting its OWN session is refused, and the only way through is
// --allow-foreign-session: teaching agents to waive a real check to do a
// legitimate thing.
func TestSessionAdopt_CrossRepoOwnerAncestryIdentifiesAnAgentWithoutAnEnvVar(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes; ancestry cannot identify a caller here by design")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible; nothing to match against")
	}
	owner := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "gemini-owned-session"
	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		TranscriptPath:      transcriptPath,
		// The signal a no-env-var agent leaves: its own process recorded as
		// the session owner, which really is an ancestor of this test.
		Owner: &owner,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	// No agent variable at all — Gemini CLI and opencode publish none.
	clearCallerSessionEnv(t)

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	}); err != nil {
		t.Fatalf("runAdopt refused an agent adopting its own cross-repo session: %v\noutput: %s", err, out.String())
	}
	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("session was not adopted")
	}
	if strings.Contains(out.String(), "Cannot confirm it is yours") {
		t.Errorf("adoption of the caller's own session should be silent, got: %q", out.String())
	}
}

// Two no-env-var agents nested, both sessions in the SOURCE store, command run
// from a different repository. The resolver sees neither state, so nothing
// contradicts either — and asking only whether the selected session's owner
// appears somewhere in our ancestry says yes for the OUTER agent, whose
// process really did spawn us several hops up. Naming it would adopt the outer
// session silently while the inner agent is the actual caller.
func TestSessionAdopt_RefusesTheOuterSessionWhenANearerOwnerExists(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) < 2 {
		t.Skip("need two ancestors to model a nested pair")
	}
	inner, outer := chain[0], chain[1]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))

	save := func(sessionID string, owner proclive.Identity) {
		transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
		if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		lastInteraction := time.Now().Add(-1 * time.Minute)
		ownerCopy := owner
		if err := sourceStore.Save(context.Background(), &session.State{
			SessionID:           sessionID,
			AgentType:           agent.AgentTypeClaudeCode,
			StartedAt:           time.Now().Add(-5 * time.Minute),
			LastInteractionTime: &lastInteraction,
			Phase:               session.PhaseActive,
			BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
			WorktreePath:        sourceRepo,
			TranscriptPath:      transcriptPath,
			Owner:               &ownerCopy,
		}); err != nil {
			t.Fatal(err)
		}
	}
	save("outer-agent-session", outer)
	save("inner-agent-session", inner)

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "outer-agent-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("adopted the outer session while a nearer owner was recorded")
	}
	state, err := sourceStore.Load(context.Background(), "outer-agent-session")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.WorktreePath != sourceRepo {
		t.Fatalf("refused adoption mutated the source session: %+v", state)
	}
}

// The other half of the same comparison: the NEAREST of the nested pair is the
// caller's own session and must still adopt silently, or the fix would have
// blocked the case the command exists for.
func TestSessionAdopt_AdoptsTheNearestNestedSessionSilently(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) < 2 {
		t.Skip("need two ancestors to model a nested pair")
	}
	inner, outer := chain[0], chain[1]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))

	save := func(sessionID string, owner proclive.Identity) {
		transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
		if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		lastInteraction := time.Now().Add(-1 * time.Minute)
		ownerCopy := owner
		if err := sourceStore.Save(context.Background(), &session.State{
			SessionID:           sessionID,
			AgentType:           agent.AgentTypeClaudeCode,
			StartedAt:           time.Now().Add(-5 * time.Minute),
			LastInteractionTime: &lastInteraction,
			Phase:               session.PhaseActive,
			BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
			WorktreePath:        sourceRepo,
			TranscriptPath:      transcriptPath,
			Owner:               &ownerCopy,
		}); err != nil {
			t.Fatal(err)
		}
	}
	save("outer-agent-session", outer)
	save("inner-agent-session", inner)

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, "inner-agent-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	}); err != nil {
		t.Fatalf("refused the nearest nested session: %v\noutput: %s", err, out.String())
	}
	if strings.Contains(out.String(), "Cannot confirm it is yours") {
		t.Errorf("adoption of the caller's own session should be silent, got: %q", out.String())
	}
}

// saveOwnedSession writes an adoptable session owned by a specific process,
// with a chosen last-interaction time, so a test can put two sessions at the
// SAME ancestry depth and differ only in recency.
func saveOwnedSession(t *testing.T, repo, sessionID string, owner proclive.Identity, lastInteraction time.Time) {
	t.Helper()
	transcriptPath := claudeAdoptTranscriptPath(t, repo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownerCopy := owner
	store := session.NewStateStoreWithDir(filepath.Join(repo, ".git", session.SessionStateDirName))
	if err := store.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           lastInteraction.Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, repo),
		WorktreePath:        repo,
		TranscriptPath:      transcriptPath,
		Owner:               &ownerCopy,
	}); err != nil {
		t.Fatal(err)
	}
}

// One long-lived agent process can own several sessions — a stale one and the
// resumed one it is now running — and they sit at the SAME ancestry depth.
// Rejecting only a STRICTLY nearer owner accepts the stale sibling silently,
// which disagrees with the resolver about a case it had already decided: at
// equal depth the rule is recency, not "either will do".
func TestSessionAdopt_RefusesAStaleSiblingAtTheSameOwnerDepth(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible")
	}
	owner := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	now := time.Now()
	// Same owner process, so identical ancestry depth; only recency differs.
	saveOwnedSession(t, sourceRepo, "stale-session", owner, now.Add(-2*time.Hour))
	saveOwnedSession(t, sourceRepo, "resumed-session", owner, now.Add(-1*time.Minute))

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "stale-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("adopted a stale session while a more recent one shares its owner depth")
	}
	store := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	state, loadErr := store.Load(context.Background(), "stale-session")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state == nil || state.WorktreePath != sourceRepo {
		t.Fatalf("refused adoption mutated the source session: %+v", state)
	}
}

// The other direction: the most recently interacting of the equal-depth pair
// IS the caller's session and must still adopt silently, or the tie-break
// would have clamped the feature shut instead of aiming it.
func TestSessionAdopt_AdoptsTheMostRecentSiblingAtTheSameOwnerDepth(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible")
	}
	owner := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	now := time.Now()
	saveOwnedSession(t, sourceRepo, "stale-session", owner, now.Add(-2*time.Hour))
	saveOwnedSession(t, sourceRepo, "resumed-session", owner, now.Add(-1*time.Minute))

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, "resumed-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	}); err != nil {
		t.Fatalf("refused the most recent session at the caller's owner depth: %v\noutput: %s", err, out.String())
	}
	if strings.Contains(out.String(), "Cannot confirm it is yours") {
		t.Errorf("adoption of the caller's own session should be silent, got: %q", out.String())
	}
}

// addAdoptLinkedWorktree adds a linked worktree of repo and returns its
// symlink-resolved path. Linked worktrees share one session store, which is
// what puts adoption on the adoptFromSameSessionStore path — a second repo
// would take the external-store path instead, where MutateSessionState
// resolves its store from the cwd and cannot see the source session at all.
func addAdoptLinkedWorktree(t *testing.T, repo, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	runAdoptGit(t, repo, "worktree", "add", path, "-b", name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runAdoptGit(t, repo, "worktree", "remove", resolved, "--force")
	})
	return resolved
}

// The ownership decision is taken on an UNLOCKED snapshot, so it has to be
// retaken under the lock — both mutation paths already reload and re-check
// every other precondition for exactly this reason. Ownership is not stable
// across that window: a turn start re-records SessionState.Owner, and a
// session created in the source store meanwhile can be a nearer owner.
//
// Simulated with the seam the neighbouring lock tests use: authorize against
// one state, then replace it before the mutation reloads.
func TestSessionAdopt_RevalidatesOwnershipAgainstTheLockedState(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible")
	}
	owner := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetWorktree := addAdoptLinkedWorktree(t, sourceRepo, "revalidate-target")
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))

	const sessionID = "ownership-revalidated-session"
	// Authorized against this: owner is our nearest ancestor.
	saveOwnedSession(t, sourceRepo, sessionID, owner, time.Now().Add(-1*time.Minute))
	authorized, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// Meanwhile a turn start re-records the owner as a process that is not in
	// our ancestry, so the evidence the adoption rested on is gone.
	elsewhere := owner
	elsewhere.PID = 999999
	elsewhere.Start = "not-our-process"
	saveOwnedSession(t, sourceRepo, sessionID, elsewhere, time.Now())

	testutil.WriteFile(t, targetWorktree, "feature.txt", "agent change\n")
	t.Chdir(targetWorktree)
	clearCallerSessionEnv(t)

	_, _, err = adoptFromSameSessionStore(context.Background(), sourceStore, sourceRepo, authorized,
		adoptOptions{Force: true}, false)
	if err == nil {
		t.Fatal("adopted on ownership evidence that no longer held under the lock")
	}
	if !strings.Contains(err.Error(), "could no longer be confirmed") {
		t.Fatalf("error should name the stale authorization, got: %v", err)
	}

	// And the source session is untouched — still where it was, still active.
	state, loadErr := sourceStore.Load(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state == nil || state.WorktreePath != sourceRepo {
		t.Fatalf("failed revalidation still mutated the source session: %+v", state)
	}
}

// An override is honoured without re-checking: the flag, or a human who
// already answered, decided about THIS session, and re-prompting mid-mutation
// would ask the same person the same question twice.
func TestSessionAdopt_OverriddenOwnershipIsNotRevalidated(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible")
	}
	owner := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetWorktree := addAdoptLinkedWorktree(t, sourceRepo, "override-target")
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))

	const sessionID = "ownership-overridden-session"
	saveOwnedSession(t, sourceRepo, sessionID, owner, time.Now().Add(-1*time.Minute))
	authorized, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Same disappearing evidence as above.
	elsewhere := owner
	elsewhere.PID = 999999
	elsewhere.Start = "not-our-process"
	saveOwnedSession(t, sourceRepo, sessionID, elsewhere, time.Now())

	testutil.WriteFile(t, targetWorktree, "feature.txt", "agent change\n")
	t.Chdir(targetWorktree)
	clearCallerSessionEnv(t)

	if _, _, err := adoptFromSameSessionStore(context.Background(), sourceStore, sourceRepo, authorized,
		adoptOptions{Force: true}, true); err != nil {
		t.Fatalf("an overridden adoption was re-checked and refused: %v", err)
	}
}

// The combined-candidate semantics, pinned as policy rather than as artifacts
// of how ownership happens to be computed.
//
// Adoption asks the shared resolver one question over every candidate in both
// repositories — "which session is running this command?" — and authorizes
// only when the answer is the session being adopted. It applies no rule of its
// own, so these tests are assertions about strategy's policy as adopt
// experiences it, and each names the policy it depends on.

// (1) An inherited environment claim does not outrank a nearer source owner.
// The outer agent publishes its ID, the inner agent forwards it, and both
// sessions live in the source store: the inner owner is nearer, so the inner
// session is the caller and adopting the outer one is refused.
func TestSessionAdopt_NearerSourceOwnerBeatsAnInheritedEnvClaim(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) < 2 {
		t.Skip("need two ancestors to model a nested pair")
	}
	inner, outer := chain[0], chain[1]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	now := time.Now()
	saveOwnedSession(t, sourceRepo, "outer-session", outer, now)
	saveOwnedSession(t, sourceRepo, "inner-session", inner, now)

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)
	// The outer agent's variable, inherited by the inner agent that publishes
	// none of its own.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-session")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "outer-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("an inherited env claim authorized the outer session over a nearer source owner")
	}
	if !strings.Contains(out.String(), shortSessionID("inner-session")) {
		t.Errorf("refusal should name the session actually identified, got: %q", out.String())
	}
}

// (2) The depth-0 exemption, explicitly. An unplaceable claim names X, while
// source session Y's owner is our immediate parent — nothing can be nearer
// than that, so Y is the caller and adopting it is authorized. This is shared
// resolver policy (claimsRuledOut), not an adopt decision.
func TestSessionAdopt_DepthZeroSourceOwnerWinsOverAnUnplaceableClaim(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible")
	}
	parent := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveOwnedSession(t, sourceRepo, "parent-owned-session", parent, time.Now())

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)
	// Named by the environment but tracked nowhere, so it cannot be placed.
	t.Setenv("CODEX_SESSION_ID", "claim-with-no-state")

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, "parent-owned-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	}); err != nil {
		t.Fatalf("depth-0 source owner was not accepted as the caller: %v\noutput: %s", err, out.String())
	}
	if strings.Contains(out.String(), "Cannot confirm it is yours") {
		t.Errorf("adoption of the identified caller should be silent, got: %q", out.String())
	}
}

// (3) The same shape one hop further out is ambiguous. With the winner deeper
// than our immediate parent, an unplaceable claim could be nearer, so nothing
// is identified and adoption refuses — including of the deeper session.
func TestSessionAdopt_UnplaceableClaimMakesADeeperSourceOwnerAmbiguous(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) < 2 {
		t.Skip("need an ancestor beyond our parent")
	}
	distant := chain[1]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveOwnedSession(t, sourceRepo, "distant-owned-session", distant, time.Now())

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)
	t.Setenv("CODEX_SESSION_ID", "claim-with-no-state")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "distant-owned-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("adopted a session an unplaceable claim could still be nearer than")
	}
	if !strings.Contains(out.String(), "none could be ordered") {
		t.Errorf("refusal should report the ambiguity, got: %q", out.String())
	}
}

// (4) A different session identified in the combined set refuses the selected
// one, with no ancestry involved: the environment names a tracked source
// session, so that one is the caller and any other is not.
func TestSessionAdopt_IdentifiedDifferentWinnerRefusesTheSelectedSession(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	now := time.Now()
	saveAdoptableSessionAt(t, sourceRepo, "the-callers-session", now)
	saveAdoptableSessionAt(t, sourceRepo, "some-other-session", now)

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)
	t.Setenv("PI_SESSION_ID", "the-callers-session")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "some-other-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("adopted a session other than the identified caller")
	}
	for _, want := range []string{shortSessionID("the-callers-session"), shortSessionID("some-other-session")} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal should name both sessions, missing %q in: %q", want, out.String())
		}
	}
}

// (5) Equal depth is broken by recency, which is
// TestSessionAdopt_RefusesAStaleSiblingAtTheSameOwnerDepth and its
// AdoptsTheMostRecent counterpart above — one owner process, two sessions,
// identical depth. Kept there rather than duplicated here.

// saveAdoptableSessionAt writes a live, ownerless adoptable session with a
// chosen last-interaction time.
func saveAdoptableSessionAt(t *testing.T, repo, sessionID string, lastInteraction time.Time) {
	t.Helper()
	transcriptPath := claudeAdoptTranscriptPath(t, repo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := session.NewStateStoreWithDir(filepath.Join(repo, ".git", session.SessionStateDirName))
	if err := store.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           lastInteraction.Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, repo),
		WorktreePath:        repo,
		TranscriptPath:      transcriptPath,
	}); err != nil {
		t.Fatal(err)
	}
}

// saveBareSessionState writes a session state with NO transcript path and
// without touching ENTIRE_TEST_CLAUDE_PROJECT_DIR.
//
// Both matter for a colliding-ID fixture. claudeAdoptTranscriptPath sets that
// override, so calling it for a second repository repoints it and the
// SELECTED source state's transcript then fails validation for a reason
// unrelated to the test. And an empty transcript path short-circuits
// validateAdoptSourceTranscript, which is correct here: the leftover copy is
// evidence about ownership, not a transcript under test.
func saveBareSessionState(t *testing.T, repo, sessionID string, owner *proclive.Identity, lastInteraction time.Time) {
	t.Helper()
	store := session.NewStateStoreWithDir(filepath.Join(repo, ".git", session.SessionStateDirName))
	state := &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           lastInteraction.Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, repo),
		WorktreePath:        repo,
	}
	if owner != nil {
		ownerCopy := *owner
		state.Owner = &ownerCopy
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
}

// Colliding session IDs across the two stores are a SUPPORTED condition —
// --force exists to replace a state the target already holds for the session
// being adopted — so which copy supplies the ownership evidence matters.
//
// (a) The live source copy is owned by this caller while the target's leftover
// is ownerless. Preferring the target's copy left no candidate at all and
// refused a legitimate adoption.
func TestSessionAdopt_SourceCopyWinsOverAStaleTargetCopyWhenIdentifying(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible")
	}
	owner := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	const sessionID = "collision-owned-in-source"
	// Live: owned by us, in the repository we are adopting from.
	saveOwnedSession(t, sourceRepo, sessionID, owner, time.Now())
	// Leftover from a previous adoption: same ID, no owner recorded. This is
	// the state --force replaces.
	saveBareSessionState(t, targetRepo, sessionID, nil, time.Now().Add(-2*time.Hour))

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)
	clearCallerSessionEnv(t) // env-less agent: ancestry is the only signal

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	}); err != nil {
		t.Fatalf("a stale target copy shadowed the caller-owned source state: %v\noutput: %s", err, out.String())
	}
	if strings.Contains(out.String(), "Cannot confirm it is yours") {
		t.Errorf("adoption of the caller's own session should be silent, got: %q", out.String())
	}
}

// (b) The other direction: the target's leftover still carries an owner from a
// previous adoption while the live source copy has none. Authorizing on the
// leftover would rest on evidence about a state nobody is adopting, so this
// must refuse.
func TestSessionAdopt_StaleTargetOwnerDoesNotAuthorizeAnUnownedSourceCopy(t *testing.T) {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		t.Skip("platform cannot introspect processes")
	}
	chain := ancestry.Chain()
	if len(chain) == 0 {
		t.Skip("no ancestors visible")
	}
	owner := chain[0]

	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	const sessionID = "collision-owned-in-target"
	// Live copy: no owner recorded, so nothing places it in our ancestry.
	saveAdoptableSessionAt(t, sourceRepo, sessionID, time.Now())
	// Leftover carrying an owner that IS our ancestor — stale evidence.
	saveBareSessionState(t, targetRepo, sessionID, &owner, time.Now().Add(-2*time.Hour))

	t.Chdir(targetRepo)
	clearCallerSessionEnv(t)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatalf("authorized on a stale target copy's owner\noutput: %s", out.String())
	}
	if !strings.Contains(out.String(), "could not be identified") {
		t.Errorf("refusal should report that nothing identified the caller, got: %q", out.String())
	}
}

// plantUnusableSessionState puts a state file in repo's store that cannot be
// loaded, so the store lists SUCCESSFULLY with one candidate missing.
//
// Unparseable rather than unreadable because it is portable and reaches the
// identical skip: both are a Load error, and a mode-000 file cannot be staged
// on every platform (nor by root, which CI sometimes is).
func plantUnusableSessionState(t *testing.T, repo, sessionID string) {
	t.Helper()
	stateDir := filepath.Join(repo, ".git", session.SessionStateDirName)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionID+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The hole this closes: a session store that cannot be read COMPLETELY leaves
// the ownership guard ranking the caller against a set the nearer owner has
// silently dropped out of, and a partial set is exactly where an environment
// claim wins uncontested. So the guard refuses even though the resolver did
// identify a caller, and identified the session being adopted.
//
// Measured before the fix: one state file at mode 000 in the target store had
// this adoption succeed, silently.
func TestSessionAdopt_RefusesWhenTheTargetStoreCannotBeReadCompletely(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "the-callers-session")

	t.Chdir(targetRepo)
	// The environment names the session being adopted, so every other branch
	// of the guard authorizes. Only incompleteness stands in the way.
	adoptAsCaller(t, "the-callers-session")
	plantUnusableSessionState(t, targetRepo, "unreadable-rival")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "the-callers-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt authorized against a candidate set it could not read completely")
	}
	if !strings.Contains(out.String(), "could not be read") {
		t.Fatalf("refusal should say the store could not be read, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "--allow-foreign-session") {
		t.Fatalf("refusal should name the flag that does grant it, got: %q", out.String())
	}

	store := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	state, loadErr := store.Load(context.Background(), "the-callers-session")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state == nil || state.WorktreePath != sourceRepo {
		t.Fatalf("refused adoption still moved the source session: %+v", state)
	}
}

// Same rule on the other side. The source listing is where the session being
// adopted is ranked against its own repository's sessions, so a candidate lost
// there hides a nearer owner just as effectively.
func TestSessionAdopt_RefusesWhenTheSourceStoreCannotBeReadCompletely(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "the-callers-session")
	plantUnusableSessionState(t, sourceRepo, "unreadable-rival")

	t.Chdir(targetRepo)
	adoptAsCaller(t, "the-callers-session")

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, "the-callers-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt authorized against a source listing it could not read completely")
	}
	if !strings.Contains(out.String(), "source repository") {
		t.Fatalf("refusal should name the source repository as the unreadable one, got: %q", out.String())
	}
}

// Refusing is not walling the user out: the store is broken and the operator
// may well know it, so the same override that covers every other unprovable
// adoption covers this one. Without this the fix would strand a repo with one
// stale corrupt file, with no way past it.
func TestSessionAdopt_AllowForeignSessionOverridesAnIncompleteStore(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)
	saveAdoptableSession(t, sourceRepo, "the-callers-session")

	t.Chdir(targetRepo)
	adoptAsCaller(t, "the-callers-session")
	plantUnusableSessionState(t, targetRepo, "unreadable-rival")

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, "the-callers-session", adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
		AllowForeign: true,
	}); err != nil {
		t.Fatalf("--allow-foreign-session did not cover an incomplete store: %v\nOutput: %q", err, out.String())
	}

	store := session.NewStateStoreWithDir(filepath.Join(targetRepo, ".git", session.SessionStateDirName))
	state, err := store.Load(context.Background(), "the-callers-session")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.WorktreePath != targetRepo {
		t.Fatalf("session was not adopted into the target worktree: %+v", state)
	}
}
