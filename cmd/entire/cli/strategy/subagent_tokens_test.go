package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestAccumulateTokenUsage_SubagentTokensReplacedNotSummed is a focused unit
// test on accumulateTokenUsage: CalculateTotalTokenUsage (claudecode and
// factoryaidroid) discovers subagent IDs from the full transcript and re-reads
// each subagent transcript from line 0 on every call, so incoming.SubagentTokens
// is always a cumulative-since-session-start snapshot, not a per-step delta.
// Summing that snapshot across steps (as accumulateTokenUsage does for the
// main-agent fields) would re-add a subagent's full usage on every subsequent
// step after it was first discovered. accumulateTokenUsage must replace
// SubagentTokens with the latest snapshot instead.
func TestAccumulateTokenUsage_SubagentTokensReplacedNotSummed(t *testing.T) {
	subagentSnapshot := &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5}

	step1 := &agent.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1, SubagentTokens: subagentSnapshot}
	existing := accumulateTokenUsage(nil, step1)
	require.NotNil(t, existing.SubagentTokens)
	require.Equal(t, 500, existing.SubagentTokens.InputTokens)
	require.Equal(t, 250, existing.SubagentTokens.OutputTokens)

	// Second step within the same checkpoint window: the subagent transcript
	// hasn't changed, so CalculateTotalTokenUsage returns the SAME cumulative
	// snapshot again. Main-agent fields are per-step deltas and should sum;
	// SubagentTokens must NOT double.
	step2 := &agent.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1, SubagentTokens: subagentSnapshot}
	existing = accumulateTokenUsage(existing, step2)

	require.Equal(t, 200, existing.InputTokens, "main-agent InputTokens should sum across steps")
	require.Equal(t, 100, existing.OutputTokens, "main-agent OutputTokens should sum across steps")
	require.NotNil(t, existing.SubagentTokens)
	require.Equal(t, 500, existing.SubagentTokens.InputTokens, "SubagentTokens must be replaced, not summed")
	require.Equal(t, 250, existing.SubagentTokens.OutputTokens, "SubagentTokens must be replaced, not summed")
}

