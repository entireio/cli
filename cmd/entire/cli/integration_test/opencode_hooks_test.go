//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestOpenCodeHookFlow verifies the full hook flow for OpenCode:
// session-start → turn-start → file changes → turn-end → checkpoint → commit → condense → session-end.
func TestOpenCodeHookFlow(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)

	// Create OpenCode session
	session := env.NewOpenCodeSession()

	// 1. session-start
	if err := env.SimulateOpenCodeSessionStart(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("session-start error: %v", err)
	}

	// 2. turn-start (equivalent to UserPromptSubmit — captures pre-prompt state)
	if err := env.SimulateOpenCodeTurnStart(session.ID, session.TranscriptPath, "Add a feature"); err != nil {
		t.Fatalf("turn-start error: %v", err)
	}

	// 3. Agent makes file changes (AFTER turn-start so they're detected as new)
	env.WriteFile("feature.go", "package main\n// new feature")

	// 4. Create transcript with the file change
	session.CreateOpenCodeTranscript("Add a feature", []FileChange{
		{Path: "feature.go", Content: "package main\n// new feature"},
	})

	// 5. turn-end (equivalent to Stop — creates checkpoint)
	if err := env.SimulateOpenCodeTurnEnd(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("turn-end error: %v", err)
	}

	// 6. Verify checkpoint was created on the shadow branch
	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after turn-end", shadowBranch)
	}
	if !env.FileExistsInBranch(shadowBranch, "feature.go") {
		t.Fatal("feature.go should exist on shadow branch after turn-end")
	}

	// 7. For manual-commit, user commits manually (triggers condensation).
	env.GitCommitWithShadowHooks("Add feature", "feature.go")

	// 8. session-end
	if err := env.SimulateOpenCodeSessionEnd(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("session-end error: %v", err)
	}

	// 9. Verify condensation happened (checkpoint on metadata branch)
	checkpointID := env.TryGetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("expected checkpoint on metadata branch after commit")
	}

	// 10. Verify condensed data
	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	_, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Error("condensed transcript should exist on metadata branch")
	}
}

// TestOpenCodeAgentStrategyComposition verifies that the OpenCode agent and strategy
// work together correctly — agent parses session, strategy saves checkpoint on the shadow branch.
func TestOpenCodeAgentStrategyComposition(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)

	ag, err := agent.Get("opencode")
	if err != nil {
		t.Fatalf("Get(opencode) error = %v", err)
	}

	// Create session and transcript for agent interface testing.
	// The transcript references feature.go but the actual file doesn't need
	// to exist for ReadSession — it only parses the transcript JSONL.
	session := env.NewOpenCodeSession()
	transcriptPath := session.CreateOpenCodeTranscript("Add a feature", []FileChange{
		{Path: "feature.go", Content: "package main\n// new feature"},
	})

	// Read session via agent interface
	agentSession, err := ag.ReadSession(&agent.HookInput{
		SessionID:  session.ID,
		SessionRef: transcriptPath,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	// Verify agent computed modified files
	if len(agentSession.ModifiedFiles) == 0 {
		t.Error("agent.ReadSession() should compute ModifiedFiles")
	}

	// Simulate session flow: session-start → turn-start → file changes → turn-end
	if err := env.SimulateOpenCodeSessionStart(session.ID, transcriptPath); err != nil {
		t.Fatalf("session-start error = %v", err)
	}
	if err := env.SimulateOpenCodeTurnStart(session.ID, transcriptPath, "Add a feature"); err != nil {
		t.Fatalf("turn-start error = %v", err)
	}

	// Create the actual file AFTER turn-start so the strategy detects it as new
	env.WriteFile("feature.go", "package main\n// new feature")

	if err := env.SimulateOpenCodeTurnEnd(session.ID, transcriptPath); err != nil {
		t.Fatalf("turn-end error = %v", err)
	}

	// Verify checkpoint was created on the shadow branch
	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after turn-end", shadowBranch)
	}
	if !env.FileExistsInBranch(shadowBranch, "feature.go") {
		t.Fatal("feature.go should exist on shadow branch after turn-end")
	}
}

