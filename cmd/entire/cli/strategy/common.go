package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/vercelconfig"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Common branch name constants for default branch detection.
const (
	branchMain   = "main"
	branchMaster = "master"
	// Strategy name constants
	StrategyNameManualCommit = "manual-commit"
)

// MaxCommitTraversalDepth is the safety limit for walking git commit history.
// Prevents unbounded traversal in repositories with very long histories.
const MaxCommitTraversalDepth = 1000

// errStop is a sentinel error used to break out of git log iteration.
// Shared across strategies that iterate through git commits.
// NOTE: A similar sentinel exists in checkpoint/temporary.go - this is intentional.
// Each package needs its own package-scoped sentinel for git log iteration patterns.
var errStop = errors.New("stop iteration")

var errNoMergeBase = errors.New("no merge base")

// checkpointRemoteBootstrapContextKey gates whether EnsurePrimaryRef is
// allowed to perform a network fetch against a configured checkpoint_remote
// (see bootstrapPrimaryFromCheckpointRemote). Unset/false in ctx means "no".
type checkpointRemoteBootstrapContextKey struct{}

// WithCheckpointRemoteBootstrap marks ctx as permitting EnsurePrimaryRef to
// fetch a missing primary metadata ref from a configured checkpoint_remote.
// This must only be set on explicit, user-initiated setup flows (`entire
// enable`, agent add/setup) — never on the per-turn hook hot path. EnsureSetup
// runs synchronously on every TurnStart hook and hook execution has a hard
// timeout; without this guard a slow/unreachable checkpoint remote would
// repeat an expensive fetch on every turn instead of self-healing once via
// the empty-orphan fallback.
func WithCheckpointRemoteBootstrap(ctx context.Context) context.Context {
	return context.WithValue(ctx, checkpointRemoteBootstrapContextKey{}, true)
}

// checkpointRemoteBootstrapAllowed reports whether ctx was marked via
// WithCheckpointRemoteBootstrap.
func checkpointRemoteBootstrapAllowed(ctx context.Context) bool {
	v := ctx.Value(checkpointRemoteBootstrapContextKey{})
	allowed, ok := v.(bool)
	return ok && allowed
}

// IsEmptyRepository returns true if the repository has no commits yet.
// After git-init, HEAD points to an unborn branch (e.g., refs/heads/main)
// whose target does not yet exist. repo.Head() returns ErrReferenceNotFound
// in this case.
func IsEmptyRepository(repo *git.Repository) bool {
	_, err := repo.Head()
	return errors.Is(err, plumbing.ErrReferenceNotFound)
}

// EnsureSetup ensures the strategy is properly set up.
func EnsureSetup(ctx context.Context) error {
	if err := EnsureEntireGitignore(ctx); err != nil {
		return err
	}

	// Ensure the entire/checkpoints/v1 orphan branch exists for permanent session storage
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()
	if err := vercelconfig.InitSettings(ctx); err != nil {
		return fmt.Errorf("failed to initialize vercel settings: %w", err)
	}
	if err := EnsurePrimaryRef(ctx, repo); err != nil {
		return fmt.Errorf("failed to ensure primary metadata ref: %w", err)
	}

	// Install generic hooks (they delegate to strategy at runtime)
	if !IsGitHookInstalled(ctx) {
		localDev, absoluteHookPath := hookSettingsFromConfig(ctx)
		if _, err := InstallGitHook(ctx, true, localDev, absoluteHookPath); err != nil {
			return fmt.Errorf("failed to install git hooks: %w", err)
		}
	}
	return nil
}

// FetchTmpRefPrefix is the namespace for temporary refs used by fetch helpers
// to land a fetched hash before safely promoting it to a final ref (via
// PromoteTmpRefSafely). Prefer using the named constants below when possible.
const FetchTmpRefPrefix = "refs/entire-fetch-tmp/"

// PromoteTmpRefSafely reads tmpRefName (the ref a fetch just landed into),
// advances destRefName to its hash via SafelyAdvanceLocalRef, then removes
// the tmp ref. The cleanup is deferred so the tmp ref is reaped even when
// the advance fails.
//
// label is a short human-readable name used in error messages. Typical use:
//
//	// fetch with refspec "+<src>:<tmpRefName>"
//	refs := checkpoint.ResolveRefs(ctx)
//	return PromoteTmpRefSafely(ctx, tmpRefName, refs.Primary, refs.Primary.Short())
func PromoteTmpRefSafely(ctx context.Context, tmpRefName, destRefName plumbing.ReferenceName, label string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository for %s promote: %w", label, err)
	}
	defer repo.Close()
	defer func() { _ = repo.Storer.RemoveReference(tmpRefName) }() //nolint:errcheck // cleanup is best-effort

	tmpRef, err := repo.Reference(tmpRefName, true)
	if err != nil {
		return fmt.Errorf("%s not found after fetch (tmp ref %s missing): %w", label, tmpRefName, err)
	}
	if err := SafelyAdvanceLocalRef(ctx, repo, destRefName, tmpRef.Hash()); err != nil {
		return fmt.Errorf("failed to advance local %s: %w", label, err)
	}
	return nil
}

// SafelyAdvanceLocalRef updates localRefName to include targetHash without
// dropping local-only commits. Missing and behind refs advance to targetHash;
// already-current or locally-ahead refs are left unchanged; diverged refs replay
// commits after the merge-base onto targetHash and move localRefName to the
// replayed tip. If there is no merge-base and the reachable histories are
// complete, the full local chain is replayed.
func SafelyAdvanceLocalRef(ctx context.Context, repo *git.Repository, localRefName plumbing.ReferenceName, targetHash plumbing.Hash) error {
	currentLocal, localErr := repo.Reference(localRefName, true)
	if localErr != nil {
		if !errors.Is(localErr, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("failed to read local ref %s: %w", localRefName, localErr)
		}
		return setRefHash(repo, localRefName, targetHash)
	}

	localHash := currentLocal.Hash()
	if localHash == targetHash {
		return nil
	}

	repoPath, err := getRepoPath(repo)
	if err != nil {
		return fmt.Errorf("failed to get repo path: %w", err)
	}

	mergeBase, err := getMergeBase(ctx, repoPath, localHash.String(), targetHash.String())
	if errors.Is(err, errNoMergeBase) {
		shallow, shallowErr := hasReachableShallowBoundary(ctx, repo, repoPath, localHash.String(), targetHash.String())
		if shallowErr != nil {
			return fmt.Errorf("failed to check shallow history for %s: %w", localRefName, shallowErr)
		}
		if shallow {
			return fmt.Errorf("no merge base for %s, and reachable shallow history prevents proving refs are disconnected; run 'entire doctor' or 'git fetch --unshallow' and try again", localRefName)
		}
		return replayDisconnectedLocalRef(ctx, repo, repoPath, localRefName, localHash, targetHash)
	}
	if err != nil {
		return fmt.Errorf("failed to find merge base for %s: %w", localRefName, err)
	}
	if mergeBase == targetHash {
		return nil
	}
	if mergeBase == localHash {
		return setRefHash(repo, localRefName, targetHash)
	}

	return replayLocalRefFromBase(ctx, repo, repoPath, localRefName, localHash, targetHash, mergeBase)
}

func replayLocalRefFromBase(ctx context.Context, repo *git.Repository, repoPath string, localRefName plumbing.ReferenceName, localHash, targetHash, baseHash plumbing.Hash) error {
	localCommits, err := collectCommitsSince(ctx, repo, repoPath, localHash, baseHash)
	if err != nil {
		return fmt.Errorf("failed to collect local replay commits for %s: %w", localRefName, err)
	}
	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("failed to load shallow boundaries for %s: %w", localRefName, err)
	}
	return replayLocalCommits(ctx, repo, "diverged", localRefName, localHash, targetHash, localCommits, shallow)
}

