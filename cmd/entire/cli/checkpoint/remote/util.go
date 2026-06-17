package remote

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
)

const originRemote = "origin"

const (
	ProtocolSSH   = gitremote.ProtocolSSH
	ProtocolHTTPS = gitremote.ProtocolHTTPS
)

// Info is an alias for gitremote.Info.
type Info = gitremote.Info

// FetchURLOptions configures FetchURL.
type FetchURLOptions struct {
	WorktreeRoot string
}

// FetchURL returns the effective checkpoint fetch URL for the current repository.
// If strategy_options.checkpoint_remote is configured, the returned URL is derived
// from the origin remote's protocol/host and the configured checkpoint repo.
// Otherwise, the origin remote URL is returned directly.
//
// If ENTIRE_CHECKPOINT_TOKEN is set and a checkpoint remote is configured, HTTPS is
// forced so the token can be used even when origin is configured via SSH.
func FetchURL(ctx context.Context, opts ...FetchURLOptions) (string, error) {
	var opt FetchURLOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	getRemoteURL := GetRemoteURL
	if opt.WorktreeRoot != "" {
		ctx = settings.WithWorktreeRoot(ctx, opt.WorktreeRoot)
		getRemoteURL = func(ctx context.Context, remoteName string) (string, error) {
			return GetRemoteURLInDir(ctx, opt.WorktreeRoot, remoteName)
		}
	}

	withToken := strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)) != ""

	originURL, originErr := getRemoteURL(ctx, originRemote)
	if originErr != nil {
		originURL = ""
	}

	if originURL != "" && withToken {
		if tokenURL, ok := deriveTokenOriginURL(originURL); ok {
			originURL = tokenURL
		}
	}

	s, err := settings.Load(ctx)
	if err != nil {
		if originURL != "" {
			logFallback(ctx, "fetch", originURL, "load settings", err)
			return originURL, nil
		}
		return "", fmt.Errorf("load settings: %w", err)
	}

	config := s.GetCheckpointRemote()
	if config == nil {
		if originURL == "" {
			return "", fmt.Errorf("no fetch URL found: %w", originErr)
		}
		return originURL, nil
	}

	if withToken {
		host, ok := providerHost(config.Provider)
		if ok {
			checkpointURL, err := deriveCheckpointURLFromInfo(&Info{
				Protocol: ProtocolHTTPS,
				Host:     host,
			}, config)
			if err == nil {
				return checkpointURL, nil
			}
		}

		// In token-based execution path, short-circuit to avoid additional
		// change in protocol.
		if originURL != "" {
			return originURL, nil
		}
	}

	if originURL == "" {
		return "", fmt.Errorf("no fetch URL found: %w", originErr)
	}

	info, err := ParseURL(originURL)
	if err != nil {
		logFallback(ctx, "fetch", originURL, "parse origin remote URL", err)
		return originURL, nil
	}

	checkpointURL, err := deriveCheckpointURLFromInfo(info, config)
	if err != nil {
		// Origin's protocol can't be mapped to a git transport (e.g. entire://,
		// file://). Honor the configured checkpoint_remote by targeting the
		// provider's canonical host over HTTPS rather than falling back to origin.
		if providerURL, ok := resolveProviderCheckpointURL(config, originRemote, opt.WorktreeRoot); ok {
			return providerURL, nil
		}
		logFallback(ctx, "fetch", originURL, "derive checkpoint remote URL", err)
		return originURL, nil
	}

	return checkpointURL, nil
}

