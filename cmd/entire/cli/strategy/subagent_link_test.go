package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

func TestUpsertSubagentLink_AppendsThenMergesFirstWriteWins(t *testing.T) {
	t.Parallel()

	var list []checkpoint.SubagentLink
	list = upsertSubagentLink(list, checkpoint.SubagentLink{ToolUseID: "t1", AgentID: "a1", SubagentType: "dev", Description: "build", FileCount: 3})
	list = upsertSubagentLink(list, checkpoint.SubagentLink{ToolUseID: "t2", AgentID: "a2", SubagentType: "reviewer"})
	require.Len(t, list, 2, "distinct keys append as separate entries (it is a list)")

	// Re-fire t1 with a DIFFERENT (higher) FileCount and empty fields. The first
	// record ran with the pre-task baseline and is authoritative: FileCount must
	// NOT change, and non-empty fields are preserved.
	list = upsertSubagentLink(list, checkpoint.SubagentLink{ToolUseID: "t1", FileCount: 99})
	require.Len(t, list, 2, "re-fire merges in place, never appends")
	require.Equal(t, "t1", list[0].ToolUseID, "ordering preserved")
	require.Equal(t, 3, list[0].FileCount, "first FileCount is authoritative — a re-fire cannot overwrite it")
	require.Equal(t, "dev", list[0].SubagentType, "non-empty field preserved")
	require.Equal(t, "build", list[0].Description, "non-empty field preserved")
	require.Equal(t, "a1", list[0].AgentID, "non-empty field preserved")

	// A re-fire fills a field the first record was missing.
	list = upsertSubagentLink(list, checkpoint.SubagentLink{ToolUseID: "t2", Description: "late description"})
	require.Equal(t, "late description", list[1].Description, "a gap in the first record is filled by a re-fire")
}

func TestUpsertSubagentLink_KeysByAgentIDWhenNoToolUseID(t *testing.T) {
	t.Parallel()

	var list []checkpoint.SubagentLink
	// Two subagents lacking a ToolUseID but with DISTINCT AgentIDs must not
	// collapse onto an empty key.
	list = upsertSubagentLink(list, checkpoint.SubagentLink{AgentID: "a1", SubagentType: "x"})
	list = upsertSubagentLink(list, checkpoint.SubagentLink{AgentID: "a2", SubagentType: "y"})
	require.Len(t, list, 2, "distinct AgentIDs (no ToolUseID) do not collapse")

	// Same AgentID merges in place.
	list = upsertSubagentLink(list, checkpoint.SubagentLink{AgentID: "a1", Description: "fill"})
	require.Len(t, list, 2)
	require.Equal(t, "fill", list[0].Description)
}

func TestCountUniqueFiles_DedupsAcrossLists(t *testing.T) {
	t.Parallel()

	require.Equal(t, 0, countUniqueFiles(), "no lists -> zero")
	require.Equal(t, 0, countUniqueFiles(nil, nil), "empty lists -> zero")
	require.Equal(t, 2, countUniqueFiles([]string{"a.go", "b.go"}))
	require.Equal(t, 3, countUniqueFiles([]string{"a.go", "b.go"}, []string{"b.go", "c.go"}), "dedup across lists")
	require.Equal(t, 1, countUniqueFiles([]string{"a.go"}, []string{"a.go"}, []string{"a.go"}), "same path across all lists counts once")
}

// TestRecordSubagentLink_AccumulatesAndMergesThroughState drives the real
// RecordSubagentLink -> MutateSessionState -> persisted state path: distinct
// subagents accumulate as a list, a no-file-change subagent is still recorded
// (the full-tree requirement), an ID-less subagent is skipped, and a re-fire
// upserts without overwriting the authoritative first record.
func TestRecordSubagentLink_AccumulatesAndMergesThroughState(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-05-08-record-subagent-link"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("x"), 0o644))
	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID: sessionID, ModifiedFiles: []string{"test.txt"},
		MetadataDir: metadataDir, MetadataDirAbs: metadataDirAbs,
		CommitMessage: "c1", AuthorName: "T", AuthorEmail: "t@t.com",
	}))

	ctx := context.Background()
	require.NoError(t, RecordSubagentLink(ctx, sessionID, "t1", "a1", "dev", "build", []string{"x.go", "y.go"}, nil, nil))
	// A subagent that changed nothing must still be recorded (FileCount 0).
	require.NoError(t, RecordSubagentLink(ctx, sessionID, "t2", "a2", "reviewer", "review", nil, nil, nil))

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, state.Subagents, 2, "two distinct subagents accumulate as a list")
	require.Equal(t, 2, state.Subagents[0].FileCount, "first subagent's file count persisted")
	require.Equal(t, "a2", state.Subagents[1].AgentID)
	require.Equal(t, 0, state.Subagents[1].FileCount, "a no-file-change subagent is still recorded")

	// An ID-less subagent (e.g. copilot) is skipped — no empty-keyed collapse.
	require.NoError(t, RecordSubagentLink(ctx, sessionID, "", "", "", "", nil, nil, nil))
	require.NoError(t, RecordSubagentLink(ctx, sessionID, "", "", "", "", nil, nil, nil))
	state, err = s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, state.Subagents, 2, "ID-less subagents are not recorded")

	// Re-fire t1 baseline-less: upsert (not append) and never overwrite the
	// authoritative first FileCount.
	require.NoError(t, RecordSubagentLink(ctx, sessionID, "t1", "a1", "", "", nil, nil, nil))
	state, err = s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, state.Subagents, 2, "re-fire upserts, does not append")
	require.Equal(t, 2, state.Subagents[0].FileCount, "re-fire does not change the authoritative FileCount")
	require.Equal(t, "dev", state.Subagents[0].SubagentType, "re-fire preserves SubagentType")
}

