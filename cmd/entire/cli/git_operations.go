package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

func formatFilteredFetchError(prefix, fetchTarget string, output []byte, fetchErr error) error {
	redactedTarget := fetchTarget
	if isFetchTargetURL(fetchTarget) {
		redactedTarget = remote.RedactURL(fetchTarget)
	}

	msg := strings.TrimSpace(string(output))
	if isFetchTargetURL(fetchTarget) {
		msg = strings.TrimSpace(strings.ReplaceAll(msg, fetchTarget, redactedTarget))
	}
	if msg != "" {
		return fmt.Errorf("%s from %s: %s: %w", prefix, redactedTarget, msg, fetchErr)
	}
	return fmt.Errorf("%s from %s: %w", prefix, redactedTarget, fetchErr)
}

func isFetchTargetURL(target string) bool {
	return strings.Contains(target, "://") || strings.Contains(target, "@")
}

// openRepository opens the git repository with linked worktree support enabled.
// This is a convenience wrapper around strategy.OpenRepository() for use in the CLI package.
func openRepository(ctx context.Context) (*git.Repository, error) {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
}

// GitAuthor represents the git user configuration
type GitAuthor struct {
	Name  string
	Email string
}

// GetGitAuthor retrieves the git user.name and user.email from the repository config.
// It checks local config first, then falls back to global config.
// If go-git can't find the config, it falls back to using the git command.
// Returns fallback defaults if no user is configured anywhere.
func GetGitAuthor(ctx context.Context) (*GitAuthor, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	name, email := strategy.GetGitAuthorFromRepo(repo)

	// If go-git returned defaults, try using git command as fallback
	// This handles cases where go-git can't find the config (e.g., different HOME paths,
	// non-standard config locations, or environment issues in hook contexts)
	if name == "Unknown" {
		if gitName := getGitConfigValue(ctx, "user.name"); gitName != "" {
			name = gitName
		}
	}
	if email == "unknown@local" {
		if gitEmail := getGitConfigValue(ctx, "user.email"); gitEmail != "" {
			email = gitEmail
		}
	}

	return &GitAuthor{
		Name:  name,
		Email: email,
	}, nil
}

// getGitConfigValue retrieves a git config value using the git command.
// Returns empty string if the value is not set or on error.
func getGitConfigValue(ctx context.Context, key string) string {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", key)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// IsOnDefaultBranch checks if the repository is currently on the default branch.
// It determines the default branch by:
// 1. Checking the remote origin's HEAD reference
// 2. Falling back to common names (main, master) if remote HEAD is unavailable
// Returns (isDefault, branchName, error)
func IsOnDefaultBranch(ctx context.Context) (bool, string, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return false, "", fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()
	return isOnDefaultBranchRepo(repo)
}

// isOnDefaultBranchRepo reports whether the repo's current branch is its default
// branch, along with the current branch name (empty on a detached HEAD). It
// operates on an already-open repository so callers that already hold one need
// not reopen it.
func isOnDefaultBranchRepo(repo *git.Repository) (bool, string, error) {
	// Get current branch
	head, err := repo.Head()
	if err != nil {
		return false, "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	if !head.Name().IsBranch() {
		// Detached HEAD - not on any branch
		return false, "", nil
	}

	currentBranch := head.Name().Short()

	// Try to get default branch from remote origin's HEAD
	defaultBranch := getDefaultBranchFromRemote(repo)

	// If we couldn't determine from remote, use common defaults
	if defaultBranch == "" {
		// Check if current branch is a common default name
		if currentBranch == defaultBaseBranch || currentBranch == masterBaseBranch {
			return true, currentBranch, nil
		}
		return false, currentBranch, nil
	}

	return currentBranch == defaultBranch, currentBranch, nil
}

// getDefaultBranchFromRemote tries to determine the default branch from the origin remote.
// Returns empty string if unable to determine.
func getDefaultBranchFromRemote(repo *git.Repository) string {
	// Try to get the symbolic reference for origin/HEAD
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), true)
	if err == nil && ref != nil {
		// ref.Target() gives us something like "refs/remotes/origin/main"
		target := ref.Target().String()
		if strings.HasPrefix(target, "refs/remotes/origin/") {
			return strings.TrimPrefix(target, "refs/remotes/origin/")
		}
	}

	// Fallback: check if origin/main or origin/master exists
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", defaultBaseBranch), true); err == nil {
		return defaultBaseBranch
	}
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", masterBaseBranch), true); err == nil {
		return masterBaseBranch
	}

	return ""
}

