package strategy

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unclaimedTrailerLogMessage is the line logUnclaimedCheckpointTrailer emits.
// Asserting on the log is the only way to observe it: it deliberately has no
// return value and no state side effect, because its whole purpose is to be
// greppable in .entire/logs.
const unclaimedTrailerLogMessage = "no session condensed into the commit's checkpoint trailer"

// TestPostCommit_UnclaimedCheckpointTrailer covers the one shape the line is
// for and the three the caller excludes. The exclusions matter even at DEBUG:
// an amend and a cherry-pick both reach "no session condensed" while their
// checkpoint exists, so without them the line says nothing useful when someone
// greps for it.
func TestPostCommit_UnclaimedCheckpointTrailer(t *testing.T) {
	const trailerID = "f6a1b2c3d4e5"

	tests := []struct {
		name string
		// phase the session is in when the trailered commit lands.
		phase session.Phase
		// noNewContent pins the transcript baseline to what the shadow branch
		// already holds, so condensation finds nothing to do — the state that
		// leaves a stamped trailer unfilled.
		noNewContent bool
		// ownsTrailer presets LastCheckpointID to the committed trailer, which
		// is what an amend looks like: the trailer is inherited and the
		// checkpoint was written by the commit being amended.
		ownsTrailer bool
		// sequenceOp simulates rebase/cherry-pick/revert via .git/rebase-merge.
		sequenceOp bool
		// endedAndCondensed makes the loop skip this session entirely, so its
		// ownership is only visible from the pre-loop snapshot.
		endedAndCondensed bool
		// signalAlreadyReported makes newCommitCondensedSignal dedupe and return
		// nil for a condensation that still happens — the amend-re-condense
		// shape, and the reason claim cannot be read off the telemetry signal.
		signalAlreadyReported bool
		wantLogged            bool
	}{
		{
			name:         "idle session with nothing to condense is unclaimed",
			phase:        session.PhaseIdle,
			noNewContent: true,
			wantLogged:   true,
		},
		{
			name:         "amend is claimed: the session already owns the checkpoint",
			phase:        session.PhaseIdle,
			noNewContent: true,
			ownsTrailer:  true,
		},
		{
			name:              "amend after the session ended is claimed: the loop skips its owner",
			phase:             session.PhaseEnded,
			noNewContent:      true,
			ownsTrailer:       true,
			endedAndCondensed: true,
		},
		{
			name:         "sequence operation is excluded: ownership is not local",
			phase:        session.PhaseIdle,
			noNewContent: true,
			sequenceOp:   true,
		},
		{
			name:  "active session that condenses is claimed",
			phase: session.PhaseActive,
		},
		{
			name:                  "condensing with a deduped telemetry signal is still claimed",
			phase:                 session.PhaseActive,
			signalAlreadyReported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupGitRepo(t)
			t.Chdir(dir)

			repo, err := OpenRepository(context.Background())
			require.NoError(t, err)

			s := &ManualCommitStrategy{}
			sessionID := "test-unclaimed-trailer"
			setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

			state, err := s.loadSessionState(context.Background(), sessionID)
			require.NoError(t, err)
			state.Phase = tt.phase
			if tt.noNewContent {
				state.LastInteractionTime = nil
				state.CheckpointTranscriptStart = 2
				state.CheckpointTranscriptSize = shadowTranscriptSize(t, repo, state)
			}
			if tt.ownsTrailer {
				state.LastCheckpointID = id.MustCheckpointID(trailerID)
			}
			if tt.endedAndCondensed {
				state.FullyCondensed = true
			}
			if tt.signalAlreadyReported {
				state.CommitCondensedSignalCheckpointID = id.MustCheckpointID(trailerID).String()
			}
			require.NoError(t, s.saveSessionState(context.Background(), state))

			if tt.sequenceOp {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "rebase-merge"), 0o750))
			}

			commitWithCheckpointTrailer(t, repo, dir, trailerID)

			logs := captureStrategyLogs(t, func(ctx context.Context) {
				require.NoError(t, s.PostCommit(ctx))
			})

			if tt.wantLogged {
				assert.Contains(t, logs, unclaimedTrailerLogMessage,
					"expected the unclaimed-trailer line")
				assert.Contains(t, logs, `"level":"DEBUG"`,
					"DEBUG, not WARN: the condition has false-positive classes this cannot exclude")
				return
			}
			assert.NotContains(t, logs, unclaimedTrailerLogMessage,
				"the trailer resolves to a real checkpoint here; logging it would be a false positive")
		})
	}
}

// captureStrategyLogs runs fn with a real Logger writing to a temp directory and
// returns everything it emitted. A real Logger rather than a fake handler keeps
// the assertion honest about level, since that is half of what is under test.
func captureStrategyLogs(t *testing.T, fn func(ctx context.Context)) string {
	t.Helper()

	worktree := t.TempDir()
	logger, err := logging.New(logging.Config{
		Root:  entiredir.OpenerAt(worktree),
		Dir:   logging.LogsName,
		Level: slog.LevelDebug,
	})
	require.NoError(t, err)

	fn(logging.WithLogger(context.Background(), logger))
	require.NoError(t, logger.Close())

	content, err := os.ReadFile(filepath.Join(worktree, logging.LogsDir, logging.LogFileName))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(content)
}

// TestPostCommit_PartialCommit_TrailerIsClaimed is the regression guard for inferring
// condensation from session state instead of reading it from
// postCommitProcessSessionLocked. A partial commit condenses successfully and
// then carries the uncommitted files forward, and carryForwardToNewShadowBranch
// clears LastCheckpointID on the way out — so the post-loop state of a
// successful condensation is identical to never having condensed.
// TestPostCommit_ActiveSession_CarryForward_PartialCommit pins that clearing as
// intended behaviour; this pins that the trailer is still counted as claimed.
func TestPostCommit_PartialCommit_TrailerIsClaimed(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-unclaimed-partial-commit"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o750))
	transcript := `{"type":"human","message":{"content":"create files A B C"}}
{"type":"assistant","message":{"content":"creating files"}}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o600))

	for _, name := range []string{"A.txt", "B.txt", "C.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
	}
	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:     sessionID,
		NewFiles:      []string{"A.txt", "B.txt", "C.txt"},
		MetadataDir:   metadataDir,
		CommitMessage: "Checkpoint: files A, B, C",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(context.Background(), state))

	// Commit A and B but not C, so condensation succeeds and carry-forward runs.
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("A.txt")
	require.NoError(t, err)
	_, err = wt.Add("B.txt")
	require.NoError(t, err)
	_, err = wt.Commit(
		"commit A and B\n\n"+trailers.CheckpointTrailerKey+": cf1cf2cf3cf4\n",
		&git.CommitOptions{Author: &object.Signature{
			Name: "Test", Email: "test@test.com", When: time.Now(),
		}})
	require.NoError(t, err)

	logs := captureStrategyLogs(t, func(ctx context.Context) {
		require.NoError(t, s.PostCommit(ctx))
	})

	state, err = s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{"C.txt"}, state.FilesTouched,
		"precondition: carry-forward must have run, which is what clears LastCheckpointID")
	require.Empty(t, state.LastCheckpointID,
		"precondition: carry-forward cleared the ID the warning must not depend on")

	assert.NotContains(t, logs, unclaimedTrailerLogMessage,
		"a partial commit condensed successfully; the trailer resolves")
}
