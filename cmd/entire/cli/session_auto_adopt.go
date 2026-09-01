package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// Caps how many worktrees prepare-commit-msg will resolve via git when hunting
// auto-adopt candidates (registry entries or sibling dirs).
const (
	maxSiblingAutoAdoptScan  = 32
	maxRegistryAutoAdoptScan = 32
)

// Time bounds for prepare-commit-msg auto-adopt. Git rev-parse calls use
// CommandContext; without a deadline a hung/stale worktree path (registry or
// sibling) could block `git commit` indefinitely.
const (
	autoAdoptDiscoveryTimeout   = 2 * time.Second
	autoAdoptGitResolveTimeout  = 500 * time.Millisecond
	autoAdoptStagedFilesTimeout = 1 * time.Second
	// autoAdoptAdoptTimeout bounds adoptFromExternalSessionStore, which takes
	// a cross-process session-state flock (strategy.WithSessionStateLocks).
	// Without a deadline, a lock held by another process (or a hung
	// concurrent hook) could block git commit indefinitely — the same
	// failure mode the timeouts above already guard against for discovery.
	autoAdoptAdoptTimeout = 2 * time.Second
	// autoAdoptOverallBudget caps the whole prepare-commit-msg attempt. The
	// per-step timeouts above are worst-case and stack (target resolve + staged +
	// registry + sibling + adopt ≈ 7.5s); wrapping the attempt in one budget
	// bounds their sum so an ordinary human commit (the miss path) can add at
	// most this much latency to git commit.
	autoAdoptOverallBudget = 5 * time.Second
	// autoAdoptMarkerTimeout bounds the post-prepare marker update and the
	// cancellation path. Both take the same cross-process session-state flocks as
	// the adopt itself, and both run straight off the git hook — outside the
	// prepare-side attempt budget — so without their own deadline a lock held by a
	// hung process would block `git commit` indefinitely.
	autoAdoptMarkerTimeout = 2 * time.Second
)

// autoAdoptCommitLookback bounds how many recent commits the finalize path
// searches for the checkpoint an adoption is bound to.
//
// Git releases the ref lock before running post-commit, so HEAD can already have
// advanced to a newer commit by the time this hook inspects it — reading only the
// current tip would fail to find the trailer of the very commit that triggered
// this hook and cancel a successful adoption. The bound checkpoint ID is a nonce
// issued by one prepare-commit-msg invocation, so any commit carrying it is that
// commit; widening the search from the mutable tip to a short window of recent
// commits identifies it without weakening the guard.
const autoAdoptCommitLookback = 16

// prepareCommitMsgSourceAmend is git's prepare-commit-msg source for `git commit --amend`.
const prepareCommitMsgSourceAmend = "commit"

// shouldTryAutoAdoptOnPrepareCommitMsg reports whether prepare-commit-msg should
// attempt cross-common-dir auto-adopt. Matches ManualCommitStrategy.PrepareCommitMsg
// skip conditions so we never retire a live session when no trailer would be written.
// Amend (source "commit"): handleAmendCommitMsg only restores an existing trailer /
// HEAD LastCheckpointID match and will not write a trailer for a freshly adopted session.
func shouldTryAutoAdoptOnPrepareCommitMsg(ctx context.Context, source string) bool {
	// Amend is auto-adopt's own extra guard: handleAmendCommitMsg only restores
	// an existing trailer and never writes one for a freshly adopted session.
	if source == prepareCommitMsgSourceAmend {
		return false
	}
	// Merge/squash and git sequence operations are the shared skip invariant.
	return !strategy.SkipsPrepareCommitMsg(ctx, source)
}

// tryAutoAdoptCrossCommonDirSession adopts a unique ACTIVE session from another
// git common dir into the current worktree when it is safe to do so.
//
// Discovery order:
//  1. Live-session registry (written on ACTIVE StateStore.Save) — reaches any
//     worktree, including non-siblings under unrelated parents (issue #1439's own
//     repro), guarded by owner + distinctive-overlap + uniqueness instead of proximity
//  2. Immediate sibling repos under the parent directory (seeded / microservices)
//
// A candidate is accepted only when exactly one match remains after filtering
// for recent ACTIVE sessions that (a) share the committing process owner and
// (b) have FilesTouched overlapping the staged commit paths on a
// non-boilerplate relative path (README.md / go.mod / package.json / … alone
// never count). The sibling scan is inherently parent-scoped; the registry path
// is not, relying on the stronger owner + distinctive-overlap + uniqueness
// guards. Idle sessions are never auto-adopted. Ambiguity skips.
//
// Best-effort: never returns an error to the git hook caller.
type pendingAutoAdoption struct {
	SessionID string
	AttemptID string
}