func replayDisconnectedLocalRef(ctx context.Context, repo *git.Repository, repoPath string, localRefName plumbing.ReferenceName, localHash, targetHash plumbing.Hash) error {
	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("failed to load shallow boundaries for %s: %w", localRefName, err)
	}
	localCommits, err := collectCommitChain(repo, localHash, shallow)
	if err != nil {
		return fmt.Errorf("failed to collect disconnected local commits for %s: %w", localRefName, err)
	}
	return replayLocalCommits(ctx, repo, "disconnected", localRefName, localHash, targetHash, localCommits, shallow)
}

func replayLocalCommits(ctx context.Context, repo *git.Repository, reason string, localRefName plumbing.ReferenceName, localHash, targetHash plumbing.Hash, localCommits []*object.Commit, shallow map[plumbing.Hash]bool) error {
	// Logged here rather than in each caller: this is the function that performs
	// the rewrite, so a future third caller cannot silently skip the trace.
	//
	// A replay rewrites the local ref to the fetched remote tip with the local
	// commits re-applied on top: no checkpoint is lost, but the ref no longer
	// points where it did and the local commits have new hashes. That is the one
	// place a fetch reaches past remote-tracking refs into local state, so it must
	// leave a trace — the whole advance/replay chain was previously silent, which
	// made an invisible rewrite impossible to reconstruct afterwards from
	// .entire/logs/. Info rather than Debug: the default log level has to carry it
	// for a post-hoc investigation to find it.
	//
	// Metadata only (ref names, hashes, counts) per the logging privacy rule.
	logging.Info(ctx, "replaying local commits onto fetched tip; local ref will be rewritten",
		slog.String("reason", reason),
		slog.String("ref", localRefName.String()),
		slog.String("local_tip", localHash.String()),
		slog.String("target_tip", targetHash.String()),
		slog.Int("commits_replayed", len(localCommits)))

	if len(localCommits) == 0 {
		return setRefHash(repo, localRefName, targetHash)
	}

	newTip, err := cherryPickOnto(ctx, repo, targetHash, localCommits, shallow)
	if err != nil {
		return fmt.Errorf("failed to replay local commits for %s: %w", localRefName, err)
	}

	return setRefHash(repo, localRefName, newTip)
}

func hasReachableShallowBoundary(ctx context.Context, repo *git.Repository, repoPath string, hashes ...string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	shallowHashes, err := repo.Storer.Shallow()
	if err != nil {
		return false, fmt.Errorf("read shallow commits: %w", err)
	}
	if len(shallowHashes) == 0 {
		return false, nil
	}

	shallowCommits := make(map[string]struct{}, len(shallowHashes))
	for _, hash := range shallowHashes {
		shallowCommits[hash.String()] = struct{}{}
	}
	for _, hash := range hashes {
		reachesBoundary, err := historyReachesShallowBoundary(ctx, repoPath, hash, shallowCommits)
		if err != nil {
			return false, err
		}
		if reachesBoundary {
			return true, nil
		}
	}
	return false, nil
}

func historyReachesShallowBoundary(ctx context.Context, repoPath, hash string, shallowCommits map[string]struct{}) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-list", hash)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git rev-list %s failed: %w", hash, err)
	}
	for _, commitHash := range strings.Fields(string(output)) {
		if _, ok := shallowCommits[commitHash]; ok {
			return true, nil
		}
	}
	return false, nil
}

func setRefHash(repo *git.Repository, refName plumbing.ReferenceName, hash plumbing.Hash) error {
	newRef := plumbing.NewHashReference(refName, hash)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to update local ref %s: %w", refName, err)
	}
	return nil
}

// ListCheckpoints returns all checkpoints from committed checkpoint storage.
func ListCheckpoints(ctx context.Context) ([]CheckpointInfo, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	// Warn (once per process) if metadata branches are disconnected
	WarnIfMetadataDisconnected(ctx)

	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{ReadRemotes: CheckpointReadRemotes(ctx)})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}
	committed, err := stores.Persistent.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list committed checkpoints: %w", err)
	}
	return checkpointInfosFromCommitted(committed), nil
}