// ShouldSkipOnDefaultBranch checks if we're on the default branch.
// Returns (shouldSkip, branchName). If shouldSkip is true, the caller should
// skip the operation to avoid polluting main/master history.
// If the branch cannot be determined, returns (false, "") to allow the operation.
func ShouldSkipOnDefaultBranch(ctx context.Context) (bool, string) {
	isDefault, branchName, err := IsOnDefaultBranch(ctx)
	if err != nil {
		// If we can't determine, allow the operation
		return false, ""
	}
	return isDefault, branchName
}

// GetCurrentBranch returns the name of the current branch.
// Returns an error if in detached HEAD state or if not in a git repository.
func GetCurrentBranch(ctx context.Context) (string, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	if !head.Name().IsBranch() {
		return "", errors.New("not on a branch (detached HEAD)")
	}

	return head.Name().Short(), nil
}

// HasUncommittedChanges checks if there are any uncommitted changes in the repository.
// This includes staged changes, unstaged changes, and untracked files.
// Uses git CLI instead of go-git because go-git doesn't respect global gitignore
// (core.excludesfile) which can cause false positives for globally ignored files.
func HasUncommittedChanges(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to get git status: %w", err)
	}

	// If output is empty, there are no changes
	return len(strings.TrimSpace(string(output))) > 0, nil
}

// BranchExistsOnRemote checks if a branch exists on the origin remote.
// First checks local remote-tracking refs, then queries the actual remote
// via git ls-remote in case local refs are stale (e.g., after a fresh clone
// that didn't fetch all branches).
func BranchExistsOnRemote(ctx context.Context, branchName string) (bool, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	// Check for remote reference: refs/remotes/origin/<branchName>
	_, err = repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, fmt.Errorf("failed to check remote branch: %w", err)
	}

	// Local remote-tracking ref not found — query the actual remote.
	lsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	lsCmd := exec.CommandContext(lsCtx, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branchName)
	output, lsErr := lsCmd.Output()
	if lsErr != nil {
		// ls-remote failed (no network, no remote, etc.) — treat as not found
		return false, nil
	}

	return len(bytes.TrimSpace(output)) > 0, nil
}

// BranchExistsLocally checks if a local branch exists.
func BranchExistsLocally(ctx context.Context, branchName string) (bool, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	_, err = repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check branch: %w", err)
	}

	return true, nil
}

// CheckoutBranch switches to the specified local branch or commit.
// Uses git CLI instead of go-git to work around go-git v5 bug where Checkout
// deletes untracked files (see https://github.com/go-git/go-git/issues/970).
// Should be switched back to go-git once we upgrade to go-git v6
// Returns an error if the ref doesn't exist or checkout fails.
func CheckoutBranch(ctx context.Context, ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("checkout failed: invalid ref %q", ref)
	}
	cmd := exec.CommandContext(ctx, "git", "checkout", ref)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkout failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// ValidateBranchName checks if a branch name is valid using git check-ref-format.
// Returns an error if the name is invalid or contains unsafe characters.
func ValidateBranchName(ctx context.Context, branchName string) error {
	if strings.HasPrefix(branchName, "-") {
		return fmt.Errorf("invalid branch name %q", branchName)
	}
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", branchName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("invalid branch name %q", branchName)
	}
	return nil
}