func tryAutoAdoptCrossCommonDirSession(ctx context.Context) (pending *pendingAutoAdoption) {
	logCtx := logging.WithComponent(ctx, "session")

	// This runs inside prepare-commit-msg, whose stderr the hook swallows. A
	// panic anywhere in the cross-common-dir adopt path would otherwise vanish
	// with no record; recover and log it at Error so it is at least visible in
	// .entire/logs. Never re-panic — a failed adopt must not break `git commit`.
	defer func() {
		if r := recover(); r != nil {
			logging.Error(logCtx, "auto-adopt: recovered from panic",
				slog.Any("panic", r),
			)
			pending = nil
		}
	}()

	// One overall wall-clock budget for the whole attempt. Every step below
	// derives its own timeout from this context, so their worst-case sum can
	// never exceed autoAdoptOverallBudget — a miss adds bounded latency to commit.
	ctx, cancel := context.WithTimeout(ctx, autoAdoptOverallBudget)
	defer cancel()

	if !targetIsEntireEnabled(ctx) {
		return nil
	}

	targetWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil || targetWorktree == "" {
		return nil
	}
	targetCtx, targetCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
	targetStore, targetWorktree, targetCommonDir, err := stateStoreForWorktree(targetCtx, targetWorktree)
	targetCancel()
	if err != nil {
		return nil
	}
	targetWorktreeID, err := paths.GetWorktreeID(targetWorktree)
	if err != nil || hasLocalActiveSession(ctx, targetStore, targetWorktree, targetWorktreeID) {
		return nil
	}

	stagedCtx, stagedCancel := context.WithTimeout(ctx, autoAdoptStagedFilesTimeout)
	staged, err := stagedFilesForAutoAdopt(stagedCtx, targetWorktree)
	stagedCancel()
	if err != nil {
		staged = nil
	}
	// Owner + staged overlap are both required for every candidate. Bail before
	// any registry/sibling I/O when either is missing (ordinary human commits).
	if len(staged) == 0 {
		return nil
	}
	owner, hasOwner := proclive.ResolveOwner()
	if !hasOwner {
		return nil
	}

	regCtx, regCancel := context.WithTimeout(ctx, autoAdoptDiscoveryTimeout)
	registryResult := collectRegistryAutoAdoptCandidates(regCtx, targetCommonDir, staged, owner, hasOwner)
	regCancel()
	scanCtx, scanCancel := context.WithTimeout(ctx, autoAdoptDiscoveryTimeout)
	siblingResult := collectSiblingAutoAdoptCandidates(scanCtx, targetWorktree, targetCommonDir, staged, owner, hasOwner)
	scanCancel()
	if !registryResult.Complete || !siblingResult.Complete {
		logging.Debug(logCtx, "auto-adopt: skipped because candidate discovery was incomplete")
		return nil
	}
	// Union both discovery sources (deduped by session ID) BEFORE the uniqueness
	// test. Gating the sibling scan on an empty registry would let a single
	// registry hit bypass the ambiguity guard when a second, distinct candidate
	// exists on disk — auto-adopt would then silently steal one of two sessions.
	candidates := unionAutoAdoptCandidates(registryResult.Candidates, siblingResult.Candidates)
	if len(candidates) != 1 {
		if len(candidates) > 1 {
			logging.Debug(logCtx, "auto-adopt: skipped ambiguous cross-common-dir sessions",
				slog.Int("candidates", len(candidates)),
			)
		}
		return nil
	}

	source := candidates[0]
	existing, err := targetStore.Load(ctx, source.SessionID)
	if err != nil || existing != nil {
		return nil
	}

	logging.Info(logCtx, "auto-adopt: adopting cross-common-dir session for commit",
		slog.String("session_id", source.SessionID),
		slog.String("from_worktree", source.WorktreePath),
		slog.String("to_worktree", targetWorktree),
	)

	attemptID, err := checkpointid.Generate()
	if err != nil {
		return nil
	}
	adoptCtx, adoptCancel := context.WithTimeout(ctx, autoAdoptAdoptTimeout)
	_, _, adoptErr := adoptFromExternalSessionStore(
		adoptCtx,
		source.Store,
		source.WorktreePath,
		source.CommonDir,
		targetStore,
		targetCommonDir,
		source.SessionID,
		// DeferSourceRetire: this runs in prepare-commit-msg, before the commit
		// exists. Register the adopted session here (so the trailer lands) but
		// defer the destructive source retire to post-commit, where the commit is
		// a fact — an aborted commit must not tombstone the source.
		adoptOptions{
			Force:                    true,
			SkipTranscriptValidation: true,
			DeferSourceRetire:        true,
			AdoptionAttemptID:        attemptID.String(),
			// Everything discovery decided was read WITHOUT the source lock. Re-run
			// the exact same predicate against the state loaded under the lock so a
			// source that went Idle, changed owner, moved worktree, or was replaced
			// in between cannot be adopted on the strength of the earlier read.
			RevalidateSource: func(locked *session.State) error {
				return revalidateAutoAdoptSource(locked, source, staged, owner, hasOwner)
			},
		},
	)
	adoptCancel()
	if adoptErr != nil {
		var rollbackFailed *adoptRollbackFailedError
		var claimed *sourceClaimedError
		switch {
		case errors.As(adoptErr, &rollbackFailed):
			// Retire failed AND rollback failed: the session is now registered in
			// both the source and target repos. This is corruption, not a miss.
			logging.Error(logCtx, "auto-adopt: adopt left session registered in both repos",
				slog.String("session_id", source.SessionID),
				slog.String("error", adoptErr.Error()),
			)
		case errors.As(adoptErr, &claimed):
			// Lost the concurrent-adopt race: another target claimed this source
			// first, under the shared source lock. Adopt-exactly-once is preserved,
			// so this is an expected skip, not a failure.
			logging.Debug(logCtx, "auto-adopt: source already claimed by a concurrent adopt",
				slog.String("session_id", source.SessionID),
			)
		default:
			logging.Warn(logCtx, "auto-adopt: adopt failed",
				slog.String("session_id", source.SessionID),
				slog.String("error", adoptErr.Error()),
			)
		}
		return nil
	}
	return &pendingAutoAdoption{SessionID: source.SessionID, AttemptID: attemptID.String()}
}

// autoAdoptTrailerSnapshot records the checkpoint trailers already present in
// the commit message BEFORE prepare-commit-msg runs, so
// finishPreparedAutoAdoption can tell a trailer this invocation issued from one
// that was handed in by the caller.
func autoAdoptTrailerSnapshot(commitMsgFile string) map[checkpointid.CheckpointID]struct{} {
	existing := make(map[checkpointid.CheckpointID]struct{})
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // path is supplied by git
	if err != nil {
		return existing
	}
	for _, cpID := range trailers.ParseAllCheckpoints(string(content)) {
		existing[cpID] = struct{}{}
	}
	return existing
}