// TestOpenCodeMultiTurnCondensation verifies that multiple turns in a session
// are correctly condensed when the user commits.
func TestOpenCodeMultiTurnCondensation(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)

	session := env.NewOpenCodeSession()
	transcriptPath := session.TranscriptPath

	// session-start
	if err := env.SimulateOpenCodeSessionStart(session.ID, transcriptPath); err != nil {
		t.Fatalf("session-start error: %v", err)
	}

	// Turn 1: create file (AFTER turn-start so it's detected as new)
	if err := env.SimulateOpenCodeTurnStart(session.ID, transcriptPath, "Create app.go"); err != nil {
		t.Fatalf("turn-start error: %v", err)
	}

	env.WriteFile("app.go", "package main\nfunc main() {}")
	session.CreateOpenCodeTranscript("Create app.go", []FileChange{
		{Path: "app.go", Content: "package main\nfunc main() {}"},
	})

	if err := env.SimulateOpenCodeTurnEnd(session.ID, transcriptPath); err != nil {
		t.Fatalf("turn-end error: %v", err)
	}

	// Verify checkpoint was created on the shadow branch
	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after first turn", shadowBranch)
	}
	if !env.FileExistsInBranch(shadowBranch, "app.go") {
		t.Fatal("app.go should exist on shadow branch after first turn")
	}

	// Commit with hooks (triggers condensation)
	env.GitCommitWithShadowHooks("Implement app", "app.go")

	// session-end
	if err := env.SimulateOpenCodeSessionEnd(session.ID, transcriptPath); err != nil {
		t.Fatalf("session-end error: %v", err)
	}

	// Verify checkpoint was condensed to metadata branch
	checkpointID := env.TryGetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("expected checkpoint on metadata branch after commit")
	}

	// Verify files are on metadata branch
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID: checkpointID,
		Strategy:     strategy.StrategyNameManualCommit,
		FilesTouched: []string{"app.go"},
		ExpectedTranscriptContent: []string{
			"Create app.go", // User prompt should appear in transcript
		},
	})
}

// TestOpenCodeMidTurnCommit verifies that when OpenCode's agent commits mid-turn
// (before turn-end), the commit gets an Entire-Checkpoint trailer AND the checkpoint
// data is written to entire/checkpoints/v1.
//
// This tests the PrepareTranscript fix: OpenCode's transcript file is created lazily
// at turn-end via `opencode export`. When a commit happens mid-turn, PrepareTranscript
// is called to create the transcript on-demand so condensation can read it.
func TestOpenCodeMidTurnCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)

	session := env.NewOpenCodeSession()

	// 1. session-start
	if err := env.SimulateOpenCodeSessionStart(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("session-start error: %v", err)
	}

	// 2. turn-start (session becomes ACTIVE)
	if err := env.SimulateOpenCodeTurnStart(session.ID, session.TranscriptPath, "Add a script and commit it"); err != nil {
		t.Fatalf("turn-start error: %v", err)
	}

	// 3. Agent creates file
	env.WriteFile("script.sh", "#!/bin/bash\necho hello")

	// 4. Create transcript reflecting the file change.
	session.CreateOpenCodeTranscript("Add a script and commit it", []FileChange{
		{Path: "script.sh", Content: "#!/bin/bash\necho hello"},
	})

	// 5. Copy transcript to .entire/tmp/ where PrepareTranscript will find it.
	// In production, `opencode export` refreshes this file on each call.
	// In tests, ENTIRE_TEST_OPENCODE_MOCK_EXPORT makes fetchAndCacheExport
	// read from the pre-written file at .entire/tmp/<sessionID>.json.
	// PrepareTranscript ALWAYS calls fetchAndCacheExport (even if file exists)
	// to ensure fresh data for resumed sessions.
	env.CopyTranscriptToEntireTmp(session.ID, session.TranscriptPath)

	// 6. Agent commits mid-turn (no turn-end yet!)
	// This triggers: PrepareCommitMsg (adds trailer) → PostCommit (runs condensation)
	// Condensation needs the transcript, which PrepareTranscript should provide.
	env.GitCommitWithShadowHooksAsAgent("Add script", "script.sh")

	// 7. Verify commit has checkpoint trailer
	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Fatal("mid-turn agent commit should have Entire-Checkpoint trailer")
	}
	t.Logf("Mid-turn commit has checkpoint ID: %s", checkpointID)

	// 8. CRITICAL: Verify checkpoint data was written to entire/checkpoints/v1
	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	_, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Error("checkpoint transcript should exist on metadata branch after mid-turn commit")
	}

	// 9. Validate checkpoint metadata
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID: checkpointID,
		Strategy:     strategy.StrategyNameManualCommit,
		FilesTouched: []string{"script.sh"},
	})
}

