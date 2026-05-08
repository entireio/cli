package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to create state store: %w", err)
	}

	if err := store.Save(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
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

// sessionMutationGate provides per-process serialization layered over the
// OS-level flock so that nested MutateSessionState calls in the same
// goroutine don't deadlock or lose updates. POSIX flock isn't reentrant
// across distinct file descriptors in the same process; on top of that, a
// nested call that did its own load → save would have its save overwritten
// by the outer save. The gate fixes both: nested calls in the same
// goroutine reuse the outer's state pointer (no second load, no second
// save), and only the outermost release drops the flock.
//
// Growth: the map accumulates one entry per session ID touched by this
// process and is never trimmed. Fine today because hook invocations are
// short-lived subprocesses; a future long-running daemon (status watcher,
// MCP server) would need a TTL or eviction pass.
var sessionMutationGate sync.Map // map[string]*sessionGate

type sessionGate struct {
	mu          sync.Mutex
	owner       int64 // goroutine ID of the current holder, 0 when unlocked
	depth       int
	flockRel    func()
	activeState *SessionState // shared state pointer for nested mutations
}

// goroutineID extracts the runtime goroutine ID from the stack header. Used
// only as a reentrancy key for the session mutation gate — never as a
// security boundary or for application logic. Returns -1 if the stack
// header doesn't parse: real goroutine IDs are positive, and gate.owner is
// initialised to 0, so a -1 sentinel can't falsely match the freshly-
// constructed gate (or a freshly-released one).
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	const prefix = "goroutine "
	s := string(buf[:n])
	if !strings.HasPrefix(s, prefix) {
		return -1
	}
	s = s[len(prefix):]
	end := strings.IndexByte(s, ' ')
	if end < 0 {
		return -1
	}
	id, err := strconv.ParseInt(s[:end], 10, 64)
	if err != nil {
		return -1
	}
	return id
}

// MutateSessionState is the safe load → mutate → save helper. It takes an
// OS-level advisory lock against .git/entire-session-locks/<id>.lock for the
// duration of the read+write so concurrent processes cannot lose each
// other's updates. fn receives the freshly-loaded state and mutates it in
// place; returning ErrMutationSkip skips the save. Reentrant within the same
// goroutine: nested calls share the outer's state pointer and skip the
// inner load/save, so all mutations are flushed by the outermost call.
//
// fn may hold the lock for slow operations — PostCommit's callback, for
// example, runs CondenseSession (shadow-branch tree builds, transcript
// compaction) inside the gate. That's deliberate: PostToolUse must not slip
// in mid-condense and revert CheckpointTranscriptStart or files_touched.
// A concurrent PostToolUse on the same session waits for the commit to
// finish.
//
// Returns ErrStateNotFound if the state file doesn't exist (event arrived
// before InitializeSession). Errors from fn or from load/save propagate.
//
// All session-state mutations funnel through this helper so the hot-path
// PostToolUse hook cannot revert fields written by lifecycle handlers
// (TurnEnd, PostCommit, ModelUpdate) that ran between our load and our save.
func MutateSessionState(ctx context.Context, sessionID string, fn func(*SessionState) error) error {
	if sessionID == "" {
		return ErrStateNotFound
	}
	gate, isOuter, release, err := acquireSessionGate(ctx, sessionID)
	if err != nil {
		return err
	}
	defer release()

	if !isOuter {
		// Nested call: reuse the outer's state pointer. The outer save will
		// flush our mutations; we don't load or save here.
		if gate.activeState == nil {
			return ErrStateNotFound
		}
		if err := fn(gate.activeState); err != nil && !errors.Is(err, ErrMutationSkip) {
			return err
		}
		return nil
	}

	state, err := LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session state: %w", err)
	}
	if state == nil {
		return ErrStateNotFound
	}
	gate.activeState = state
	defer func() { gate.activeState = nil }()

	if err := fn(state); err != nil {
		if errors.Is(err, ErrMutationSkip) {
			return nil
		}
		return err
	}
	if err := SaveSessionState(ctx, state); err != nil {
		return fmt.Errorf("save session state: %w", err)
	}
	return nil
}