// FetchAndCheckoutRemoteBranch fetches a branch from origin and creates a local tracking branch.
// Uses git CLI instead of go-git for fetch because go-git doesn't use credential helpers,
// which breaks HTTPS URLs that require authentication.
func FetchAndCheckoutRemoteBranch(ctx context.Context, branchName string) error {
	// Validate branch name before using in shell command (branchName comes from user CLI input)
	if err := ValidateBranchName(ctx, branchName); err != nil {
		return err
	}

	// Use git CLI for fetch (go-git's fetch can be tricky with auth)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName)

	// NoFilter: resume needs the full branch content (source files), not just
	// tree structure. A partial clone would leave blobs missing.
	output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   "origin",
		RefSpecs: []string{refSpec},
		NoFilter: true,
	})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("fetch timed out after 2 minutes")
		}
		return fmt.Errorf("failed to fetch branch from origin: %s: %w", strings.TrimSpace(string(output)), err)
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	// Get the remote branch reference
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return fmt.Errorf("branch '%s' not found on origin: %w", branchName, err)
	}

	// Create local branch pointing to the same commit
	localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
	err = repo.Storer.SetReference(localRef)
	if err != nil {
		return fmt.Errorf("failed to create local branch: %w", err)
	}

	// Checkout the new local branch
	return CheckoutBranch(ctx, branchName)
}

// metadataFetchDepth is the absolute --depth used when fetching the metadata
// branch. It is far above any realistic checkpoint-branch length, so it fully
// fetches the branch (and heals a prior --depth=1 shallow boundary on it),
// while staying below math.MaxInt32 (2147483647) — which git special-cases as
// a global unshallow that would also deepen an unrelated shallow source tree.
const metadataFetchDepth = 1_000_000_000

// FetchMetadataBranch fetches the entire/checkpoints/v1 branch from the
// checkpoint read-candidate remotes with full blob content. Used as a
// fallback by resume/explain when the tree-only probe is insufficient (e.g.
// the metadata.json blob is missing).
func FetchMetadataBranch(ctx context.Context) error {
	return fetchMetadataFromReadRemotes(ctx, true /* noFilter */)
}

// FetchMetadataTreeOnly fetches the entire/checkpoints/v1 commit+tree graph
// from the checkpoint read-candidate remotes to resolve the latest
// checkpoint, relying on --filter=blob:none (when filtered fetches are
// enabled) to skip blob content rather than on a shallow --depth=1 fetch.
//
// It deliberately does NOT use --depth=1. A depth-1 fetch adds the fetched tip
// to .git/shallow, and any ref pointing at a shallow commit (the durable
// refs/remotes/origin/<branch> that git updates opportunistically, or the local
// primary) can no longer be walked past that boundary. A later `git merge-base`
// against it then falsely reports "no common ancestor", which makes push and
// `entire doctor` treat an ordinary diverged-but-behind branch as disconnected
// (see strategy.IsMetadataDisconnected). Fetching at full depth keeps the
// remote-tracking ref connected; git fetches incrementally, so after the first
// fetch only new commits/trees travel.
//
// It also heals a repo that an older CLI already shallowed: the ref-scoped deep
// fetch removes the boundary left by a prior --depth=1 fetch rather than letting
// it linger forever, without deepening an independently-shallow source tree.
func FetchMetadataTreeOnly(ctx context.Context) error {
	return fetchMetadataFromReadRemotes(ctx, false /* noFilter */)
}

