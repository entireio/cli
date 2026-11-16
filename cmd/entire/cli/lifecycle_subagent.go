package cli

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// noteSubagentFollowUp handles the subagent side of a dropped follow-up
// TurnStart: the session is still mid-turn, but it may be a subagent or a
// parent resumed by its subagent's reply.
func noteSubagentFollowUp(ctx context.Context, ag agent.Agent, event *agent.Event) {
	// A subagent that never edits a file reaches no ToolUse; its
	// follow-up calls are where it can still be marked, which keeps its
	// waiting parent's interaction time fresh.
	markSubagentSession(ctx, ag, event.SessionID, event.SessionRef)
	// The session runs again (a parent resumed by its subagent's reply):
	// its own next Stop decides whether the turn has ended.
	if err := recordAwaitingSubagents(ctx, event.SessionID, false); err != nil {
		logging.Warn(logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name()), "failed to clear awaiting-subagents mark",
			slog.String("session_id", event.SessionID), slog.String("error", err.Error()))
	}
}

// dispatchTurnEnd routes a TurnEnd: a Stop that pauses to wait for subagents
// only records the pause, and a Stop for a turn already ended for its
// subagents is dropped; any other ends the turn.
func dispatchTurnEnd(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	if event.AwaitingSubagents {
		if err := recordAwaitingSubagents(ctx, event.SessionID, true); err != nil {
			return err
		}
		// The last subagent may have finished just before this Stop, in a
		// hook that found no pause to act on; check from this side too.
		endParentTurnIfSubagentsDone(ctx, ag, event.SessionID)
		return nil
	}
	if subagentTurnEndClaimed(ctx, event.SessionID) {
		logging.Info(logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name()),
			"dropping TurnEnd for a turn already ended for its subagents",
			slog.String("session_id", event.SessionID))
		return nil
	}
	return handleLifecycleTurnEnd(ctx, ag, event)
}

// markSubagentSession resolves whether sessionID is a subagent's own session
// and, when it is, records that on both sides: ParentSessionID on the
// subagent's state, which keeps it out of commit linking and standalone
// condensation, and a live task record on the parent, so a commit made while
// the subagent still runs carries its transcript so far. The live record holds
// no files: the subagent's file changes go to the parent's FilesTouched as they
// happen, and the record's own list is filled at turn end from changes no
// commit has taken. Best effort: a failure leaves the session top-level.
func markSubagentSession(ctx context.Context, ag agent.Agent, sessionID, transcriptRef string) (agent.SubagentSessionLink, bool) {
	if _, ok := agent.AsSubagentSessionResolver(ag); !ok {
		return agent.SubagentSessionLink{}, false
	}
	logCtx := logging.WithComponent(ctx, "lifecycle")
	// Unreadable state resolves from the transcript below.
	child, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		logging.Warn(logCtx, "failed to load subagent session state",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}

	// Already marked, with its live record on the parent: rebuild the link from
	// that record instead of re-reading both transcripts on every hook.
	if child.IsSubagentSession() {
		if parent, err := strategy.LoadSessionState(ctx, child.ParentSessionID); err == nil && parent != nil {
			if rec := findTaskRecordByAgent(parent, sessionID); rec != nil {
				// Both sides age into IsStuckActive without hooks of their own:
				// the waiting parent fires none, and a stuck-looking child would
				// count as done to it while still running.
				touchInteraction(ctx, logCtx, parent)
				touchInteraction(ctx, logCtx, child)
				return agent.SubagentSessionLink{
					ParentSessionID: child.ParentSessionID,
					ToolUseID:       rec.ToolUseID,
					SubagentType:    rec.SubagentType,
					TaskDescription: rec.TaskDescription,
				}, true
			}
		}
	}

	link, ok := resolveSubagentSessionLink(ctx, ag, transcriptRef)
	if !ok || link.ParentSessionID == sessionID {
		return agent.SubagentSessionLink{}, false
	}
	if child != nil && child.ParentSessionID != link.ParentSessionID {
		err := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
			state.ParentSessionID = link.ParentSessionID
			return nil
		})
		if err != nil && !errors.Is(err, strategy.ErrStateNotFound) {
			logging.Warn(logCtx, "failed to mark subagent session",
				slog.String("session_id", sessionID), slog.String("error", err.Error()))
		}
	}
	if parent, err := strategy.LoadSessionState(ctx, link.ParentSessionID); err != nil || parent == nil {
		if err := GetStrategy(ctx).EnsureSessionExists(ctx, link.ParentSessionID, ag.Type()); err != nil {
			logging.Warn(logCtx, "failed to ensure parent session for subagent",
				slog.String("parent_session_id", link.ParentSessionID), slog.String("error", err.Error()))
			return link, true
		}
	}
	now := time.Now()
	if err := strategy.MutateSessionState(ctx, link.ParentSessionID, func(state *strategy.SessionState) error {
		state.EnsureTaskRecord(session.TaskRecord{
			ToolUseID:              link.ToolUseID,
			AgentID:                sessionID,
			StartedAt:              now,
			SubagentType:           link.SubagentType,
			TaskDescription:        link.TaskDescription,
			DeclaredTranscriptPath: transcriptRef,
		})
		state.LastInteractionTime = &now
		return nil
	}); err != nil {
		logging.Warn(logCtx, "failed to record live subagent task on parent",
			slog.String("parent_session_id", link.ParentSessionID), slog.String("error", err.Error()))
	}
	return link, true
}