// TestSaveStep_SubagentTokensNotDoubleCountedAcrossCheckpoints exercises the
// real SaveStep path for both Claude Code and Factory AI Droid (the two
// agents whose CalculateTotalTokenUsage implementations discover subagent IDs
// from the full transcript per #329) and proves that a subagent discovered
// before a checkpoint window is folded into that checkpoint's token usage
// exactly once, not re-added on every subsequent checkpoint it remains
// discoverable in.
func TestSaveStep_SubagentTokensNotDoubleCountedAcrossCheckpoints(t *testing.T) {
	agentTypes := []types.AgentType{agent.AgentTypeClaudeCode, agent.AgentTypeFactoryAIDroid}

	for _, agentType := range agentTypes {
		t.Run(string(agentType), func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			repo, err := git.PlainOpen(dir)
			require.NoError(t, err)

			worktree, err := repo.Worktree()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v1"), 0o644))
			_, err = worktree.Add("test.txt")
			require.NoError(t, err)
			_, err = worktree.Commit("Initial commit", &git.CommitOptions{
				Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
			})
			require.NoError(t, err)

			t.Chdir(dir)
			ctx := context.Background()
			s := &ManualCommitStrategy{}
			sessionID := "2026-07-10-subagent-dedup-" + string(agentType)

			metadataDir := ".entire/metadata/" + sessionID
			metadataDirAbs := filepath.Join(dir, metadataDir)
			require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
			transcript := `{"type":"human","message":{"content":"test"}}` + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

			// Checkpoint 1, step 1: a subagent spawned before this checkpoint's
			// window is discovered via the full-transcript scan (#329) and its
			// cumulative usage as of now is 500/250 across 5 calls.
			subagentAtCheckpoint1 := &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5}
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 1 step 1",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: subagentAtCheckpoint1,
				},
			}))

			// Checkpoint 1, step 2: same turn window, subagent transcript
			// unchanged (CalculateTotalTokenUsage would return the identical
			// cumulative snapshot again since it always re-reads from line 0).
			// Change the working tree so SaveStep sees a real diff to save.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v2"), 0o644))
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 1 step 2",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: subagentAtCheckpoint1,
				},
			}))

			state, err := s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)
			require.NotNil(t, state.CheckpointTokenUsage)
			require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens)
			require.Equal(t, 500, state.CheckpointTokenUsage.SubagentTokens.InputTokens,
				"subagent usage must be folded once per checkpoint window, not once per step")
			require.Equal(t, 250, state.CheckpointTokenUsage.SubagentTokens.OutputTokens)
			require.Equal(t, 200, state.CheckpointTokenUsage.InputTokens, "main-agent deltas still sum across steps")

			// Simulate the condensation reset that happens between checkpoints:
			// CheckpointTokenUsage is cleared and SubagentTokensBaseline snapshots
			// the cumulative subagent total counted so far, so the next
			// checkpoint's CheckpointTokenUsage.SubagentTokens is scoped to
			// "since this reset" instead of the whole session again.
			require.NoError(t, MutateSessionState(ctx, sessionID, func(st *SessionState) error {
				st.StepCount = 0
				st.CheckpointTokenUsage = nil
				if st.TokenUsage != nil {
					st.SubagentTokensBaseline = st.TokenUsage.SubagentTokens
				}
				st.CheckpointTranscriptStart = 10
				return nil
			}))

			// Checkpoint 2, step 1: the same subagent is still discoverable (its
			// marker line is still in the full transcript) and has grown a bit
			// more since checkpoint 1.
			subagentAtCheckpoint2 := &agent.TokenUsage{InputTokens: 620, OutputTokens: 310, APICallCount: 6}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v3"), 0o644))
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 2 step 1",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: subagentAtCheckpoint2,
				},
			}))

			state2, err := s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)

			// The session-wide total tracks the latest cumulative subagent
			// snapshot directly (it is already cumulative) — not the sum of the
			// checkpoint-1 and checkpoint-2 snapshots.
			require.NotNil(t, state2.TokenUsage.SubagentTokens)
			require.Equal(t, 620, state2.TokenUsage.SubagentTokens.InputTokens,
				"session-wide subagent total must be the latest cumulative snapshot, not summed across checkpoints")
			require.Equal(t, 310, state2.TokenUsage.SubagentTokens.OutputTokens)

			// Checkpoint 2's own CheckpointTokenUsage.SubagentTokens must be
			// rescoped to just what grew since the checkpoint-1 baseline
			// (620-500, 310-250), not the full cumulative total again.
			require.NotNil(t, state2.CheckpointTokenUsage)
			require.NotNil(t, state2.CheckpointTokenUsage.SubagentTokens)
			require.Equal(t, 120, state2.CheckpointTokenUsage.SubagentTokens.InputTokens,
				"checkpoint 2's subagent delta must exclude what was already counted in checkpoint 1")
			require.Equal(t, 60, state2.CheckpointTokenUsage.SubagentTokens.OutputTokens)
		})
	}
}

