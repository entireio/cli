package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// Session state management functions shared across all strategies.
// SessionState is stored in .git/entire-sessions/{session_id}.json

// getSessionStateDir returns the path to the session state directory.
// This is stored in the git common dir so it's shared across all worktrees.
func getSessionStateDir(ctx context.Context) (string, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, session.SessionStateDirName), nil
}

// sessionStateFile returns the path to a session state file.
func sessionStateFile(ctx context.Context, sessionID string) (string, error) {
	stateDir, err := getSessionStateDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, sessionID+".json"), nil
}

// LoadSessionState loads the session state for the given session ID.
// Returns (nil, nil) when session file doesn't exist or session is stale (not an error condition).
// Stale sessions are automatically deleted by the underlying StateStore.
func LoadSessionState(ctx context.Context, sessionID string) (*SessionState, error) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create state store: %w", err)
	}

	state, err := store.Load(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session state: %w", err)
	}
	return state, nil
}

// SaveSessionState saves the session state atomically.
func SaveSessionState(ctx context.Context, state *SessionState) error {
	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(state.SessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	stateDir, err := getSessionStateDir(ctx)
	if err != nil {
		return fmt.Errorf("failed to get session state directory: %w", err)
	}

	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("failed to create session state directory: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session state: %w", err)
	}

	stateFile, err := sessionStateFile(ctx, state.SessionID)
	if err != nil {
		return fmt.Errorf("failed to get session state file path: %w", err)
	}

	// Atomic write: write to temp file, then rename
	tmpFile := stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write session state: %w", err)
	}
	if err := os.Rename(tmpFile, stateFile); err != nil {
		return fmt.Errorf("failed to rename session state file: %w", err)
	}
	return nil
}

// ListSessionStates returns all session states from the state directory.
// This is a package-level function that doesn't require a specific strategy instance.
func ListSessionStates(ctx context.Context) ([]*SessionState, error) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create state store: %w", err)
	}

	states, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}
	return states, nil
}