// findTaskRecordByAgent returns the parent's task record for the subagent
// session agentID, nil if none.
func findTaskRecordByAgent(parent *strategy.SessionState, agentID string) *session.TaskRecord {
	for i := range parent.TaskRecords {
		if parent.TaskRecords[i].AgentID == agentID {
			return &parent.TaskRecords[i]
		}
	}
	return nil
}

// subagentInteractionRefresh bounds how often a subagent's hooks refresh its
// own and its parent's LastInteractionTime.
const subagentInteractionRefresh = time.Minute

// touchInteraction refreshes state's LastInteractionTime, at most once per
// subagentInteractionRefresh. It keeps a subagent and the parent waiting on it
// from aging into IsStuckActive while the subagent works: a stuck parent's
// wake-up would restart its turn, and a stuck subagent counts as done.
func touchInteraction(ctx, logCtx context.Context, state *strategy.SessionState) {
	if state.LastInteractionTime != nil && time.Since(*state.LastInteractionTime) < subagentInteractionRefresh {
		return
	}
	now := time.Now()
	if err := strategy.MutateSessionState(ctx, state.SessionID, func(s *strategy.SessionState) error {
		s.LastInteractionTime = &now
		return nil
	}); err != nil && !errors.Is(err, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to refresh session interaction time",
			slog.String("session_id", state.SessionID), slog.String("error", err.Error()))
	}
}

// claimSubagentTurnEnd claims ending the session's current turn for its
// subagents: it clears the pause and records the claim, and reports whether
// this call made it, so of the hooks that can find the subagents done exactly
// one ends the turn, and a pause reported afterwards cannot re-arm it.
func claimSubagentTurnEnd(ctx context.Context, sessionID string) bool {
	claimed := false
	err := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		if state.AwaitingSubagentsAt == nil || state.SubagentTurnEndClaimed || state.Phase != session.PhaseActive {
			return strategy.ErrMutationSkip
		}
		state.AwaitingSubagentsAt = nil
		state.SubagentTurnEndClaimed = true
		claimed = true
		return nil
	})
	if err != nil && !errors.Is(err, strategy.ErrStateNotFound) {
		logging.Warn(logging.WithComponent(ctx, "lifecycle"), "failed to claim the subagent turn end",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
	return err == nil && claimed
}

// subagentTurnEndClaimed reports whether the session's current turn was
// claimed to be ended for its subagents. A Stop the agent reports afterwards
// belongs to that same turn, whichever phase it finds: ACTIVE means the claimed
// turn end is still running, IDLE that it finished and saved the turn. Either
// way a second turn end would save the turn's step and tokens again. A failed
// claimed end releases the claim, and the next TurnStart clears it.
func subagentTurnEndClaimed(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	state, err := strategy.LoadSessionState(ctx, sessionID)
	return err == nil && state != nil && state.SubagentTurnEndClaimed
}

// resetSubagentPause clears the pause and the claim at a new turn. Best
// effort: a failure is logged.
func resetSubagentPause(ctx, logCtx context.Context, sessionID string) {
	err := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		if state.AwaitingSubagentsAt == nil && !state.SubagentTurnEndClaimed {
			return strategy.ErrMutationSkip
		}
		state.AwaitingSubagentsAt = nil
		state.SubagentTurnEndClaimed = false
		return nil
	})
	if err != nil && !errors.Is(err, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to clear awaiting-subagents mark",
			slog.String("error", err.Error()))
	}
}

// recordAwaitingSubagents sets or clears AwaitingSubagentsAt. Setting applies
// only to an ACTIVE session: a pause reported after the turn already ended
// marks nothing.
func recordAwaitingSubagents(ctx context.Context, sessionID string, awaiting bool) error {
	if sessionID == "" {
		return nil
	}
	err := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		if !awaiting {
			if state.AwaitingSubagentsAt == nil {
				return strategy.ErrMutationSkip
			}
			state.AwaitingSubagentsAt = nil
			return nil
		}
		if state.Phase != session.PhaseActive || state.AwaitingSubagentsAt != nil || state.SubagentTurnEndClaimed {
			return strategy.ErrMutationSkip
		}
		now := time.Now()
		state.AwaitingSubagentsAt = &now
		return nil
	})
	if errors.Is(err, strategy.ErrStateNotFound) {
		return nil
	}
	return err //nolint:wrapcheck // callers log it with the session ID
}