// finishPreparedAutoAdoption binds the deferred retire to the exact checkpoint
// trailer newly issued by this prepare-commit-msg invocation. If no such trailer
// was written (including the documented user opt-out), it rolls back only this
// attempt's target copy and restores the source registry pointer.
//
// before is the trailer set captured by autoAdoptTrailerSnapshot and prepareErr
// is the result of the PrepareCommitMsg call this adoption is riding on.
func finishPreparedAutoAdoption(
	ctx context.Context,
	pending *pendingAutoAdoption,
	commitMsgFile string,
	before map[checkpointid.CheckpointID]struct{},
	prepareErr error,
) {
	if pending == nil {
		return
	}
	// The whole finish runs straight off the git hook, so give it its own
	// wall-clock bound: both branches below take cross-process state flocks.
	ctx, cancel := context.WithTimeout(ctx, autoAdoptMarkerTimeout)
	defer cancel()

	// A failed prepare wrote no trailer we can attribute to this adoption (and may
	// have left the message half-written), so there is nothing to bind to.
	if prepareErr != nil {
		cancelPendingAutoAdoption(ctx, pending)
		return
	}
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // path is supplied by git
	if err != nil {
		cancelPendingAutoAdoption(ctx, pending)
		return
	}
	cpID, found := newlyIssuedCheckpoint(before, string(content))
	if !found {
		cancelPendingAutoAdoption(ctx, pending)
		return
	}
	mutatePendingAutoAdoption(ctx, pending, func(_ *session.State, marker *session.PendingSourceRetire) error {
		marker.ExpectedCheckpointID = cpID
		return nil
	})
}

// newlyIssuedCheckpoint returns the first checkpoint trailer in message that was
// not already present in before.
//
// Only a freshly issued trailer may authorize retiring the source.
// ManualCommitStrategy.PrepareCommitMsg deliberately PRESERVES a pre-existing
// Entire-Checkpoint — a user can hand one in with `git commit -m` — and such a
// trailer links the commit to some unrelated session's checkpoint, so it is no
// evidence that this adoption was recorded anywhere.
func newlyIssuedCheckpoint(before map[checkpointid.CheckpointID]struct{}, message string) (checkpointid.CheckpointID, bool) {
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		if _, existed := before[cpID]; !existed {
			return cpID, true
		}
	}
	return checkpointid.EmptyCheckpointID, false
}

func mutatePendingAutoAdoption(
	ctx context.Context,
	pending *pendingAutoAdoption,
	mutate func(*session.State, *session.PendingSourceRetire) error,
) {
	// Independent bound on the state-lock wait; see autoAdoptMarkerTimeout.
	ctx, cancel := context.WithTimeout(ctx, autoAdoptMarkerTimeout)
	defer cancel()

	targetWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil || targetWorktree == "" {
		return
	}
	targetStore, _, targetCommonDir, err := stateStoreForWorktree(ctx, targetWorktree)
	if err != nil {
		return
	}
	target, err := targetStore.Load(ctx, pending.SessionID)
	if err != nil || target == nil || target.PendingSourceRetire == nil ||
		target.PendingSourceRetire.AdoptionAttemptID != pending.AttemptID {
		return
	}
	marker := target.PendingSourceRetire
	if lockErr := strategy.WithSessionStateLocks(ctx, pending.SessionID, []string{marker.SourceCommonDir, targetCommonDir}, func() error {
		target, err = targetStore.Load(ctx, pending.SessionID)
		if err != nil || target == nil || target.PendingSourceRetire == nil ||
			target.PendingSourceRetire.AdoptionAttemptID != pending.AttemptID {
			if err != nil {
				return fmt.Errorf("reload pending adoption: %w", err)
			}
			return nil
		}
		if err := mutate(target, target.PendingSourceRetire); err != nil {
			return err
		}
		return targetStore.Save(ctx, target)
	}); lockErr != nil {
		logging.Debug(logging.WithComponent(ctx, "session"), "failed to update pending auto-adoption",
			slog.String("session_id", pending.SessionID),
			slog.String("error", lockErr.Error()),
		)
	}
}

func cancelPendingAutoAdoption(ctx context.Context, pending *pendingAutoAdoption) {
	// Bound the lock waits below: cancellation is reached directly from
	// prepare-commit-msg as well as from the already-bounded finalize path, and a
	// contended source lock must not hold up `git commit`.
	ctx, cancel := context.WithTimeout(ctx, autoAdoptMarkerTimeout)
	defer cancel()

	targetWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil || targetWorktree == "" {
		return
	}
	targetStore, _, targetCommonDir, err := stateStoreForWorktree(ctx, targetWorktree)
	if err != nil {
		return
	}
	target, err := targetStore.Load(ctx, pending.SessionID)
	if err != nil || target == nil || target.PendingSourceRetire == nil ||
		target.PendingSourceRetire.AdoptionAttemptID != pending.AttemptID {
		return
	}
	marker := target.PendingSourceRetire
	if lockErr := strategy.WithSessionStateLocks(ctx, pending.SessionID, []string{marker.SourceCommonDir, targetCommonDir}, func() error {
		target, err = targetStore.Load(ctx, pending.SessionID)
		if err != nil || target == nil || target.PendingSourceRetire == nil ||
			target.PendingSourceRetire.AdoptionAttemptID != pending.AttemptID {
			if err != nil {
				return fmt.Errorf("reload pending adoption for cancellation: %w", err)
			}
			return nil
		}
		marker = target.PendingSourceRetire
		expected := pendingClaimFor(targetCommonDir, target, marker)
		claim, claimErr := session.LiveSessionClaimContext(ctx, target.SessionID)
		if claimErr != nil {
			return fmt.Errorf("read pending adoption claim: %w", claimErr)
		}
		owned := claimMatchesPending(claim, expected)
		// Remove the target copy FIRST and release the claim only once that removal
		// is confirmed gone. The claim is this rollback's guard: while it is held no
		// other repo can adopt the source. Releasing it before the target copy is
		// actually deleted would let a second repo claim a source that is still
		// ACTIVE while this repo keeps a live adopted copy of the same session —
		// the double registration the claim exists to prevent. A failed cleanup
		// therefore keeps both the marker and the claim, and the next post-commit
		// retries the cancellation.
		if err := targetStore.Clear(ctx, target.SessionID); err != nil {
			return fmt.Errorf("clear canceled target adoption: %w", err)
		}
		remaining, loadErr := targetStore.Load(ctx, target.SessionID)
		if loadErr != nil {
			return fmt.Errorf("verify canceled target removal: %w", loadErr)
		}
		if remaining != nil {
			return errors.New("canceled target adoption is still registered")
		}
		if !owned {
			return nil
		}
		if _, releaseErr := session.ReleaseLiveSessionClaimIfOwned(ctx, target.SessionID, expected); releaseErr != nil {
			return fmt.Errorf("release pending adoption claim: %w", releaseErr)
		}
		sourceStore := session.NewStateStoreWithDir(filepath.Join(marker.SourceCommonDir, session.SessionStateDirName))
		source, loadErr := sourceStore.Load(ctx, target.SessionID)
		if loadErr != nil {
			return fmt.Errorf("reload source after canceled adoption: %w", loadErr)
		}
		if source == nil {
			return nil
		}
		return session.RegisterLiveSession(source, marker.SourceCommonDir)
	}); lockErr != nil {
		logging.Debug(logging.WithComponent(ctx, "session"), "failed to cancel pending auto-adoption",
			slog.String("session_id", pending.SessionID),
			slog.String("error", lockErr.Error()),
		)
	}
}

