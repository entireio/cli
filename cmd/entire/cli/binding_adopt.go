package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
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
	if !settings.IsSetUpAtRoot(ev.Repo.WorktreeRoot) {
		return false, nil
	}
	rec, err := binding.LoadRecord(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("load session binding record: %w", err)
	}
	if rec == nil {
		return false, errors.New("session binding record not found")
	}
	markedAdopted := false
	for _, br := range rec.BoundRepos {
		if br.CommonDir == ev.Repo.CommonDir && br.AdoptedAt != nil {
			markedAdopted = true
			break
		}
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
	targetStore := session.NewStateStoreWithDir(filepath.Join(ev.Repo.CommonDir, session.SessionStateDirName))
	if markedAdopted {
		existing, loadErr := targetStore.Load(ctx, sessionID)
		if loadErr != nil {
			return false, fmt.Errorf("verify adopted target session state: %w", loadErr)
		}
		if existing != nil {
			return true, nil
		}
	}

	var source *session.State
	if currentWorktreeRoot != "" {
		source, err = strategy.LoadSessionState(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("load source session state: %w", err)
		}
	}

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
	if err := binding.MarkRepoAdopted(ctx, sessionID, ev.Repo.CommonDir); err != nil {
		return false, fmt.Errorf("mark target repo adopted: %w", err)
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
	untrackedFiles := targetUntrackedFiles(ctx, repo)

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
		headHash:       head.Hash().String(),
		worktreeRoot:   target.WorktreeRoot,
		worktreeID:     worktreeID,
		branch:         strategy.GetCurrentBranchName(repo),
		filesTouched:   append([]string(nil), evidenceFiles...),
		untrackedFiles: untrackedFiles,
		now:            now,
	}, true)
	return &replicated, nil
}

func targetUntrackedFiles(ctx context.Context, repo *git.Repository) []string {
	status, err := gitrepo.StatusWithBudget(ctx, repo)
	if err != nil {
		return nil
	}
	var untracked []string
	for name, fileStatus := range status {
		if name == ".entire" || strings.HasPrefix(name, ".entire/") {
			continue
		}
		if fileStatus.Worktree == git.Untracked || fileStatus.Staging == git.Untracked {
			untracked = append(untracked, filepath.ToSlash(name))
		}
	}
	sort.Strings(untracked)
	return untracked
}