// TestSaveStep_SubagentBaselineNotDoubleSubtractedWhenLaterStepDropsSubagent
// pins the double-subtraction bug: within a single checkpoint window, once a
// step has set CheckpointTokenUsage.SubagentTokens (rescoped by subtracting the
// baseline), a LATER step whose TokenUsage is non-nil but carries no
// SubagentTokens (subagent transcript cleaned up, so CalculateTotalTokenUsage
// returns APICallCount==0 and leaves SubagentTokens nil) must not cause the
// baseline to be subtracted a second time. accumulateTokenUsage only REPLACES
// SubagentTokens when the incoming snapshot is non-nil, so a nil-subagent step
// leaves CheckpointTokenUsage.SubagentTokens at its already-rescoped value; a
// per-step re-subtraction would shrink (and via clampSubtract zero) a real
// subagent total. The checkpoint delta must be derived FRESH each call from the
// session-wide cumulative snapshot minus the baseline instead.
func TestSaveStep_SubagentBaselineNotDoubleSubtractedWhenLaterStepDropsSubagent(t *testing.T) {
	agentTypes := []types.AgentType{agent.AgentTypeClaudeCode, agent.AgentTypeFactoryAIDroid}

	for _, agentType := range agentTypes {
		t.Run(string(agentType), func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			repo, err := git.PlainOpen(dir)
			require.NoError(t, err)

			worktree, err := repo.Worktree()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v1"), 0o644))
			_, err = worktree.Add("test.txt")
			require.NoError(t, err)
			_, err = worktree.Commit("Initial commit", &git.CommitOptions{
				Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
			})
			require.NoError(t, err)

			t.Chdir(dir)
			ctx := context.Background()
			s := &ManualCommitStrategy{}
			sessionID := "2026-07-13-subagent-nodouble-" + string(agentType)

			metadataDir := ".entire/metadata/" + sessionID
			metadataDirAbs := filepath.Join(dir, metadataDir)
			require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
			transcript := `{"type":"human","message":{"content":"test"}}` + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

			// Checkpoint 1: a subagent is discovered with cumulative usage 500/250.
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 1",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5},
				},
			}))

			// Condensation reset: baseline snapshots the cumulative subagent total.
			require.NoError(t, MutateSessionState(ctx, sessionID, func(st *SessionState) error {
				st.StepCount = 0
				st.CheckpointTokenUsage = nil
				if st.TokenUsage != nil {
					st.SubagentTokensBaseline = st.TokenUsage.SubagentTokens
				}
				st.CheckpointTranscriptStart = 10
				return nil
			}))

			// Checkpoint 2, step 1: the subagent has grown to 620/310. The
			// checkpoint delta must be 620-500 / 310-250 = 120 / 60.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v2"), 0o644))
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 2 step 1",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: &agent.TokenUsage{InputTokens: 620, OutputTokens: 310, APICallCount: 6},
				},
			}))

			// Checkpoint 2, step 2: same window, but this step's TokenUsage carries
			// NO SubagentTokens (the subagent transcript was cleaned up, so
			// CalculateTotalTokenUsage found APICallCount==0 and left SubagentTokens
			// nil). accumulateTokenUsage will not replace SubagentTokens, so it stays
			// at the checkpoint-1-baseline-subtracted 120/60 — and the baseline must
			// NOT be subtracted again.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v3"), 0o644))
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 2 step 2",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					// SubagentTokens intentionally nil.
				},
			}))

			state, err := s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)

			// Session-wide total keeps the latest cumulative snapshot (620/310):
			// the nil-subagent step must not clobber or shrink it.
			require.NotNil(t, state.TokenUsage.SubagentTokens)
			require.Equal(t, 620, state.TokenUsage.SubagentTokens.InputTokens,
				"session-wide subagent total must retain the latest cumulative snapshot")
			require.Equal(t, 310, state.TokenUsage.SubagentTokens.OutputTokens)

			// Checkpoint delta must remain the checkpoint-1-baseline-subtracted
			// 120/60, NOT 620-500-500 clamped to 0. This is the regression.
			require.NotNil(t, state.CheckpointTokenUsage)
			require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens)
			require.Equal(t, 120, state.CheckpointTokenUsage.SubagentTokens.InputTokens,
				"baseline must be subtracted once, not re-subtracted on a later nil-subagent step")
			require.Equal(t, 60, state.CheckpointTokenUsage.SubagentTokens.OutputTokens)
			// Main-agent deltas still sum across all three steps in the window.
			require.Equal(t, 200, state.CheckpointTokenUsage.InputTokens,
				"main-agent deltas sum across checkpoint-2 steps")
		})
	}
}