// fetchMetadataFromReadRemotes fetches the metadata branch from every checkpoint
// read candidate. A successful branch fetch does not prove that branch contains
// the checkpoint a caller will request, so stopping at the first existing branch
// would let partial elected-remote history hide legacy origin data. Candidate
// failures are logged; the operation succeeds when any candidate was fetched,
// and surfaces the first error only when every candidate fails.
//
// Local-ref advancement is confined to the elected checkpoint sync remote: a
// successful fetch from the legacy origin tier only updates origin's tracking
// ref (which the candidate-aware tracking-ref readers consult) and never feeds
// SafelyAdvanceLocalRef — a stale origin driving the local v1 advance is the
// #1374-class hazard. The election result comes from the same resolver call
// that produced the chain (never inferred from the chain's first entry, which
// can be the fail-open origin), so chain and election cannot disagree
// mid-operation.
func fetchMetadataFromReadRemotes(ctx context.Context, noFilter bool) error {
	resolution := strategy.CheckpointReadRemotesWithElection(ctx)
	candidates := resolution.Candidates
	if len(candidates) == 0 {
		return errors.New("no git remotes configured to fetch checkpoint metadata from")
	}

	// Per-candidate budgets (inside fetchMetadataFromRemote) nested in one chain
	// ceiling, so a stalled candidate cannot starve the rest and the total stays
	// bounded — these read paths have no outer deadline above them.
	chainCtx, cancelChain := context.WithTimeout(ctx, remote.ReadChainBudget)
	defer cancelChain()

	var firstErr error
	fetched := false
	for i, remoteName := range candidates {
		err := fetchMetadataFromRemote(chainCtx, remoteName, noFilter, resolution.ElectedName != "" && remoteName == resolution.ElectedName)
		if err == nil {
			fetched = true
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
		logging.Debug(ctx, "metadata branch fetch: read candidate failed",
			slog.String("candidate", remoteName),
			slog.Int("candidate_index", i),
			slog.String("error", err.Error()))
	}
	if fetched {
		return nil
	}
	return firstErr
}

// fetchMetadataFromRemote fetches the metadata branch from one remote into
// that remote's tracking ref. advanceLocal must be true ONLY for the elected
// checkpoint sync remote — it gates the SafelyAdvanceLocalRef step, which on
// divergence replays local commits onto the fetched tip and so must never be
// driven by the legacy origin tier.
func fetchMetadataFromRemote(ctx context.Context, remoteName string, noFilter, advanceLocal bool) error {
	refs := checkpoint.ResolveRefs(ctx)
	if !refs.Primary.IsBranch() {
		return fmt.Errorf("primary metadata ref %s is not a branch", refs.Primary)
	}
	branchName := refs.Primary.Short()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	fetchTarget, err := remote.ResolveFetchTarget(ctx, remoteName)
	if err != nil {
		return fmt.Errorf("failed to resolve fetch target: %w", err)
	}

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branchName, remoteName, branchName)

	output, fetchErr := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
		NoFilter: noFilter,
		// Heal a repo that an older CLI already shallowed with --depth=1: the
		// metadata tip is grafted in .git/shallow, which breaks merge-base
		// connectivity checks for the metadata branch. A ref-scoped deep fetch
		// removes that boundary without deepening an independently-shallow
		// source-tree clone (unlike --unshallow), and is a no-op on a normally
		// cloned repo.
		Depth: metadataFetchDepth,
	})
	if fetchErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("fetch timed out after 2 minutes")
		}
		return formatFilteredFetchError("failed to fetch "+branchName, fetchTarget, output, fetchErr)
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName(remoteName, branchName), true)
	if err != nil {
		return fmt.Errorf("branch '%s' not found on %s: %w", branchName, remoteName, err)
	}
	if !advanceLocal {
		// Legacy read tier: the fetched data is readable through the tracking
		// ref, but the local primary is never advanced from it.
		return nil
	}
	if err := strategy.SafelyAdvanceLocalRef(ctx, repo, refs.Primary, remoteRef.Hash()); err != nil {
		return fmt.Errorf("failed to advance local %s branch: %w", branchName, err)
	}
	return nil
}

