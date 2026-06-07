package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// checkpointRemoteFetchTimeout is the timeout for fetching branches from the checkpoint URL.
const checkpointRemoteFetchTimeout = 30 * time.Second

// pushSettings holds the resolved push configuration from a single settings load.
type pushSettings struct {
	// remote is the git remote name to use for pushing (the user's push remote).
	remote string
	// checkpointURL is the derived URL for pushing checkpoint branches.
	// When set, checkpoint/trails branches are pushed directly to this URL
	// instead of the remote name. Empty means use the remote name.
	checkpointURL string
	// pushDisabled is true if push_sessions is explicitly set to false.
	pushDisabled bool
}

// pushTarget returns the target to use for git push/fetch commands for checkpoint branches.
// If a checkpoint URL is configured, returns that; otherwise returns the remote name.
func (ps *pushSettings) pushTarget() string {
	if ps.checkpointURL != "" {
		return ps.checkpointURL
	}
	return ps.remote
}

// hasCheckpointURL returns true if a dedicated checkpoint URL is configured.
func (ps *pushSettings) hasCheckpointURL() bool {
	return ps.checkpointURL != ""
}

// resolvePushSettings loads settings once and returns the resolved push config.
// If a structured checkpoint_remote is configured (e.g., {"provider": "github", "repo": "org/repo"}):
//   - Derives the checkpoint URL from the push remote's protocol (SSH vs HTTPS)
//   - Skips if the push remote owner differs from the checkpoint repo owner (fork detection)
//   - If a checkpoint branch doesn't exist locally, attempts to fetch it from the URL
//
// The push itself handles failures gracefully (doPushRef warns and continues),
// so no reachability check is needed here.
func resolvePushSettings(ctx context.Context, pushRemoteName string) pushSettings {
	s, err := settings.Load(ctx)
	if err != nil {
		return pushSettings{remote: pushRemoteName}
	}

	ps := pushSettings{
		remote:       pushRemoteName,
		pushDisabled: s.IsPushSessionsDisabled(),
	}

	config := s.GetCheckpointRemote()
	if config == nil {
		return ps
	}
	checkpointURL, enabled, err := remote.PushURL(ctx, pushRemoteName)
	if err != nil {
		logging.Warn(ctx, "checkpoint-remote: could not derive URL from push remote",
			slog.String("remote", pushRemoteName),
			slog.String("repo", config.Repo),
			slog.String("error", err.Error()),
		)
		return ps
	}
	if !enabled || checkpointURL == "" {
		return ps
	}

	ps.checkpointURL = checkpointURL

	// If the v1 checkpoint branch doesn't exist locally, try to fetch it from the URL.
	// This is a one-time operation — once the branch exists locally, subsequent pushes
	// skip the fetch entirely. Only fetch the metadata branch; trails are always pushed
	// to the user's push remote, not the checkpoint remote.
	if err := fetchMetadataBranchIfMissing(ctx, checkpointURL); err != nil {
		logging.Warn(ctx, "checkpoint-remote: failed to fetch metadata branch",
			slog.String("error", err.Error()),
		)
	}

	return ps
}

