package remote

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const (
	originRemote          = "origin"
	checkpointTokenEnvVar = "ENTIRE_CHECKPOINT_TOKEN"
)

const (
	ProtocolSSH   = gitremote.ProtocolSSH
	ProtocolHTTPS = gitremote.ProtocolHTTPS
)

// Info is an alias for gitremote.Info.
type Info = gitremote.Info

// FetchURL returns the effective checkpoint fetch URL for the current repository.
// If strategy_options.checkpoint_remote is configured, the returned URL is derived
// from the origin remote's protocol/host and the configured checkpoint repo.
// Otherwise, the origin remote URL is returned directly.
//
// If ENTIRE_CHECKPOINT_TOKEN is set and a checkpoint remote is configured, HTTPS is
// forced so the token can be used even when origin is configured via SSH.
func FetchURL(ctx context.Context) (string, error) {
	withToken := strings.TrimSpace(os.Getenv(checkpointTokenEnvVar)) != ""

	originURL, originErr := GetRemoteURL(ctx, originRemote)
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
	if strings.TrimSpace(os.Getenv(checkpointTokenEnvVar)) != "" {
		pushInfo = &Info{
			Protocol: ProtocolHTTPS,
			Host:     pushInfo.Host,
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

// ExtractOwnerFromRemoteURL extracts the owner component from a git remote URL.
func ExtractOwnerFromRemoteURL(rawURL string) string {
	return gitremote.ExtractOwnerFromRemoteURL(rawURL)
}

func deriveCheckpointURLFromInfo(info *Info, config *settings.CheckpointRemoteConfig) (string, error) {
	switch info.Protocol {
	case ProtocolSSH:
		return fmt.Sprintf("git@%s:%s.git", info.Host, config.Repo), nil
	case ProtocolHTTPS:
		return fmt.Sprintf("https://%s/%s.git", info.Host, config.Repo), nil
	default:
		return "", fmt.Errorf("unsupported protocol %q in origin remote", info.Protocol)
	}
}

func deriveTokenOriginURL(originURL string) (string, bool) {
	info, err := gitremote.ParseURL(originURL)
	if err != nil {
		return "", false
	}
	if info.Host == "" || info.Owner == "" || info.Repo == "" {
		return "", false
	}
	return fmt.Sprintf("https://%s/%s/%s.git", info.Host, info.Owner, info.Repo), true
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
		return originURL, nil
	}
	if pushRemoteName == "" || pushRemoteName == originRemote {
		return "", fmt.Errorf("remote %q not found", originRemote)
	}
	return GetRemoteURL(ctx, pushRemoteName)
}