// FetchMetadataFromCheckpointRemote fetches the entire/checkpoints/v1 branch from the
// configured checkpoint_remote URL and updates the local branch.
// Returns an error if the fetch fails or no checkpoint_remote is configured.
func FetchMetadataFromCheckpointRemote(ctx context.Context) error {
	configured := remote.Configured(ctx)
	if !configured {
		return errors.New("no checkpoint_remote configured")
	}
	checkpointURL, err := remote.FetchURL(ctx)
	if err != nil {
		return fmt.Errorf("checkpoint_remote configured but could not resolve URL: %w", err)
	}

	if err := strategy.FetchMetadataBranch(ctx, checkpointURL); err != nil {
		return fmt.Errorf("failed to fetch from checkpoint remote: %w", err)
	}
	return nil
}

// resolveCheckpointFetchTarget returns the fetch target for checkpoint data.
// Thin alias for remote.CheckpointFetchTarget (the single source of truth).
func resolveCheckpointFetchTarget(ctx context.Context) string {
	return remote.CheckpointFetchTarget(ctx)
}

// FetchCheckpointRef fetches a single per-checkpoint ref from the checkpoint
// read-candidate remotes (elected sync remote first, then the legacy origin
// tier). It is the single cli-side RefFetchFunc wiring point — every
// checkpoint.OpenOptions.RefFetcher and direct call in this package routes
// through here, so all read paths consult the candidate chain via
// remote.FetchCheckpointRefFrom while keeping the public (ctx, ref)
// RefFetchFunc shape. See that function for the candidate semantics and the
// absence-vs-failure contract (no candidate has the ref wraps
// plumbing.ErrReferenceNotFound; transport failures surface as-is). Write-side
// hook probes deliberately stay on the single-target
// remote.HookCheckpointRefFetcher instead.
func FetchCheckpointRef(ctx context.Context, ref plumbing.ReferenceName) error {
	resolution := strategy.CheckpointReadRemotesWithElection(ctx)
	return remote.FetchCheckpointRefFrom(ctx, ref, resolution.Candidates, resolution.ElectionErr) //nolint:wrapcheck // thin alias; the remote error carries full context
}

// checkpointRefListTimeout bounds the names-only ls-remote used by user-facing
// `entire checkpoint list` / branch explain. Kept short (not a full fetch
// budget): discovery is best-effort and additive — on timeout or unreachable
// remote the store falls back to local refs rather than stalling a previously
// instant command for tens of seconds.
const checkpointRefListTimeout = 5 * time.Second

// ListCheckpointRefsOnRemote enumerates the per-checkpoint refs
// (refs/entire/checkpoints/<shard>/<id>) present on the checkpoint remote(s),
// names only, via `git ls-remote refs/entire/checkpoints/*` — no object
// transfer. The git-refs store's List uses it to discover checkpoints written
// on another machine that have no local ref yet, then hydrates each lazily on
// read through FetchCheckpointRef.
//
// Scope:
//   - checkpoint_remote configured → queries the resolved dedicated URL via
//     remote.FetchURL (which can still fall through to origin in edge cases
//     such as settings-load failure or an underivable checkpoint URL) —
//     unchanged single-target behavior;
//   - otherwise → queries EVERY checkpoint read candidate (elected sync
//     remote, then the legacy origin tier) and MERGES the listings — a union
//     deduped by ref name. Merging rather than first-non-empty because
//     pre-single-remote-sync, per-checkpoint refs landed on whichever remote
//     the pre-push hook fired for, so disjoint legacy refs on origin
//     coexisting with new refs on the elected remote are realistic and
//     first-non-empty would shadow one side. Discovery is best-effort: a
//     candidate failing logs at debug and doesn't block the others. When every
//     candidate fails, the first error is returned so the store warns before
//     showing local-only results. No candidates (remoteless repo) → (nil, nil).
//
// Each candidate gets its own checkpointRefListTimeout budget so a hung elected
// remote cannot starve the legacy origin tier.
// Resolution and ls-remote are pinned to the worktree root (not process cwd) so
// repo-local git config (url.*.insteadOf, credential helpers, remotes) applies.
func ListCheckpointRefsOnRemote(ctx context.Context) ([]plumbing.ReferenceName, error) {
	return listCheckpointRefsOnRemote(ctx, checkpointRefListTimeout)
}