func checkpointInfosFromCommitted(committed []checkpoint.CheckpointInfo) []CheckpointInfo {
	result := make([]CheckpointInfo, 0, len(committed))
	for _, c := range committed {
		result = append(result, CheckpointInfo{
			CheckpointID:     c.CheckpointID,
			SessionID:        c.SessionID,
			CreatedAt:        c.CreatedAt,
			CheckpointsCount: c.CheckpointsCount,
			FilesTouched:     c.FilesTouched,
			Agent:            c.Agent,
			IsTask:           c.IsTask,
			ToolUseID:        c.ToolUseID,
			SessionCount:     c.SessionCount,
			SessionIDs:       c.SessionIDs,
			Imported:         c.Imported,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

const (
	entireGitignore    = ".entire/.gitignore"
	entireDir          = ".entire"
	gitDir             = ".git"
	shadowBranchPrefix = "entire/"
)

// isProtectedPath returns true if relPath is inside a directory that should
// never be modified or deleted during rewind or other destructive operations.
// Protected directories include git internals, entire metadata, and all
// registered agent config directories.
func isProtectedPath(relPath string) bool {
	for _, dir := range protectedDirs() {
		if paths.IsProtectedSubpath(dir, relPath) {
			return true
		}
	}
	return false
}

// protectedDirs returns the list of directories to protect. This combines
// static infrastructure dirs with agent-reported dirs from the registry.
// The result is cached via sync.Once since it's called per-file when filtering untracked files.
//
// NOTE: The cache is never invalidated. In production this is fine (the agent registry
// is populated at init time and never changes). However, tests that mutate the agent
// registry after the first call to protectedDirs/isProtectedPath will see stale results.
// If you need to test isProtectedPath with a custom registry, either:
//   - run those tests in a separate process, or
//   - call resetProtectedDirsForTest() to clear the cache.
func protectedDirs() []string {
	protectedDirsOnce.Do(func() {
		protectedDirsCache = append([]string{gitDir, entireDir}, agent.AllProtectedDirs()...)
	})
	return protectedDirsCache
}

var (
	protectedDirsOnce  sync.Once
	protectedDirsCache []string
)

var initRedactionOnce sync.Once

// EnsureRedactionConfigured loads redaction settings and configures the
// redact package: PII detection (opt-in), inline custom_redactions, and rule
// packs auto-discovered from .entire/redactors/.
//
// Must be called at each process entry point before checkpoint writes.
func EnsureRedactionConfigured() {
	initRedactionOnce.Do(func() {
		ctx := context.Background()
		s, err := settings.Load(ctx)
		if err != nil {
			logCtx := logging.WithComponent(ctx, "redaction")
			logging.Warn(logCtx, "failed to load settings for redaction", slog.String("error", err.Error()))
			return
		}

		// PII detection (opt-in).
		if s.Redaction != nil && s.Redaction.PII != nil && s.Redaction.PII.Enabled {
			pii := s.Redaction.PII
			cfg := redact.PIIConfig{
				Enabled:        true,
				Categories:     make(map[redact.PIICategory]bool),
				CustomPatterns: pii.CustomPatterns,
			}
			cfg.Categories[redact.PIIEmail] = pii.Email == nil || *pii.Email
			cfg.Categories[redact.PIIPhone] = pii.Phone == nil || *pii.Phone
			cfg.Categories[redact.PIIAddress] = pii.Address != nil && *pii.Address
			redact.ConfigurePII(cfg)
		}

		// Custom rules: inline + packs.
		var inline map[string]string
		if s.Redaction != nil {
			inline = s.Redaction.CustomRedactions
		}
		packsRelPath := filepath.Join(paths.EntireDir, redact.RedactorsDirName)
		packsDir, perr := paths.AbsPath(ctx, packsRelPath)
		if perr != nil {
			logCtx := logging.WithComponent(ctx, "redaction")
			logging.Warn(logCtx, "failed to resolve redactors path", slog.String("error", perr.Error()))
			packsDir = packsRelPath
		}
		packs, lerr := redact.LoadPacks(packsDir)
		if lerr != nil {
			logCtx := logging.WithComponent(ctx, "redaction")
			logging.Warn(logCtx, "failed to load redactor packs", slog.String("error", lerr.Error()))
			// Hooks log to .entire/logs/entire.log, where most users never
			// look. Surface a one-line breadcrumb on stderr when we have a
			// real terminal so the user can find the detail.
			if interactive.IsTerminalWriter(os.Stderr) {
				fmt.Fprintf(os.Stderr, "[entire] redactor packs failed to load (%v); see .entire/logs/entire.log or run `entire doctor`.\n", lerr)
			}
		}
		if len(inline) > 0 || len(packs) > 0 {
			redact.ConfigureCustomRules(redact.CustomRulesConfig{
				Inline: inline,
				Packs:  packs,
			})
		}

		// OpenAI Privacy Filter (opt-in 9th layer).
		if s.Redaction != nil && s.Redaction.OpenAIPrivacyFilter != nil {
			opf := s.Redaction.OpenAIPrivacyFilter
			redact.ConfigurePrivacyFilter(redact.OPFConfig{
				Enabled:    opf.Enabled,
				Categories: opf.Categories,
				Command:    opf.Command,
				Timeout:    opf.TimeoutSeconds,
			})
		}
	})
}

// resolveAgentType picks the best agent type from the context and existing state.
// Priority: existing state > context value.
func resolveAgentType(ctxAgentType types.AgentType, state *SessionState) types.AgentType {
	if state != nil && state.AgentType != "" {
		return state.AgentType
	}
	return ctxAgentType
}

// EnsurePrimaryRef creates or updates the local primary metadata ref.
//
// A configured checkpoint_remote is the authoritative checkpoint store: when one
// is set the local ref is only ever sourced from it, never from a (possibly
// stale) tracking ref (issue #1374). Without a checkpoint_remote, the elected
// checkpoint sync remote holds the store — when Primary is in Push and the
// local ref is missing or un-initialized, the local ref is created/updated
// from the ELECTED remote's tracking ref only; the legacy origin read tier
// never seeds or advances local refs. Otherwise an empty orphan is created.
// primaryIsGitRefs reports whether the configured primary checkpoint backend is
// git-refs. Resolution is fail-soft: any config-load error is treated as the
// default git-branch backend (returns false), preserving legacy behavior.
func primaryIsGitRefs(ctx context.Context) bool {
	cfg, err := settings.LoadCheckpointsConfig(ctx)
	if err != nil {
		return false
	}
	return checkpoint.PrimaryIsRefs(cfg)
}

func EnsurePrimaryRef(ctx context.Context, repo *git.Repository) error {
	// Rebind settings/remote resolution to the repository being modified rather
	// than the ambient working directory, so the "is a checkpoint_remote
	// configured" decision (and any fetch below) targets repo even when a caller
	// passes a handle for a repo that is not CWD. EnsureSetup opens repo from CWD
	// and tests t.Chdir into it, so in normal use this is a no-op.
	if root, rootErr := getRepoPath(repo); rootErr == nil {
		ctx = settings.WithWorktreeRoot(ctx, root)
	}

	refs := checkpoint.ResolveRefs(ctx)
	primaryName := refs.Primary.Short()

	// Under the git-refs primary backend, checkpoints are written to
	// per-checkpoint refs and nothing is ever written to the v1 metadata
	// branch. Seeding an empty orphan v1 here would leave a vestigial,
	// never-written branch — the surprise a user hit when `entire enable`
	// selected git-refs yet still created entire/checkpoints/v1. We still adopt
	// real v1 data that already exists on origin below; we suppress the
	// empty-orphan fallback and, since there is no orphan that could diverge,
	// the checkpoint-remote bootstrap fetch too. Legacy checkpoints stay
	// readable either way — the read path recovers the branch on demand (see
	// checkpoint.MetadataBranchFetchFunc). Resolution is fail-soft: an
	// unreadable config keeps the legacy git-branch seeding behavior.
	skipEmptyOrphan := primaryIsGitRefs(ctx)

	localRef, localErr := repo.Reference(refs.Primary, true)
	if localErr != nil && !errors.Is(localErr, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("failed to check metadata ref: %w", localErr)
	}
	localExists := localErr == nil

	// A configured checkpoint_remote is the authoritative checkpoint store and
	// takes precedence over origin's (possibly stale) primary tracking ref. When
	// it is configured we never seed from origin: a machine migrated from
	// origin-hosted checkpoints to a dedicated checkpoint_remote must not replay
	// origin's stale checkpoints into it (issue #1374). The network fetch is
	// additionally gated on an explicit enable flow (WithCheckpointRemoteBootstrap):
	// the per-turn hook hot path stays network-free and self-heals via the
	// empty-orphan fallback.
	if remote.Configured(ctx) {
		if localExists {
			// Heal an un-initialized orphan (empty, or vercel.json-only on a
			// vercel-enabled repo) from the checkpoint remote. Whatever the
			// outcome, never fall through to origin.
			healed, err := healPrimaryFromCheckpointRemote(ctx, repo, localRef, refs.Primary)
			if err != nil {
				return err
			}
			if healed {
				fmt.Fprintf(os.Stderr, "✓ Updated local ref '%s' from checkpoint remote\n", primaryName)
			}
			return nil
		}
		// Local ref missing — fetch the real branch before creating a fresh
		// orphan: an orphan would diverge from it, hide existing checkpoints, and
		// be rejected non-fast-forward on later syncs.
		if bootstrapPrimaryFromCheckpointRemote(ctx, repo, refs.Primary, skipEmptyOrphan) {
			fmt.Fprintf(os.Stderr, "✓ Created local ref '%s' from checkpoint remote\n", primaryName)
			return nil
		}
		// Nothing usable on the checkpoint remote (fetch skipped/failed or the
		// remote has no data) — create a fresh orphan that future pushes publish
		// there. Still never origin.
		if skipEmptyOrphan {
			return nil
		}
		return createOrphanMetadataRef(ctx, repo, refs)
	}

	// No checkpoint_remote configured — the elected checkpoint sync remote
	// holds the checkpoint store. Local-ref seeding and healing are confined
	// to that elected remote: the legacy origin read tier is strictly
	// read-only, so a stale refs/remotes/origin/... tracking ref must never
	// seed or update the local primary when the election points elsewhere or
	// failed (a stale origin driving local-ref writes is the #1374-class
	// hazard). When the elected remote's tracking ref is unavailable there is
	// nothing to advance from — never substitute origin. The election result
	// is passed explicitly (not inferred from the read-candidate chain, whose
	// first entry can be the fail-open origin). A remote only tracks Primary
	// when Primary is in Push.
	electedName := ""
	if elected, electErr := ResolveCheckpointSyncRemote(ctx); electErr == nil {
		electedName = elected.Name
	} else {
		logging.Debug(ctx, "primary metadata ref: checkpoint sync remote election failed; skipping remote-tracking seed",
			slog.String("error", electErr.Error()))
	}
	var remoteRef *plumbing.Reference
	if electedName != "" && refs.PrimaryFetchableFromRemote() {
		var remoteErr error
		remoteRef, remoteErr = repo.Reference(plumbing.NewRemoteReferenceName(electedName, primaryName), true)
		if remoteErr != nil && !errors.Is(remoteErr, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("failed to check remote metadata ref: %w", remoteErr)
		}
	}

	if localExists {
		if remoteRef != nil && localRef.Hash() != remoteRef.Hash() {
			// Local and remote exist but differ — determine relationship
			hasData, checkErr := metadataBranchHasData(repo, localRef)
			if checkErr != nil {
				return fmt.Errorf("failed to check metadata ref contents: %w", checkErr)
			}
			if !hasData {
				// Un-initialized orphan — just point to remote
				if setErr := setRefHash(repo, refs.Primary, remoteRef.Hash()); setErr != nil {
					return fmt.Errorf("failed to update metadata ref from remote: %w", setErr)
				}
				fmt.Fprintf(os.Stderr, "[entire] Updated local ref '%s' from %s\n", primaryName, electedName)
			} else {
				// Local has real data and differs from remote — if disconnected
				// (no common ancestor), reconciliation happens at pre-push time
				// or via 'entire doctor'. Read paths warn but do not auto-fix.
				logging.Debug(ctx, "metadata ref differs from remote, reconciliation deferred to read/write time",
					"local_hash", localRef.Hash().String()[:7],
					"remote_hash", remoteRef.Hash().String()[:7],
				)
			}
		}
		return nil
	}

	// Local ref doesn't exist — create from the elected remote if available.
	if remoteRef != nil {
		if err := setRefHash(repo, refs.Primary, remoteRef.Hash()); err != nil {
			return fmt.Errorf("failed to create metadata ref from remote: %w", err)
		}
		fmt.Fprintf(os.Stderr, "✓ Created local ref '%s' from %s\n", primaryName, electedName)
		return nil
	}

	// No local ref and no elected-remote tracking ref — create an empty orphan.
	if skipEmptyOrphan {
		return nil
	}
	return createOrphanMetadataRef(ctx, repo, refs)
}

// createOrphanMetadataRef creates the local primary metadata ref as a fresh empty
// orphan commit (plus any merged vercel config). Used when no metadata ref can be
// sourced from origin or a checkpoint_remote.
func createOrphanMetadataRef(ctx context.Context, repo *git.Repository, refs checkpoint.PersistentRefs) error {
	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	obj := repo.Storer.NewEncodedObject()
	if err := emptyTree.Encode(obj); err != nil {
		return fmt.Errorf("failed to encode empty tree: %w", err)
	}
	emptyTreeHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return fmt.Errorf("failed to store empty tree: %w", err)
	}
	emptyTreeHash, err = vercelconfig.MaybeMergeMetadataBranchConfig(repo, emptyTreeHash)
	if err != nil {
		return fmt.Errorf("failed to initialize metadata ref vercel config: %w", err)
	}

	// Create orphan commit (no parent)
	now := time.Now()
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	sig := object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  now,
	}

	commit := &object.Commit{
		TreeHash:  emptyTreeHash,
		Author:    sig,
		Committer: sig,
		Message:   "Initialize metadata ref\n\nThis ref stores session metadata.\n",
	}
	// Note: No ParentHashes - this is an orphan commit

	// Sign the orphan commit when signing is enabled, matching the path used
	// for every other metadata commit (see metadata_reconcile.go and
	// push_common.go). Without this, repos that enforce a "verified
	// signatures" ruleset on entire/* refs reject the very first push of
	// the metadata ref with GH013, even though every later commit on it
	// is correctly signed.
	checkpoint.SignCommitBestEffort(ctx, commit)

	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return fmt.Errorf("failed to encode orphan commit: %w", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		return fmt.Errorf("failed to store orphan commit: %w", err)
	}

	if err := setRefHash(repo, refs.Primary, commitHash); err != nil {
		return fmt.Errorf("failed to create metadata ref: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  ✓ Created orphan ref %s for session metadata\n", refs.Primary.Short())
	return nil
}

// bootstrapPrimaryFromCheckpointRemote tries to populate a missing local primary
// metadata ref from a configured checkpoint_remote before the caller falls back
// to creating an empty orphan. When a separate checkpoint_remote already holds
// the real entire/checkpoints/v1 branch (the common second-device case), a fresh
// local orphan would diverge from it — hiding existing checkpoints and causing
// non-fast-forward rejections on the next fetch.
//
// All resolution is pinned to repo's worktree (settings, origin URL, fetch
// working directory, ref promotion) so the decision depends on the repository
// being ensured rather than the ambient working directory.
//
// It returns true only when the fetch succeeds and the local primary ref now
// points at the remote branch. Every failure is non-fatal and returns false so
// the caller creates the empty orphan: `entire enable` must never break on a
// missing checkpoint remote, an unresolvable URL, or a network/auth error.
func bootstrapPrimaryFromCheckpointRemote(ctx context.Context, repo *git.Repository, primary plumbing.ReferenceName, primaryIsRefs bool) bool {
	if !checkpointRemoteBootstrapAllowed(ctx) {
		// Not an explicit setup flow (e.g. the per-turn hook hot path via
		// EnsureSetup). Never fetch here: hook execution has a hard timeout,
		// and a slow/unreachable checkpoint remote must not stall every turn.
		// Fall back to the empty orphan, which self-heals immediately.
		logging.Debug(ctx, "checkpoint-remote: skipping bootstrap fetch outside explicit enable flow")
		return false
	}

	if primaryIsRefs {
		// The whole point of this bootstrap is to stop a fresh local orphan from
		// diverging from the real v1 branch. Under the git-refs primary backend
		// no orphan is ever created and nothing is ever written to v1, so there
		// is no divergence to prevent and the fetch buys nothing at enable time.
		//
		// It is not free, either: v1 holds the full legacy transcript history, so
		// this would make every `entire enable` on a fresh clone download the
		// entire checkpoint archive before doing anything else. Legacy hex-ID
		// checkpoints stay readable — the read path recovers the branch on demand
		// (checkpoint.MetadataBranchFetchFunc), fetching individual blobs by hash
		// rather than the whole archive.
		logging.Debug(ctx, "checkpoint-remote: skipping bootstrap fetch under git-refs primary")
		return false
	}

	worktreeRoot, err := getRepoPath(repo)
	if err != nil {
		logging.Debug(ctx, "checkpoint-remote: cannot resolve worktree root for enable bootstrap",
			slog.String("error", err.Error()),
		)
		return false
	}
	// Pin settings/remote resolution to this repo's worktree.
	ctx = settings.WithWorktreeRoot(ctx, worktreeRoot)

	// Resolve and verify the checkpoint fetch URL. resolveCheckpointFetchURL
	// rejects remote.FetchURL's silent origin fallback, so bootstrap never adopts
	// origin's data when the derived URL doesn't actually target the configured
	// checkpoint repo (issue #1374).
	url, ok := resolveCheckpointFetchURL(ctx, worktreeRoot)
	if !ok {
		return false
	}

	branchName := primary.Short()
	tmpRefName := plumbing.ReferenceName(FetchTmpRefPrefix + branchName)
	// Unfiltered. The bootstrap itself only needs the ref, but it is only ever
	// reached under the git-branch primary (git-refs returns above), and there
	// the branch it lands IS the repo's checkpoint store — refs.Read and
	// refs.Primary are the same v1 branch. GitStore.List reads each checkpoint's
	// metadata.json through a plain tree with no blob fetcher, and once this ref
	// resolves the recovery tier never fires again, so a blob-filtered bootstrap
	// would leave `checkpoint list` permanently showing bare IDs with no prompt,
	// date, or counts.
	if err := fetchURLIntoTmpRef(ctx, worktreeRoot, url, primary.String(), tmpRefName.String(), "metadata branch", true, checkpointRemoteForegroundFetchTimeout); err != nil {
		// Warn, not Debug: `entire enable` is an explicit user action, and this
		// failure means existing checkpoints stay unreachable until a read
		// re-fetches them. At Debug the command looked like it had simply
		// paused, then succeeded.
		logging.Warn(ctx, "checkpoint-remote: enable bootstrap fetch failed; creating empty orphan",
			slog.String("url", remote.RedactURL(url)),
			slog.String("error", err.Error()),
		)
		fmt.Fprintf(os.Stderr, "  ! Could not fetch existing checkpoints from %s\n", remote.RedactURL(url))
		fmt.Fprintln(os.Stderr, "    Continuing — they will be fetched on demand when a command needs them.")
		return false
	}
	defer func() { _ = repo.Storer.RemoveReference(tmpRefName) }() //nolint:errcheck // cleanup is best-effort

	tmpRef, err := repo.Reference(tmpRefName, true)
	if err != nil {
		logging.Debug(ctx, "checkpoint-remote: fetched metadata ref missing after enable bootstrap",
			slog.String("error", err.Error()),
		)
		return false
	}
	if err := SafelyAdvanceLocalRef(ctx, repo, primary, tmpRef.Hash()); err != nil {
		logging.Debug(ctx, "checkpoint-remote: could not advance local metadata ref on enable bootstrap",
			slog.String("error", err.Error()),
		)
		return false
	}
	logging.Info(ctx, "checkpoint-remote: bootstrapped metadata branch on enable",
		slog.String("ref", primary.String()),
	)
	return true
}

// healPrimaryFromCheckpointRemote replaces a local primary metadata ref that holds
// no checkpoint data — an empty orphan, or a vercel.json-only orphan on a
// vercel-enabled repo — with the real branch fetched from a configured
// checkpoint_remote. It repairs second-device repos left with an un-initialized
// orphan disjoint from the remote history (issue #1374).
//
// The network fetch is gated on an explicit enable flow
// (WithCheckpointRemoteBootstrap); the per-turn hook hot path stays network-free.
// Returns true only when the local ref was replaced with real remote data. A
// local branch that already carries checkpoints, a non-enable context, an
// unresolvable URL, or a fetch that yields no usable data all return (false, nil)
// so the caller keeps the local ref as-is.
func healPrimaryFromCheckpointRemote(ctx context.Context, repo *git.Repository, localRef *plumbing.Reference, primary plumbing.ReferenceName) (bool, error) {
	hasData, err := metadataBranchHasData(repo, localRef)
	if err != nil {
		return false, fmt.Errorf("failed to check metadata branch contents: %w", err)
	}
	if hasData {
		return false, nil
	}
	if !checkpointRemoteBootstrapAllowed(ctx) {
		// Not an explicit setup flow (e.g. the per-turn hook hot path). Never
		// fetch here: the local orphan self-heals on the next explicit enable.
		logging.Debug(ctx, "checkpoint-remote: skipping empty-orphan heal outside explicit enable flow")
		return false, nil
	}
	if primaryIsGitRefs(ctx) {
		// Same reasoning as the bootstrap: under git-refs nothing writes v1, so a
		// data-free orphan cannot diverge from the remote and there is nothing to
		// protect at enable time. Healing it here would make `entire enable` pull
		// the whole transcript archive — the exact cost this backend's gating
		// exists to avoid — for a branch only legacy reads consult.
		//
		// The orphan does hide those legacy reads (it makes the ref resolve), so
		// the read path treats a data-free branch as a miss and recovers it on
		// demand; see checkpoint.getSessionsBranchTree.
		logging.Debug(ctx, "checkpoint-remote: skipping empty-orphan heal under git-refs primary")
		return false, nil
	}
	return healEmptyOrphanFromCheckpointRemote(ctx, repo, primary)
}

// healEmptyOrphanFromCheckpointRemote fetches the metadata branch from the
// configured checkpoint_remote and force-sets the local ref to the fetched tip.
// It deliberately bypasses SafelyAdvanceLocalRef: the local orphan is disconnected
// from the real branch, so a safe advance would cherry-pick the orphan commit onto
// the fetched tip and leave a stray empty commit. Discarding the data-free orphan
// is always correct here. Fetch failures and a data-free fetched branch are
// non-fatal and return (false, nil) so the caller keeps the local orphan.
func healEmptyOrphanFromCheckpointRemote(ctx context.Context, repo *git.Repository, primary plumbing.ReferenceName) (bool, error) {
	if !primary.IsBranch() {
		return false, nil
	}
	worktreeRoot, err := getRepoPath(repo)
	if err != nil {
		logging.Debug(ctx, "checkpoint-remote: cannot resolve worktree root for empty-orphan heal",
			slog.String("error", err.Error()),
		)
		return false, nil
	}
	// Pin settings/remote resolution to this repo's worktree.
	ctx = settings.WithWorktreeRoot(ctx, worktreeRoot)

	url, ok := resolveCheckpointFetchURL(ctx, worktreeRoot)
	if !ok {
		return false, nil
	}

	tmpRefName := plumbing.ReferenceName(FetchTmpRefPrefix + primary.Short())
	defer func() { _ = repo.Storer.RemoveReference(tmpRefName) }() //nolint:errcheck // cleanup is best-effort

	// Unfiltered, for the same reason as the bootstrap fetch above: this heal
	// replaces a data-free orphan with the real branch, which readers then treat
	// as the checkpoint store.
	if err := fetchURLIntoTmpRef(ctx, worktreeRoot, url, primary.String(), tmpRefName.String(), "metadata branch", true, checkpointRemoteForegroundFetchTimeout); err != nil {
		// Warn for the same reason as the bootstrap fetch: this only runs from an
		// explicit setup flow, and failing leaves the repo on a data-free orphan.
		logging.Warn(ctx, "checkpoint-remote: empty-orphan heal fetch failed; keeping local orphan",
			slog.String("url", remote.RedactURL(url)),
			slog.String("error", err.Error()),
		)
		return false, nil
	}

	fetchedRef, err := repo.Reference(tmpRefName, true)
	if err != nil {
		return false, nil
	}
	fetchedHasData, err := metadataBranchHasData(repo, fetchedRef)
	if err != nil || !fetchedHasData {
		return false, nil
	}

	// Force-set the local ref to the fetched tip, discarding the data-free orphan.
	if err := setRefHash(repo, primary, fetchedRef.Hash()); err != nil {
		return false, fmt.Errorf("failed to replace empty orphan metadata ref: %w", err)
	}
	logging.Info(ctx, "checkpoint-remote: healed empty orphan metadata branch from checkpoint remote",
		slog.String("ref", primary.String()),
	)
	return true, nil
}

// metadataBranchInitFiles are the root-level files that orphan initialization may
// write into an otherwise-empty metadata branch (currently only the Vercel
// config). A branch containing nothing but these holds no checkpoint data.
var metadataBranchInitFiles = map[string]bool{
	vercelconfig.FileName: true, // "vercel.json"
}

// metadataBranchHasData reports whether the branch tip tree contains anything
// beyond orphan-initialization artifacts. It returns false for an empty tree, or
// a tree holding only init files such as vercel.json — the un-initialized-orphan
// state that is safe to adopt from the authoritative remote — and true for any
// other content (checkpoint shards or pre-existing files).
//
// A literal empty-tree check missed vercel.json-only orphans on vercel-enabled
// second devices, leaving them unhealed (issue #1374). Ignoring only the known
// init files (rather than treating "no shards" as empty) avoids clobbering a
// branch that carries other real content. Only the tip commit is inspected — the
// bug this detects creates a single un-initialized orphan as the tip.
func metadataBranchHasData(repo *git.Repository, ref *plumbing.Reference) (bool, error) {
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return false, fmt.Errorf("failed to get commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("failed to get tree: %w", err)
	}
	for _, entry := range tree.Entries {
		if !metadataBranchInitFiles[entry.Name] {
			return true, nil
		}
	}
	return false, nil
}

// GetMetadataRefTree returns the tree object at the given committed-metadata ref.
func GetMetadataRefTree(repo *git.Repository, ref plumbing.ReferenceName) (*object.Tree, error) {
	resolvedRef, err := repo.Reference(ref, true)
	if err != nil {
		return nil, fmt.Errorf("read ref %s: %w", ref, err)
	}
	commit, err := repo.CommitObject(resolvedRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("read commit at %s: %w", ref, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read tree at %s: %w", ref, err)
	}
	return tree, nil
}

// ExtractFirstPrompt extracts and truncates the first meaningful prompt from prompt content.
// Prompts are separated by "\n\n---\n\n". Skips empty prompts and separator-only content.
// Returns empty string if no valid prompt is found.
func ExtractFirstPrompt(content string) string {
	if content == "" {
		return ""
	}

	// Prompts are separated by "\n\n---\n\n"
	// Find the first non-empty prompt
	prompts := strings.Split(content, "\n\n---\n\n")
	var firstPrompt string
	for _, p := range prompts {
		cleaned := strings.TrimSpace(p)
		// Skip empty prompts or prompts that are just dashes/separators
		if cleaned == "" || isOnlySeparators(cleaned) {
			continue
		}
		firstPrompt = cleaned
		break
	}

	if firstPrompt == "" {
		return ""
	}

	return TruncateDescription(firstPrompt, MaxDescriptionLength)
}

// ReadSessionPromptFromTree reads the first meaningful prompt from a checkpoint's prompt.txt file in a git tree.
// Returns an empty string if the prompt cannot be read.
func ReadSessionPromptFromTree(tree *object.Tree, checkpointPath string) string {
	promptPath := checkpointPath + "/" + paths.PromptFileName
	file, err := tree.File(promptPath)
	if err != nil {
		return ""
	}

	content, err := file.Contents()
	if err != nil {
		return ""
	}

	return ExtractFirstPrompt(content)
}

// ReadAgentTypeFromTree reads the agent type from a checkpoint's metadata.json file in a git tree.
// If metadata.json doesn't exist (shadow branches), it falls back to detecting the agent
// from the presence of agent-specific config files (.gemini/settings.json or .claude/).
// Returns agent.AgentTypeUnknown if the agent type cannot be determined.
func ReadAgentTypeFromTree(tree *object.Tree, checkpointPath string) types.AgentType {
	// First, try to read from metadata.json (present in condensed/committed checkpoints)
	metadataPath := checkpointPath + "/" + paths.MetadataFileName
	if file, err := tree.File(metadataPath); err == nil {
		if content, err := file.Contents(); err == nil {
			var metadata checkpoint.Metadata
			if err := json.Unmarshal([]byte(content), &metadata); err == nil && metadata.Agent != "" {
				return metadata.Agent
			}
		}
	}

	// Fall back to detecting agent from config markers (shadow branches don't have metadata.json).
	// Multiple agent config markers may coexist when users configure multiple agents via
	// `entire configure`. Only return a specific agent type when exactly one agent config
	// marker (directory or file) is present; otherwise return Unknown since we can't
	// determine which agent created the checkpoint.
	var detected types.AgentType
	detectedCount := 0

	if _, err := tree.File(".gemini/settings.json"); err == nil {
		detected = agent.AgentTypeGemini
		detectedCount++
	}
	if _, err := tree.Tree(".claude"); err == nil {
		detected = agent.AgentTypeClaudeCode
		detectedCount++
	}
	if _, err := tree.Tree(".opencode"); err == nil {
		detected = agent.AgentTypeOpenCode
		detectedCount++
	} else if _, err := tree.File("opencode.json"); err == nil {
		detected = agent.AgentTypeOpenCode
		detectedCount++
	}
	if _, err := tree.Tree(".codex"); err == nil {
		detected = agent.AgentTypeCodex
		detectedCount++
	}
	if _, err := tree.Tree(".cursor"); err == nil {
		detected = agent.AgentTypeCursor
		detectedCount++
	}
	if _, err := tree.Tree(".factory"); err == nil {
		detected = agent.AgentTypeFactoryAIDroid
		detectedCount++
	}

	if detectedCount == 1 {
		return detected
	}
	return agent.AgentTypeUnknown
}

// isOnlySeparators checks if a string contains only dashes, spaces, and newlines.
func isOnlySeparators(s string) bool {
	for _, r := range s {
		if r != '-' && r != ' ' && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

// ReadLatestSessionPromptFromCommittedTree reads the first prompt from a committed checkpoint's
// latest session on the metadata branch tree. This navigates the sharded directory layout:
// <cpID.Path()>/<latestSessionIndex>/prompt.txt
//
// Falls back through earlier sessions when the latest has no prompt.
// Avoids reading full transcripts — only reads prompt.txt files.
// sessionCount is the number of sessions in the checkpoint (from CheckpointInfo.SessionCount).
func ReadLatestSessionPromptFromCommittedTree(tree *object.Tree, cpID id.CheckpointID, sessionCount int) string {
	cpPath := cpID.Path()
	cpTree, err := tree.Tree(cpPath)
	if err != nil {
		return ""
	}

	// Find the latest session subdirectory with a prompt.
	// Sessions use 0-based indexing: 0/, 1/, 2/, etc.
	// Start from the latest and fall back through earlier sessions
	// when the latest has no prompt (e.g. a test or empty session was
	// condensed alongside a real one).
	latestIndex := max(sessionCount-1, 0)

	for i := latestIndex; i >= 0; i-- {
		sessionPath := strconv.Itoa(i)
		sessionTree, err := cpTree.Tree(sessionPath)
		if err != nil {
			continue
		}

		file, err := sessionTree.File(paths.PromptFileName)
		if err != nil {
			continue
		}

		content, err := file.Contents()
		if err != nil {
			continue
		}

		if prompt := ExtractFirstPrompt(content); prompt != "" {
			return prompt
		}
	}

	return ""
}

// ReadAllSessionPromptsFromTree reads the first prompt for all sessions in a multi-session checkpoint.
// Returns a slice of prompts parallel to sessionIDs (oldest to newest).
// For single-session checkpoints, returns a slice with just the session prompt.
func ReadAllSessionPromptsFromTree(tree *object.Tree, checkpointPath string, sessionCount int, sessionIDs []string) []string {
	if sessionCount <= 1 || len(sessionIDs) <= 1 {
		prompt := ReadSessionPromptFromTree(tree, checkpointPath+"/0")
		if prompt == "" {
			prompt = ReadSessionPromptFromTree(tree, checkpointPath)
		}
		if prompt != "" {
			return []string{prompt}
		}
		return nil
	}

	prompts := make([]string, len(sessionIDs))

	sessionLimit := min(sessionCount, len(prompts))
	for i := range sessionLimit {
		sessionPath := fmt.Sprintf("%s/%d", checkpointPath, i)
		prompts[i] = ReadSessionPromptFromTree(tree, sessionPath)
	}

	// Older committed metadata stored the latest prompt at the checkpoint root.
	latestIndex := sessionLimit - 1
	if latestIndex >= 0 && prompts[latestIndex] == "" {
		prompts[latestIndex] = ReadSessionPromptFromTree(tree, checkpointPath)
	}

	return prompts
}

// GetRemotePrimaryTree returns the tree at the first checkpoint read
// candidate's remote-tracking ref for the configured Primary (elected sync
// remote first, then the legacy origin tier; first existing tracking ref
// wins). Pure read — it never writes local refs. Errors when Primary isn't in
// Push (no remote-tracking shadow), when no read candidate is configured, or
// when no candidate's tracking ref exists (surfacing the first candidate's
// error).
func GetRemotePrimaryTree(ctx context.Context, repo *git.Repository) (*object.Tree, error) {
	refs := checkpoint.ResolveRefs(ctx)
	if !refs.PrimaryFetchableFromRemote() {
		return nil, fmt.Errorf("primary metadata ref %s is not pushed to a remote", refs.Primary)
	}
	candidates := CheckpointReadRemotes(ctx)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no git remotes configured to read primary metadata ref %s from", refs.Primary)
	}
	var ref *plumbing.Reference
	var firstErr error
	for _, remoteName := range candidates {
		refName := plumbing.NewRemoteReferenceName(remoteName, refs.Primary.Short())
		candidateRef, err := repo.Reference(refName, true)
		if err == nil {
			ref = candidateRef
			break
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("failed to get remote metadata reference %s: %w", refName, err)
		}
	}
	if ref == nil {
		return nil, firstErr
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata tree: %w", err)
	}
	return tree, nil
}

// OpenRepository opens the git repository from the repo root.
// It uses 'git rev-parse --show-toplevel' to find the repository root,
// which works correctly even when called from a subdirectory or a linked worktree.
func OpenRepository(ctx context.Context) (*git.Repository, error) {
	repo, err := gitrepo.OpenCurrent(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
}

// GetGitCommonDir returns the path to the shared git directory.
// In a regular checkout, this is .git/
// In a worktree, this is the main repo's .git/ (not .git/worktrees/<name>/)
// Uses git rev-parse --git-common-dir for reliable handling of worktrees.
func GetGitCommonDir(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = "."
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}

	commonDir := strings.TrimSpace(string(output))

	// git rev-parse --git-common-dir returns relative paths from the working directory,
	// so we need to make it absolute if it isn't already
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(".", commonDir)
	}

	return filepath.Clean(commonDir), nil
}

// EnsureEntireGitignore ensures all required entries are in .entire/.gitignore
// Works correctly from any subdirectory within the repository.
func EnsureEntireGitignore(ctx context.Context) error {
	// Get absolute path for the gitignore file
	gitignoreAbs, err := paths.AbsPath(ctx, entireGitignore)
	if err != nil {
		gitignoreAbs = entireGitignore // Fallback to relative
	}

	// Read existing content
	var content string
	if data, err := os.ReadFile(gitignoreAbs); err == nil { //nolint:gosec // path is from AbsPath or constant
		content = string(data)
	}

	// All entries that should be in .entire/.gitignore
	requiredEntries := []string{
		"tmp/",
		"settings.local.json",
		"metadata/",
		"logs/",
		redact.RedactorsDirName + "/local/",
	}

	// Track what needs to be added
	var toAdd []string
	for _, entry := range requiredEntries {
		if !strings.Contains(content, entry) {
			toAdd = append(toAdd, entry)
		}
	}

	// Nothing to add
	if len(toAdd) == 0 {
		return nil
	}

	// Ensure .entire directory exists
	if err := os.MkdirAll(filepath.Dir(gitignoreAbs), 0o750); err != nil {
		return fmt.Errorf("failed to create .entire directory: %w", err)
	}

	// Append missing entries to gitignore
	var sb strings.Builder
	for _, entry := range toAdd {
		sb.WriteString(entry + "\n")
	}
	content += sb.String()

	if err := os.WriteFile(gitignoreAbs, []byte(content), 0o644); err != nil { //nolint:gosec // path is from AbsPath or constant
		return fmt.Errorf("failed to write gitignore: %w", err)
	}
	return nil
}

// checkCanRewindWithWarning checks working directory and returns a warning with diff stats.
// Always returns canRewind=true but includes a warning message with +/- line stats for
// uncommitted changes. Used by manual-commit strategy.
func checkCanRewindWithWarning(ctx context.Context) (bool, string, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		// Can't open repo - still allow rewind but without stats
		return true, "", nil
	}
	defer repo.Close()

	status, err := gitrepo.Status(ctx, repo)
	if err != nil {
		return true, "", nil
	}

	if status.IsClean() {
		return true, "", nil
	}

	// Get HEAD commit tree for comparison - if we can't get it, just return without stats
	head, err := repo.Head()
	if err != nil {
		return true, "", nil
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return true, "", nil
	}

	headTree, err := headCommit.Tree()
	if err != nil {
		return true, "", nil
	}

	type fileChange struct {
		status   string // "modified", "added", "deleted"
		added    int
		removed  int
		filename string
	}

	var changes []fileChange
	// Use repo root, not cwd - git status returns paths relative to repo root
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return true, "", nil
	}

	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Skip untracked files
		if st.Worktree == git.Untracked {
			continue
		}

		var change fileChange
		change.filename = file

		switch {
		case st.Staging == git.Added || st.Worktree == git.Added:
			change.status = "added"
			// New file - count all lines as added
			absPath := filepath.Join(repoRoot, file)
			if content, err := os.ReadFile(absPath); err == nil { //nolint:gosec // absPath is repo root + relative path from git status
				change.added = countLines(content)
			}
		case st.Staging == git.Deleted || st.Worktree == git.Deleted:
			change.status = "deleted"
			// Deleted file - count lines from HEAD as removed
			if entry, err := headTree.File(file); err == nil {
				if content, err := entry.Contents(); err == nil {
					change.removed = countLines([]byte(content))
				}
			}
		case st.Staging == git.Modified || st.Worktree == git.Modified:
			change.status = "modified"
			// Modified file - compute diff stats
			var headContent, workContent []byte
			if entry, err := headTree.File(file); err == nil {
				if content, err := entry.Contents(); err == nil {
					headContent = []byte(content)
				}
			}
			absPath := filepath.Join(repoRoot, file)
			if content, err := os.ReadFile(absPath); err == nil { //nolint:gosec // absPath is repo root + relative path from git status
				workContent = content
			}
			if headContent != nil && workContent != nil {
				change.added, change.removed = computeDiffStats(headContent, workContent)
			}
		default:
			continue
		}

		changes = append(changes, change)
	}

	if len(changes) == 0 {
		return true, "", nil
	}

	// Sort changes by filename for consistent output
	sort.Slice(changes, func(i, j int) bool {
		return changes[i].filename < changes[j].filename
	})

	var msg strings.Builder
	msg.WriteString("The following uncommitted changes will be reverted:\n")

	totalAdded, totalRemoved := 0, 0
	for _, c := range changes {
		totalAdded += c.added
		totalRemoved += c.removed

		var stats string
		switch {
		case c.added > 0 && c.removed > 0:
			stats = fmt.Sprintf("+%d/-%d", c.added, c.removed)
		case c.added > 0:
			stats = fmt.Sprintf("+%d", c.added)
		case c.removed > 0:
			stats = fmt.Sprintf("-%d", c.removed)
		}

		fmt.Fprintf(&msg, "  %-10s %s", c.status+":", c.filename)
		if stats != "" {
			fmt.Fprintf(&msg, " (%s)", stats)
		}
		msg.WriteString("\n")
	}

	if totalAdded > 0 || totalRemoved > 0 {
		fmt.Fprintf(&msg, "\nTotal: +%d/-%d lines\n", totalAdded, totalRemoved)
	}

	return true, msg.String(), nil
}

// countLines counts the number of lines in content.
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	count := 1
	for _, b := range content {
		if b == '\n' {
			count++
		}
	}
	// Don't count trailing newline as extra line
	if len(content) > 0 && content[len(content)-1] == '\n' {
		count--
	}
	return count
}

