package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v6"
)

const bindingAdoptLockTimeout = 2 * time.Second

// ensureSessionReplicated performs additive adoption for one enabled binding:
// the same session becomes active in the target repo without retiring or
// modifying its source state. The binding record's marker is only written
// after target state exists durably, so every failure is safe to retry.
func ensureSessionReplicated(ctx context.Context, sessionID string, meta binding.SessionMeta, currentWorktreeRoot string, ev binding.Evidence) (bool, error) {
	if !ev.Enabled {
		return false, nil
	}
	// Enabled is evidence-time data. Re-check immediately before the repo write
	// so a concurrent explicit disable remains an absolute veto.
	if !settings.IsActiveAtRoot(ctx, ev.Repo.WorktreeRoot) {
		return false, nil
	}
	rec, err := binding.LoadRecord(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("load session binding record: %w", err)
	}
	if rec == nil {
		return false, errors.New("session binding record not found")
	}

	if sameAdoptPath(currentWorktreeRoot, ev.Repo.WorktreeRoot) {
		return false, nil
	}
	// ResolveRepoForPath accepts an evidence FILE path and starts at its parent;
	// use the repo's .git entry so the first probed directory is the worktree.
	resolved, ok := binding.ResolveRepoForPath(ctx, filepath.Join(ev.Repo.WorktreeRoot, ".git"))
	if !ok || !sameAdoptStore(resolved.CommonDir, ev.Repo.CommonDir) {
		return false, errors.New("target repo identity changed before adoption")
	}
	// A linked worktree of the SAME clone is not a target: session state lives
	// in the git common dir, shared by every worktree of a clone, so the
	// "target store" here would be the source's own store — the fast path
	// below would read the source state as an existing replica and the replay
	// child would then run the launching session's hooks in a worktree it does
	// not belong to. The commit-linking path already handles multi-worktree
	// sessions (identity-first linking); evidence for the sibling is still
	// recorded, it just never adopts.
	if currentWorktreeRoot != "" {
		if current, ok := binding.ResolveRepoForPath(ctx, filepath.Join(currentWorktreeRoot, ".git")); ok && sameAdoptStore(current.CommonDir, ev.Repo.CommonDir) {
			return false, nil
		}
	}
	targetStore := session.NewStateStoreWithDir(filepath.Join(ev.Repo.CommonDir, session.SessionStateDirName))

	var source *session.State
	if currentWorktreeRoot != "" {
		source, err = strategy.LoadSessionState(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("load source session state: %w", err)
		}
	}

	// The adopted marker in the record is a hint, never the authority: the
	// existence check runs under the target's session-state lock, both for a
	// marked and an unmarked repo, so a replica removed between an unlocked
	// check and the write (cleanup, stale-state collection) is rebuilt rather
	// than assumed present. The lock timeout bounds the ACQUIRE only
	// (WithSessionStateLocks uses its ctx for that alone); the work inside
	// runs under the hook's own ctx so the agent's deadline and a Ctrl-C are
	// still honored, and the target's status walk gets its own small,
	// isolated budget (see targetWorktreeBaseline) so a slow foreign repo can
	// neither hold the target's lock for long nor arm the process-wide status
	// latch that would degrade the launching repo's own capture.
	lockCtx, cancel := context.WithTimeout(ctx, bindingAdoptLockTimeout)
	defer cancel()
	err = strategy.WithSessionStateLocks(lockCtx, sessionID, []string{ev.Repo.CommonDir}, func() error {
		existing, loadErr := targetStore.Load(ctx, sessionID)
		if loadErr != nil {
			return fmt.Errorf("load target session state: %w", loadErr)
		}
		if existing != nil {
			return nil
		}
		state, buildErr := buildReplicatedSessionState(ctx, source, rec, meta, ev.Repo, ev.Files)
		if buildErr != nil {
			return buildErr
		}
		if saveErr := targetStore.Save(ctx, state); saveErr != nil {
			return fmt.Errorf("save target session state: %w", saveErr)
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("replicate target session state: %w", err)
	}
	// The replica exists durably at this point, which is what the caller's
	// "replicated" answer (and the replay it gates) stands for. The marker is
	// bookkeeping: a failure to write it — the record was rewritten by a
	// concurrent evidence write, the lock timed out — must not withhold the
	// replay, or this turn's checkpoint in the target is lost while the state
	// says the session is active there.
	if err := binding.MarkRepoAdopted(ctx, sessionID, ev.Repo.CommonDir); err != nil {
		logging.Warn(logging.WithComponent(ctx, "binding"), "target session state replicated but the adopted marker was not recorded",
			slog.String("session_id", sessionID),
			slog.String("repo", ev.Repo.WorktreeRoot),
			slog.String("error", err.Error()))
	}
	return true, nil
}

func buildReplicatedSessionState(ctx context.Context, source *session.State, rec *binding.SessionRecord, meta binding.SessionMeta, target binding.RepoIdentity, evidenceFiles []string) (*session.State, error) {
	repo, err := gitrepo.OpenPath(target.WorktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("open target repository: %w", err)
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve target HEAD: %w", err)
	}
	worktreeID, err := paths.GetWorktreeID(target.WorktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve target worktree ID: %w", err)
	}
	untrackedFiles, dirtyTrackedFiles := targetWorktreeBaseline(ctx, repo)

	now := time.Now()
	var replicated session.State
	if source != nil {
		replicated = cloneAdoptSourceState(source)
		if replicated.AgentType == "" {
			replicated.AgentType = types.AgentType(meta.AgentType)
		}
		if replicated.TranscriptPath == "" {
			replicated.TranscriptPath = meta.TranscriptPath
		}
	} else {
		startedAt := rec.CreatedAt
		if startedAt.IsZero() {
			startedAt = now
		}
		replicated = session.State{
			SessionID:      rec.SessionID,
			StartedAt:      startedAt,
			AgentType:      types.AgentType(rec.AgentType),
			TranscriptPath: rec.TranscriptPath,
		}
		if replicated.AgentType == "" {
			replicated.AgentType = types.AgentType(meta.AgentType)
		}
		if replicated.TranscriptPath == "" {
			replicated.TranscriptPath = meta.TranscriptPath
		}
		turnID, generateErr := id.Generate()
		if generateErr != nil {
			return nil, fmt.Errorf("generate target turn ID: %w", generateErr)
		}
		replicated.TurnID = turnID.String()
	}

	resetSessionStateForTarget(&replicated, sessionTargetSnapshot{
		headHash:          head.Hash().String(),
		worktreeRoot:      target.WorktreeRoot,
		worktreeID:        worktreeID,
		branch:            strategy.GetCurrentBranchName(repo),
		filesTouched:      append([]string(nil), evidenceFiles...),
		untrackedFiles:    untrackedFiles,
		dirtyTrackedFiles: dirtyTrackedFiles,
		now:               now,
	}, true)
	return &replicated, nil
}

// bindingAdoptStatusBudget bounds the target's status walk during adoption.
// It runs under the target's session-state lock, which every hook in that
// repo waits on, so it must stay far below StatusWalkBudget.
const bindingAdoptStatusBudget = 5 * time.Second

// targetWorktreeBaseline seeds the replica's two baselines from one status
// walk: untracked files (so pre-existing untracked files are not attributed
// as new) and tracked paths already modified/deleted/staged (so a replayed
// turn-end, which has no pre-prompt baseline, does not attribute the user's
// own pending edits or deletions to the session). Best-effort: a failure is
// logged and adoption proceeds with empty baselines.
func targetWorktreeBaseline(ctx context.Context, repo *git.Repository) (untracked, dirtyTracked []string) {
	status, err := gitrepo.StatusWithIsolatedBudget(ctx, repo, bindingAdoptStatusBudget)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "binding"), "adopted target's worktree baseline unavailable; pre-existing untracked files and pending edits may be attributed to the session",
			slog.String("error", err.Error()))
		return nil, nil
	}
	for name, fileStatus := range status {
		if name == ".entire" || strings.HasPrefix(name, ".entire/") {
			continue
		}
		slashed := filepath.ToSlash(name)
		switch {
		case fileStatus.Worktree == git.Untracked || fileStatus.Staging == git.Untracked:
			untracked = append(untracked, slashed)
		case fileStatus.Worktree != git.Unmodified || fileStatus.Staging != git.Unmodified:
			dirtyTracked = append(dirtyTracked, slashed)
		}
	}
	sort.Strings(untracked)
	sort.Strings(dirtyTracked)
	return untracked, dirtyTracked
}