// TestCondenseSession_RoundTripsSubagents proves the parent->child subagent list
// seeded on session.State flows through condensation into the pushed per-session
// metadata.json — the payload the UI reads. Mirrors the InvestigateRunID
// round-trip test (the precedent this feature follows).
func TestCondenseSession_RoundTripsSubagents(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "2026-05-08-subagents-condensation"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"spawn a worker"}}
{"type":"assistant","message":{"content":"On it."}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	// Touch a tracked file so condensation has non-empty work.
	trackedFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(trackedFile, []byte("agent-modified content"), 0o644))

	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.txt"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)

	// Seed the parent->child link list exactly as RecordSubagentLink would.
	state.Subagents = []checkpoint.SubagentLink{
		{ToolUseID: "tool-1", AgentID: "agent-1", SubagentType: "dev", Description: "build the thing", FileCount: 2},
		{ToolUseID: "tool-2", AgentID: "agent-2", SubagentType: "reviewer", Description: "review it", FileCount: 1},
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	checkpointID := id.MustCheckpointID("aabbccdd3344")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped, "condensation must not skip when files are touched")

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	checkpointTree, err := tree.Tree(checkpointID.Path())
	require.NoError(t, err)

	sessionMeta, err := checkpointTree.File(checkpointID.Path() + "/0/" + paths.MetadataFileName)
	if err != nil {
		// Path style varies by tree iteration. Fall back to subtree lookup.
		subtree, subErr := checkpointTree.Tree("0")
		require.NoError(t, subErr)
		sessionMeta, err = subtree.File(paths.MetadataFileName)
		require.NoError(t, err)
	}
	sessionBytes, err := sessionMeta.Contents()
	require.NoError(t, err)

	// Pin the on-disk v1/UI contract key independently of the struct round-trip,
	// so a tag rename on Metadata.Subagents is caught.
	var rawMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(sessionBytes), &rawMeta))
	rawSubs, ok := rawMeta["subagents"].([]any)
	require.True(t, ok, "metadata.json must carry the literal 'subagents' key")
	require.Len(t, rawSubs, 2)
	rawFirst, ok := rawSubs[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool-1", rawFirst["tool_use_id"])
	require.Equal(t, "agent-1", rawFirst["agent_id"])

	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(sessionBytes), &meta))

	require.Len(t, meta.Subagents, 2, "per-session metadata must round-trip the full subagent list")
	require.Equal(t, "tool-1", meta.Subagents[0].ToolUseID)
	require.Equal(t, "agent-1", meta.Subagents[0].AgentID, "explicit child link survives the round-trip")
	require.Equal(t, "dev", meta.Subagents[0].SubagentType)
	require.Equal(t, "build the thing", meta.Subagents[0].Description)
	require.Equal(t, 2, meta.Subagents[0].FileCount)
	require.Equal(t, "reviewer", meta.Subagents[1].SubagentType)
}

// TestHandleTurnEnd_PublishesTrailingSubagents proves the finalize wiring
// (UpdateOptions.Subagents = state.Subagents in finalizeAllTurnCheckpoints): a
// subagent recorded AFTER the last condensation is published to the turn's
// already-written checkpoint at turn-end. Mirrors TestHandleTurnEnd_PartialFailure.
func TestHandleTurnEnd_PublishesTrailingSubagents(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-trailing-subagents"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	state.TurnCheckpointIDs = nil
	require.NoError(t, s.saveSessionState(context.Background(), state))

	// Commit -> a real v1 checkpoint is condensed while state.Subagents is empty.
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")
	require.NoError(t, s.PostCommit(context.Background()))

	state, err = s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.Contains(t, state.TurnCheckpointIDs, "a1b2c3d4e5f6", "PostCommit records the turn checkpoint")

	// A subagent completes AFTER the condensation (trailing). Only the turn-end
	// finalize can publish it onto the already-written checkpoint.
	state.Subagents = []checkpoint.SubagentLink{
		{ToolUseID: "trail-1", AgentID: "trail-a", SubagentType: "reader", FileCount: 0},
	}

	fullTranscript := `{"type":"human","message":{"content":"spawn a reader"}}
{"type":"assistant","message":{"content":"done"}}
`
	transcriptPath := filepath.Join(dir, ".entire", "metadata", sessionID, "full_transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte(fullTranscript), 0o644))
	state.TranscriptPath = transcriptPath
	require.NoError(t, s.saveSessionState(context.Background(), state))

	require.NoError(t, s.HandleTurnEnd(context.Background(), state))

	// The trailing subagent must now be present in the checkpoint's v1 metadata.json.
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	f, err := tree.File(cpID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err)
	content, err := f.Contents()
	require.NoError(t, err)

	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))
	require.Len(t, meta.Subagents, 1, "turn-end finalize must publish the trailing subagent into the checkpoint metadata")
	require.Equal(t, "trail-1", meta.Subagents[0].ToolUseID)
	require.Equal(t, "reader", meta.Subagents[0].SubagentType)
}
