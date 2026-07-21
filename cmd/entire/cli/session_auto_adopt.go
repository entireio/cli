package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// maxSiblingAutoAdoptScan caps how many sibling directories prepare-commit-msg will
// inspect when the live-session registry has no unique candidate. Keeps the
// hook bounded on large parent directories.
const maxSiblingAutoAdoptScan = 32

// tryAutoAdoptCrossCommonDirSession adopts a unique ACTIVE session from another
// git common dir into the current worktree when it is safe to do so.
//
// Discovery order:
//  1. Live-session registry (written on ACTIVE StateStore.Save)
//  2. Immediate sibling repos under the parent directory (seeded / microservices)
//
// A candidate is accepted only when exactly one match remains after filtering
// for recent ACTIVE sessions that (a) share the committing process owner and
// (b) have FilesTouched overlapping the staged commit paths. Owner match is
// required so an unrelated sibling touching the same relative filename
// (README.md, package.json, …) cannot silently steal/retire another repo's
// live session on a human commit. Overlap alone is never enough. Ambiguity skips.
//
// Best-effort: never returns an error to the git hook caller.
func tryAutoAdoptCrossCommonDirSession(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, "session")

	if !targetIsEntireEnabled(ctx) {
		return
	}

	targetWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil || targetWorktree == "" {
		return
	}
	targetStore, _, targetCommonDir, err := stateStoreForWorktree(ctx, targetWorktree)
	if err != nil {
		return
	}
	if hasLocalActiveSession(ctx, targetStore) {
		return
	}

	staged, err := stagedFilesForAutoAdopt(ctx, targetWorktree)
	if err != nil {
		staged = nil
	}
	owner, hasOwner := proclive.ResolveOwner()

	candidates := collectRegistryAutoAdoptCandidates(ctx, targetCommonDir, staged, owner, hasOwner)
	if len(candidates) == 0 {
		candidates = collectSiblingAutoAdoptCandidates(ctx, targetWorktree, targetCommonDir, staged, owner, hasOwner)
	}
	if len(candidates) != 1 {
		if len(candidates) > 1 {
			logging.Debug(logCtx, "auto-adopt: skipped ambiguous cross-common-dir sessions",
				slog.Int("candidates", len(candidates)),
			)
		}
		return
	}

	source := candidates[0]
	existing, err := targetStore.Load(ctx, source.SessionID)
	if err != nil || existing != nil {
		return
	}

	logging.Info(logCtx, "auto-adopt: adopting cross-common-dir session for commit",
		slog.String("session_id", source.SessionID),
		slog.String("from_worktree", source.WorktreePath),
		slog.String("to_worktree", targetWorktree),
	)

	_, _, adoptErr := adoptFromExternalSessionStore(
		ctx,
		source.Store,
		source.WorktreePath,
		source.CommonDir,
		targetStore,
		targetCommonDir,
		source.SessionID,
		adoptOptions{Force: true, SkipTranscriptValidation: true},
	)
	if adoptErr != nil {
		logging.Debug(logCtx, "auto-adopt: adopt failed",
			slog.String("session_id", source.SessionID),
			slog.String("error", adoptErr.Error()),
		)
	}
}

type autoAdoptCandidate struct {
	SessionID    string
	WorktreePath string
	CommonDir    string
	Store        *session.StateStore
	OwnerMatch   bool
	OverlapMatch bool
}

func hasLocalActiveSession(ctx context.Context, store *session.StateStore) bool {
	states, err := store.List(ctx)
	if err != nil {
		return true // fail closed: do not steal when local store is unreadable
	}
	for _, state := range states {
		if isAdoptableSourceSession(state) {
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
) []autoAdoptCandidate {
	entries, err := session.ListLiveSessions()
	if err != nil || len(entries) == 0 {
		return nil
	}

	var out []autoAdoptCandidate
	for _, entry := range entries {
		if sameAdoptStore(entry.CommonDir, targetCommonDir) {
			continue
		}
		if !isRecentLiveEntry(entry) {
			continue
		}
		cand, ok := candidateFromSource(ctx, entry.WorktreePath, entry.SessionID, staged, owner, hasOwner)
		if ok {
			out = append(out, cand)
		}
	}
	return out
}

func collectSiblingAutoAdoptCandidates(
	ctx context.Context,
	targetWorktree, targetCommonDir string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) []autoAdoptCandidate {
	parent := filepath.Dir(targetWorktree)
	if parent == "" || parent == targetWorktree {
		return nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}

	var out []autoAdoptCandidate
	scanned := 0
	for _, entry := range entries {
		if scanned >= maxSiblingAutoAdoptScan {
			break
		}
		if !entry.IsDir() {
			continue
		}
		sibling := filepath.Join(parent, entry.Name())
		if sameAdoptPath(sibling, targetWorktree) {
			continue
		}
		scanned++

		store, worktree, commonDir, err := stateStoreForWorktree(ctx, sibling)
		if err != nil || sameAdoptStore(commonDir, targetCommonDir) {
			continue
		}
		states, err := store.List(ctx)
		if err != nil {
			continue
		}
		for _, state := range states {
			if !isRecentAdoptCandidate(state) {
				continue
			}
			cand, ok := candidateFromLoaded(store, worktree, commonDir, state, staged, owner, hasOwner)
			if ok {
				out = append(out, cand)
			}
		}
	}
	return out
}

func candidateFromSource(
	ctx context.Context,
	sourceWorktree, sessionID string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) (autoAdoptCandidate, bool) {
	store, worktree, commonDir, err := stateStoreForWorktree(ctx, sourceWorktree)
	if err != nil {
		return autoAdoptCandidate{}, false
	}
	state, err := store.Load(ctx, sessionID)
	if err != nil || state == nil {
		return autoAdoptCandidate{}, false
	}
	return candidateFromLoaded(store, worktree, commonDir, state, staged, owner, hasOwner)
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
	if state.WorktreePath != "" && !sameAdoptPath(state.WorktreePath, worktree) {
		// Prefer the recorded worktree path when validating source ownership.
		worktree = state.WorktreePath
	}
	// Both owner match and FilesTouched overlap are required. Overlap alone
	// would let unrelated sibling repos steal via common relative names;
	// owner alone would retire a session on an unrelated commit.
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
		OwnerMatch:   ownerMatch,
		OverlapMatch: overlapMatch,
	}, true
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

func ownerMatches(recorded *proclive.Identity, current proclive.Identity) bool {
	if recorded == nil || recorded.PID == 0 || current.PID == 0 {
		return false
	}
	if recorded.PID != current.PID || recorded.Start != current.Start {
		return false
	}
	if recorded.Host != "" && current.Host != "" && recorded.Host != current.Host {
		return false
	}
	return true
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
		if _, ok := stagedSet[key]; ok {
			return true
		}
	}
	return false
}

func stagedFilesForAutoAdopt(ctx context.Context, repoRoot string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --cached --name-only: %w", err)
	}
	trimmed := strings.TrimSpace(string(output))
	trimmed = strings.ReplaceAll(trimmed, "\r\n", "\n")
	if trimmed == "" {
		return nil, nil
	}
	var staged []string
	for _, line := range strings.Split(trimmed, "\n") {
		if line != "" {
			staged = append(staged, filepath.ToSlash(line))
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
