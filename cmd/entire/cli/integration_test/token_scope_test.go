//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// appendUsageMessage appends an assistant message carrying token usage, the
// shape Claude Code writes, so the transcript's token total is known.
func appendUsageMessage(s *Session, messageID string, tokens int) {
	s.TranscriptBuilder.messages = append(s.TranscriptBuilder.messages, map[string]interface{}{
		"uuid": "usage-" + messageID,
		"type": roleAssistant,
		"message": map[string]interface{}{
			"id":      messageID,
			"model":   "claude-test",
			"content": []map[string]interface{}{{"type": blockTypeText, "text": "working"}},
			"usage":   map[string]interface{}{"input_tokens": tokens, "output_tokens": tokens},
		},
	})
}

// TestTokenScope_PartialCommitDoesNotRecountEarlierTurns: a partial commit
// carries the remaining files forward and restarts the next checkpoint's
// transcript window at line 0 so it shows the conversation behind them. Its
// tokens must still count only the turns no earlier checkpoint has counted;
// the server sums per-checkpoint tokens into the session total.
func TestTokenScope_PartialCommitDoesNotRecountEarlierTurns(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	s := env.NewSession()

	// Turn 1 (100 tokens) writes a and b; only a is committed.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("a.txt", "a")
	env.WriteFile("b.txt", "b")
	appendUsageMessage(s, "msg-1", 100)
	s.CreateTranscript("make a and b", []FileChange{{Path: "a.txt", Content: "a"}, {Path: "b.txt", Content: "b"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("a only", "a.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)

	// Turn 2 (7 tokens) writes c; b and c are committed.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("c.txt", "c")
	appendUsageMessage(s, "msg-2", 7)
	s.CreateTranscript("make c", []FileChange{{Path: "c.txt", Content: "c"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("b and c", "b.txt", "c.txt")
	cp2 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp2)
	require.NotEqual(t, cp1, cp2)

	// Turn 3 (3 tokens) writes d and e; only d is committed, then e.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("d.txt", "d")
	env.WriteFile("e.txt", "e")
	appendUsageMessage(s, "msg-3", 3)
	s.CreateTranscript("make d and e", []FileChange{{Path: "d.txt", Content: "d"}, {Path: "e.txt", Content: "e"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("d only", "d.txt")
	cp3 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp3)

	u1 := readCommittedTokenUsage(t, env, cp1)
	u2 := readCommittedTokenUsage(t, env, cp2)
	u3 := readCommittedTokenUsage(t, env, cp3)
	require.NotNil(t, u1)
	require.NotNil(t, u2)
	require.NotNil(t, u3)
	require.Equal(t, 100, u1.OutputTokens, "checkpoint 1 counts turn 1")
	require.Equal(t, 7, u2.OutputTokens, "checkpoint 2 counts turn 2 only, not turn 1 again")
	require.Equal(t, 3, u3.OutputTokens, "checkpoint 3 counts turn 3 only")
	require.Equal(t, 7, u2.InputTokens)

	// The carried-forward checkpoint still shows the conversation behind b.
	transcript, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(cp2, paths.TranscriptFileName))
	require.True(t, found, "checkpoint 2 transcript should exist")
	require.Contains(t, transcript, "make a and b")
	metaJSON, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(cp2))
	require.True(t, found)
	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(metaJSON), &meta))
	require.Equal(t, 0, meta.CheckpointTranscriptStart,
		"carry-forward keeps the displayed window at the start of the session")
}

// TestTokenScope_AttachCountsOnlyUncheckpointedTokens: attaching a session
// whose earlier turns are already in a checkpoint stores only the later
// turns' tokens, and the session's next hook checkpoint doesn't count the
// attached turns again.
func TestTokenScope_AttachCountsOnlyUncheckpointedTokens(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	s := env.NewSession()

	// Turn 1 (100 tokens) writes a and b; only a is committed. Carry-forward
	// clears LastCheckpointID, so attach will write a new checkpoint.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("a.txt", "a")
	env.WriteFile("b.txt", "b")
	appendUsageMessage(s, "msg-1", 100)
	s.CreateTranscript("make a and b", []FileChange{{Path: "a.txt", Content: "a"}, {Path: "b.txt", Content: "b"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("a only", "a.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)

	// Turn 2 (7 tokens) changes no files.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	appendUsageMessage(s, "msg-2", 7)
	s.CreateTranscript("explain b", nil)
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))

	// Attach resolves the transcript from the agent's project dir and records
	// that path; keep writing the session there from now on.
	transcriptData, err := os.ReadFile(s.TranscriptPath)
	require.NoError(t, err)
	s.TranscriptPath = filepath.Join(env.ClaudeProjectDir, s.ID+".jsonl")
	require.NoError(t, os.WriteFile(s.TranscriptPath, transcriptData, 0o600))
	env.WriteFile("notes.txt", "notes")
	env.GitAdd("notes.txt")
	env.GitCommit("notes")

	// attach -f amends HEAD, which runs the installed git hooks; they resolve
	// "entire" through PATH, so point it at the binary under test.
	env.ExtraEnv = append(env.ExtraEnv,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := env.RunCLI("session", "attach", s.ID, "-a", agentClaudeCode, "-f")
	require.Contains(t, output, "Attached session")
	attached := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, attached)
	require.NotEqual(t, cp1, attached)
	ua := readCommittedTokenUsage(t, env, attached)
	require.NotNil(t, ua)
	require.Equal(t, 7, ua.OutputTokens, "attach counts turn 2 only; turn 1 is in checkpoint 1")

	// Turn 3 (3 tokens) writes c; b and c are committed through hooks.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("c.txt", "c")
	appendUsageMessage(s, "msg-3", 3)
	s.CreateTranscript("make c", []FileChange{{Path: "c.txt", Content: "c"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("b and c", "b.txt", "c.txt")
	cp3 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp3)
	require.NotEqual(t, attached, cp3)
	u3 := readCommittedTokenUsage(t, env, cp3)
	require.NotNil(t, u3)
	require.Equal(t, 3, u3.OutputTokens, "the next checkpoint counts turn 3 only, not the attached turn 2")
}

// TestTokenScope_MidTurnCommitTailCountsInNextCheckpoint: when the agent
// commits mid-turn, the rest of the turn is written after condensation. Stop
// moves the displayed window past that tail, but its tokens are in no
// checkpoint yet, so the next checkpoint must count them.
func TestTokenScope_MidTurnCommitTailCountsInNextCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	s := env.NewSession()

	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(s.ID, "create and commit", s.TranscriptPath))
	env.WriteFile("feature.go", "package feature\n")
	s.TranscriptBuilder.AddUserMessage("create and commit")
	appendUsageMessage(s, "msg-1", 100)
	toolID := s.TranscriptBuilder.AddToolUse("mcp__acp__Write", "feature.go", "package feature\n")
	s.TranscriptBuilder.AddToolResult(toolID)
	require.NoError(t, s.TranscriptBuilder.WriteToFile(s.TranscriptPath))
	env.GitCommitWithHooksAsAgent("Add feature", "feature.go")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)

	// The turn continues after the commit (5 tokens), then Stop fires.
	appendUsageMessage(s, "msg-tail", 5)
	require.NoError(t, s.TranscriptBuilder.WriteToFile(s.TranscriptPath))
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))

	// Turn 2 (7 tokens) writes another file, committed by the user.
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(s.ID, "add more", s.TranscriptPath))
	env.WriteFile("more.go", "package feature\n")
	appendUsageMessage(s, "msg-2", 7)
	s.CreateTranscript("add more", []FileChange{{Path: "more.go", Content: "package feature\n"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("more", "more.go")
	cp2 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp2)
	require.NotEqual(t, cp1, cp2)

	u1 := readCommittedTokenUsage(t, env, cp1)
	u2 := readCommittedTokenUsage(t, env, cp2)
	require.NotNil(t, u1)
	require.NotNil(t, u2)
	require.Equal(t, 100, u1.OutputTokens)
	require.Equal(t, 12, u2.OutputTokens, "checkpoint 2 counts turn 1's post-commit tail (5) and turn 2 (7)")
}

// TestTokenScope_AttachIntoOwnCheckpointKeepsItsTokens: when HEAD's trailer
// already names a checkpoint holding this session, attach leaves that entry
// alone. The entry keeps its tokens, and the turn attach didn't record is
// counted by the session's next checkpoint, so nothing is lost or counted twice.
func TestTokenScope_AttachIntoOwnCheckpointKeepsItsTokens(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	s := env.NewSession()

	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("a.txt", "a")
	appendUsageMessage(s, "msg-1", 100)
	s.CreateTranscript("make a", []FileChange{{Path: "a.txt", Content: "a"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("a", "a.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)

	// Turn 2 (7 tokens) changes no files; turn start clears LastCheckpointID,
	// so only the checkpoint's own contents show it already holds the session.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	appendUsageMessage(s, "msg-2", 7)
	s.CreateTranscript("explain", nil)
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))

	transcriptData, err := os.ReadFile(s.TranscriptPath)
	require.NoError(t, err)
	s.TranscriptPath = filepath.Join(env.ClaudeProjectDir, s.ID+".jsonl")
	require.NoError(t, os.WriteFile(s.TranscriptPath, transcriptData, 0o600))
	env.ExtraEnv = append(env.ExtraEnv,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := env.RunCLI("session", "attach", s.ID, "-a", agentClaudeCode, "-f")
	require.Contains(t, output, "is already in checkpoint")
	require.Equal(t, cp1, env.TryGetLatestCheckpointID())

	usage := readCommittedTokenUsage(t, env, cp1)
	require.NotNil(t, usage)
	require.Equal(t, 100, usage.OutputTokens, "cp1 keeps its 100 tokens")

	// Turn 3 (3 tokens) writes c; its checkpoint also carries turn 2's 7.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("c.txt", "c")
	appendUsageMessage(s, "msg-3", 3)
	s.CreateTranscript("make c", []FileChange{{Path: "c.txt", Content: "c"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("c", "c.txt")
	cp3 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp3)
	require.NotEqual(t, cp1, cp3)
	u3 := readCommittedTokenUsage(t, env, cp3)
	require.NotNil(t, u3)
	require.Equal(t, 10, u3.OutputTokens, "the next checkpoint counts turn 2 (7) and turn 3 (3)")
}

// TestTokenScope_AttachDuringActiveTurnConsumesTokensOnly: attaching a session
// mid-turn stores the turn's tokens so far and marks them counted, so the
// turn's own checkpoint doesn't count them again. The displayed window belongs
// to the running session's hooks and stays where it was.
func TestTokenScope_AttachDuringActiveTurnConsumesTokensOnly(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	s := env.NewSession()

	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("a.txt", "a")
	appendUsageMessage(s, "msg-1", 100)
	s.CreateTranscript("make a", []FileChange{{Path: "a.txt", Content: "a"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("a", "a.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)

	// Turn 2 starts and spends 7 tokens; Stop hasn't fired, so it's active.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	appendUsageMessage(s, "msg-2", 7)
	s.CreateTranscript("make c", nil)
	transcriptData, err := os.ReadFile(s.TranscriptPath)
	require.NoError(t, err)
	s.TranscriptPath = filepath.Join(env.ClaudeProjectDir, s.ID+".jsonl")
	require.NoError(t, os.WriteFile(s.TranscriptPath, transcriptData, 0o600))

	before, err := env.GetSessionState(s.ID)
	require.NoError(t, err)
	require.True(t, before.Phase.IsActive())

	env.WriteFile("notes.txt", "notes")
	env.GitAdd("notes.txt")
	env.GitCommit("notes")
	env.ExtraEnv = append(env.ExtraEnv,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := env.RunCLI("session", "attach", s.ID, "-a", agentClaudeCode, "-f")
	require.Contains(t, output, "Attached session")
	attached := env.TryGetLatestCheckpointID()
	require.NotEqual(t, cp1, attached)
	ua := readCommittedTokenUsage(t, env, attached)
	require.NotNil(t, ua)
	require.Equal(t, 7, ua.OutputTokens, "attach stores turn 2's tokens so far")

	after, err := env.GetSessionState(s.ID)
	require.NoError(t, err)
	require.True(t, after.Phase.IsActive(), "attach must not end a running session")
	require.Equal(t, before.CheckpointTranscriptStart, after.CheckpointTranscriptStart,
		"a running session's displayed window belongs to its hooks")

	// The turn continues (3 tokens), writes c, and is committed through hooks.
	env.WriteFile("c.txt", "c")
	appendUsageMessage(s, "msg-3", 3)
	s.CreateTranscript("make c", []FileChange{{Path: "c.txt", Content: "c"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("c", "c.txt")
	cp3 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp3)
	require.NotEqual(t, attached, cp3)
	u3 := readCommittedTokenUsage(t, env, cp3)
	require.NotNil(t, u3)
	require.Equal(t, 3, u3.OutputTokens, "the turn's checkpoint counts only what attach didn't store")
}
