package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6/plumbing"
)

// checkpointRemoteFetchTimeout is the timeout for fetching branches from the checkpoint URL.
const checkpointRemoteFetchTimeout = 30 * time.Second

// Git remote protocol identifiers.
const (
	protocolSSH   = "ssh"
	protocolHTTPS = "https"
)

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

	// Get the push remote URL for protocol detection and fork detection
	pushRemoteURL, err := getRemoteURL(ctx, pushRemoteName)
	if err != nil {
		logging.Debug(ctx, "checkpoint-remote: could not get push remote URL, skipping",
			slog.String("remote", pushRemoteName),
			slog.String("error", err.Error()),
		)
		return ps
	}

	// Parse the push remote URL once for both fork detection and URL derivation
	pushInfo, err := parseGitRemoteURL(pushRemoteURL)
	if err != nil {
		logging.Warn(ctx, "checkpoint-remote: could not parse push remote URL",
			slog.String("remote", pushRemoteName),
			slog.String("error", err.Error()),
		)
		return ps
	}

	// Fork detection: compare owners
	checkpointOwner := config.Owner()
	if pushInfo.owner != "" && checkpointOwner != "" && !strings.EqualFold(pushInfo.owner, checkpointOwner) {
		logging.Debug(ctx, "checkpoint-remote: push remote owner differs from checkpoint remote owner, skipping (fork detected)",
			slog.String("push_owner", pushInfo.owner),
			slog.String("checkpoint_owner", checkpointOwner),
		)
		return ps
	}

	// Derive checkpoint URL using same protocol as push remote
	checkpointURL, err := deriveCheckpointURLFromInfo(pushInfo, config)
	if err != nil {
		logging.Warn(ctx, "checkpoint-remote: could not derive URL from push remote",
			slog.String("remote", pushRemoteName),
			slog.String("repo", config.Repo),
			slog.String("error", err.Error()),
		)
		return ps
	}

	ps.checkpointURL = checkpointURL

	// If the checkpoint branch doesn't exist locally, try to fetch it from the URL.
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

// ResolveRemoteRepo returns the host, owner, and repo name for the given git remote.
// It parses the remote URL (SSH or HTTPS) and extracts the components.
// For example, git@github.com:org/my-repo.git returns ("github.com", "org", "my-repo").
func ResolveRemoteRepo(ctx context.Context, remoteName string) (host, owner, repo string, err error) {
	rawURL, err := getRemoteURL(ctx, remoteName)
	if err != nil {
		return "", "", "", fmt.Errorf("get remote URL for %q: %w", remoteName, err)
	}
	info, err := parseGitRemoteURL(rawURL)
	if err != nil {
		return "", "", "", fmt.Errorf("parse remote URL: %w", err)
	}
	return info.host, info.owner, info.repo, nil
}

// gitRemoteInfo holds parsed components of a git remote URL.
type gitRemoteInfo struct {
	protocol string // "ssh" or "https"
	host     string // e.g., "github.com"
	owner    string // e.g., "org"
	repo     string // e.g., "my-repo" (without .git)
}

// parseGitRemoteURL parses a git remote URL into its components.
// Supports:
//   - SSH SCP format: git@github.com:org/repo.git
//   - HTTPS format: https://github.com/org/repo.git
//   - SSH protocol format: ssh://git@github.com/org/repo.git
func parseGitRemoteURL(rawURL string) (*gitRemoteInfo, error) {
	rawURL = strings.TrimSpace(rawURL)

	// SSH SCP format: git@github.com:org/repo.git
	if strings.Contains(rawURL, ":") && !strings.Contains(rawURL, "://") {
		// Split on the first ":"
		parts := strings.SplitN(rawURL, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid SSH URL: %s", redactURL(rawURL))
		}
		hostPart := parts[0] // e.g., "git@github.com"
		pathPart := parts[1] // e.g., "org/repo.git"

		host := hostPart
		if idx := strings.Index(host, "@"); idx >= 0 {
			host = host[idx+1:]
		}

		owner, repo, err := splitOwnerRepo(pathPart)
		if err != nil {
			return nil, err
		}

		return &gitRemoteInfo{protocol: protocolSSH, host: host, owner: owner, repo: repo}, nil
	}

	// URL format: https://github.com/org/repo.git or ssh://git@github.com/org/repo.git
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %s", redactURL(rawURL))
	}

	protocol := u.Scheme
	if protocol == "" {
		return nil, fmt.Errorf("no protocol in URL: %s", redactURL(rawURL))
	}
	host := u.Hostname()

	// Path is like /org/repo.git — trim leading slash
	pathPart := strings.TrimPrefix(u.Path, "/")
	owner, repo, err := splitOwnerRepo(pathPart)
	if err != nil {
		return nil, err
	}

	return &gitRemoteInfo{protocol: protocol, host: host, owner: owner, repo: repo}, nil
}

// splitOwnerRepo splits "org/repo.git" into owner and repo (without .git suffix).
func splitOwnerRepo(path string) (string, string, error) {
	path = strings.TrimSuffix(path, ".git")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from path: %s", path)
	}
	return parts[0], parts[1], nil
}

