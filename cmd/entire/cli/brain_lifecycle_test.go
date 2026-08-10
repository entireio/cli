package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/brainnotify"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

type capturedBrainLifecycleHint struct {
	repoRoot  string
	event     brainnotify.Event
	sessionID string
	branch    string
}

func captureBrainLifecycleHints(t *testing.T) *[]capturedBrainLifecycleHint {
	t.Helper()
	originalSend := sendBrainLifecycleHint
	t.Cleanup(func() { sendBrainLifecycleHint = originalSend })

	var hints []capturedBrainLifecycleHint
	sendBrainLifecycleHint = func(repoRoot string, event brainnotify.Event, sessionID, branch string) {
		hints = append(hints, capturedBrainLifecycleHint{
			repoRoot:  repoRoot,
			event:     event,
			sessionID: sessionID,
			branch:    branch,
		})
	}
	return &hints
}

func TestSessionStartSendsBrainLifecycleHint(t *testing.T) {
	setupStopTestRepo(t)
	hints := captureBrainLifecycleHints(t)

	repoRoot, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	branch, err := GetCurrentBranch(context.Background())
	require.NoError(t, err)

	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "brain-start-session",
		Timestamp: time.Now(),
	}
	require.NoError(t, handleLifecycleSessionStart(context.Background(), newMockAgent(), event))

	require.Equal(t, []capturedBrainLifecycleHint{{
		repoRoot:  repoRoot,
		event:     brainnotify.EventSessionStart,
		sessionID: event.SessionID,
		branch:    branch,
	}}, *hints)
}

type failingHookResponseAgent struct {
	mockLifecycleAgent
}

func (f *failingHookResponseAgent) WriteHookResponse(string) error {
	return errors.New("hook response failed")
}

func TestSessionStartHintIsAdvisoryWhenLaterHookResponseFails(t *testing.T) {
	setupStopTestRepo(t)
	hints := captureBrainLifecycleHints(t)

	ag := &failingHookResponseAgent{mockLifecycleAgent: *newMockAgent()}
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "brain-start-advisory",
		Timestamp: time.Now(),
	}
	err := handleLifecycleSessionStart(context.Background(), ag, event)
	require.ErrorContains(t, err, "failed to write hook response")
	require.Len(t, *hints, 1)
	require.Equal(t, brainnotify.EventSessionStart, (*hints)[0].event)
	require.Equal(t, event.SessionID, (*hints)[0].sessionID)
}

func TestBrainLifecycleHintOmitsBranchOnDetachedHead(t *testing.T) {
	setupStopTestRepo(t)
	hints := captureBrainLifecycleHints(t)
	ctx := context.Background()

	repo, err := strategy.OpenRepository(ctx)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, head.Hash())))
	repo.Close()

	notifyBrainLifecycleEvent(ctx, brainnotify.EventSessionStart, "brain-detached-session")

	require.Len(t, *hints, 1)
	require.Empty(t, (*hints)[0].branch)
}

func TestSessionEndSendsBrainLifecycleHintAfterStateTransition(t *testing.T) {
	setupStopTestRepo(t)
	hints := captureBrainLifecycleHints(t)
	ctx := context.Background()

	repoRoot, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)
	branch, err := GetCurrentBranch(ctx)
	require.NoError(t, err)
	repo, err := strategy.OpenRepository(ctx)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	repo.Close()

	sessionID := "brain-end-session"
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:    sessionID,
		BaseCommit:   head.Hash().String(),
		WorktreePath: repoRoot,
		Branch:       branch,
		StartedAt:    time.Now(),
		Phase:        session.PhaseIdle,
	}))

	require.NoError(t, handleLifecycleSessionEnd(ctx, newMockAgent(), &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: sessionID,
		Timestamp: time.Now(),
	}))

	state, err := strategy.LoadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.EndedAt)
	require.Equal(t, []capturedBrainLifecycleHint{{
		repoRoot:  repoRoot,
		event:     brainnotify.EventSessionEnd,
		sessionID: sessionID,
		branch:    branch,
	}}, *hints)
}

func TestEmptySessionIDsDoNotSendBrainLifecycleHints(t *testing.T) {
	hints := captureBrainLifecycleHints(t)
	ag := newMockAgent()

	require.Error(t, handleLifecycleSessionStart(context.Background(), ag, &agent.Event{Type: agent.SessionStart}))
	require.NoError(t, handleLifecycleSessionEnd(context.Background(), ag, &agent.Event{Type: agent.SessionEnd}))
	require.Empty(t, *hints)
}