// finalizePendingSourceRetires completes cross-common-dir auto-adopts that were
// deferred at prepare-commit-msg time. For each session in the current
// (committing) worktree carrying a PendingSourceRetire marker, it tombstones the
// source session — now that the commit is a fact — and clears the marker. Runs
// from the post-commit git hook, after strategy.PostCommit. Best-effort: it
// never returns an error to the hook caller, and is panic-guarded so a fault in
// the retire path can never break `git commit`.
//
// An aborted commit (editor abort, empty-message strip) never reaches
// post-commit, so this never runs for it — leaving the source ACTIVE, which is
// the whole point of splitting the adopt across the two hooks.
func finalizePendingSourceRetires(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, "session")

	defer func() {
		if r := recover(); r != nil {
			logging.Error(logCtx, "auto-adopt finalize: recovered from panic",
				slog.Any("panic", r),
			)
		}
	}()

	// Bound the whole finalize the same way the prepare-side attempt is bounded:
	// each pending retire takes a cross-process source flock, and a lock held by
	// a hung process must not block git's post-commit indefinitely.
	ctx, cancel := context.WithTimeout(ctx, autoAdoptOverallBudget)
	defer cancel()

	targetWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil || targetWorktree == "" {
		return
	}
	targetCtx, targetCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
	targetStore, _, targetCommonDir, err := stateStoreForWorktree(targetCtx, targetWorktree)
	targetCancel()
	if err != nil {
		return
	}

	targetWorktreeID, err := paths.GetWorktreeID(targetWorktree)
	if err != nil {
		return
	}
	committed, err := committedCheckpointSet(ctx, targetWorktree)
	if err != nil {
		return
	}
	states, err := targetStore.List(ctx)
	if err != nil {
		return
	}
	for _, state := range states {
		if ctx.Err() != nil {
			return
		}
		if state.PendingSourceRetire == nil {
			continue
		}
		if !stateBelongsToTargetWorktree(state, targetWorktree, targetWorktreeID) {
			continue
		}
		marker := state.PendingSourceRetire
		if marker.AdoptionAttemptID == "" {
			continue // legacy marker: preserve source, never retire blindly
		}
		pending := &pendingAutoAdoption{SessionID: state.SessionID, AttemptID: marker.AdoptionAttemptID}
		if marker.ExpectedCheckpointID.IsEmpty() {
			cancelPendingAutoAdoption(ctx, pending)
			continue
		}
		if _, ok := committed[marker.ExpectedCheckpointID]; !ok {
			// The bound checkpoint never landed. Before canceling, check for the
			// abort-after-prepare retry: the first commit aborted, so on the next
			// commit this worktree already owned the adopted session locally,
			// auto-adopt correctly skipped, and PrepareCommitMsg issued a NEW
			// checkpoint that did commit. The adoption succeeded — just under a
			// different checkpoint — so rebind the marker instead of canceling a
			// session the user has committed against.
			rebound, ok := reboundRetryCheckpoint(state, committed)
			if !ok {
				cancelPendingAutoAdoption(ctx, pending)
				continue
			}
			logging.Info(logCtx, "auto-adopt finalize: rebinding pending adoption to the retried commit",
				slog.String("session_id", state.SessionID),
				slog.String("checkpoint_id", rebound.String()),
			)
			mutatePendingAutoAdoption(ctx, pending, func(_ *session.State, m *session.PendingSourceRetire) error {
				m.ExpectedCheckpointID = rebound
				return nil
			})
			// retirePendingSource reloads the marker under the lock and revalidates
			// it against the commit window, so a failed rebind simply declines to
			// retire rather than retiring on unverified state.
			state.PendingSourceRetire.ExpectedCheckpointID = rebound
		}
		retirePendingSource(ctx, targetStore, targetCommonDir, targetWorktree, targetWorktreeID, state)
	}
}

// reboundRetryCheckpoint returns the checkpoint this session was actually
// committed under, when the checkpoint its pending adoption was bound to never
// made it into a commit.
//
// LastCheckpointID is written by PostCommit condensation and names the checkpoint
// that linked THIS session to the commit that just landed; adoption resets it to
// empty, so a non-empty value can only have come from a target-side commit. It is
// the only ID attributable to this adoption, and it still has to appear in the
// commit window before it may authorize the destructive source retire.
func reboundRetryCheckpoint(
	state *session.State,
	committed map[checkpointid.CheckpointID]struct{},
) (checkpointid.CheckpointID, bool) {
	if state.LastCheckpointID.IsEmpty() {
		return checkpointid.EmptyCheckpointID, false
	}
	if _, ok := committed[state.LastCheckpointID]; !ok {
		return checkpointid.EmptyCheckpointID, false
	}
	return state.LastCheckpointID, true
}