// TestSaveStep_CheckpointSubagentAlwaysDerivedFromSessionCumulative walks the
// finding-1 edge matrix in one window after a baseline reset: a nil-subagent
// first step, a step that grows the subagent, then repeated nil-subagent steps.
// After every step the checkpoint subagent total must equal the session-wide
// cumulative minus the baseline (idempotent), never drifting from repeated
// subtraction. The strategy-layer accounting is agent-agnostic, so one agent
// exercises it.
func TestSaveStep_CheckpointSubagentAlwaysDerivedFromSessionCumulative(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v0"), 0o644))
	_, err = worktree.Add("test.txt")
	require.NoError(t, err)
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	t.Chdir(dir)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	sessionID := "2026-07-13-subagent-edgematrix"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(`{"type":"human","message":{"content":"test"}}`+"\n"), 0o644))

	rev := 0
	save := func(sub *agent.TokenUsage) {
		rev++
		require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte(fmt.Sprintf("rev%d", rev)), 0o644))
		require.NoError(t, s.SaveStep(ctx, StepContext{
			SessionID:      sessionID,
			MetadataDir:    metadataDir,
			MetadataDirAbs: metadataDirAbs,
			ModifiedFiles:  []string{"test.txt"},
			CommitMessage:  fmt.Sprintf("step %d", rev),
			AuthorName:     "Test",
			AuthorEmail:    "test@test.com",
			AgentType:      agent.AgentTypeClaudeCode,
			TokenUsage:     &agent.TokenUsage{InputTokens: 10, APICallCount: 1, SubagentTokens: sub},
		}))
	}
	// checkpointSubIn returns the current checkpoint subagent InputTokens (0 when nil).
	checkpointSubIn := func() int {
		st, loadErr := s.loadSessionState(ctx, sessionID)
		require.NoError(t, loadErr)
		if st.CheckpointTokenUsage == nil || st.CheckpointTokenUsage.SubagentTokens == nil {
			return 0
		}
		return st.CheckpointTokenUsage.SubagentTokens.InputTokens
	}

	// Establish a baseline of 400 via a first window + reset.
	save(&agent.TokenUsage{InputTokens: 400, APICallCount: 4})
	require.NoError(t, MutateSessionState(ctx, sessionID, func(st *SessionState) error {
		st.StepCount = 0
		st.CheckpointTokenUsage = nil
		st.SubagentTokensBaseline = st.TokenUsage.SubagentTokens // 400
		st.CheckpointTranscriptStart = 5
		return nil
	}))

	// Edge: nil first step of the window — cumulative stays 400, delta 0.
	save(nil)
	require.Equal(t, 0, checkpointSubIn(), "nil first step: delta is cumulative(400)-baseline(400)=0")

	// Growth step — cumulative 550, delta 150.
	save(&agent.TokenUsage{InputTokens: 550, APICallCount: 5})
	require.Equal(t, 150, checkpointSubIn(), "growth step: delta is 550-400")

	// Repeated nil steps must NOT shrink the delta (idempotent derive-fresh).
	save(nil)
	require.Equal(t, 150, checkpointSubIn(), "nil step must not re-subtract baseline")
	save(nil)
	require.Equal(t, 150, checkpointSubIn(), "second nil step must not re-subtract baseline")
}