// FindMostRecentSession returns the session ID of the most recently interacted session
// (by LastInteractionTime) in the current worktree. Returns empty string if no sessions exist.
// Scoping to the current worktree prevents cross-worktree pollution in log routing.
// Falls back to unfiltered search if the worktree path can't be determined.
func FindMostRecentSession(ctx context.Context) string {
	states, err := ListSessionStates(ctx)
	if err != nil || len(states) == 0 {
		return ""
	}

	// Scope to current worktree to prevent cross-worktree pollution.
	worktreePath, wpErr := paths.WorktreeRoot(ctx)
	if wpErr == nil && worktreePath != "" {
		var filtered []*SessionState
		for _, s := range states {
			if s.WorktreePath == worktreePath {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) > 0 {
			states = filtered
		}
		// If no sessions match the worktree, fall back to all sessions
	}

	var best *SessionState
	for _, s := range states {
		if s.LastInteractionTime == nil {
			continue
		}
		if best == nil || s.LastInteractionTime.After(*best.LastInteractionTime) {
			best = s
		}
	}
	if best != nil {
		return best.SessionID
	}

	// Fallback: return most recently started session
	for _, s := range states {
		if best == nil || s.StartedAt.After(best.StartedAt) {
			best = s
		}
	}
	if best != nil {
		return best.SessionID
	}
	return ""
}

// TransitionAndLog runs a session phase transition, applies actions via the
// handler, and logs the transition. Returns the first handler error from
// ApplyTransition (if any) so callers can surface it. The error is also
// logged internally for diagnostics.
// This is the single entry point for all state machine transitions to ensure
// consistent logging of phase changes.
func TransitionAndLog(goCtx context.Context, state *SessionState, event session.Event, ctx session.TransitionContext, handler session.ActionHandler) error {
	oldPhase := state.Phase
	result := session.Transition(oldPhase, event, ctx)
	logCtx := logging.WithComponent(goCtx, "session")

	handlerErr := session.ApplyTransition(goCtx, state, result, handler)
	if handlerErr != nil {
		logging.Error(logCtx, "action handler error during transition",
			slog.String("session_id", state.SessionID),
			slog.String("event", event.String()),
			slog.Any("error", handlerErr),
		)
	}

	if result.NewPhase != oldPhase {
		logging.Info(logCtx, "phase transition",
			slog.String("session_id", state.SessionID),
			slog.String("event", event.String()),
			slog.String("from", string(oldPhase)),
			slog.String("to", string(result.NewPhase)),
		)
	} else {
		logging.Debug(logCtx, "phase unchanged",
			slog.String("session_id", state.SessionID),
			slog.String("event", event.String()),
			slog.String("phase", string(result.NewPhase)),
			slog.Any("result", result),
		)
	}

	if handlerErr != nil {
		return fmt.Errorf("transition %s: %w", event, handlerErr)
	}
	return nil
}

// StoreModelHint writes the LLM model name to a lightweight hint file
// (.git/entire-sessions/{session_id}.model) for cross-process persistence.
//
// Why a separate file instead of SessionState?
//
// SessionState requires BaseCommit (used for shadow branch naming, checkpoint
// writing, doctor classification, etc.) and is only created during TurnStart
// when the git repo is fully inspected. Some agents report the model on earlier
// hooks that fire as separate CLI processes before TurnStart:
//
//   - Claude Code sends "model" on SessionStart (before any TurnStart)
//   - Gemini CLI sends "llm_request.model" on BeforeModel (after TurnStart,
//     so handleLifecycleModelUpdate writes to SessionState directly when it
//     exists and only falls back to this hint file otherwise)
//
// The hint is read by handleLifecycleTurnStart/TurnEnd when event.Model is
// empty, passed to InitializeSession, and persisted in state.ModelName. After
// that the hint file is redundant — it sits unused until ClearSessionState
// removes it alongside the session state file.
func StoreModelHint(ctx context.Context, sessionID, model string) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	if model == "" {
		return nil
	}

	stateDir, err := getSessionStateDir(ctx)
	if err != nil {
		return fmt.Errorf("failed to get session state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("failed to create session state directory: %w", err)
	}

	hintFile := filepath.Join(stateDir, sessionID+".model")
	if err := os.WriteFile(hintFile, []byte(model), 0o600); err != nil {
		return fmt.Errorf("failed to write model hint file: %w", err)
	}
	return nil
}

// LoadModelHint reads the LLM model name from the hint file for the given session.
// Returns empty string if the hint file doesn't exist or can't be read.
func LoadModelHint(ctx context.Context, sessionID string) string {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return ""
	}

	stateDir, err := getSessionStateDir(ctx)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "session"), "failed to resolve state dir for model hint",
			slog.String("session_id", sessionID),
			slog.Any("error", err))
		return ""
	}

	hintPath := filepath.Join(stateDir, sessionID+".model")
	data, err := os.ReadFile(hintPath) //nolint:gosec // sessionID is validated above
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Warn(logging.WithComponent(ctx, "session"), "failed to read model hint file",
				slog.String("path", hintPath),
				slog.Any("error", err))
		}
		return ""
	}
	return strings.TrimSpace(string(data))
}