// computeDiffStats computes added and removed line counts between old and new content.
// Uses a simple line-based diff algorithm.
func computeDiffStats(oldContent, newContent []byte) (added, removed int) {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)

	// Build a set of old lines with counts
	oldSet := make(map[string]int)
	for _, line := range oldLines {
		oldSet[line]++
	}

	// Check which new lines are truly new
	for _, line := range newLines {
		if oldSet[line] > 0 {
			oldSet[line]--
		} else {
			added++
		}
	}

	// Remaining old lines are removed
	for _, count := range oldSet {
		removed += count
	}

	return added, removed
}

// splitLines splits content into lines, preserving empty lines.
// Handles both Unix (\n) and Windows (\r\n) line endings.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	s := string(content)
	// Normalize Windows line endings to Unix
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Remove trailing newline to avoid empty last element
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// fileExists checks if a file exists at the given path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// getTaskCheckpointFromTree retrieves a task checkpoint from a commit tree.
// Shared implementation for shadow and linear-shadow strategies.
func getTaskCheckpointFromTree(ctx context.Context, point RewindPoint) (*TaskCheckpoint, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// Read checkpoint.json from the tree
	checkpointPath := point.MetadataDir + "/checkpoint.json"
	file, err := tree.File(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find checkpoint at %s: %w", checkpointPath, err)
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var checkpoint TaskCheckpoint
	if err := json.Unmarshal([]byte(content), &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint: %w", err)
	}

	return &checkpoint, nil
}

// getTaskTranscriptFromTree retrieves a task transcript from a commit tree.
// Shared implementation for shadow and linear-shadow strategies.
func getTaskTranscriptFromTree(ctx context.Context, point RewindPoint) ([]byte, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// MetadataDir format: .entire/metadata/<session>/tasks/<toolUseID>
	// Session transcript is at: .entire/metadata/<session>/<TranscriptFileName>
	sessionDir := filepath.Dir(filepath.Dir(point.MetadataDir))

	// Try current format first, then legacy
	transcriptPath := sessionDir + "/" + paths.TranscriptFileName
	file, err := tree.File(transcriptPath)
	if err != nil {
		transcriptPath = sessionDir + "/" + paths.TranscriptFileNameLegacy
		file, err = tree.File(transcriptPath)
		if err != nil {
			return nil, fmt.Errorf("failed to find transcript: %w", err)
		}
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	return []byte(content), nil
}

// ErrBranchNotFound is returned by DeleteBranchCLI when the branch does not exist.
var ErrBranchNotFound = errors.New("branch not found")

// DeleteBranchCLI deletes a git branch using the git CLI.
// Uses `git branch -D` instead of go-git's RemoveReference because go-git v5
// doesn't properly persist deletions when refs are packed (.git/packed-refs)
// or in a worktree context. This is the same class of go-git v5 bug that
// affects checkout and reset --hard (see HardResetWithProtection).
//
// Returns ErrBranchNotFound if the branch does not exist, allowing callers
// to use errors.Is for idempotent deletion patterns.
func DeleteBranchCLI(ctx context.Context, branchName string) error {
	// Pre-check: verify the branch exists so callers get a structured error
	// instead of parsing git's output string (which varies across locales).
	// git show-ref exits 1 for "not found" and 128+ for fatal errors (corrupt
	// repo, permissions, not a git directory). Only map exit code 1 to
	// ErrBranchNotFound; propagate other failures as-is.
	check := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	if err := check.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return fmt.Errorf("%w: %s", ErrBranchNotFound, branchName)
		}
		return fmt.Errorf("failed to check branch %s: %w", branchName, err)
	}

	cmd := exec.CommandContext(ctx, "git", "branch", "-D", "--", branchName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete branch %s: %s: %w", branchName, strings.TrimSpace(string(output)), err)
	}
	return nil
}

