package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// sessionLockDeadlineKey carries an optional wall-clock deadline bounding how
// long acquireSessionGate waits for the cross-process session flock. It exists
// so latency-critical, best-effort callers (the pre-agent TurnStart hook) can
// degrade gracefully instead of blocking behind a long-running lock holder —
// e.g. the previous turn's checkpoint condensation, which holds the same
// per-session lock while it reads and rewrites the multi-MB transcript. Without
// a bound the blocking flock stalls TurnStart for the full duration of that work
// (observed ~30s on large sessions), even though TurnStart's own state update is
// trivial and repaired on the next turn / at turn-end.
//
// The bound is a single absolute deadline shared across every acquisition on the
// path (a caller like TurnStart runs several best-effort mutations), so the
// worst-case contended cost is the budget once — not the budget times the number
// of mutations. A passed deadline only fails an acquisition when the lock is
// actually held; a free lock is always taken (non-blocking), so the deadline
// never penalizes the common uncontended case.
type sessionLockDeadlineKey struct{}

// WithSessionLockWait returns a context whose session-state flock acquisitions
// are bounded to complete within wait from now (a shared wall-clock budget).
// Zero or negative leaves the wait unbounded (the default, used by
// turn-end/condensation which must not drop work). Only the lock acquisition is
// bounded; the mutation itself still runs under the caller's original context.
func WithSessionLockWait(ctx context.Context, wait time.Duration) context.Context {
	if wait <= 0 {
		return ctx
	}
	return context.WithValue(ctx, sessionLockDeadlineKey{}, time.Now().Add(wait))
}

func sessionLockDeadlineFromContext(ctx context.Context) (time.Time, bool) {
	deadline, ok := ctx.Value(sessionLockDeadlineKey{}).(time.Time)
	return deadline, ok
}

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

// openSessionStateRoot creates the session state directory if needed and returns
// an os.Root scoped to it. Hint/marker files are named from the (already
// validated) session ID; routing their writes through os.Root makes escaping
// the directory impossible at the kernel level even if validation were bypassed.
// Callers must Close the returned root.
//
// The root is derived from the git common dir's shared root with
// Root.OpenRoot, not opened on an assembled path. That is what makes the
// containment transitive: the state directory is proven to be a real directory
// inside .git before anything is named within it, so neither the directory nor
// the files under it can be redirected out of the clone.
func openSessionStateRoot(ctx context.Context) (*os.Root, error) {
	commonRoot, err := gitdir.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git common dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(commonRoot, session.SessionStateDirName, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create session state directory: %w", err)
	}
	root, err := osroot.OpenChild(commonRoot, session.SessionStateDirName)
	if err != nil {
		return nil, fmt.Errorf("failed to open session state directory: %w", err)
	}
	return root, nil
}

// openSessionStateRootForRead returns an os.Root scoped to the session state
// directory without creating it. Returns (nil, nil) when the directory does not
// exist, so read paths can treat a missing directory as "no hint".
//
// osroot.OpenChild, not commonRoot.OpenRoot: the write path above is protected
// because MkdirAllNoSymlink refuses a symlinked entire-sessions before this
// runs, but nothing precedes the read path. os.Root follows a symlink that stays
// INSIDE the common dir, so a bare OpenRoot would read another directory's
// session state as this repo's.
func openSessionStateRootForRead(ctx context.Context) (*os.Root, error) {
	commonRoot, err := gitdir.Open(ctx)
	if os.IsNotExist(err) {
		return nil, nil //nolint:nilnil // no common dir = no hint; callers handle nil root
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open git common dir: %w", err)
	}
	root, err := osroot.OpenChild(commonRoot, session.SessionStateDirName)
	if os.IsNotExist(err) {
		return nil, nil //nolint:nilnil // missing dir = no hint; callers handle nil root
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open session state directory: %w", err)
	}
	return root, nil
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
	if filtered := sessionStatesForCurrentWorktree(ctx, states); len(filtered) > 0 {
		states = filtered
		// If no sessions match the worktree, fall back to all sessions.
	}

	return mostRecentSessionID(states)
}

// FindMostRecentSessionInCurrentWorktree returns the most recently interacted
// session from the current worktree only. Unlike FindMostRecentSession, it does
// not fall back to sessions from other worktrees.
func FindMostRecentSessionInCurrentWorktree(ctx context.Context) string {
	states, err := ListSessionStates(ctx)
	if err != nil || len(states) == 0 {
		return ""
	}
	return mostRecentSessionID(sessionStatesForCurrentWorktree(ctx, states))
}

