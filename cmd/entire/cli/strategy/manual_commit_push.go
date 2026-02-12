package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes the entire/checkpoints/v1 branch alongside the user's push.
// Configuration options (stored in .entire/settings.json under strategy_options.push_sessions):
//   - "auto": always push automatically
//   - "prompt" (default): ask user with option to enable auto
//   - "false"/"off"/"no": never push
func (s *ManualCommitStrategy) PrePush(remote string) error {
	s.recordPendingPushRemote(remote)
	return pushSessionsBranchCommon(remote, paths.MetadataBranchName)
}

// flushPendingPush pushes the metadata branch if a push was recorded while
// condensation was deferred, then clears PendingPushRemote. Intended to be
// called via defer so that all exit paths in handleTurnEndCondense clear the
// field.
func (s *ManualCommitStrategy) flushPendingPush(logCtx context.Context, state *SessionState) {
	if state.PendingPushRemote == "" {
		return
	}
	remote := state.PendingPushRemote
	state.PendingPushRemote = ""
	logging.Info(logCtx, "turn-end: pushing checkpoint data", slog.String("remote", remote))
	if pushErr := pushSessionsBranchCommon(remote, paths.MetadataBranchName); pushErr != nil {
		logging.Warn(logCtx, "turn-end: failed to push checkpoint data",
			slog.String("remote", remote), slog.String("error", pushErr.Error()))
	}
}

// recordPendingPushRemote records the push remote on sessions that have deferred
// condensation (ACTIVE_COMMITTED with PendingCheckpointID). When condensation
// completes at turn-end, the metadata branch is pushed to this remote.
func (s *ManualCommitStrategy) recordPendingPushRemote(remote string) {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	worktreePath, err := GetWorktreePath()
	if err != nil {
		logging.Debug(logCtx, "recordPendingPushRemote: failed to get worktree path",
			slog.String("error", err.Error()))
		return
	}

	sessions, err := s.findSessionsForWorktree(worktreePath)
	if err != nil || len(sessions) == 0 {
		return
	}

	for _, state := range sessions {
		if state.Phase != session.PhaseActiveCommitted || state.PendingCheckpointID == "" {
			continue
		}

		state.PendingPushRemote = remote
		if err := s.saveSessionState(state); err != nil {
			logging.Warn(logCtx, "recordPendingPushRemote: failed to save session state",
				slog.String("session_id", state.SessionID),
				slog.String("error", err.Error()))
			continue
		}

		logging.Info(logCtx, "pre-push: recorded pending push remote for deferred condensation",
			slog.String("session_id", state.SessionID),
			slog.String("remote", remote))
	}
}