// branchExistsCLI checks if a branch exists using git CLI.
// Returns nil if the branch exists, or an error if it does not.
func branchExistsCLI(ctx context.Context, branchName string) error {
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("branch %s not found: %w", branchName, err)
	}
	return nil
}

// collectUntrackedFiles collects untracked files in the working directory that are
// NOT ignored by .gitignore. This is used to capture the initial state when starting
// a session, ensuring untracked files present at session start are preserved during rewind.
// Uses "git ls-files --others --exclude-standard -z" to respect .gitignore rules,
// avoiding bloated session state from large ignored directories like node_modules/.
// Returns paths relative to the repository root.
func collectUntrackedFiles(ctx context.Context) ([]string, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	cmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard", "-z")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git ls-files failed: %s: %w", strings.TrimSpace(string(exitErr.Stderr)), err)
		}
		return nil, fmt.Errorf("git ls-files failed: %w", err)
	}

	raw := string(output)
	if raw == "" {
		return nil, nil
	}

	var files []string
	for _, f := range strings.Split(raw, "\x00") {
		// Defense-in-depth: filter protected paths even though --exclude-standard should already handle them
		if f != "" && !isProtectedPath(f) {
			files = append(files, f)
		}
	}
	return files, nil
}

// CollectUntrackedFiles collects untracked, non-ignored paths relative to the
// repository root.
func CollectUntrackedFiles(ctx context.Context) ([]string, error) {
	return collectUntrackedFiles(ctx)
}