// TestCondenseSessionByID_CapturesSubagentBaselineViaRealResetPath drives a REAL
// condensation (CondenseSessionByID) rather than hand-simulating the reset, so
// the production baseline-snapshot code in resetCheckpointWindow — shared by the
// three condensation reset sites — is exercised where it actually lives. It then
// runs a follow-up checkpoint to prove the baseline captured by the real path is
// used to rescope the next checkpoint's subagent delta.
func TestCondenseSessionByID_CapturesSubagentBaselineViaRealResetPath(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v1"), 0o644))
	_, err = worktree.Add("test.txt")
	require.NoError(t, err)
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	t.Chdir(dir)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	sessionID := "2026-07-13-subagent-realreset"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	// The assistant line carries real usage data (message.id + usage). Real
	// Claude Code transcripts always do, which makes sessionStateBackfillTokenUsage
	// fire during condensation (its InputTokens > 0 branch) and overwrite
	// state.TokenUsage with the transcript-recomputed value — which is computed
	// with subagentsDir="" and therefore drops SubagentTokens. This is what makes
	// this test guard the REAL condensation path: without preserving the
	// cumulative subagent total across the backfill, resetCheckpointWindow would
	// snapshot a nil baseline and the next checkpoint would re-report the full
	// cumulative subagent total (finding 019f5ebf-a57e).
	transcript := `{"type":"human","message":{"content":"do the thing"}}
{"type":"assistant","uuid":"a1","message":{"id":"m1","usage":{"input_tokens":300,"output_tokens":150}}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	// Checkpoint 1: subagent discovered with cumulative usage 500/250.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v2"), 0o644))
	require.NoError(t, s.SaveStep(ctx, StepContext{
		SessionID:      sessionID,
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		ModifiedFiles:  []string{"test.txt"},
		CommitMessage:  "checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
		AgentType:      agent.AgentTypeClaudeCode,
		TokenUsage: &agent.TokenUsage{
			InputTokens: 100, OutputTokens: 50, APICallCount: 1,
			SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5},
		},
	}))

	// Drive the REAL condensation reset path (not a hand-simulated one). This
	// executes resetCheckpointWindow inside CondenseSessionByID.
	require.NoError(t, s.CondenseSessionByID(ctx, sessionID))

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 0, state.StepCount, "real condensation must reset StepCount")
	require.Nil(t, state.CheckpointTokenUsage, "real condensation must clear CheckpointTokenUsage")
	require.NotNil(t, state.SubagentTokensBaseline,
		"real condensation must snapshot the subagent baseline")
	require.Equal(t, 500, state.SubagentTokensBaseline.InputTokens,
		"baseline must capture the cumulative subagent total at condensation")
	require.Equal(t, 250, state.SubagentTokensBaseline.OutputTokens)

	// Checkpoint 2 after the real reset: the subagent grew to 620/310. Its
	// checkpoint delta must be rescoped against the real-path baseline (120/60).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v3"), 0o644))
	require.NoError(t, s.SaveStep(ctx, StepContext{
		SessionID:      sessionID,
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		ModifiedFiles:  []string{"test.txt"},
		CommitMessage:  "checkpoint 2",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
		AgentType:      agent.AgentTypeClaudeCode,
		TokenUsage: &agent.TokenUsage{
			InputTokens: 100, OutputTokens: 50, APICallCount: 1,
			SubagentTokens: &agent.TokenUsage{InputTokens: 620, OutputTokens: 310, APICallCount: 6},
		},
	}))

	state2, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state2.CheckpointTokenUsage)
	require.NotNil(t, state2.CheckpointTokenUsage.SubagentTokens)
	require.Equal(t, 120, state2.CheckpointTokenUsage.SubagentTokens.InputTokens,
		"checkpoint delta must be rescoped against the real-path baseline")
	require.Equal(t, 60, state2.CheckpointTokenUsage.SubagentTokens.OutputTokens)

	// Condense the second window and assert the *committed* checkpoint carries that
	// same window delta. Condensation recomputes usage with subagentsDir="", which
	// leaves SubagentTokens nil and would otherwise replace the rescoped total —
	// which is why committed checkpoints reported "subagent_tokens": null. Sourcing
	// the fill from state.TokenUsage instead of CheckpointTokenUsage would put the
	// 620 cumulative here, and every checkpoint would re-report the session total.
	secondID := id.MustCheckpointID("aabbccdd7788")
	_, err = s.CondenseSession(ctx, repo, secondID, state2, nil)
	require.NoError(t, err)

	summary := readCommittedSummary(t, repo, secondID)
	require.NotNil(t, summary.TokenUsage)
	require.NotNil(t, summary.TokenUsage.SubagentTokens,
		"committed checkpoint must carry the subagent total")
	require.Equal(t, 120, summary.TokenUsage.SubagentTokens.InputTokens,
		"committed value must be this window's delta, not the session cumulative")
	require.Equal(t, 60, summary.TokenUsage.SubagentTokens.OutputTokens)
}

// TestWithSubagentTokensFrom_DoesNotMutateInput guards the copy semantics directly.
// The condensation tests cannot: applyBackfilledSessionTokenUsage already hands back
// a copy on that path, so a mutate-in-place implementation passes them. Mutating
// would overwrite the session-wide cumulative with a window delta and make
// resetCheckpointWindow snapshot a too-small baseline for the next window.
func TestWithSubagentTokensFrom_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	usage := &agent.TokenUsage{InputTokens: 1}
	src := &agent.TokenUsage{SubagentTokens: &agent.TokenUsage{InputTokens: 9}}

	got := withSubagentTokensFrom(usage, src)

	require.Nil(t, usage.SubagentTokens, "must not mutate the input")
	require.NotNil(t, got.SubagentTokens)
	require.Equal(t, 9, got.SubagentTokens.InputTokens)
}

// readCommittedSummary reads a committed checkpoint's root CheckpointSummary through
// the store's own read path, rather than walking the metadata branch by hand — the
// sharded tree layout is the git-branch backend's business, not the test's.
func readCommittedSummary(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID) *cpkg.CheckpointSummary {
	t.Helper()

	store := cpkg.NewGitStore(repo, cpkg.DefaultV1Refs())
	summary, err := store.Read(context.Background(), checkpointID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	return summary
}