func sessionStatesForCurrentWorktree(ctx context.Context, states []*SessionState) []*SessionState {
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil || worktreePath == "" {
		return nil
	}
	filtered := make([]*SessionState, 0, len(states))
	for _, s := range states {
		if s.WorktreePath == worktreePath {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func mostRecentSessionID(states []*SessionState) string {
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

	root, err := openSessionStateRoot(ctx)
	if err != nil {
		return err
	}
	defer root.Close()

	if err := jsonutil.WriteFileAtomicIn(root, sessionID+".model", []byte(model), 0o600); err != nil {
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

	root, err := openSessionStateRootForRead(ctx)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "session"), "failed to resolve state dir for model hint",
			slog.String("session_id", sessionID),
			slog.Any("error", err))
		return ""
	}
	if root == nil {
		return ""
	}
	defer root.Close()

	data, err := osroot.ReadFileNoFollow(root, sessionID+".model")
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Warn(logging.WithComponent(ctx, "session"), "failed to read model hint file",
				slog.String("session_id", sessionID),
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

	root, rErr := openSessionStateRoot(ctx)
	if rErr != nil {
		return false, rErr
	}
	defer root.Close()

	f, oErr := root.OpenFile(sessionID+".agent", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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

	root, rErr := openSessionStateRoot(ctx)
	if rErr != nil {
		return false, rErr
	}
	defer root.Close()

	f, oErr := root.OpenFile(sessionID+".banner", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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

	root, err := openSessionStateRootForRead(ctx)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "session"), "failed to resolve state dir for agent hint",
			slog.String("session_id", sessionID),
			slog.Any("error", err))
		return ""
	}
	if root == nil {
		return ""
	}
	defer root.Close()

	data, err := osroot.ReadFileNoFollow(root, sessionID+".agent")
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Warn(logging.WithComponent(ctx, "session"), "failed to read agent hint file",
				slog.String("session_id", sessionID),
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
	// afterSave holds post-save effects registered by nested frames, in
	// registration order. Only the outermost frame saves, so only it can
	// know whether those effects are owed; it drains this queue (and clears
	// it) when it exits. Guarded by gate ownership rather than gate.mu: only
	// the owning goroutine ever touches it.
	afterSave []func()
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
//
// A nil return is not proof that anything was written. Callers with a side
// effect that must follow a durable write want MutateSessionStateOnSaved.
func MutateSessionState(ctx context.Context, sessionID string, fn func(*SessionState) error) error {
	return MutateSessionStateOnSaved(ctx, sessionID, fn, nil)
}

// runPostSaveEffect runs one post-save effect, containing a panic to that
// effect rather than letting it escape.
//
// Two things make propagating wrong here, and neither is about the effect being
// likely to panic. The effects are best-effort telemetry — a settings read and a
// detached spawn — and they run from the outermost frame's defer, so an escaping
// panic both skips every effect queued behind it and unwinds out of
// MutateSessionStateOnSaved into callers that do not recover (PostCommit),
// killing a git hook mid-commit over a signal that is fail-open everywhere else.
// Note the asymmetry this closes: a panic in the mutation function is already
// handled deliberately (the queue is discarded, and the gate is released first
// so it stays usable), so the effects were the one path where the same
// discipline was not applied.
//
// The panic is logged rather than swallowed. It is a bug wherever it comes from,
// and .entire/logs is where the next person looks for it; a silent recover would
// trade a crash for an invisible failure.
func runPostSaveEffect(ctx context.Context, effect func()) {
	defer func() {
		if r := recover(); r != nil {
			logging.Error(ctx, "post-save effect panicked",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	effect()
}

// MutateSessionStateOnSaved is MutateSessionState plus onSaved, an effect that
// runs only once the state fn mutated has been durably written — and never
// while the session gate is held.
//
// It exists because a nil error is not proof of a save (ErrMutationSkip reports
// success without writing) and because *this* frame is not necessarily the one
// that saves. Only the outermost frame loads and saves; a nested frame's
// mutations are flushed by whoever owns that save. So a nested frame cannot
// answer "was this persisted?" at the moment it returns — the answer arrives
// later, from a frame further up the stack. Handing the effect to the helper
// rather than returning a bool moves the decision to the only frame that can
// make it: nested registrations are queued on the gate, and the outermost frame
// runs them after its own save succeeds, discarding them if it skips or fails.
//
// That distinction is load-bearing for anything irreversible. Skill-invocation
// telemetry is the motivating case: extraction re-derives from transcript
// offset 0 on every pass and dedupes against a ledger in session state, so
// announcing an event whose ledger entry never landed does not self-correct —
// the next pass sees an unrecorded event and reports it again, duplicating it
// in PostHog. Not emitting is the recoverable direction: the append is
// re-derived and re-announced by the next pass.
//
// onSaved runs after the gate is released, so its I/O (settings load, detached
// process spawn) never extends the hold time for a concurrent hook. Effects run
// in registration order, so nested registrations precede the outermost frame's
// own effect.
//
// One gap this does not close: a nested frame whose fn *errors* has already
// mutated the shared state, and an outer frame that swallows that error still
// saves those mutations — with no effect registered. That direction loses an
// announcement rather than duplicating one, and is inherent to nested frames
// sharing a state pointer.
func MutateSessionStateOnSaved(ctx context.Context, sessionID string, fn func(*SessionState) error, onSaved func()) error {
	if sessionID == "" {
		return ErrStateNotFound
	}
	gate, isOuter, release, err := acquireSessionGate(ctx, sessionID)
	if err != nil {
		return err
	}

	if !isOuter {
		defer release()
		// Nested call: reuse the outer's state pointer. The outer save will
		// flush our mutations; we don't load or save here.
		if gate.activeState == nil {
			return ErrStateNotFound
		}
		if err := fn(gate.activeState); err != nil {
			if errors.Is(err, ErrMutationSkip) {
				return nil
			}
			return err
		}
		if onSaved != nil {
			gate.afterSave = append(gate.afterSave, onSaved)
		}
		return nil
	}

	// Outermost frame: it owns the save, so it owns every queued effect too.
	// One defer keeps the order explicit — clear the gate's per-frame fields,
	// drop the lock, then run the effects outside it.
	var effects []func()
	defer func() {
		gate.activeState = nil
		gate.afterSave = nil
		release()
		for _, effect := range effects {
			runPostSaveEffect(ctx, effect)
		}
	}()

	state, err := LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session state: %w", err)
	}
	if state == nil {
		return ErrStateNotFound
	}
	gate.activeState = state

	if err := fn(state); err != nil {
		if errors.Is(err, ErrMutationSkip) {
			return nil
		}
		return err
	}
	if err := SaveSessionState(ctx, state); err != nil {
		return fmt.Errorf("save session state: %w", err)
	}
	// Copied out before the defer clears gate.afterSave, into a fresh slice so
	// the queue never shares a backing array with the gate. Guarded because the
	// overwhelmingly common case is a plain MutateSessionState with nothing
	// queued, and that runs on the PostToolUse hot path — no effects, no alloc.
	if len(gate.afterSave) > 0 || onSaved != nil {
		effects = make([]func(), 0, len(gate.afterSave)+1)
		effects = append(effects, gate.afterSave...)
		if onSaved != nil {
			effects = append(effects, onSaved)
		}
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

	lock, err := stateLockForSession(ctx, sessionID)
	if err != nil {
		return nil, false, nil, fmt.Errorf("resolve state lock path: %w", err)
	}
	// Bound the acquisition when the caller opted in (TurnStart), so a
	// best-effort mutation can't stall behind a long-running lock holder. The
	// deadline is shared across the whole path, so several mutations don't sum
	// their waits. Only the acquire is bounded — the mutation runs under the
	// original ctx.
	var flockRel func()
	if deadline, ok := sessionLockDeadlineFromContext(ctx); ok {
		acqCtx, cancel := context.WithDeadline(ctx, deadline)
		flockRel, err = flock.AcquireContextIn(acqCtx, lock.root, lock.name)
		cancel()
	} else {
		flockRel, err = flock.AcquireIn(lock.root, lock.name)
	}
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

// WithSessionStateLocks acquires the per-session state lock in each git common
// dir, then runs fn. Lock paths are deduplicated and sorted so callers that
// span repositories or worktrees can safely acquire more than one lock.
func WithSessionStateLocks(ctx context.Context, sessionID string, commonDirs []string, fn func() error) error {
	locks := make([]stateLock, 0, len(commonDirs))
	seen := make(map[string]struct{}, len(commonDirs))
	for _, commonDir := range commonDirs {
		lock, err := stateLockInCommonDir(commonDir, sessionID)
		if err != nil {
			return err
		}
		if _, ok := seen[lock.path]; ok {
			continue
		}
		seen[lock.path] = struct{}{}
		locks = append(locks, lock)
	}
	// Sorted by absolute path so callers spanning repositories acquire in a
	// consistent order and cannot deadlock against each other.
	slices.SortFunc(locks, func(a, b stateLock) int { return strings.Compare(a.path, b.path) })

	releases := make([]func(), 0, len(locks))
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, lock := range locks {
		if err := ctx.Err(); err != nil {
			releaseAll()
			return fmt.Errorf("session state lock canceled: %w", err)
		}
		// AcquireContextIn: rooted (the lock name cannot escape the state
		// dir) AND bounded by the caller's ctx — cross-repo adoption relies
		// on the acquire honoring its 2s deadline.
		release, err := flock.AcquireContextIn(ctx, lock.root, lock.name)
		if err != nil {
			releaseAll()
			return fmt.Errorf("acquire session state lock: %w", err)
		}
		releases = append(releases, release)
	}
	defer releaseAll()

	return fn()
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

// stateLock names a per-session lock file two ways: path, for dedup and for the
// deterministic ordering WithSessionStateLocks needs across repositories, and
// (root, name) for the acquire itself so the session-ID-derived name resolves
// inside the git common dir rather than as an assembled string.
type stateLock struct {
	path string
	root *os.Root
	name string
}

// SessionLockDirName is the lock directory inside the git common dir. Exported
// so uninstall can sweep it without re-spelling the name.
const SessionLockDirName = "entire-session-locks"

// stateLockPath returns the lock file path for a session. Lock files live in
// .git/entire-session-locks/ (a sibling to entire-sessions/) so callers that
// enumerate session state files don't have to filter lock entries. A
// separate file (rather than locking the state file itself) keeps the lock
// holder distinct from the data — Save's atomic-rename pattern would
// otherwise unlink the inode the flock is held on.
func stateLockPath(ctx context.Context, sessionID string) (string, error) {
	lock, err := stateLockForSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	return lock.path, nil
}

func stateLockForSession(ctx context.Context, sessionID string) (stateLock, error) {
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return stateLock{}, fmt.Errorf("resolve git common dir: %w", err)
	}
	return stateLockInCommonDir(commonDir, sessionID)
}

func stateLockInCommonDir(commonDir, sessionID string) (stateLock, error) {
	if strings.TrimSpace(commonDir) == "" {
		return stateLock{}, errors.New("empty git common dir")
	}
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return stateLock{}, fmt.Errorf("invalid session ID: %w", err)
	}
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return stateLock{}, fmt.Errorf("open git common dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, SessionLockDirName, 0o750); err != nil {
		return stateLock{}, fmt.Errorf("create session lock directory: %w", err)
	}
	name := SessionLockDirName + "/" + sessionID + ".lock"
	return stateLock{
		path: filepath.Join(commonDir, filepath.FromSlash(name)),
		root: root,
		name: name,
	}, nil
}

// ClearSessionState removes the session state file for the given session ID.
func ClearSessionState(ctx context.Context, sessionID string) error {
	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	root, err := openSessionStateRootForRead(ctx)
	if err != nil {
		return fmt.Errorf("failed to open session state directory for cleanup: %w", err)
	}
	if root == nil {
		return nil // no state directory => nothing to clear
	}
	defer root.Close()

	// Remove all files for this session (state .json, .model hint, any future
	// hint files). Match by literal prefix rather than filepath.Glob: the
	// session ID is user-controlled, and a glob pattern would let metacharacters
	// match and delete other sessions' files. os.Root ensures traversal-resistant
	// removal.
	prefix := sessionID + "."
	entries, _ := osroot.ReadDirNoSymlinks(root, ".") //nolint:errcheck // best-effort cleanup; missing dir => nothing to clear
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, prefix) {
			_ = osroot.RemoveNoSymlinks(root, name) //nolint:errcheck // best-effort cleanup
		}
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
