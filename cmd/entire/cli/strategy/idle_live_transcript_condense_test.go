package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const idleLiveSessionID = "sess-idle-live"

// ENT-3537: an IDLE session whose new work since its last checkpoint is only
// in its live transcript, which edited the committed file. Prepare-commit-msg
// links it on that transcript evidence, so post-commit must condense into the
// trailer rather than leave the commit pointing at a checkpoint that was never
// written.
func TestIdleSessionLinkedByLiveTranscript_TrailerResolves(t *testing.T) {
	for _, tt := range []struct {
		name      string
		stepCount int
		records   []session.TaskRecord
	}{
		// No turn end has recorded a step since the last checkpoint.
		{name: "no turn-end step"},
		{name: "completed task records", records: []session.TaskRecord{
			{ToolUseID: "toolu_01X", AgentID: "a123", StartedAt: time.Now().Add(-time.Hour), CompletedAt: time.Now().Add(-30 * time.Minute)},
		}},
		// A turn end recorded a step but left no files and no stored transcript.
		{name: "turn-end step without a record", stepCount: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := idleLiveTranscriptRepo(t)
			saveIdleLiveTranscriptSession(t, dir, dir, func(st *SessionState) {
				st.StepCount = tt.stepCount
				st.TaskRecords = tt.records
			})
			testutil.WriteFile(t, dir, "feature.go", "package feature\n")
			testutil.GitAdd(t, dir, "feature.go")

			ids := commitThroughHooks(t, dir, NewManualCommitStrategy(), "feat: add the feature\n")
			require.Len(t, ids, 1, "prepare-commit-msg links the session on its live transcript: %v", ids)
			requireFreshTrailersResolve(t, ids)
		})
	}
}

// The same session stays out of a commit its transcript did not edit, though
// another session's trailer is on it.
func TestIdleSessionLinkedByLiveTranscript_NotCondensedIntoUnrelatedCommit(t *testing.T) {
	dir := idleLiveTranscriptRepo(t)
	saveIdleLiveTranscriptSession(t, dir, dir, nil)
	testutil.WriteFile(t, dir, "feature.go", "package feature\n")
	testutil.WriteFile(t, dir, "notes.txt", "notes\n")
	testutil.GitAdd(t, dir, "notes.txt")
	testutil.GitCommit(t, dir, "docs: notes\n\nEntire-Checkpoint: a1b2c3d4e5f6")
	s := NewManualCommitStrategy()
	require.NoError(t, s.PostCommit(context.Background()))

	state, err := s.loadSessionState(context.Background(), idleLiveSessionID)
	require.NoError(t, err)
	require.Empty(t, state.LastCheckpointID, "the commit holds none of the transcript's edits")
}

// The IDLE session's transcript edit claims the commit, so a file-less ACTIVE
// session (a reader) is held off it by the read-only gate rather than
// condensed into the writer's checkpoint.
func TestIdleSessionLinkedByLiveTranscript_ClaimsCommitFromReadOnlySession(t *testing.T) {
	dir := idleLiveTranscriptRepo(t)
	saveIdleLiveTranscriptSession(t, dir, dir, nil)
	readerTranscript := filepath.Join(t.TempDir(), "reader.jsonl")
	require.NoError(t, os.WriteFile(readerTranscript, []byte(testTranscriptPromptResponse), 0o600))
	now := time.Now()
	require.NoError(t, SaveSessionState(context.Background(), &SessionState{
		SessionID:           "sess-reader",
		BaseCommit:          testutil.GetHeadHash(t, dir),
		WorktreePath:        dir,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      readerTranscript,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
	}))
	testutil.WriteFile(t, dir, "feature.go", "package feature\n")
	testutil.GitAdd(t, dir, "feature.go")

	s := NewManualCommitStrategy()
	ids := commitThroughHooks(t, dir, s, "feat: add the feature\n")
	require.Len(t, ids, 1, "%v", ids)
	requireFreshTrailersResolve(t, ids)

	writer, err := s.loadSessionState(context.Background(), idleLiveSessionID)
	require.NoError(t, err)
	require.Equal(t, ids[0], writer.LastCheckpointID.String(), "the writer condensed into the commit")
	reader, err := s.loadSessionState(context.Background(), "sess-reader")
	require.NoError(t, err)
	require.Empty(t, reader.LastCheckpointID, "the reader touched nothing the commit holds")
}