// NOTE: The following git tree helper functions have been moved to checkpoint/ package:
// - FlattenTree -> checkpoint.FlattenTree
// - CreateBlobFromContent -> checkpoint.CreateBlobFromContent
// - BuildTreeFromEntries -> checkpoint.BuildTreeFromEntries
// - sortTreeEntries (internal to checkpoint package)
// - treeNode, insertIntoTree, buildTreeObject (internal to checkpoint package)
//
// See push_common.go and session_test.go for usage examples.

// GetGitAuthorFromRepo retrieves the git user.name and user.email,
// checking both the repository-local config and the global ~/.gitconfig.
// Delegates to checkpoint.GetGitAuthorFromRepo — this wrapper exists so
// callers within the strategy package don't need a qualified import.
func GetGitAuthorFromRepo(repo *git.Repository) (name, email string) {
	return checkpoint.GetGitAuthorFromRepo(repo)
}

// GetCurrentBranchName returns the short name of the current branch if HEAD points to a branch.
// Returns an empty string if in detached HEAD state or if there's an error reading HEAD.
// This is used to capture branch metadata for checkpoints.
func GetCurrentBranchName(repo *git.Repository) string {
	head, err := repo.Head()
	if err != nil || !head.Name().IsBranch() {
		return ""
	}
	return head.Name().Short()
}