// endParentTurnIfSubagentsDone ends the parent's turn when it stopped to wait
// for its subagents and they are done: the agent will not report the turn's
// end itself (Antigravity sends one fullyIdle=false Stop and nothing after
// it). Every subagent the turn involved must have stopped, must have taken in
// every message the parent sent it (one still on its way will wake it), and
// every reply it sent must be in the parent's transcript: an unread reply
// wakes the parent, so its turn is not over. A subagent whose agent died
// sends nothing more.
// The turn then ends exactly as a reported one would, from the parent's own
// transcript and turn start, so its tokens and files count once.
//
// agy can still wake the parent afterwards: when a subagent keeps calling tools
// after its last reply, agy sends the parent a system notice that it has gone
// idle. That wake starts a new turn. Nothing is counted twice or lost; the
// parent's work is only split across two turns.
func endParentTurnIfSubagentsDone(ctx context.Context, ag agent.Agent, parentID string) {
	counter, ok := agent.AsSubagentReplyCounter(ag)
	if !ok {
		return
	}
	logCtx := logging.WithComponent(ctx, "lifecycle")
	parent, err := strategy.LoadSessionState(ctx, parentID)
	if err != nil || parent == nil || parent.Phase != session.PhaseActive ||
		parent.AwaitingSubagentsAt == nil || parent.SubagentTurnEndClaimed || parent.TranscriptPath == "" {
		return
	}
	turnStart := 0
	if pre, preErr := LoadPrePromptState(ctx, parentID); preErr == nil && pre != nil {
		turnStart = pre.TranscriptOffset
	}
	subagents, err := counter.TurnSubagents(parent.TranscriptPath, turnStart)
	if err != nil || len(subagents) == 0 {
		return // not waiting on subagents (a background command), or unknown
	}
	for id, exchange := range subagents {
		if !subagentDone(ctx, counter, id, parentID, exchange) {
			return
		}
	}
	if !claimSubagentTurnEnd(ctx, parentID) {
		return
	}
	logging.Info(logCtx, "ending turn of a session whose subagents finished after it stopped",
		slog.String("session_id", parentID))
	if err := handleLifecycleTurnEnd(ctx, ag, &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  parentID,
		SessionRef: parent.TranscriptPath,
		Timestamp:  time.Now(),
	}); err != nil {
		logging.Warn(logCtx, "failed to end the parent's turn",
			slog.String("session_id", parentID), slog.String("error", err.Error()))
		if rbErr := releaseSubagentTurnEndClaim(ctx, parentID); rbErr != nil {
			logging.Warn(logCtx, "failed to release the parent's turn-end claim",
				slog.String("session_id", parentID), slog.String("error", rbErr.Error()))
		}
	}
}

// releaseSubagentTurnEndClaim undoes claimSubagentTurnEnd after the turn end it
// claimed failed, so a later hook can retry: the pause is restored and the
// claim cleared. A turn that still reached IDLE is left alone; it ended, and a
// retry would count it again.
func releaseSubagentTurnEndClaim(ctx context.Context, sessionID string) error {
	err := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		if !state.SubagentTurnEndClaimed || state.Phase != session.PhaseActive {
			return strategy.ErrMutationSkip
		}
		now := time.Now()
		state.AwaitingSubagentsAt = &now
		state.SubagentTurnEndClaimed = false
		return nil
	})
	if errors.Is(err, strategy.ErrStateNotFound) {
		return nil
	}
	return err //nolint:wrapcheck // the caller logs it with the session ID
}

// subagentDone reports whether a subagent is finished from its parent's point
// of view: its session is not running, and either its agent died or it has
// taken in every message the parent sent it and the parent has received every
// reply it sent. A subagent without a session yet is starting, not done; nor
// is an IDLE one the parent just messaged, before its next hook marks it
// running again.
//
// A stuck-looking subagent (ACTIVE with no hook past StuckActiveThreshold)
// counts as dead only when its owner's liveness is unknown. With a recorded,
// checkable owner, OwnerExited already answers that: a live owner means the
// subagent may still be doing long hookless work, and ending the parent's turn
// then would split it early.
func subagentDone(ctx context.Context, counter agent.SubagentReplyCounter, subagentID, parentID string, parentSide agent.SubagentExchange) bool {
	state, err := strategy.LoadSessionState(ctx, subagentID)
	if err != nil || state == nil {
		return false
	}
	if state.OwnerExited() {
		return true
	}
	if state.IsStuckActive() && state.OwnerLiveness() == proclive.LivenessUnknown {
		return true
	}
	if state.Phase.IsActive() || state.TranscriptPath == "" {
		return false
	}
	childSide, err := counter.ExchangeWithParent(state.TranscriptPath, parentID)
	return err == nil && childSide.Replies <= parentSide.Replies && childSide.Messages >= parentSide.Messages
}
