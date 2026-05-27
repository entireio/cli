package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
// The push itself handles failures gracefully (doPushBranch warns and continues),
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
	branchName := paths.MetadataBranchName
	tmpRef := FetchTmpRefPrefix + branchName
	srcRef := "refs/heads/" + branchName

	if err := fetchURLIntoTmpRef(ctx, remoteURL, srcRef, tmpRef, "metadata branch", true); err != nil {
		return err
	}
	return PromoteTmpRefSafely(ctx, plumbing.ReferenceName(tmpRef), plumbing.NewBranchReferenceName(branchName), branchName)
}

// FetchV2MainFromURL fetches the v2 /main ref from a remote URL and advances
// the local ref only when doing so cannot rewind locally-ahead commits.
// Uses explicit refspec since v2 refs are under refs/entire/, not refs/heads/.
//
// The fetch is unfiltered (NoFilter: true) because resume needs full metadata.
func FetchV2MainFromURL(ctx context.Context, remoteURL string) error {
	if err := fetchURLIntoTmpRef(ctx, remoteURL, paths.V2MainRefName, V2MainFetchTmpRef, "v2 /main", true); err != nil {
		return err
	}
	return PromoteTmpRefSafely(ctx, V2MainFetchTmpRef, paths.V2MainRefName, "v2 /main")
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

// fetchMetadataBranchIfMissing fetches the metadata branch from a URL only if it doesn't exist locally.
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
	branchRef := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	if _, err := repo.Reference(branchRef, true); err == nil {
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