// getMainBranchHash returns the hash of the main branch (main or master).
// Returns ZeroHash if no main branch is found.
func GetMainBranchHash(repo *git.Repository) plumbing.Hash {
	// Try common main branch names
	for _, branchName := range []string{branchMain, branchMaster} {
		// Try local branch first
		ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
		if err == nil {
			return ref.Hash()
		}
		// Try remote tracking branch
		ref, err = repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
		if err == nil {
			return ref.Hash()
		}
	}
	return plumbing.ZeroHash
}

// GetDefaultBranchName returns the name of the default branch.
// First checks origin/HEAD, then falls back to checking if main/master exists.
// Returns empty string if unable to determine.
// NOTE: Duplicated from cli/git_operations.go - see ENT-129 for consolidation.
func GetDefaultBranchName(repo *git.Repository) string {
	// Try to get the symbolic reference for origin/HEAD
	// Use resolved=false to get the symbolic ref itself, then extract its target
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), false)
	if err == nil && ref != nil && ref.Type() == plumbing.SymbolicReference {
		target := ref.Target().String()
		if branchName, found := strings.CutPrefix(target, "refs/remotes/origin/"); found {
			return branchName
		}
	}

	// Fallback: check if origin/main or origin/master exists
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchMain), true); err == nil {
		return branchMain
	}
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchMaster), true); err == nil {
		return branchMaster
	}

	// Final fallback: check local branches
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(branchMain), true); err == nil {
		return branchMain
	}
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(branchMaster), true); err == nil {
		return branchMaster
	}

	return ""
}