// PushURL returns the effective checkpoint push URL for the current repository.
// Unlike FetchURL:
//   - it derives protocol from the requested push remote, not always origin
//   - it skips checkpoint remote use when the push remote owner differs
//     from the configured checkpoint remote owner
//
// If ENTIRE_CHECKPOINT_TOKEN is set, HTTPS is forced so the token can be used
// even when the push remote is configured via SSH.
//
// The boolean return value reports whether a dedicated checkpoint_remote is
// configured and should be used for push. When false, the returned URL is the
// repository's origin URL as a fallback.
func PushURL(ctx context.Context, pushRemoteName string) (string, bool, error) {
	originURL := ""
	if resolvedOriginURL, err := GetRemoteURL(ctx, originRemote); err == nil {
		originURL = resolvedOriginURL
	}

	s, err := settings.Load(ctx)
	if err != nil {
		fallbackURL, fallbackErr := resolvePushFallbackURL(ctx, pushRemoteName, originURL)
		if fallbackErr == nil {
			logFallback(ctx, "push", fallbackURL, "load settings", err,
				slog.String("push_remote", pushRemoteName),
			)
			return fallbackURL, false, nil
		}
		return "", false, fmt.Errorf("load settings: %w", err)
	}

	config := s.GetCheckpointRemote()
	if config == nil {
		fallbackURL, fallbackErr := resolvePushFallbackURL(ctx, pushRemoteName, originURL)
		if fallbackErr != nil {
			return "", false, fmt.Errorf("no push URL found: %w", fallbackErr)
		}
		return fallbackURL, false, nil
	}

	pushRemoteURL, err := GetRemoteURL(ctx, pushRemoteName)
	if err != nil {
		fallbackURL, fallbackErr := resolvePushFallbackURL(ctx, pushRemoteName, originURL)
		if fallbackErr == nil {
			logFallback(ctx, "push", fallbackURL, "get push remote URL", err,
				slog.String("push_remote", pushRemoteName),
			)
			return fallbackURL, false, nil
		}
		return "", true, fmt.Errorf("no push URL found: %w", err)
	}

	pushInfo, err := ParseURL(pushRemoteURL)
	if err != nil {
		if originURL != "" {
			logFallback(ctx, "push", originURL, "parse push remote URL", err,
				slog.String("push_remote", pushRemoteName),
			)
			return originURL, false, nil
		}
		return "", true, fmt.Errorf("no push URL found: %w", err)
	}
	if strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)) != "" && isDerivableProtocol(pushInfo.Protocol) {
		// Coerce a derivable (ssh/https) remote to HTTPS so the token applies,
		// keeping the host so enterprise installations stay on their own host.
		// A non-derivable protocol (e.g. entire://) carries a host that isn't a
		// usable HTTPS host, so it's left untouched and falls through to the
		// providerCheckpointURL fallback below.
		//
		// Keep the port only when the source was already HTTPS. SSH ports
		// (e.g., :2222) don't map to HTTPS ports on the same host.
		port := ""
		if pushInfo.Protocol == ProtocolHTTPS {
			port = pushInfo.Port
		}
		pushInfo = &Info{
			Protocol: ProtocolHTTPS,
			Host:     pushInfo.Host,
			Port:     port,
			Owner:    pushInfo.Owner,
			Repo:     pushInfo.Repo,
		}
	}

	checkpointOwner := config.Owner()
	if pushInfo.Owner != "" && checkpointOwner != "" && !strings.EqualFold(pushInfo.Owner, checkpointOwner) {
		fallbackURL, fallbackErr := resolvePushFallbackURL(ctx, pushRemoteName, originURL)
		if fallbackErr != nil {
			return "", false, fmt.Errorf("no push URL found: %w", fallbackErr)
		}
		return fallbackURL, false, nil
	}

	pushURL, err := deriveCheckpointURLFromInfo(pushInfo, config)
	if err != nil {
		// The push remote's protocol can't be mapped to a git transport
		// (e.g. entire://, file://). Honor the configured checkpoint_remote by
		// targeting the provider's canonical host over HTTPS rather than
		// misrouting checkpoints to the origin remote.
		if providerURL, ok := resolveProviderCheckpointURL(config, pushRemoteName, ""); ok {
			return providerURL, true, nil
		}
		fallbackURL, fallbackErr := resolvePushFallbackURL(ctx, pushRemoteName, originURL)
		if fallbackErr == nil {
			logFallback(ctx, "push", fallbackURL, "derive push checkpoint URL", err,
				slog.String("push_remote", pushRemoteName),
			)
			return fallbackURL, false, nil
		}
		return "", true, fmt.Errorf("no push URL found: %w", err)
	}

	return pushURL, true, nil
}

// Configured reports whether a structured checkpoint_remote is configured.
func Configured(ctx context.Context) bool {
	s, err := settings.Load(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint remote configuration unavailable; treating as not configured",
			slog.String("error", err.Error()),
		)
		return false
	}
	return s.GetCheckpointRemote() != nil
}

// GetRemoteURL returns the URL configured for the named git remote.
func GetRemoteURL(ctx context.Context, remoteName string) (string, error) {
	url, err := gitremote.GetRemoteURL(ctx, remoteName)
	if err != nil {
		return "", fmt.Errorf("get remote URL: %w", err)
	}
	return url, nil
}