// A session homed in one worktree that edits and commits in a sibling. The
// hooks must agree whichever way the sibling's edit reached the session: every
// trailer resolves, and the files the session still has waiting at home and in
// a third worktree stay pending. With a record of the sibling, the commit is
// judged in that record's view; with only the transcript, the sibling's
// commit links nothing, whether or not the home has files pending.
func TestIdleSessionLinkedByLiveTranscript_SiblingWorktreeCommit(t *testing.T) {
	for _, tt := range []struct {
		name        string
		recorded    bool
		homePending []string
		wantLinked  bool
	}{
		{name: "sibling recorded", recorded: true, homePending: []string{"home.go"}, wantLinked: true},
		{name: "sibling only in the transcript", homePending: []string{"home.go"}},
		// Nothing pending at home, so the transcript is the session's only
		// evidence and its sibling path is what decides.
		{name: "sibling only in the transcript, nothing pending at home"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mainDir, linkedDir := twoWorktrees(t)
			thirdDir := filepath.Join(resolvedTempDir(t), "third")
			createSessionMatchWorktree(t, mainDir, thirdDir, "third")
			t.Cleanup(func() { removeSessionMatchWorktree(mainDir, thirdDir) })
			thirdKey, siblingKey := linkedKey(t, thirdDir), linkedKey(t, linkedDir)

			chdirWorktree(t, mainDir)
			saveIdleLiveTranscriptSession(t, mainDir, linkedDir, func(st *SessionState) {
				st.FilesTouched = tt.homePending
				st.OtherWorktrees = map[string]*session.WorktreeWork{
					thirdKey: {Path: thirdDir, FilesTouched: []string{"other.go"}, Incarnation: mark(t, thirdDir)},
				}
				if tt.recorded {
					st.OtherWorktrees[siblingKey] = &session.WorktreeWork{Path: linkedDir, FilesTouched: []string{"feature.go"}, Incarnation: mark(t, linkedDir)}
				}
			})
			chdirWorktree(t, linkedDir)
			testutil.WriteFile(t, linkedDir, "feature.go", "package feature\n")
			testutil.GitAdd(t, linkedDir, "feature.go")

			s := NewManualCommitStrategy()
			ids := commitThroughHooks(t, linkedDir, s, "feat: add the feature\n")
			requireFreshTrailersResolve(t, ids)

			state, err := s.loadSessionState(context.Background(), idleLiveSessionID)
			require.NoError(t, err)
			// Not LastCheckpointID: carry-forward clears it while files stay pending.
			if tt.wantLinked {
				require.Len(t, ids, 1, "%v", ids)
			} else {
				require.Empty(t, ids, "sibling paths outside the home link nothing")
			}
			require.Equal(t, tt.homePending, state.FilesTouched, "the home's pending files are untouched")
			require.Contains(t, state.OtherWorktrees, thirdKey, "the third worktree's record survived")
			require.Equal(t, []string{"other.go"}, state.OtherWorktrees[thirdKey].FilesTouched,
				"the files waiting in the third worktree are still pending")
			require.NotContains(t, state.OtherWorktrees, siblingKey, "the commit took the sibling's only edit, or there was no record of it")
		})
	}
}

// ENT-3537's stacked worktrees: the session's transcript edited feature.go at
// home, and a sibling worktree commits its own feature.go first. Home-relative
// transcript paths say nothing about the sibling's files, so the sibling's
// commit must neither link the session nor consume its work, and the home's
// later commit of feature.go still gets a trailer that resolves.
func TestIdleSessionLinkedByLiveTranscript_StackedSiblingCommitsSameFile(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	chdirWorktree(t, mainDir)
	saveIdleLiveTranscriptSession(t, mainDir, mainDir, nil)
	s := NewManualCommitStrategy()

	chdirWorktree(t, linkedDir)
	testutil.WriteFile(t, linkedDir, "feature.go", "package feature // sibling\n")
	testutil.GitAdd(t, linkedDir, "feature.go")
	siblingIDs := commitThroughHooks(t, linkedDir, s, "feat: the sibling's feature\n")
	require.Empty(t, siblingIDs, "the transcript edited the home's feature.go, not the sibling's")

	chdirWorktree(t, mainDir)
	testutil.WriteFile(t, mainDir, "feature.go", "package feature\n")
	testutil.GitAdd(t, mainDir, "feature.go")
	homeIDs := commitThroughHooks(t, mainDir, s, "feat: add the feature\n")
	require.Len(t, homeIDs, 1, "the home commit holds the transcript's edit: %v", homeIDs)
	requireFreshTrailersResolve(t, homeIDs)
}

// idleLiveTranscriptRepo is a repository with one commit, the process inside it.
func idleLiveTranscriptRepo(t *testing.T) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	return dir
}