func listCheckpointRefsOnRemote(ctx context.Context, candidateTimeout time.Duration) ([]plumbing.ReferenceName, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}

	s, settingsErr := settings.Load(settings.WithWorktreeRoot(ctx, worktreeRoot))
	if settingsErr != nil {
		logging.Warn(ctx, "checkpoint ref discovery: settings unavailable; leaving list local-only",
			slog.String("error", settingsErr.Error()))
		return nil, nil
	}
	if s.GetCheckpointRemote() != nil {
		url, err := remote.FetchURL(ctx, remote.FetchURLOptions{WorktreeRoot: worktreeRoot})
		if err != nil {
			return nil, fmt.Errorf("resolve checkpoint remote URL: %w", err)
		}

		ctx, cancel := context.WithTimeout(ctx, candidateTimeout)
		defer cancel()

		output, err := remote.LsRemoteInDir(ctx, worktreeRoot, url, checkpoint.CheckpointRefPrefix+"*")
		if err != nil {
			return nil, fmt.Errorf("ls-remote checkpoint refs from %s: %w", remote.RedactURL(url), err)
		}
		return parseCheckpointRefNames(output), nil
	}
	if s.HasCheckpointRemoteKey() {
		return nil, nil
	}

	candidates := strategy.CheckpointReadRemotes(ctx)
	if len(candidates) == 0 {
		return nil, nil
	}

	seen := make(map[plumbing.ReferenceName]bool)
	var names []plumbing.ReferenceName
	var firstErr error
	succeeded := false
	for _, candidate := range candidates {
		candidateCtx, cancel := context.WithTimeout(ctx, candidateTimeout)
		// Prefer the candidate's resolved URL (token-aware, worktree-pinned);
		// fall back to the bare remote name, which git resolves itself.
		target := candidate
		if url, urlErr := remote.FetchURL(candidateCtx, remote.FetchURLOptions{WorktreeRoot: worktreeRoot, LeadReadRemote: candidate}); urlErr == nil {
			target = url
		}
		output, lsErr := remote.LsRemoteInDir(candidateCtx, worktreeRoot, target, checkpoint.CheckpointRefPrefix+"*")
		cancel()
		if lsErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("ls-remote checkpoint refs from %s: %w", remote.RedactURLOrPath(target), lsErr)
			}
			logging.Debug(ctx, "checkpoint ref discovery: read candidate listing failed; continuing with remaining candidates",
				slog.String("candidate", candidate),
				slog.String("error", lsErr.Error()))
			continue
		}
		succeeded = true
		for _, name := range parseCheckpointRefNames(output) {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if !succeeded {
		return nil, firstErr
	}
	return names, nil
}

// parseCheckpointRefNames extracts the checkpoint ref names from `git ls-remote`
// output. Each line is "<hash>\t<refname>"; only refs under CheckpointRefPrefix
// are kept (the store re-validates each via ParseRef). Checkpoint refs point at
// commits so no peeled (`^{}`) lines appear for them; refs/tags peeled lines
// lack the checkpoint prefix and drop out here; any anomalous
// refs/entire/checkpoints/...^{} name is rejected by ParseRef downstream (the
// "{}" shard never matches ShardFor).
func parseCheckpointRefNames(output []byte) []plumbing.ReferenceName {
	var names []plumbing.ReferenceName
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[1]
		if !strings.HasPrefix(name, checkpoint.CheckpointRefPrefix) {
			continue
		}
		names = append(names, plumbing.ReferenceName(name))
	}
	return names
}