// GetRemoteURLInDir returns the URL configured for the named git remote in dir.
func GetRemoteURLInDir(ctx context.Context, dir, remoteName string) (string, error) {
	url, err := gitremote.GetRemoteURLInDir(ctx, dir, remoteName)
	if err != nil {
		return "", fmt.Errorf("get remote URL: %w", err)
	}
	return url, nil
}

// ParseURL parses a git remote URL (SSH SCP-style or HTTPS) into its components.
func ParseURL(rawURL string) (*Info, error) {
	info, err := gitremote.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	return info, nil
}

func DeriveCheckpointURL(pushRemoteURL string, config *settings.CheckpointRemoteConfig) (string, error) {
	info, err := gitremote.ParseURL(pushRemoteURL)
	if err != nil {
		return "", fmt.Errorf("cannot parse push remote URL: %w", err)
	}
	return deriveCheckpointURLFromInfo(info, config)
}

// isDerivableProtocol reports whether deriveCheckpointURLFromInfo can map the
// protocol to a checkpoint URL (i.e. it's a real git transport, not a remote
// helper scheme like entire:// or a local file://).
func isDerivableProtocol(protocol string) bool {
	return protocol == ProtocolSSH || protocol == ProtocolHTTPS
}

func deriveCheckpointURLFromInfo(info *Info, config *settings.CheckpointRemoteConfig) (string, error) {
	switch info.Protocol {
	case ProtocolSSH:
		// SCP-style (git@host:repo) doesn't support ports. When a non-default
		// port is set (e.g., from ssh://git@host:2222/...), use the ssh:// URL form.
		if info.Port != "" {
			return fmt.Sprintf("ssh://git@%s/%s.git", info.HostPort(), config.Repo), nil
		}
		return fmt.Sprintf("git@%s:%s.git", info.Host, config.Repo), nil
	case ProtocolHTTPS:
		return fmt.Sprintf("https://%s/%s.git", info.HostPort(), config.Repo), nil
	default:
		return "", fmt.Errorf("unsupported protocol %q in origin remote", info.Protocol)
	}
}

// originalURLConfigKey is the git config option, under remote.<name>., where
// entiredb's `entire-repo mirror use` records the URL a remote had before it was
// switched to entire://. It is the most faithful record of the endpoint and auth
// method the user had for that remote.
const originalURLConfigKey = "entiredb-original-url"

// resolveProviderCheckpointURL builds the checkpoint URL for the configured
// provider, choosing the transport from what's already configured for that
// endpoint. It is the fallback used when the push/origin remote's protocol can't
// be mapped to a git transport (e.g. entire://, file://): the configured
// checkpoint_remote names a concrete provider, so checkpoints go there rather
// than being misrouted to the origin remote.
//
// Transport precedence:
//  1. The remote's pre-mirror URL (remote.<name>.entiredb-original-url) — the
//     endpoint and scheme the remote used before `entire-repo mirror use`
//     switched it to entire://. Reused verbatim (host + scheme + port).
//  2. ENTIRE_CHECKPOINT_TOKEN set -> HTTPS on the provider host (the token is
//     the credential).
//  3. An existing remote already targets the provider host -> reuse its scheme,
//     so checkpoints use the same auth the user already has for that endpoint.
//  4. Otherwise SSH on the provider host.
//
// Returns ok=false when no transport can be determined (unknown provider with no
// usable signal), in which case the caller falls back to the origin remote.
func resolveProviderCheckpointURL(config *settings.CheckpointRemoteConfig, remoteName, dir string) (string, bool) {
	repo, err := openRepoAt(dir)
	if err != nil {
		repo = nil // Fall back to env/provider-only signals.
	}

	info, ok := pickProviderTransport(repo, config, remoteName)
	if !ok {
		return "", false
	}
	url, err := deriveCheckpointURLFromInfo(info, config)
	if err != nil {
		return "", false
	}
	return url, true
}

