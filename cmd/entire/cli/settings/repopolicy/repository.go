package repopolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/internal/worktreeid"
)

// Repository contains the stable repository facts needed by policy.
type Repository struct {
	WorktreeRoot string
	GitCommonDir string
	WorktreeID   string
	WorktreeKey  string
}

// GlobalRuntimeRoot is where the global tier keeps this worktree's runtime
// data (metadata, logs, tmp): <git-common-dir>/entire/worktree/<key>. It is a
// function of repository identity alone — not of the current policy — so
// uninstall can find data left by a globally tracked period after the tier
// was turned off or the repo excluded. "" when identity is incomplete.
func (r Repository) GlobalRuntimeRoot() string {
	if r.GitCommonDir == "" || r.WorktreeKey == "" {
		return ""
	}
	return filepath.Join(r.GitCommonDir, WorktreeRegistryRelative, r.WorktreeKey)
}

// RepositoryResolver resolves repository facts lazily after global enablement
// and exclusion validation have passed their no-Git fast path.
type RepositoryResolver func(context.Context) (Repository, error)

// runtimeKeyLength is the hex length of the per-worktree runtime directory
// name: 64 bits. The 6-char hash that names shadow branches tolerates a
// collision (checkpoints interleave on a shared branch by design); a runtime
// root does not — two linked worktrees colliding would merge their session
// state, logs, and tmp on disk — so the directory gets a wider key.
const runtimeKeyLength = 16

// runtimeKey returns the stable runtime-directory name for a Git worktree ID.
func runtimeKey(worktreeID string) string {
	sum := sha256.Sum256([]byte(worktreeID))
	return hex.EncodeToString(sum[:])[:runtimeKeyLength]
}

// ResolveRepository resolves repository facts for the current directory.
func ResolveRepository(ctx context.Context) (Repository, error) {
	return ResolveRepositoryAt(ctx, ".")
}

// ResolveRepositoryAt resolves repository facts without importing the parent
// settings or paths packages.
func ResolveRepositoryAt(ctx context.Context, dir string) (Repository, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel", "--git-common-dir")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return Repository{}, fmt.Errorf("resolving repository: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 2 {
		return Repository{}, fmt.Errorf("resolving repository: expected two paths, got %d", len(lines))
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return Repository{}, fmt.Errorf("resolving repository base: %w", err)
	}
	root := canonicalPath(absolutizeGitPath(base, lines[0]))
	commonDir := canonicalPath(absolutizeGitPath(base, lines[1]))
	worktreeID, err := worktreeid.Get(root)
	if err != nil {
		return Repository{}, fmt.Errorf("resolving worktree identity: %w", err)
	}
	return Repository{
		WorktreeRoot: root,
		GitCommonDir: commonDir,
		WorktreeID:   worktreeID,
		WorktreeKey:  runtimeKey(worktreeID),
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

	excluded, err := ExcludedByGlobalConfig(ctx, config, repository)
	if err != nil || excluded {
		policy.ActivationSource = ActivationInactive
		policy.InactiveReason = InactiveReasonGlobalExcluded
		return policy, err
	}
	policy.Active = true
	policy.InactiveReason = InactiveReasonNone
	return policy, nil
}

// ExcludedByGlobalConfig evaluates the user's exclude lists against one
// repository: exclude_paths (glob), exclude_paths_exact, and exclude_origins
// (against every origin URL, normalized). An unusable pattern or an origin
// that cannot be normalized is an error — callers fail closed, treating the
// repo as excluded. This is the user's explicit "never here", so it is
// consulted for BOTH activation sources once the tier is on: the global tier
// (ClassifyGlobalConfig) and a repo activated by its own committed
// settings.json (ClassifyRepoPolicyAt) — repository content must not outrank
// the user's machine-local exclusions.
func ExcludedByGlobalConfig(ctx context.Context, config *GlobalConfig, repository Repository) (bool, error) {
	if config == nil {
		return false, nil
	}
	if problems := validateGlobalExclusions(config); len(problems) > 0 {
		return true, errors.New(strings.Join(problems, "; "))
	}
	excluded, err := MatchesExcludePath(ctx, config.ExcludePaths, repository.WorktreeRoot)
	if err != nil || excluded {
		return true, err
	}
	excluded, err = MatchesExcludePathExact(ctx, config.ExcludePathsExact, repository.WorktreeRoot)
	if err != nil || excluded {
		return true, err
	}
	if len(config.ExcludeOrigins) == 0 {
		return false, nil
	}
	origins, fetchFound, lookupErr := gitremote.GetRemoteURLsInDirIfSet(ctx, repository.WorktreeRoot, "origin")
	if lookupErr != nil {
		return true, fmt.Errorf("reading origin remote: %w", lookupErr)
	}
	pushOrigins, pushFound, lookupErr := gitremote.GetRemotePushURLsInDirIfSet(ctx, repository.WorktreeRoot, "origin")
	if lookupErr != nil {
		return true, fmt.Errorf("reading origin pushurl: %w", lookupErr)
	}
	origins = append(origins, pushOrigins...)
	if !fetchFound && !pushFound {
		return false, nil
	}
	for _, origin := range origins {
		normalized := NormalizeOrigin(origin)
		if normalized == "" {
			return true, errors.New("origin remote cannot be normalized")
		}
		matched, matchErr := MatchesExcludeOrigin(ctx, config.ExcludeOrigins, normalized)
		if matchErr != nil || matched {
			return true, matchErr
		}
	}
	return false, nil
}