// FetchMetadataBranch fetches the metadata branch from the checkpoint remote URL
// and updates the local branch. Unlike fetchMetadataBranchIfMissing, this always
// fetches regardless of whether the branch exists locally (for resume scenarios
// where the local branch may be stale).
//
// The fetch is unfiltered (NoFilter: true) because resume needs blob content
// (transcripts, metadata JSON) — not just tree objects.
func FetchMetadataBranch(ctx context.Context, remoteURL string) error {
	refs := checkpoint.ResolveCommittedRefs(ctx)
	if !refs.Primary.IsBranch() {
		return fmt.Errorf("primary metadata ref %s is not a branch", refs.Primary)
	}
	branchName := refs.Primary.Short()
	tmpRef := FetchTmpRefPrefix + branchName
	srcRef := refs.Primary.String()

	if err := fetchURLIntoTmpRef(ctx, remoteURL, srcRef, tmpRef, "metadata branch", true); err != nil {
		return err
	}
	if err := PromoteTmpRefSafely(ctx, plumbing.ReferenceName(tmpRef), refs.Primary, branchName); err != nil {
		return err
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "committed-ref mirror skipped after metadata fetch",
			slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()
	mirrorCommittedMetadataRefBestEffort(ctx, repo, refs)
	return nil
}

// fetchURLIntoTmpRef runs `git fetch <remoteURL> +<srcRef>:<tmpRef>` via the
// checkpoint git wrapper, disabling the terminal prompt so a misconfigured
// credential helper doesn't hang the process. Errors include the redacted URL
// and any captured stderr so operators can diagnose without credentials
// leaking into logs.
//
// When noFilter is true, --filter=blob:none is suppressed even if filtered
// fetches are globally enabled. Use noFilter for operations that need blob
// content (resume, explain) as opposed to sync operations (push recovery)
// that only need tree structure.
func fetchURLIntoTmpRef(ctx context.Context, remoteURL, srcRef, tmpRef, label string, noFilter bool) error {
	fetchCtx, cancel := context.WithTimeout(ctx, checkpointRemoteFetchTimeout)
	defer cancel()

	refSpec := fmt.Sprintf("+%s:%s", srcRef, tmpRef)
	output, fetchErr := remote.Fetch(fetchCtx, remote.FetchOptions{
		Remote:   remoteURL,
		RefSpecs: []string{refSpec},
		NoTags:   true,
		NoFilter: noFilter,
	})
	if fetchErr == nil {
		return nil
	}

	redactedURL := remote.RedactURL(remoteURL)
	msg := strings.TrimSpace(strings.ReplaceAll(string(output), remoteURL, redactedURL))
	if msg != "" {
		return fmt.Errorf("fetch %s from %s failed: %s: %w", label, redactedURL, msg, fetchErr)
	}
	return fmt.Errorf("fetch %s from %s failed: %w", label, redactedURL, fetchErr)
}

// fetchMetadataBranchIfMissing fetches the primary metadata ref from a URL only if it doesn't exist locally.
// This avoids network calls on every push — once the branch exists locally, this is a no-op.
// Fetch failures are silently swallowed (returns nil): the push will handle creating the
// branch on the remote. Only fatal errors (opening repo, creating local branch) are returned.
func fetchMetadataBranchIfMissing(ctx context.Context, remoteURL string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	// Check if branch already exists locally - if so, nothing to do
	refs := checkpoint.ResolveCommittedRefs(ctx)
	if _, err := repo.Reference(refs.Primary, true); err == nil {
		return nil // Branch exists locally, skip fetch
	}

	// Branch doesn't exist locally - try to fetch it from the URL.
	// Fetch failures are not fatal: push will create it on the remote when it succeeds.
	if err := FetchMetadataBranch(ctx, remoteURL); err != nil {
		return nil
	}

	logging.Info(ctx, "checkpoint-remote: fetched metadata branch from URL")
	return nil
}

// resolveCheckpointRemoteURLFn resolves the checkpoint_remote fetch URL. It is a
// package var so tests can point bootstrap/heal at a local repository instead of
// the provider-derived (e.g. github.com) URL, which can't be reached offline.
var resolveCheckpointRemoteURLFn = resolveCheckpointRemoteURL

// resolveCheckpointRemoteURL returns the fetch URL for the configured
// checkpoint_remote, or ok=false when no checkpoint_remote is configured or a
// dedicated checkpoint URL cannot be resolved. It never returns an error: callers
// treat a missing URL as "no checkpoint remote to adopt from" and fall back to
// keeping the local branch / creating an orphan.
//
// remote.FetchURL silently falls back to the origin remote URL when it cannot
// derive a checkpoint URL (e.g. origin uses a non-derivable protocol and the
// provider host is unknown). Adopting from origin would contradict the rule that
// origin is never authoritative when a checkpoint_remote is configured (issue
// #1374), so we verify the resolved URL actually targets the configured checkpoint
// repo and treat the origin fallback as "unresolved".
func resolveCheckpointRemoteURL(ctx context.Context) (string, bool) {
	s, err := settings.Load(ctx)
	if err != nil {
		return "", false
	}
	config := s.GetCheckpointRemote()
	if config == nil {
		return "", false
	}
	url, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Debug(ctx, "checkpoint-remote: could not resolve fetch URL for metadata bootstrap",
			slog.String("error", err.Error()))
		return "", false
	}
	if !urlTargetsCheckpointRepo(url, config) {
		logging.Debug(ctx, "checkpoint-remote: fetch URL did not resolve to the configured checkpoint repo; not adopting from origin",
			slog.String("repo", config.Repo))
		return "", false
	}
	return url, true
}

// urlTargetsCheckpointRepo reports whether url points at the configured
// checkpoint repository (host-agnostic owner/repo match). It distinguishes a
// derived checkpoint URL from remote.FetchURL's origin fallback, which targets the
// origin repository instead. A same-repo checkpoint_remote (origin == checkpoint
// repo) still matches, which is correct: adopting from that URL is adopting the
// checkpoint repo.
func urlTargetsCheckpointRepo(url string, config *settings.CheckpointRemoteConfig) bool {
	info, err := remote.ParseURL(url)
	if err != nil || info.Owner == "" || info.Repo == "" {
		return false
	}
	return strings.EqualFold(info.Owner+"/"+info.Repo, config.Repo)
}

// BootstrapMetadataFromCheckpointRemote populates the local metadata branch from a
// configured checkpoint_remote when no local or origin-tracked branch exists.
//
// This prevents `entire enable` on a second device from creating an empty orphan
// branch that conflicts with the real metadata branch already stored in the
// checkpoint remote — the orphan would otherwise leave `entire checkpoint list`
// empty and cause a non-fast-forward rejection on the next fetch (issue #1374).
//
// Returns true when a non-empty branch was fetched and the local ref now points at
// it. When no checkpoint_remote is configured or its URL can't be resolved, returns
// (false, nil) so setup can fall back to creating an orphan.
func BootstrapMetadataFromCheckpointRemote(ctx context.Context, repo *git.Repository) (bool, error) {
	url, ok := resolveCheckpointRemoteURLFn(ctx)
	if !ok {
		return false, nil
	}
	return bootstrapMetadataFromURL(ctx, repo, url)
}