// retirePendingSource tombstones the source session recorded on adopted's
// PendingSourceRetire marker and clears the marker on the target. Idempotent:
// if the source is already gone or already retired it just clears the marker, so
// a crash between the source retire and the marker clear self-heals on the next
// post-commit.
func retirePendingSource(
	ctx context.Context,
	targetStore *session.StateStore,
	targetCommonDir, targetWorktree, targetWorktreeID string,
	adopted *session.State,
) {
	logCtx := logging.WithComponent(ctx, "session")
	marker := adopted.PendingSourceRetire
	if marker == nil || marker.SourceCommonDir == "" {
		return
	}
	sourceStore := session.NewStateStoreWithDir(filepath.Join(marker.SourceCommonDir, session.SessionStateDirName))

	retireCtx, cancel := context.WithTimeout(ctx, autoAdoptAdoptTimeout)
	defer cancel()

	err := strategy.WithSessionStateLocks(retireCtx, adopted.SessionID, []string{marker.SourceCommonDir, targetCommonDir}, func() error {
		// Reload the target under the lock so clearing the marker never clobbers a
		// concurrent write to the adopted session state.
		target, err := targetStore.Load(retireCtx, adopted.SessionID)
		if err != nil {
			return fmt.Errorf("reload adopted session state: %w", err)
		}
		if target == nil || target.PendingSourceRetire == nil {
			return nil // already finalized by a concurrent hook
		}
		marker = target.PendingSourceRetire
		if !stateBelongsToTargetWorktree(target, targetWorktree, targetWorktreeID) ||
			marker.AdoptionAttemptID == "" || marker.ExpectedCheckpointID.IsEmpty() {
			return errors.New("pending adoption no longer belongs to this worktree")
		}
		committed, headErr := committedCheckpointSet(retireCtx, targetWorktree)
		if headErr != nil {
			return fmt.Errorf("revalidate committed checkpoint: %w", headErr)
		}
		if _, ok := committed[marker.ExpectedCheckpointID]; !ok {
			return errors.New("pending adoption checkpoint is not in any recent commit")
		}
		expectedClaim := pendingClaimFor(targetCommonDir, target, marker)
		sourceState, err := sourceStore.Load(retireCtx, adopted.SessionID)
		if err != nil {
			return fmt.Errorf("load source session state: %w", err)
		}
		sourceRetiredThisCall := false
		if sourceState != nil && isAdoptableSourceSession(sourceState) {
			claim, claimErr := session.LiveSessionClaimContext(retireCtx, adopted.SessionID)
			if claimErr != nil {
				return fmt.Errorf("reload adoption claim: %w", claimErr)
			}
			if !claimMatchesPending(claim, expectedClaim) {
				return errors.New("pending adoption claim was replaced")
			}
			retired := retireAdoptedSourceSession(sourceState, target)
			if err := sourceStore.Save(retireCtx, &retired); err != nil {
				return fmt.Errorf("retire source session state: %w", err)
			}
			sourceRetiredThisCall = true
		} else if sourceState != nil && sourceState.AdoptedIntoWorktreePath != "" &&
			!sameAdoptPath(sourceState.AdoptedIntoWorktreePath, target.WorktreePath) {
			return errors.New("source was retired into a different worktree")
		}
		// Once the bound checkpoint is committed, a source that independently ended
		// before finalize can never be retired here — clear the marker anyway so
		// post-commit does not warn forever on an unrecoverable source state.
		finishCleanupDespiteRecheckFailure := sourceRetiredThisCall || sourceState == nil
		// Clear before releasing the claim. If the target save fails, the next
		// post-commit observes the already-retired source and safely retries the
		// marker clear without requiring a claim that no longer exists.
		//
		// Re-read HEAD immediately before release: source retire above may have
		// taken long enough for a new commit to land without the bound checkpoint.
		// After a successful source retire (or when the source is already gone /
		// unrecoverable), a recheck failure must still finish cleanup — aborting
		// leaves a stuck marker that eventually cancels the target copy even though
		// the source was correctly retired.
		committed, headErr = committedCheckpointSet(retireCtx, targetWorktree)
		if headErr != nil {
			return fmt.Errorf("revalidate committed checkpoint before release: %w", headErr)
		}
		if _, ok := committed[marker.ExpectedCheckpointID]; !ok && !finishCleanupDespiteRecheckFailure {
			return errors.New("pending adoption checkpoint is not in any recent commit")
		}
		target.PendingSourceRetire = nil
		if err := targetStore.Save(retireCtx, target); err != nil {
			return fmt.Errorf("clear pending source retire marker: %w", err)
		}
		if _, err := session.ReleaseLiveSessionClaimIfOwned(retireCtx, adopted.SessionID, expectedClaim); err != nil {
			return fmt.Errorf("release live-session claim: %w", err)
		}
		return nil
	})
	if err != nil {
		logging.Warn(logCtx, "auto-adopt finalize: failed to retire source session",
			slog.String("session_id", adopted.SessionID),
			slog.String("error", err.Error()),
		)
		return
	}
	logging.Info(logCtx, "auto-adopt finalize: retired source session after commit",
		slog.String("session_id", adopted.SessionID),
		slog.String("source_common_dir", marker.SourceCommonDir),
	)
}

func pendingClaimFor(targetCommonDir string, target *session.State, marker *session.PendingSourceRetire) session.AdoptClaim {
	return session.AdoptClaim{
		ByCommonDir:    targetCommonDir,
		ByWorktreePath: target.WorktreePath,
		ByWorktreeID:   target.WorktreeID,
		AttemptID:      marker.AdoptionAttemptID,
	}
}

func claimMatchesPending(actual *session.AdoptClaim, expected session.AdoptClaim) bool {
	if actual == nil {
		return false
	}
	return sameAdoptStore(actual.ByCommonDir, expected.ByCommonDir) &&
		sameAdoptPath(actual.ByWorktreePath, expected.ByWorktreePath) &&
		actual.ByWorktreeID == expected.ByWorktreeID &&
		actual.AttemptID == expected.AttemptID
}