// saveIdleLiveTranscriptSession saves an IDLE Claude Code session homed in
// home whose live transcript wrote feature.go in editDir and that no turn end
// recorded (StepCount 0, no FilesTouched), then lets mutate adjust it.
func saveIdleLiveTranscriptSession(t *testing.T, home, editDir string, mutate func(*SessionState)) {
	t.Helper()
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, claudeWriteTranscript(t, filepath.Join(editDir, "feature.go"), "package feature\n"), 0o600))
	worktreeID, err := paths.GetWorktreeID(home)
	require.NoError(t, err)
	now := time.Now()
	state := &SessionState{
		SessionID:           idleLiveSessionID,
		BaseCommit:          testutil.GetHeadHash(t, home),
		WorktreePath:        home,
		WorktreeID:          worktreeID,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      transcriptPath,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseIdle,
	}
	if mutate != nil {
		mutate(state)
	}
	require.NoError(t, SaveSessionState(context.Background(), state))
}

// claudeWriteTranscript is a Claude Code transcript of one prompt whose reply
// writes content to path. Encoded with encoding/json so a Windows path's
// backslashes stay valid JSON.
func claudeWriteTranscript(t *testing.T, path, content string) []byte {
	t.Helper()
	lines := []any{
		map[string]any{"type": "user", "message": map[string]any{"content": "add the feature"}},
		map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Write", "input": map[string]any{"file_path": path, "content": content}},
		}}},
	}
	var out []byte
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		require.NoError(t, err)
		out = append(out, encoded...)
		out = append(out, '\n')
	}
	return out
}

// An ENDED session with no persisted files takes the discard action whatever
// its transcript shows, so its transcript must not count as new content in
// post-commit: that would keep it from ever being marked fully condensed, and
// every later commit would judge it again.
func TestEndedSessionWithTranscriptOnlyEdits_FullyCondensedAfterCommit(t *testing.T) {
	dir := idleLiveTranscriptRepo(t)
	saveIdleLiveTranscriptSession(t, dir, dir, func(st *SessionState) {
		endedAt := time.Now()
		st.Phase = session.PhaseEnded
		st.EndedAt = &endedAt
		st.LastCheckpointID = id.MustCheckpointID("0a0b0c0d0e0f") // condensed before, so it is kept
	})
	testutil.WriteFile(t, dir, "feature.go", "package feature\n")
	testutil.GitAdd(t, dir, "feature.go")
	testutil.GitCommit(t, dir, "feat: add the feature\n\nEntire-Checkpoint: a1b2c3d4e5f6")
	s := NewManualCommitStrategy()
	require.NoError(t, s.PostCommit(context.Background()))

	state, err := s.loadSessionState(context.Background(), idleLiveSessionID)
	require.NoError(t, err)
	require.True(t, state.FullyCondensed, "an ENDED session with nothing to condense is done")
}

// A subagent's edits are read from its full transcript, so an IDLE session
// whose main transcript has not grown since its last checkpoint must not
// claim a commit through edits that checkpoint already holds: the claim would
// hold a file-less ACTIVE session off the commit at the read-only gate, while
// the IDLE session itself has no new content to condense, leaving the trailer
// unresolved.
func TestIdleSessionWithCheckpointedSubagentEdits_DoesNotClaimCommit(t *testing.T) {
	dir := idleLiveTranscriptRepo(t)
	transcriptDir := t.TempDir()
	mainTranscript := filepath.Join(transcriptDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(mainTranscript, []byte(`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"toolu_task1","name":"Task","input":{"prompt":"implement feature"}}]}}
{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_task1","content":"agentId: sub123"}]}}
`), 0o600))
	subagentsDir := paths.SubagentsDir(transcriptDir, idleLiveSessionID)
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subagentsDir, paths.AgentTranscriptFileName("sub123")),
		claudeWriteTranscript(t, filepath.Join(dir, "feature.go"), "package feature\n"), 0o600))
	saveIdleLiveTranscriptSession(t, dir, dir, func(st *SessionState) {
		st.TranscriptPath = mainTranscript
		st.CheckpointTranscriptStart = 2 // the last checkpoint holds the whole main transcript
	})
	readerTranscript := filepath.Join(t.TempDir(), "reader.jsonl")
	require.NoError(t, os.WriteFile(readerTranscript, []byte(testTranscriptPromptResponse), 0o600))
	// Backdated so the reader's condensation doesn't wait for a transcript flush.
	stale := time.Now().Add(-3 * time.Minute)
	require.NoError(t, os.Chtimes(readerTranscript, stale, stale))
	now := time.Now()
	require.NoError(t, SaveSessionState(context.Background(), &SessionState{
		SessionID:           "sess-reader",
		BaseCommit:          testutil.GetHeadHash(t, dir),
		WorktreePath:        dir,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      readerTranscript,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
	}))
	testutil.WriteFile(t, dir, "feature.go", "package feature\n")
	testutil.GitAdd(t, dir, "feature.go")

	ids := commitThroughHooks(t, dir, NewManualCommitStrategy(), "feat: add the feature\n")
	require.Len(t, ids, 1, "the ACTIVE reader is stamped by the fast path: %v", ids)
	requireFreshTrailersResolve(t, ids)
}