// TestOpenCodeResumedSessionAfterCommit verifies that resuming an OpenCode session
// after a commit correctly creates a checkpoint for the second turn.
//
// Scenario:
//  1. Turn 1: create new file → checkpoint → user commits (condensation)
//  2. Turn 2 (resumed): modify the now-tracked file → checkpoint should be created
func TestOpenCodeResumedSessionAfterCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)

	session := env.NewOpenCodeSession()
	transcriptPath := session.TranscriptPath

	// === Turn 1: Create a new file ===
	if err := env.SimulateOpenCodeSessionStart(session.ID, transcriptPath); err != nil {
		t.Fatalf("session-start error: %v", err)
	}
	if err := env.SimulateOpenCodeTurnStart(session.ID, transcriptPath, "Create app.go"); err != nil {
		t.Fatalf("turn-start 1 error: %v", err)
	}

	env.WriteFile("app.go", "package main\nfunc main() {}")
	session.CreateOpenCodeTranscript("Create app.go", []FileChange{
		{Path: "app.go", Content: "package main\nfunc main() {}"},
	})

	if err := env.SimulateOpenCodeTurnEnd(session.ID, transcriptPath); err != nil {
		t.Fatalf("turn-end 1 error: %v", err)
	}

	shadowBranch1 := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch1) {
		t.Fatalf("shadow branch %s should exist after turn 1", shadowBranch1)
	}

	// === User commits (triggers condensation) ===
	env.GitCommitWithShadowHooks("Create app", "app.go")

	// Verify condensation happened
	checkpointID := env.TryGetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("expected checkpoint on metadata branch after commit")
	}

	// === Turn 2 (resumed): Modify the now-tracked file ===
	if err := env.SimulateOpenCodeTurnStart(session.ID, transcriptPath, "Add color output"); err != nil {
		t.Fatalf("turn-start 2 error: %v", err)
	}

	env.WriteFile("app.go", "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello\") }")
	session.CreateOpenCodeTranscript("Add color output", []FileChange{
		{Path: "app.go", Content: "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello\") }"},
	})

	if err := env.SimulateOpenCodeTurnEnd(session.ID, transcriptPath); err != nil {
		t.Fatalf("turn-end 2 error: %v", err)
	}

	// === Verify: a new checkpoint was created for turn 2 on the new shadow branch ===
	shadowBranch2 := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch2) {
		t.Fatalf("shadow branch %s should exist after turn 2 (resumed session)", shadowBranch2)
	}
	content, found := env.ReadFileFromBranch(shadowBranch2, "app.go")
	if !found {
		t.Fatal("app.go should exist on shadow branch after turn 2 (resumed session)")
	}
	if content != "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello\") }" {
		t.Errorf("app.go on shadow branch = %q, want turn 2 content", content)
	}

	// For manual-commit: commit turn 2 and verify second condensation
	env.GitCommitWithShadowHooks("Add color output", "app.go")

	checkpointID2 := env.TryGetLatestCheckpointID()
	if checkpointID2 == "" {
		t.Fatal("expected second checkpoint on metadata branch after turn 2 commit")
	}
	if checkpointID2 == checkpointID {
		t.Error("second checkpoint ID should differ from first")
	}
}