// committedCheckpointSet returns the checkpoint IDs trailered on the last
// autoAdoptCommitLookback commits reachable from HEAD. See
// autoAdoptCommitLookback for why the tip alone is not enough.
func committedCheckpointSet(ctx context.Context, worktree string) (map[checkpointid.CheckpointID]struct{}, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "log",
		"-n", strconv.Itoa(autoAdoptCommitLookback), "--format=%B%x00", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read recent commit messages: %w", err)
	}
	result := make(map[checkpointid.CheckpointID]struct{})
	// Split on the NUL separator so each commit message is parsed on its own:
	// ParseAllCheckpoints is trailer-aware, and concatenating bodies could let one
	// commit's text change how another's trailer block parses.
	for _, message := range strings.Split(string(out), "\x00") {
		for _, cpID := range trailers.ParseAllCheckpoints(message) {
			result[cpID] = struct{}{}
		}
	}
	return result, nil
}

type autoAdoptCandidate struct {
	SessionID    string
	WorktreePath string
	CommonDir    string
	Store        *session.StateStore
}

type autoAdoptDiscoveryResult struct {
	Candidates []autoAdoptCandidate
	Complete   bool
}

// unionAutoAdoptCandidates concatenates candidate sets, deduping by session ID
// (session IDs are globally unique, so the same session found by both the
// registry and the sibling scan collapses to one). The union feeds the
// uniqueness test so a candidate present in only one source still counts.
func unionAutoAdoptCandidates(sets ...[]autoAdoptCandidate) []autoAdoptCandidate {
	seen := make(map[string]struct{})
	var out []autoAdoptCandidate
	for _, set := range sets {
		for _, cand := range set {
			if _, ok := seen[cand.SessionID]; ok {
				continue
			}
			seen[cand.SessionID] = struct{}{}
			out = append(out, cand)
		}
	}
	return out
}

func hasLocalActiveSession(ctx context.Context, store *session.StateStore, worktreePath, worktreeID string) bool {
	states, err := store.List(ctx)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"),
			"auto-adopt: local session list failed; failing closed",
			slog.String("error", err.Error()),
		)
		return true // fail closed: do not steal when local store is unreadable
	}
	for _, state := range states {
		if !stateBelongsToTargetWorktree(state, worktreePath, worktreeID) {
			continue
		}
		// Bound by the same recency window as the candidate guards. Without it a
		// single months-old Idle state file would count as a local session and
		// permanently disable cross-common-dir auto-adopt for the whole repo.
		if isRecentAdoptCandidate(state) {
			return true
		}
	}
	return false
}

func collectRegistryAutoAdoptCandidates(
	ctx context.Context,
	targetCommonDir string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) autoAdoptDiscoveryResult {
	entries, complete, err := session.ListLiveSessionsContext(ctx, maxRegistryAutoAdoptScan+1)
	if err != nil {
		return autoAdoptDiscoveryResult{Complete: false}
	}
	if !complete {
		return autoAdoptDiscoveryResult{Complete: false}
	}
	if len(entries) == 0 {
		return autoAdoptDiscoveryResult{Complete: true}
	}

	var out []autoAdoptCandidate
	resolved := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
		}
		if sameAdoptStore(entry.CommonDir, targetCommonDir) {
			continue
		}
		if !isRecentLiveEntry(entry) {
			continue
		}
		if freshAdoptClaim(entry.AdoptClaim) {
			continue
		}
		if resolved >= maxRegistryAutoAdoptScan {
			return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
		}
		// The registry path deliberately does NOT gate on parent-dir proximity
		// (unlike the sibling scan, which is inherently parent-scoped). Issue
		// #1439's own repro is cross-parent — a session in
		// …/entire.io/.worktrees/… committing in an unrelated /private/tmp/…
		// checkout — so requiring siblinghood here would leave the motivating case
		// with no trailer. A registry candidate's stronger guards stand in for
		// proximity: candidateFromSource → candidateFromLoaded requires an exact
		// process-owner match AND a non-boilerplate distinctive FilesTouched∩staged
		// overlap, and the caller's global exactly-one-candidate uniqueness test
		// still applies. A shared owner (tmux/IDE) plus a common relative filename
		// alone therefore cannot steal across unrelated repos.
		resolved++
		resolveCtx, resolveCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
		cand, ok, inspectErr := candidateFromSource(resolveCtx, entry.WorktreePath, entry.SessionID, staged, owner, hasOwner)
		resolveCancel()
		if inspectErr != nil {
			// An entry we could not inspect may name a session that WOULD have
			// matched, so the uniqueness test downstream can no longer be trusted:
			// with one readable and one unreadable match, treating the unreadable
			// one as a non-match would adopt the readable one as if it were unique.
			logging.Debug(logging.WithComponent(ctx, "session"),
				"auto-adopt: registry candidate could not be inspected; failing closed",
				slog.String("session_id", entry.SessionID),
				slog.String("error", inspectErr.Error()),
			)
			return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
		}
		if ok {
			out = append(out, cand)
		}
	}
	return autoAdoptDiscoveryResult{Candidates: out, Complete: true}
}