// IsOnDefaultBranch checks if the repository HEAD is on the default branch.
// Returns (isOnDefault, currentBranchName).
// NOTE: Duplicated from cli/git_operations.go - see ENT-129 for consolidation.
func IsOnDefaultBranch(repo *git.Repository) (bool, string) {
	currentBranch := GetCurrentBranchName(repo)
	if currentBranch == "" {
		return false, ""
	}

	defaultBranch := GetDefaultBranchName(repo)
	if defaultBranch == "" {
		// Can't determine default, check common names
		if currentBranch == branchMain || currentBranch == branchMaster {
			return true, currentBranch
		}
		return false, currentBranch
	}

	return currentBranch == defaultBranch, currentBranch
}

// prepareTranscriptForState ensures the transcript is up-to-date for the given session.
// Only prepares for ACTIVE sessions — IDLE/ENDED sessions are already flushed.
// Resolves the agent from state.AgentType internally. Multiple calls are safe but
// not free — callers should avoid redundant calls for performance.
func prepareTranscriptForState(ctx context.Context, state *SessionState) {
	if !state.Phase.IsActive() || state.TranscriptPath == "" || state.AgentType == "" {
		return
	}
	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		logging.Debug(ctx, "prepareTranscriptForState: unknown agent type",
			slog.String("session_id", state.SessionID),
			slog.String("agent_type", string(state.AgentType)),
			slog.Any("error", err),
		)
		return
	}
	prepareTranscriptIfNeeded(ctx, ag, state.TranscriptPath)
}

// prepareTranscriptIfNeeded calls PrepareTranscript for agents that implement
// the TranscriptPreparer interface. This ensures transcript files exist before
// they are read (e.g., OpenCode creates its transcript lazily via `opencode export`).
// Errors are silently ignored — this is best-effort for hook paths.
func prepareTranscriptIfNeeded(ctx context.Context, ag agent.Agent, transcriptPath string) {
	if ag == nil || transcriptPath == "" {
		return
	}
	if preparer, ok := agent.AsTranscriptPreparer(ag); ok {
		// Best-effort: callers handle missing files gracefully.
		// Transcript may not be available yet (e.g., agent not installed).
		_ = preparer.PrepareTranscript(ctx, transcriptPath) //nolint:errcheck // Best-effort in hook path
	}
}
