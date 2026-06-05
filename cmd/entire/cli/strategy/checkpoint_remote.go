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

	// If the v1 checkpoint branch doesn't exist locally (or is only the empty
	// bootstrap orphan), try to fetch it from the URL. Once the branch has
	// checkpoint data, subsequent pushes skip the fetch entirely. Only fetch
	// the metadata branch; trails are always pushed to the user's push
	// remote, not the checkpoint remote.
	if _, err := fetchMetadataBranchIfMissing(ctx, checkpointURL); err != nil {
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

// bootstrapMetadataFromCheckpointRemote populates the local metadata branch
// from the configured checkpoint_remote when it doesn't exist locally yet,
// or exists only as the empty orphan minted by a previous failed bootstrap.
//
// On a fresh clone of a repo whose checkpoints live in a separate repo,
// neither the local branch nor origin's remote-tracking ref exists — without
// this fetch, EnsureMetadataBranch would mint an unrelated empty orphan that
// hides existing checkpoints and rejects later fetches as non-fast-forward
// (issue #1374). Best-effort: on failure the caller falls back to orphan
// creation (the remote branch legitimately doesn't exist before the first
// push from any device).
func bootstrapMetadataFromCheckpointRemote(ctx context.Context) {
	if !remote.Configured(ctx) {
		return
	}
	checkpointURL, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint-remote: could not resolve fetch URL for metadata branch bootstrap",
			slog.String("error", err.Error()),
		)
		return
	}
	fetched, err := fetchMetadataBranchIfMissing(ctx, checkpointURL)
	if err != nil {
		logging.Warn(ctx, "checkpoint-remote: metadata branch bootstrap failed",
			slog.String("error", err.Error()),
		)
		return
	}
	if fetched {
		fmt.Fprintf(os.Stderr, "✓ Created local branch '%s' from checkpoint remote\n", paths.MetadataBranchName)
	}
}

// fetchMetadataBranchIfMissing fetches the primary metadata ref from a URL
// only if it doesn't exist locally with real data. This avoids network calls
// on every push — once the branch has checkpoint data, this is a no-op.
//
// An empty bootstrap orphan does not count as existing: it means a previous
// bootstrap couldn't reach the checkpoint remote (no token, network down,
// branch not pushed yet) and EnsureSetup fell back to orphan creation —
// fetching again is the recovery path, and SafelyAdvanceLocalRef discards
// the no-op orphan commit during the promote.
//
// Returns true when the branch was actually fetched. Fetch failures are
// logged but swallowed (returns nil error): the remote branch legitimately
// doesn't exist before the first push from any device, and push will create
// it. Only failures to open the repository are returned.
func fetchMetadataBranchIfMissing(ctx context.Context, remoteURL string) (bool, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	// Skip the network call when the branch already has checkpoint data.
	refs := checkpoint.ResolveCommittedRefs(ctx)
	if ref, refErr := repo.Reference(refs.Primary, true); refErr == nil {
		empty, emptyErr := isEmptyMetadataBranch(repo, ref)
		if emptyErr != nil || !empty {
			return false, nil // Branch has data (or is unreadable) — skip fetch
		}
		// Empty bootstrap orphan — fall through and try the fetch again.
	}

	// Branch is missing (or an empty orphan) — try to fetch it from the URL.
	// Not fatal on failure, but log it: a silent swallow here made auth and
	// network problems invisible while EnsureSetup fell back to minting an
	// empty orphan.
	if err := FetchMetadataBranch(ctx, remoteURL); err != nil {
		logging.Warn(ctx, "checkpoint-remote: metadata branch fetch failed, continuing without it",
			slog.String("error", err.Error()),
		)
		return false, nil
	}

	logging.Info(ctx, "checkpoint-remote: fetched metadata branch from URL")
	return true, nil
}