func collectSiblingAutoAdoptCandidates(
	ctx context.Context,
	targetWorktree, targetCommonDir string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) autoAdoptDiscoveryResult {
	parent := filepath.Dir(targetWorktree)
	if parent == "" || parent == targetWorktree {
		return autoAdoptDiscoveryResult{Complete: true}
	}
	dir, err := os.Open(parent) //nolint:gosec // parent derives from the validated current worktree root
	if err != nil {
		return autoAdoptDiscoveryResult{Complete: false}
	}
	defer dir.Close()

	var out []autoAdoptCandidate
	scanned := 0
	for {
		if ctx.Err() != nil {
			return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
		}
		entries, readErr := dir.ReadDir(32)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			sibling := filepath.Join(parent, entry.Name())
			if sameAdoptPath(sibling, targetWorktree) {
				continue
			}
			// Cheap pre-filters before shelling out to git rev-parse:
			//  1. .git entry (skip node_modules / build outputs)
			//  2. .entire/ (skip git repos that don't use Entire — no sessions to adopt)
			if !siblingLooksLikeGitWorktree(sibling) || !siblingLooksLikeEntireRepo(sibling) {
				continue
			}
			if scanned >= maxSiblingAutoAdoptScan {
				return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
			}
			scanned++

			resolveCtx, resolveCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
			store, worktree, commonDir, err := stateStoreForWorktree(resolveCtx, sibling)
			resolveCancel()
			if err != nil {
				// The pre-filters above already established that this sibling has both
				// a .git entry and a .entire directory, so it is a plausible Entire
				// repo whose sessions we simply could not enumerate. Fail closed
				// rather than count it as holding no candidates.
				logging.Debug(logging.WithComponent(ctx, "session"),
					"auto-adopt: sibling repo could not be resolved; failing closed",
					slog.String("sibling", sibling),
					slog.String("error", err.Error()),
				)
				return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
			}
			if sameAdoptStore(commonDir, targetCommonDir) {
				continue
			}
			states, err := store.List(ctx)
			if err != nil {
				logging.Debug(logging.WithComponent(ctx, "session"),
					"auto-adopt: sibling session store could not be listed; failing closed",
					slog.String("sibling", sibling),
					slog.String("error", err.Error()),
				)
				return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
			}
			for _, state := range states {
				if !isRecentAdoptCandidate(state) {
					continue
				}
				claim, claimErr := session.LiveSessionClaimContext(ctx, state.SessionID)
				if claimErr != nil {
					return autoAdoptDiscoveryResult{Candidates: out, Complete: false}
				}
				if freshAdoptClaim(claim) {
					continue
				}
				cand, ok := candidateFromLoaded(store, worktree, commonDir, state, staged, owner, hasOwner)
				if ok {
					out = append(out, cand)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return autoAdoptDiscoveryResult{Candidates: out, Complete: true}
		}
	}
}

// worktreePathGone reports whether path no longer exists. Any other stat error
// (permissions, I/O) is deliberately NOT "gone": the caller uses this only to
// separate a definitively stale registry pointer from a candidate it merely
// failed to inspect, and it must fail closed on the latter.
func worktreePathGone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// candidateFromSource resolves and screens one registry entry. The bool reports
// whether the session is a candidate; a non-nil error means the entry could NOT
// be screened either way, which callers must treat as an incomplete scan rather
// than a rejection. A registry entry whose worktree no longer exists on disk is
// a definitive rejection (a stale pointer), not an inspection failure — treating
// it as one would disable auto-adopt for the registry TTL after every
// `git worktree remove`.
func candidateFromSource(
	ctx context.Context,
	sourceWorktree, sessionID string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) (autoAdoptCandidate, bool, error) {
	if strings.TrimSpace(sourceWorktree) == "" {
		return autoAdoptCandidate{}, false, nil
	}
	if worktreePathGone(sourceWorktree) {
		return autoAdoptCandidate{}, false, nil
	}
	store, worktree, commonDir, err := stateStoreForWorktree(ctx, sourceWorktree)
	if err != nil {
		return autoAdoptCandidate{}, false, fmt.Errorf("resolve candidate worktree %s: %w", sourceWorktree, err)
	}
	state, err := store.Load(ctx, sessionID)
	if err != nil {
		return autoAdoptCandidate{}, false, fmt.Errorf("load candidate session state: %w", err)
	}
	if state == nil {
		return autoAdoptCandidate{}, false, nil
	}
	cand, ok := candidateFromLoaded(store, worktree, commonDir, state, staged, owner, hasOwner)
	return cand, ok, nil
}

func candidateFromLoaded(
	store *session.StateStore,
	worktree, commonDir string,
	state *session.State,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) (autoAdoptCandidate, bool) {
	if !isRecentAdoptCandidate(state) {
		return autoAdoptCandidate{}, false
	}
	// Auto-adopt is ACTIVE-only (matches the live-registry prefilter; see
	// docs/architecture/sessions-and-checkpoints.md, "Automatic cross-common-dir
	// adoption").
	// Idle is adoptable manually but must not be silently relocated on a commit hook.
	if state.Phase != session.PhaseActive {
		return autoAdoptCandidate{}, false
	}
	// Reject stale WorktreePath mismatches. Overriding worktree with
	// state.WorktreePath would make sessionBelongsToSourceWorktree a tautology
	// (comparing the recorded path against itself) and miss the ownership check.
	if state.WorktreePath != "" && !sameAdoptPath(state.WorktreePath, worktree) {
		return autoAdoptCandidate{}, false
	}
	// Both owner match and distinctive FilesTouched overlap are required.
	// Boilerplate-only overlap (README.md, go.mod, …) is ignored even when
	// the owner matches — those names collide across unrelated siblings.
	overlapMatch := filesTouchedOverlap(state.FilesTouched, staged)
	if !overlapMatch {
		return autoAdoptCandidate{}, false
	}
	ownerMatch := hasOwner && ownerMatches(state.Owner, owner)
	if !ownerMatch {
		return autoAdoptCandidate{}, false
	}
	return autoAdoptCandidate{
		SessionID:    state.SessionID,
		WorktreePath: worktree,
		CommonDir:    commonDir,
		Store:        store,
	}, true
}

// revalidateAutoAdoptSource re-applies the automatic-adoption guards to the
// source state as loaded under the source lock.
//
// The locked reload inside adoptFromExternalSessionStore only re-checks the broad
// manual-adopt eligibility (not ended / not condensed, belongs to the source
// worktree). Every guard that makes AUTOMATIC adoption safe — ACTIVE phase,
// recency, matching process owner, distinctive overlap with the staged paths —
// was evaluated on an unlocked read, so it is re-run here against the same
// candidate. Reusing candidateFromLoaded keeps discovery and revalidation from
// drifting apart.
func revalidateAutoAdoptSource(
	locked *session.State,
	expected autoAdoptCandidate,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) error {
	if locked == nil {
		return errors.New("source session state disappeared under the adopt lock")
	}
	if locked.SessionID != expected.SessionID {
		return fmt.Errorf("source session identity changed under the adopt lock: got %s, want %s",
			locked.SessionID, expected.SessionID)
	}
	if _, ok := candidateFromLoaded(
		expected.Store, expected.WorktreePath, expected.CommonDir, locked, staged, owner, hasOwner,
	); !ok {
		return errors.New("source no longer satisfies the automatic-adoption guards")
	}
	return nil
}

// stateBelongsToTargetWorktree reports whether state was written by the worktree
// identified by worktreePath/worktreeID.
//
// The worktree ID is the durable identity and wins whenever both sides have one;
// the path is only a fallback for states written before WorktreeID was recorded.
// Matching on either would make a reused path sufficient: remove worktree A and
// create B at the same path, and A's leftover state would suppress auto-adopt in
// B, or B's post-commit would finalize (and destructively retire the source of)
// A's pending adoption. Mirrors sessionBelongsToSourceWorktree.
func stateBelongsToTargetWorktree(state *session.State, worktreePath, worktreeID string) bool {
	if state == nil {
		return false
	}
	if state.WorktreeID != "" && worktreeID != "" {
		return state.WorktreeID == worktreeID
	}
	return state.WorktreePath != "" && sameAdoptPath(state.WorktreePath, worktreePath)
}

func isRecentLiveEntry(entry session.LiveSessionEntry) bool {
	if entry.Phase != session.PhaseActive {
		return false
	}
	if entry.LastInteractionTime == nil {
		return false
	}
	// LiveSessionMaxAge matches adoptRecentWindow; registry TTL sweep uses the same bound.
	return time.Since(*entry.LastInteractionTime) <= session.LiveSessionMaxAge
}

// siblingLooksLikeGitWorktree reports whether dir has a .git entry (directory,
// gitfile, or symlink) so we can skip non-repos before spawning git.
func siblingLooksLikeGitWorktree(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// siblingLooksLikeEntireRepo reports whether dir has a .entire/ directory
// (written by `entire enable`). Non-Entire siblings have nothing to adopt.
func siblingLooksLikeEntireRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".entire"))
	return err == nil
}