// deriveCheckpointURL constructs a checkpoint remote URL using the same protocol
// as the push remote. For example, if push remote uses SSH, the checkpoint URL
// will also use SSH.
func deriveCheckpointURL(pushRemoteURL string, config *settings.CheckpointRemoteConfig) (string, error) {
	info, err := parseGitRemoteURL(pushRemoteURL)
	if err != nil {
		return "", fmt.Errorf("cannot parse push remote URL: %w", err)
	}
	return deriveCheckpointURLFromInfo(info, config)
}

// deriveCheckpointURLFromInfo constructs a checkpoint URL from already-parsed remote info.
func deriveCheckpointURLFromInfo(info *gitRemoteInfo, config *settings.CheckpointRemoteConfig) (string, error) {
	switch info.protocol {
	case protocolSSH:
		// SCP format: git@host:owner/repo.git
		return fmt.Sprintf("git@%s:%s.git", info.host, config.Repo), nil
	case protocolHTTPS:
		return fmt.Sprintf("https://%s/%s.git", info.host, config.Repo), nil
	default:
		return "", fmt.Errorf("unsupported protocol %q in push remote", info.protocol)
	}
}

// extractOwnerFromRemoteURL extracts the owner from a git remote URL.
// Returns empty string if the URL cannot be parsed.
func extractOwnerFromRemoteURL(rawURL string) string {
	info, err := parseGitRemoteURL(rawURL)
	if err != nil {
		return ""
	}
	return info.owner
}

// getRemoteURL returns the URL configured for a git remote.
func getRemoteURL(ctx context.Context, remoteName string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", remoteName)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("remote %q not found", remoteName)
	}
	return strings.TrimSpace(string(output)), nil
}

// redactURL removes credentials from a URL for safe logging.
// Handles both HTTPS URLs with embedded credentials and general URLs.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		// For non-URL formats (SSH SCP), just return the host portion
		if idx := strings.Index(rawURL, "@"); idx >= 0 {
			if colonIdx := strings.Index(rawURL[idx:], ":"); colonIdx >= 0 {
				return rawURL[idx+1:idx+colonIdx] + ":***"
			}
		}
		return "<unparseable>"
	}
	u.User = nil
	u.RawQuery = ""
	host := u.Host
	path := u.Path
	return u.Scheme + "://" + host + path
}

// fetchMetadataBranchIfMissing fetches the metadata branch from a URL only if it doesn't exist locally.
// This avoids network calls on every push — once the branch exists locally, this is a no-op.
// Fetch failures are silently swallowed (returns nil): the push will handle creating the
// branch on the remote. Only fatal errors (opening repo, creating local branch) are returned.
func fetchMetadataBranchIfMissing(ctx context.Context, remoteURL string) error {
	branchName := paths.MetadataBranchName

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Check if branch already exists locally - if so, nothing to do
	branchRef := plumbing.NewBranchReferenceName(branchName)
	if _, err := repo.Reference(branchRef, true); err == nil {
		return nil // Branch exists locally, skip fetch
	}

	// Branch doesn't exist locally - try to fetch it from the URL.
	// Fetch to a temp ref to avoid polluting the remote-tracking namespace.
	fetchCtx, cancel := context.WithTimeout(ctx, checkpointRemoteFetchTimeout)
	defer cancel()

	tmpRef := "refs/entire-fetch-tmp/" + branchName
	refSpec := fmt.Sprintf("+refs/heads/%s:%s", branchName, tmpRef)
	fetchCmd := CheckpointGitCommand(fetchCtx, remoteURL, "fetch", "--no-tags", remoteURL, refSpec)
	// Merge GIT_TERMINAL_PROMPT=0 into whatever env CheckpointGitCommand set.
	// If the token was injected, cmd.Env is already populated; otherwise use os.Environ().
	if fetchCmd.Env == nil {
		fetchCmd.Env = os.Environ()
	}
	fetchCmd.Env = append(fetchCmd.Env, "GIT_TERMINAL_PROMPT=0")
	if err := fetchCmd.Run(); err != nil {
		// Fetch failed - remote may be unreachable or branch doesn't exist there yet.
		// Not fatal: push will create it on the remote when it succeeds.
		return nil
	}

	// Fetch succeeded - create local branch from the fetched ref
	fetchedRef, err := repo.Reference(plumbing.ReferenceName(tmpRef), true)
	if err != nil {
		// Fetch succeeded but ref not found - branch may not exist on remote
		return nil
	}

	newRef := plumbing.NewHashReference(branchRef, fetchedRef.Hash())
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to create local branch from fetched ref: %w", err)
	}

	// Clean up the temp ref (best-effort, not critical if it fails)
	_ = repo.Storer.RemoveReference(plumbing.ReferenceName(tmpRef)) //nolint:errcheck // cleanup is best-effort

	logging.Info(ctx, "checkpoint-remote: fetched metadata branch from URL")
	return nil
}