// acquireSessionGate takes the per-process gate (in-memory) and, on the
// outermost call, the cross-process flock. Returns isOuter=true on the
// outermost call so MutateSessionState knows whether to load/save.
func acquireSessionGate(ctx context.Context, sessionID string) (gate *sessionGate, isOuter bool, release func(), err error) {
	val, _ := sessionMutationGate.LoadOrStore(sessionID, &sessionGate{})
	gate, ok := val.(*sessionGate)
	if !ok {
		return nil, false, nil, fmt.Errorf("session gate type assertion failed for %s", sessionID)
	}

	gid := goroutineID()
	gate.mu.Lock()
	if gate.owner == gid {
		gate.depth++
		gate.mu.Unlock()
		return gate, false, func() {
			gate.mu.Lock()
			gate.depth--
			gate.mu.Unlock()
		}, nil
	}
	gate.mu.Unlock()

	lockPath, err := stateLockPath(ctx, sessionID)
	if err != nil {
		return nil, false, nil, fmt.Errorf("resolve state lock path: %w", err)
	}
	flockRel, err := acquireStateFileLock(lockPath)
	if err != nil {
		return nil, false, nil, fmt.Errorf("acquire state lock: %w", err)
	}

	gate.mu.Lock()
	gate.owner = gid
	gate.depth = 1
	gate.flockRel = flockRel
	gate.mu.Unlock()

	return gate, true, func() {
		gate.mu.Lock()
		gate.depth--
		if gate.depth == 0 {
			rel := gate.flockRel
			gate.flockRel = nil
			gate.owner = 0
			gate.mu.Unlock()
			rel()
			return
		}
		gate.mu.Unlock()
	}, nil
}

// ErrMutationSkip signals MutateSessionState to skip the save without
// treating fn's return as an error. Use it when the mutation function
// observes the loaded state and decides no write is needed (for example,
// when a merge produces no new entries).
var ErrMutationSkip = errors.New("session state mutation skipped")

// ErrStateNotFound is returned by MutateSessionState when no state file
// exists for the session ID (typically because the event arrived before
// InitializeSession ran). Callers that need to distinguish "no state"
// from a successful no-op can branch on errors.Is(err, ErrStateNotFound).
var ErrStateNotFound = errors.New("session state not found")

// RecordFilesTouched merges paths into the session's FilesTouched, used by
// mid-turn lifecycle events (per-tool-use hooks) so PostCommit's carry-forward
// decision sees an accurate file list. Caller must pre-normalize paths to
// repo-relative form. No-ops when the session state doesn't exist or the
// merge produced no changes.
func RecordFilesTouched(ctx context.Context, sessionID string, modified, added, deleted []string) error {
	if len(modified) == 0 && len(added) == 0 && len(deleted) == 0 {
		return nil
	}
	err := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		merged := mergeFilesTouched(state.FilesTouched, modified, added, deleted)
		if slices.Equal(merged, state.FilesTouched) {
			return ErrMutationSkip
		}
		state.FilesTouched = merged
		return nil
	})
	if errors.Is(err, ErrStateNotFound) {
		return nil
	}
	return err
}

// stateLockPath returns the lock file path for a session. Lock files live in
// .git/entire-session-locks/ (a sibling to entire-sessions/) so callers that
// enumerate session state files don't have to filter lock entries. A
// separate file (rather than locking the state file itself) keeps the lock
// holder distinct from the data — Save's atomic-rename pattern would
// otherwise unlink the inode the flock is held on.
func stateLockPath(ctx context.Context, sessionID string) (string, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID: %w", err)
	}
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	lockDir := filepath.Join(commonDir, "entire-session-locks")
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		return "", fmt.Errorf("create session lock directory: %w", err)
	}
	return filepath.Join(lockDir, sessionID+".lock"), nil
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

	// Intentionally do NOT remove the per-session lock file under
	// entire-session-locks/. POSIX flock and Windows LockFileEx are bound to
	// the inode/file-handle: unlinking the lock path while another process
	// holds it lets a third caller recreate the file and acquire an
	// independent lock, breaking mutual exclusion. Lock files are 0-byte
	// sentinels and session IDs aren't reused, so leaving them in place is
	// harmless. Bulk cleanup happens via RemoveAll on uninstall.
	return nil
}