func ownerMatches(recorded *proclive.Identity, current proclive.Identity) bool {
	if recorded == nil || recorded.PID == 0 || current.PID == 0 {
		return false
	}
	if recorded.PID != current.PID || recorded.Start != current.Start {
		return false
	}
	// Boot mismatch means a reboot; Start is only unique within a single boot
	// (same guard as proclive.Check).
	if recorded.Boot != "" && current.Boot != "" && recorded.Boot != current.Boot {
		return false
	}
	if recorded.Host != "" && current.Host != "" && recorded.Host != current.Host {
		return false
	}
	return true
}

// autoAdoptBoilerplateBasenames are relative-path basenames that commonly exist
// in every sibling repo. A match on these alone is not evidence the agent was
// working on the commit's real change set.
var autoAdoptBoilerplateBasenames = map[string]struct{}{
	"readme":              {},
	"readme.md":           {},
	"readme.rst":          {},
	"readme.txt":          {},
	"license":             {},
	"license.md":          {},
	"copying":             {},
	"changelog.md":        {},
	"contributing.md":     {},
	"codeowners":          {},
	".gitignore":          {},
	".gitattributes":      {},
	".editorconfig":       {},
	".env":                {},
	".env.example":        {},
	".nvmrc":              {},
	".node-version":       {},
	"go.mod":              {},
	"go.sum":              {},
	"package.json":        {},
	"package-lock.json":   {},
	"yarn.lock":           {},
	"pnpm-lock.yaml":      {},
	"cargo.toml":          {},
	"cargo.lock":          {},
	"gemfile":             {},
	"gemfile.lock":        {},
	"makefile":            {},
	"dockerfile":          {},
	"docker-compose.yml":  {},
	"docker-compose.yaml": {},
	"tsconfig.json":       {},
	"jsconfig.json":       {},
	"pyproject.toml":      {},
	"requirements.txt":    {},
	"setup.py":            {},
	"setup.cfg":           {},
	"pipfile":             {},
	"pipfile.lock":        {},
	"composer.json":       {},
	"composer.lock":       {},
}

func filesTouchedOverlap(touched, staged []string) bool {
	if len(touched) == 0 || len(staged) == 0 {
		return false
	}
	stagedSet := make(map[string]struct{}, len(staged))
	for _, f := range staged {
		stagedSet[filepath.ToSlash(filepath.Clean(f))] = struct{}{}
	}
	for _, f := range touched {
		key := filepath.ToSlash(filepath.Clean(f))
		if _, ok := stagedSet[key]; !ok {
			continue
		}
		if isAutoAdoptBoilerplatePath(key) {
			continue
		}
		return true
	}
	return false
}

func isAutoAdoptBoilerplatePath(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	_, ok := autoAdoptBoilerplateBasenames[base]
	return ok
}

func stagedFilesForAutoAdopt(ctx context.Context, repoRoot string) ([]string, error) {
	// -z emits NUL-separated, unquoted paths. Without it git C-quotes non-ASCII
	// names (core.quotepath defaults on, e.g. "caf\303\251.txt") which would
	// never match the UTF-8 paths recorded in FilesTouched, so an agent working
	// on a non-ASCII/spaced file would silently miss the overlap check.
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "-z")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --cached --name-only -z: %w", err)
	}
	var staged []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			staged = append(staged, filepath.ToSlash(name))
		}
	}
	return staged, nil
}

func targetIsEntireEnabled(ctx context.Context) bool {
	stngs, err := settings.Load(ctx)
	if err != nil || stngs == nil {
		return false
	}
	return stngs.Enabled
}