// bootstrapMetadataFromURL fetches the metadata branch from remoteURL into the
// local branch. It assumes the local branch is absent, so FetchMetadataBranch's
// SafelyAdvanceLocalRef sets the ref directly to the fetched tip (no replay, no
// empty commit). Fetch failures and an empty fetched branch are non-fatal and
// return (false, nil) so callers can fall back to creating an orphan.
func bootstrapMetadataFromURL(ctx context.Context, repo *git.Repository, remoteURL string) (bool, error) {
	if err := FetchMetadataBranch(ctx, remoteURL); err != nil {
		logging.Debug(ctx, "checkpoint-remote: metadata bootstrap fetch failed; will create orphan",
			slog.String("error", err.Error()))
		return false, nil
	}
	ref, err := repo.Reference(checkpoint.ResolveCommittedRefs(ctx).Primary, true)
	if err != nil {
		// Fetch reported success but the local ref is unreadable — treat as "no
		// usable data" and let the caller create an orphan rather than fail setup.
		return false, nil //nolint:nilerr // intentional non-fatal fallback
	}
	hasData, err := metadataBranchHasData(repo, ref)
	if err != nil || !hasData {
		return false, nil //nolint:nilerr // unreadable or data-free tree → fall back to orphan
	}
	fmt.Fprintf(os.Stderr, "✓ Fetched metadata branch '%s' from checkpoint remote\n", paths.MetadataBranchName)
	return true, nil
}

// HealEmptyOrphanMetadataFromCheckpointRemote replaces a local metadata branch that
// holds no checkpoint data (an empty orphan, or a vercel.json-only orphan — the bug
// behind issue #1374) with the real branch fetched from a configured
// checkpoint_remote. This repairs second-device repos that were enabled before the
// checkpoint_remote bootstrap existed and were left with an un-initialized orphan
// disjoint from the remote history.
//
// Returns true when the branch was healed. A local branch that already has
// checkpoints, a missing checkpoint_remote, or an unresolvable URL all return
// (false, nil).
func HealEmptyOrphanMetadataFromCheckpointRemote(ctx context.Context, repo *git.Repository, localRef *plumbing.Reference) (bool, error) {
	hasData, err := metadataBranchHasData(repo, localRef)
	if err != nil {
		return false, fmt.Errorf("failed to check metadata branch contents: %w", err)
	}
	if hasData {
		return false, nil
	}
	url, ok := resolveCheckpointRemoteURLFn(ctx)
	if !ok {
		return false, nil
	}
	return healEmptyOrphanFromURL(ctx, repo, url)
}

// healEmptyOrphanFromURL fetches the metadata branch from remoteURL and force-sets
// the local ref to the fetched tip. It deliberately bypasses SafelyAdvanceLocalRef:
// the local orphan is disconnected from the real branch, so a safe advance would
// cherry-pick the orphan commit onto the real tip, leaving a stray empty commit.
// Discarding the checkpoint-free orphan is always correct here. Fetch failures and
// a checkpoint-free fetched branch are non-fatal and return (false, nil).
func healEmptyOrphanFromURL(ctx context.Context, repo *git.Repository, remoteURL string) (bool, error) {
	refs := checkpoint.ResolveCommittedRefs(ctx)
	if !refs.Primary.IsBranch() {
		return false, nil
	}
	tmpRef := plumbing.ReferenceName(FetchTmpRefPrefix + refs.Primary.Short())
	defer func() { _ = repo.Storer.RemoveReference(tmpRef) }() //nolint:errcheck // cleanup is best-effort

	if err := fetchURLIntoTmpRef(ctx, remoteURL, refs.Primary.String(), tmpRef.String(), "metadata branch", true); err != nil {
		logging.Debug(ctx, "checkpoint-remote: empty-orphan heal fetch failed",
			slog.String("error", err.Error()))
		return false, nil
	}

	fetchedRef, err := repo.Reference(tmpRef, true)
	if err != nil {
		return false, nil
	}
	fetchedHasData, err := metadataBranchHasData(repo, fetchedRef)
	if err != nil || !fetchedHasData {
		return false, nil
	}

	// Force-set the primary ref to the fetched tip (and best-effort-advance the
	// mirror). AdvanceCommittedPrimary does a plain SetReference rather than a safe
	// advance, which is what we want: the local orphan is disconnected from the
	// real branch and must be discarded, not replayed onto it.
	if err := AdvanceCommittedPrimary(ctx, repo, refs, fetchedRef.Hash()); err != nil {
		return false, fmt.Errorf("failed to replace empty orphan metadata ref: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Replaced empty metadata ref '%s' with data from checkpoint remote\n", refs.Primary.Short())
	return true, nil
}
