package repopolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

const worktreeIDHashLength = 6

// Repository contains the stable repository facts needed by policy.
type Repository struct {
	WorktreeRoot string
	GitCommonDir string
	WorktreeID   string
	WorktreeKey  string
}

// RepositoryResolver resolves repository facts lazily after global enablement
// and exclusion validation have passed their no-Git fast path.
type RepositoryResolver func(context.Context) (Repository, error)

// HashWorktreeID returns the stable namespace key used for a Git worktree.
func HashWorktreeID(worktreeID string) string {
	hash := sha256.Sum256([]byte(worktreeID))
	return hex.EncodeToString(hash[:])[:worktreeIDHashLength]
}

// ResolveRepository resolves repository facts for the current directory.
func ResolveRepository(ctx context.Context) (Repository, error) {
	return ResolveRepositoryAt(ctx, ".")
}

// ResolveRepositoryAt resolves repository facts without importing the parent
// settings or paths packages.
func ResolveRepositoryAt(ctx context.Context, dir string) (Repository, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel", "--git-common-dir", "--git-dir")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return Repository{}, fmt.Errorf("resolving repository: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 3 {
		return Repository{}, fmt.Errorf("resolving repository: expected three paths, got %d", len(lines))
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return Repository{}, fmt.Errorf("resolving repository base: %w", err)
	}
	root := canonicalPath(absolutizeGitPath(base, lines[0]))
	commonDir := canonicalPath(absolutizeGitPath(base, lines[1]))
	gitDir := canonicalPath(absolutizeGitPath(base, lines[2]))
	worktreeID, err := worktreeIDFromGitDirs(commonDir, gitDir)
	if err != nil {
		return Repository{}, err
	}
	return Repository{
		WorktreeRoot: root,
		GitCommonDir: commonDir,
		WorktreeID:   worktreeID,
		WorktreeKey:  HashWorktreeID(worktreeID),
	}, nil
}

func absolutizeGitPath(base, path string) string {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func worktreeIDFromGitDirs(commonDir, gitDir string) (string, error) {
	if gitDir == commonDir {
		return "", nil
	}
	relative, err := filepath.Rel(commonDir, gitDir)
	if err != nil {
		return "", fmt.Errorf("resolving worktree identity: %w", err)
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) == 2 && parts[0] == "worktrees" && parts[1] != "" {
		return parts[1], nil
	}
	return "", fmt.Errorf("unexpected git worktree directory %q for common directory %q", gitDir, commonDir)
}

func inactiveGlobalPolicy(reason InactiveReason) RepoPolicy {
	return RepoPolicy{ActivationSource: ActivationInactive, InactiveReason: reason}
}

func policyForRepository(repository Repository) RepoPolicy {
	return RepoPolicy{
		ActivationSource: ActivationGlobal,
		WorktreeRoot:     repository.WorktreeRoot,
		GitCommonDir:     repository.GitCommonDir,
		WorktreeKey:      repository.WorktreeKey,
	}
}

// ClassifyGlobalConfig evaluates global activation without logging or writes.
// Disabled and invalid configurations return before repository resolution.
func ClassifyGlobalConfig(ctx context.Context, config *GlobalConfig, resolve RepositoryResolver) (RepoPolicy, error) {
	if config == nil || !config.Enabled {
		return inactiveGlobalPolicy(InactiveReasonGlobalOff), nil
	}
	if problems := validateGlobalExclusions(config); len(problems) > 0 {
		return inactiveGlobalPolicy(InactiveReasonGlobalExcluded), errors.New(strings.Join(problems, "; "))
	}
	if resolve == nil {
		return inactiveGlobalPolicy(InactiveReasonGlobalOff), errors.New("repository resolver is nil")
	}
	repository, err := resolve(ctx)
	if err != nil {
		return inactiveGlobalPolicy(InactiveReasonGlobalOff), err
	}
	policy := policyForRepository(repository)

	excluded, err := MatchesExcludePath(ctx, config.ExcludePaths, repository.WorktreeRoot)
	if err != nil {
		policy.ActivationSource = ActivationInactive
		policy.InactiveReason = InactiveReasonGlobalExcluded
		return policy, err
	}
	if excluded {
		policy.ActivationSource = ActivationInactive
		policy.InactiveReason = InactiveReasonGlobalExcluded
		return policy, nil
	}
	excluded, err = MatchesExcludePathExact(ctx, config.ExcludePathsExact, repository.WorktreeRoot)
	if err != nil {
		policy.ActivationSource = ActivationInactive
		policy.InactiveReason = InactiveReasonGlobalExcluded
		return policy, err
	}
	if excluded {
		policy.ActivationSource = ActivationInactive
		policy.InactiveReason = InactiveReasonGlobalExcluded
		return policy, nil
	}
	if len(config.ExcludeOrigins) > 0 {
		origins, found, lookupErr := gitremote.GetRemoteURLsInDirIfSet(ctx, repository.WorktreeRoot, "origin")
		if lookupErr != nil {
			policy.ActivationSource = ActivationInactive
			policy.InactiveReason = InactiveReasonGlobalExcluded
			return policy, fmt.Errorf("reading origin remote: %w", lookupErr)
		}
		if found {
			for _, origin := range origins {
				normalized := NormalizeOrigin(origin)
				if normalized == "" {
					policy.ActivationSource = ActivationInactive
					policy.InactiveReason = InactiveReasonGlobalExcluded
					return policy, fmt.Errorf("origin %q cannot be normalized", origin)
				}
				matched, matchErr := MatchesExcludeOrigin(ctx, config.ExcludeOrigins, normalized)
				if matchErr != nil {
					policy.ActivationSource = ActivationInactive
					policy.InactiveReason = InactiveReasonGlobalExcluded
					return policy, matchErr
				}
				if matched {
					policy.ActivationSource = ActivationInactive
					policy.InactiveReason = InactiveReasonGlobalExcluded
					return policy, nil
				}
			}
		}
	}
	policy.Active = true
	policy.InactiveReason = InactiveReasonNone
	return policy, nil
}

var globalModeCache struct {
	sync.Mutex

	key    string
	set    bool
	policy RepoPolicy
	err    error
}

// GlobalModeStatus loads and memoizes global classification per working
// directory. The resolver is reached only for an enabled, valid tier.
func GlobalModeStatus(ctx context.Context) (RepoPolicy, error) {
	cwd, err := os.Getwd() //nolint:forbidigo // cache key; resolving Git here would defeat the disabled fast path
	if err != nil {
		cwd = ""
	}
	globalModeCache.Lock()
	defer globalModeCache.Unlock()
	if globalModeCache.set && globalModeCache.key == cwd {
		return globalModeCache.policy, globalModeCache.err
	}
	settings, loadErr := LoadUserSettings(ctx)
	if loadErr != nil {
		globalModeCache.key = cwd
		globalModeCache.set = true
		globalModeCache.policy = inactiveGlobalPolicy(InactiveReasonGlobalOff)
		globalModeCache.err = loadErr
		return globalModeCache.policy, loadErr
	}
	policy, classifyErr := ClassifyGlobalConfig(ctx, settings.Global, ResolveRepository)
	globalModeCache.key = cwd
	globalModeCache.set = true
	globalModeCache.policy = policy
	globalModeCache.err = classifyErr
	return policy, classifyErr
}

// ClearGlobalModeCache clears package-owned classification state.
func ClearGlobalModeCache() {
	globalModeCache.Lock()
	globalModeCache.key = ""
	globalModeCache.set = false
	globalModeCache.policy = RepoPolicy{}
	globalModeCache.err = nil
	globalModeCache.Unlock()
}
