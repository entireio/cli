package strategy

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/brainnotify"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

type capturedBrainCheckpointHint struct {
	repoRoot  string
	event     brainnotify.Event
	sessionID string
	branch    string
}

func captureBrainCheckpointHints(t *testing.T) *[]capturedBrainCheckpointHint {
	t.Helper()
	originalSend := sendBrainCheckpointHint
	t.Cleanup(func() { sendBrainCheckpointHint = originalSend })

	var hints []capturedBrainCheckpointHint
	sendBrainCheckpointHint = func(repoRoot string, event brainnotify.Event, sessionID, branch string) {
		hints = append(hints, capturedBrainCheckpointHint{
			repoRoot:  repoRoot,
			event:     event,
			sessionID: sessionID,
			branch:    branch,
		})
	}
	return &hints
}

func TestPostCommitSendsCheckpointHintForEachCondensedSession(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	hints := captureBrainCheckpointHints(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	s := &ManualCommitStrategy{}
	sessionIDs := []string{"brain-checkpoint-a", "brain-checkpoint-b"}
	files := []string{"brain-a.txt", "brain-b.txt"}

	for i, sessionID := range sessionIDs {
		setupSessionWithCheckpointAndFile(t, s, dir, sessionID, files[i])
		state, loadErr := s.loadSessionState(context.Background(), sessionID)
		require.NoError(t, loadErr)
		state.Phase = session.PhaseIdle
		state.FilesTouched = []string{files[i]}
		require.NoError(t, s.saveSessionState(context.Background(), state))
	}

	commitFilesWithTrailer(t, repo, dir, "abc123def456", files...)
	require.NoError(t, s.PostCommit(context.Background()))

	repoRoot, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	branch := GetCurrentBranchName(repo)
	require.Len(t, *hints, 2)
	gotSessions := []string{(*hints)[0].sessionID, (*hints)[1].sessionID}
	slices.Sort(gotSessions)
	slices.Sort(sessionIDs)
	require.Equal(t, sessionIDs, gotSessions)
	for _, hint := range *hints {
		require.Equal(t, repoRoot, hint.repoRoot)
		require.Equal(t, brainnotify.EventCheckpoint, hint.event)
		require.Equal(t, branch, hint.branch)
	}
}

func TestPostCommitDoesNotSendCheckpointHintWhenCondensationIsSkipped(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	hints := captureBrainCheckpointHints(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	s := &ManualCommitStrategy{}
	sessionID := "brain-checkpoint-skipped"
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(context.Background(), state))

	// PostCommit deliberately skips all lifecycle transitions during a Git
	// sequence operation, so no canonical checkpoint is produced.
	require.NoError(t, createRebaseMarker(dir))
	commitWithCheckpointTrailer(t, repo, dir, "abc123def456")
	require.NoError(t, s.PostCommit(context.Background()))
	require.Empty(t, *hints)
}

func TestPostCommitDoesNotSendCheckpointHintWhenCondensationFails(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	hints := captureBrainCheckpointHints(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	s := &ManualCommitStrategy{}
	sessionID := "brain-checkpoint-failed"
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(context.Background(), state))

	// Force CondenseSession itself to return an error after PostCommit selects
	// this active session for condensation.
	writeUnsupportedCheckpointPolicy(t, repo)
	commitWithCheckpointTrailer(t, repo, dir, "abc123def456")
	require.NoError(t, s.PostCommit(context.Background()))

	state, err = s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.Positive(t, state.StepCount, "failed condensation must remain retryable")
	require.Empty(t, *hints)
}

func createRebaseMarker(dir string) error {
	return os.MkdirAll(filepath.Join(dir, ".git", "rebase-merge"), 0o755)
}