// StoreAgentTypeHint records the agent type that owns a session before
// SessionState exists. Used by the lifecycle dispatcher when SessionStart fires
// (state isn't created until TurnStart, so we need a place to remember which
// agent claimed the session first).
//
// Semantics: first writer wins. When multiple agents fire hooks for the same
// session ID — e.g., Cursor IDE running cursor-agent while also forwarding to
// Claude Code's hook system — only the agent that fires SessionStart first
// gets recorded. Subsequent calls return nil without overwriting.
//
// At TurnStart, InitializeSession reads this hint to override agentType when
// the hook firing isn't the same agent that owns the session. After the state
// file is written, the hint is unused but remains until ClearSessionState
// removes it alongside the state file.
//
// Returns (created=true) when this call wrote the hint, (created=false) when
// the hint already existed (no-op) or agentType was empty/Unknown.
//
// Banner display is gated separately via ClaimSessionStartBanner — winning
// the ownership claim does NOT mean this agent should also print the banner,
// because the winner may not implement HookResponseWriter (e.g., Cursor).
func StoreAgentTypeHint(ctx context.Context, sessionID string, agentType types.AgentType) (created bool, err error) {
	if vErr := validation.ValidateSessionID(sessionID); vErr != nil {
		return false, fmt.Errorf("invalid session ID: %w", vErr)
	}
	if agentType == "" || agentType == agent.AgentTypeUnknown {
		return false, nil
	}

	stateDir, sErr := getSessionStateDir(ctx)
	if sErr != nil {
		return false, fmt.Errorf("failed to get session state directory: %w", sErr)
	}
	if mErr := os.MkdirAll(stateDir, 0o750); mErr != nil {
		return false, fmt.Errorf("failed to create session state directory: %w", mErr)
	}

	hintFile := filepath.Join(stateDir, sessionID+".agent")
	f, oErr := os.OpenFile(hintFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // hintFile path is built from validated sessionID
	if oErr != nil {
		if errors.Is(oErr, os.ErrExist) {
			// First-writer-wins: another caller already claimed this session.
			return false, nil
		}
		return false, fmt.Errorf("failed to create agent hint file: %w", oErr)
	}
	defer f.Close()
	if _, wErr := f.WriteString(string(agentType)); wErr != nil {
		return false, fmt.Errorf("failed to write agent hint file: %w", wErr)
	}
	return true, nil
}

// ClaimSessionStartBanner records that the SessionStart banner has been emitted
// for a session. First-writer-wins semantics, separate from StoreAgentTypeHint
// so a non-banner-capable agent winning the ownership race (e.g. Cursor, which
// doesn't implement HookResponseWriter) doesn't suppress the banner from a
// banner-capable agent that fires SessionStart for the same session.
//
// Callers MUST only invoke this from within the HookResponseWriter branch — the
// claim represents "a banner was actually shown", not just "an agent considered
// showing one". Otherwise a non-writer claimant would re-introduce the bug.
//
// Returns (claimed=true) when this call won the race and the caller should
// emit the banner; (claimed=false) when an earlier call already claimed it.
func ClaimSessionStartBanner(ctx context.Context, sessionID string) (claimed bool, err error) {
	if vErr := validation.ValidateSessionID(sessionID); vErr != nil {
		return false, fmt.Errorf("invalid session ID: %w", vErr)
	}

	stateDir, sErr := getSessionStateDir(ctx)
	if sErr != nil {
		return false, fmt.Errorf("failed to get session state directory: %w", sErr)
	}
	if mErr := os.MkdirAll(stateDir, 0o750); mErr != nil {
		return false, fmt.Errorf("failed to create session state directory: %w", mErr)
	}

	markerFile := filepath.Join(stateDir, sessionID+".banner")
	f, oErr := os.OpenFile(markerFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // markerFile path is built from validated sessionID
	if oErr != nil {
		if errors.Is(oErr, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to create banner marker file: %w", oErr)
	}
	_ = f.Close()
	return true, nil
}

// LoadAgentTypeHint reads the agent type hint written by SessionStart.
// Returns empty string if the hint file doesn't exist, can't be read, or the
// value isn't a registered agent type.
func LoadAgentTypeHint(ctx context.Context, sessionID string) types.AgentType {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return ""
	}

	stateDir, err := getSessionStateDir(ctx)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "session"), "failed to resolve state dir for agent hint",
			slog.String("session_id", sessionID),
			slog.Any("error", err))
		return ""
	}

	hintPath := filepath.Join(stateDir, sessionID+".agent")
	data, err := os.ReadFile(hintPath) //nolint:gosec // sessionID is validated above
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Warn(logging.WithComponent(ctx, "session"), "failed to read agent hint file",
				slog.String("path", hintPath),
				slog.Any("error", err))
		}
		return ""
	}
	return types.AgentType(strings.TrimSpace(string(data)))
}

// ClearSessionState removes the session state file for the given session ID.
func ClearSessionState(ctx context.Context, sessionID string) error {
	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	stateDir, err := getSessionStateDir(ctx)
	if err != nil {
		return fmt.Errorf("failed to get session state directory: %w", err)
	}

	// Remove all files for this session (state .json, .model hint, any future hint files).
	matches, _ := filepath.Glob(filepath.Join(stateDir, sessionID+".*")) //nolint:errcheck // pattern is always valid
	for _, f := range matches {
		_ = os.Remove(f)
	}

	return nil
}