// pickProviderTransport returns the protocol/host/port to use when deriving a
// checkpoint URL, following the precedence documented on
// resolveProviderCheckpointURL.
func pickProviderTransport(repo *git.Repository, config *settings.CheckpointRemoteConfig, remoteName string) (*Info, bool) {
	// 1. The remote's saved pre-mirror URL: the endpoint and auth the user had.
	if repo != nil {
		if original := originalRemoteURL(repo, remoteName); original != "" {
			if info, err := gitremote.ParseURL(original); err == nil && isDerivableProtocol(info.Protocol) {
				return info, true
			}
		}
	}

	host, hostOK := providerHost(config.Provider)

	// 2. Explicit token -> HTTPS on the provider host.
	if hostOK && strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)) != "" {
		return &Info{Protocol: ProtocolHTTPS, Host: host}, true
	}

	// 3. An existing remote already targeting the provider host -> reuse scheme.
	if hostOK && repo != nil {
		if info, ok := findRemoteInfoForHost(repo, host); ok {
			return &Info{Protocol: info.Protocol, Host: info.Host, Port: info.Port}, true
		}
	}

	// 4. Default to SSH on the provider host.
	if hostOK {
		return &Info{Protocol: ProtocolSSH, Host: host}, true
	}

	return nil, false
}

// openRepoAt opens the git repository at dir (current directory when dir is
// empty), walking up to the enclosing .git directory.
func openRepoAt(dir string) (*git.Repository, error) {
	if dir == "" {
		dir = "."
	}
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, fmt.Errorf("open git repository: %w", err)
	}
	return repo, nil
}

// originalRemoteURL returns the pre-mirror URL saved by `entire-repo mirror use`
// in remote.<name>.entiredb-original-url, or "" when absent.
func originalRemoteURL(repo *git.Repository, remoteName string) string {
	cfg, err := repo.Config()
	if err != nil {
		return ""
	}
	return cfg.Raw.Section("remote").Subsection(remoteName).Option(originalURLConfigKey)
}

// findRemoteInfoForHost returns the parsed Info of the first configured git
// remote (in deterministic name order) whose host matches host and whose
// protocol is a usable git transport (ssh/https). entire:// and other
// non-transport remotes are ignored.
func findRemoteInfoForHost(repo *git.Repository, host string) (*Info, bool) {
	cfg, err := repo.Config()
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(cfg.Remotes))
	for name := range cfg.Remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, rawURL := range cfg.Remotes[name].URLs {
			info, err := gitremote.ParseURL(rawURL)
			if err != nil {
				continue
			}
			if strings.EqualFold(info.Host, host) && isDerivableProtocol(info.Protocol) {
				return info, true
			}
		}
	}
	return nil, false
}

func deriveTokenOriginURL(originURL string) (string, bool) {
	info, err := gitremote.ParseURL(originURL)
	if err != nil {
		return "", false
	}
	if info.Host == "" || info.Owner == "" || info.Repo == "" {
		return "", false
	}
	// Keep the port only when the source was already HTTPS. SSH ports
	// (e.g., :2222) don't map to HTTPS ports on the same host.
	hostPort := info.Host
	if info.Protocol == ProtocolHTTPS {
		hostPort = info.HostPort()
	}
	return fmt.Sprintf("https://%s/%s/%s.git", hostPort, info.Owner, info.Repo), true
}

func providerHost(provider string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "github":
		return "github.com", true
	case "gitlab":
		return "gitlab.com", true
	default:
		return "", false
	}
}

// RedactURL removes credentials and query parameters from a URL for safe logging.
func RedactURL(rawURL string) string {
	return gitremote.RedactURL(rawURL)
}

func logFallback(ctx context.Context, operation, fallbackURL, reason string, err error, attrs ...any) {
	logAttrs := []any{
		slog.String("operation", operation),
		slog.String("fallback_url", RedactURL(fallbackURL)),
		slog.String("reason", reason),
		slog.String("error", err.Error()),
	}
	logAttrs = append(logAttrs, attrs...)
	logging.Warn(ctx, "checkpoint remote URL resolution fell back to alternate remote URL", logAttrs...)
}

func resolvePushFallbackURL(ctx context.Context, pushRemoteName, originURL string) (string, error) {
	if originURL != "" {
		if strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)) != "" {
			if tokenURL, ok := deriveTokenOriginURL(originURL); ok {
				return tokenURL, nil
			}
		}
		return originURL, nil
	}
	if pushRemoteName == "" {
		return "", fmt.Errorf("no push remote specified and remote %q not found", originRemote)
	}
	if pushRemoteName == originRemote {
		return "", fmt.Errorf("remote %q not found", originRemote)
	}
	pushURL, err := GetRemoteURL(ctx, pushRemoteName)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)) != "" {
		if tokenURL, ok := deriveTokenOriginURL(pushURL); ok {
			return tokenURL, nil
		}
	}
	return pushURL, nil
}