// FetchBlobsByHash fetches specific blob objects from the remote by their SHA-1 hashes.
// Uses "git fetch <target> <hash>" which goes through normal credential helpers,
// unlike fetch-pack which bypasses them. Requires the server to support
// uploadpack.allowReachableSHA1InWant (GitHub, GitLab, Bitbucket all do).
//
// The fetch targets come from checkpointBlobFetchTargets: the single dedicated
// checkpoint_remote URL when one is configured, otherwise one target per
// checkpoint read candidate, tried in order (first success wins; blob fetches
// land in the object store, never in local refs, so both tiers are legal).
//
// If fetching by hash fails on every target, falls back to a full metadata
// branch fetch.
func FetchBlobsByHash(ctx context.Context, hashes []plumbing.Hash) error {
	return fetchBlobsByHash(ctx, hashes, 2*time.Minute, remote.ReadChainBudget, remote.FetchBlobs)
}

func fetchBlobsByHash(
	ctx context.Context,
	hashes []plumbing.Hash,
	fetchTimeout time.Duration,
	chainBudget time.Duration,
	fetchBlobs func(context.Context, string, []string) error,
) error {
	if len(hashes) == 0 {
		return nil
	}

	// One ceiling over the whole operation — the per-target loop AND the
	// fallback fetches below. Before the read-candidate chain this function was
	// wrapped in a single 2-minute budget that covered its fallbacks; moving to
	// per-target budgets dropped that, leaving the fallbacks on the caller's
	// uncapped context, so a fully-stalled hydration could run per-target
	// budgets and then a fresh per-candidate metadata chain on top.
	ctx, cancelChain := context.WithTimeout(ctx, chainBudget)
	defer cancelChain()

	targets := checkpointBlobFetchTargets(ctx)

	hashStrs := make([]string, len(hashes))
	for i, h := range hashes {
		hashStrs[i] = h.String()
	}

	var firstErr error
	for i, fetchTarget := range targets {
		candidateCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
		fetchErr := fetchBlobs(candidateCtx, fetchTarget, hashStrs)
		cancel()
		if fetchErr == nil {
			return nil
		}
		if firstErr == nil {
			firstErr = fetchErr
		}
		logging.Debug(ctx, "fetch-by-hash failed on target",
			slog.Int("blob_count", len(hashes)),
			slog.String("fetch_target", remote.RedactURLOrPath(fetchTarget)),
			slog.Int("target_index", i),
			slog.String("error", fetchErr.Error()),
		)
	}

	logging.Debug(ctx, "fetch-by-hash failed, falling back to full metadata fetch",
		slog.Int("blob_count", len(hashes)),
		slog.String("error", firstErr.Error()),
	)
	// Fallback: try checkpoint remote first (if configured), then the
	// read-candidate chain.
	if cpErr := FetchMetadataFromCheckpointRemote(ctx); cpErr != nil {
		if fallbackErr := FetchMetadataBranch(ctx); fallbackErr != nil {
			return fmt.Errorf("fetch-by-hash failed: %w; fallback fetch also failed: %w",
				firstErr, fallbackErr)
		}
	}

	return nil
}

// checkpointBlobFetchTargets returns the ordered fetch targets for blob
// hydration. A configured checkpoint_remote is a dedicated store with a single
// authoritative target; otherwise each checkpoint read candidate becomes a
// target (its URL when resolvable — reusing the checkpoint-token URL
// derivation — else the bare remote name). An empty candidate chain keeps the
// legacy single-target shape so error reporting matches today's remoteless
// behavior.
func checkpointBlobFetchTargets(ctx context.Context) []string {
	if remote.Configured(ctx) {
		return []string{resolveCheckpointFetchTarget(ctx)}
	}
	candidates := strategy.CheckpointReadRemotes(ctx)
	if len(candidates) == 0 {
		return []string{resolveCheckpointFetchTarget(ctx)}
	}
	targets := make([]string, 0, len(candidates))
	for _, name := range candidates {
		targets = append(targets, remote.CheckpointFetchTargetFrom(ctx, name))
	}
	return targets
}
