package remote

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
)

const originRemote = "origin"

const (
	ProtocolSSH    = gitremote.ProtocolSSH
	ProtocolHTTPS  = gitremote.ProtocolHTTPS
	ProtocolEntire = gitremote.ProtocolEntire
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
	url, _, err := fetchURLAuthoritative(ctx, opts...)
	return url, err
}

// fetchURLAuthoritative is FetchURL plus whether the returned URL is
// authoritative for checkpoint refs. It is false exactly when a
// checkpoint_remote IS configured (or cannot be determined) but resolution
// fell back to the origin URL — a remote that by construction does not host
// the configured checkpoint refs. Callers that classify "ref absent on the
// remote" (FetchCheckpointRef's ls-remote probe) must not treat emptiness on
// a non-authoritative target as absence.
func fetchURLAuthoritative(ctx context.Context, opts ...FetchURLOptions) (string, bool, error) {
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
			// Settings unreadable → checkpoint_remote unknown; conservative:
			// do not certify origin as authoritative for checkpoint refs.
			return originURL, false, nil
		}
		return "", false, fmt.Errorf("load settings: %w", err)
	}

	config := s.GetCheckpointRemote()
	if config == nil {
		if originURL == "" {
			return "", false, fmt.Errorf("no fetch URL found: %w", originErr)
		}
		// No checkpoint_remote configured: origin IS the checkpoint host.
		return originURL, true, nil
	}

	if withToken {
		host, ok := providerHost(config.Provider)
		if ok {
			checkpointURL, err := deriveCheckpointURLFromInfo(&Info{
				Protocol: ProtocolHTTPS,
				Host:     host,
			}, config)
			if err == nil {
				return checkpointURL, true, nil
			}
		}

		// In token-based execution path, short-circuit to avoid additional
		// change in protocol.
		if originURL != "" {
			return originURL, false, nil
		}
	}

	if originURL == "" {
		return "", false, fmt.Errorf("no fetch URL found: %w", originErr)
	}

	info, err := ParseURL(originURL)
	if err != nil {
		logFallback(ctx, "fetch", originURL, "parse origin remote URL", err)
		return originURL, false, nil
	}

	checkpointURL, err := deriveCheckpointURLFromInfo(info, config)
	if err != nil {
		// Origin's protocol can't be mapped to a checkpoint URL (e.g. file://,
		// or an entire:// mirror of a different forge than the configured
		// provider). Honor the configured checkpoint_remote by targeting the
		// provider's canonical host over HTTPS rather than falling back to origin.
		if providerURL, ok := resolveProviderCheckpointURL(ctx, config, opt.WorktreeRoot); ok {
			return providerURL, true, nil
		}
		logFallback(ctx, "fetch", originURL, "derive checkpoint remote URL", err)
		return originURL, false, nil
	}

	return checkpointURL, true, nil
}

// PushURL returns the effective checkpoint push URL for the current repository.
// Unlike FetchURL:
//   - it derives protocol from the requested push remote, not always origin
//   - it skips checkpoint remote use unless BOTH origin and the push
//     destination are owned by the configured checkpoint repo's owner
//     (see checkpointRemoteIsInherited); FetchURL applies no ownership check
//     at all, so a repo whose writes were diverted still reads from the
//     configured checkpoint repo
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

	pushRemoteURL, err := GetPushURL(ctx, pushRemoteName)
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

	// Whether to use the checkpoint remote at all is a question about ownership
	// ("is this checkpoint_remote mine, or did I inherit it by cloning?"), which
	// both remotes get a vote on: origin says which project this is, the push
	// destination says which repo we are writing to. Either one owned by somebody
	// other than the checkpoint repo's owner means inherited. See
	// checkpointRemoteIsInherited.
	if inherited, reason := checkpointRemoteIsInherited(ctx, config, originURL, pushRemoteURL); inherited {
		fallbackURL, fallbackErr := resolvePushFallbackURL(ctx, pushRemoteName, originURL)
		if fallbackErr != nil {
			return "", false, fmt.Errorf("no push URL found: %w", fallbackErr)
		}
		logging.Warn(ctx, "checkpoint-remote: ignoring checkpoint_remote that appears to belong to another owner; pushing checkpoints to the push remote instead",
			slog.String("checkpoint_repo", config.Repo),
			slog.String("reason", reason),
			slog.String("hint", "if this checkpoint repo is yours, configure checkpoint_remote in .entire/settings.local.json"),
		)
		return fallbackURL, false, nil
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
	withToken := strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)) != ""
	if withToken && isDirectGitTransport(pushInfo.Protocol) {
		// Coerce a direct (ssh/https) remote to HTTPS so the token applies,
		// keeping the host so enterprise installations stay on their own host.
		// An entire:// remote carries a cluster host that isn't a usable HTTPS
		// host, so it's handled separately after the owner check below.
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

	if withToken && pushInfo.Protocol == ProtocolEntire {
		// The checkpoint token is an HTTPS credential for the provider host;
		// it can't ride through the entire:// helper (which does its own
		// auth). Route to the provider over HTTPS instead of the mirror.
		if providerURL, ok := resolveProviderCheckpointURL(ctx, config, ""); ok {
			return providerURL, true, nil
		}
	}

	pushURL, err := deriveCheckpointURLFromInfo(pushInfo, config)
	if err != nil {
		// The push remote's protocol can't be mapped to a checkpoint URL
		// (e.g. file://, or an entire:// mirror of a different forge than the
		// configured provider). Honor the configured checkpoint_remote by
		// targeting the provider's canonical host over HTTPS rather than
		// misrouting checkpoints to the origin remote.
		if providerURL, ok := resolveProviderCheckpointURL(ctx, config, ""); ok {
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

// GetPushURLs returns every URL a push to remoteName delivers to, in the order
// git will use them. See gitremote.GetPushURLs for why this differs from
// GetRemoteURL.
func GetPushURLs(ctx context.Context, remoteName string) ([]string, error) {
	urls, err := gitremote.GetPushURLs(ctx, remoteName)
	if err != nil {
		return nil, fmt.Errorf("get push URLs: %w", err)
	}
	return urls, nil
}

// GetPushURL returns the URL a push to remoteName delivers to. A remote's push
// destination is remote.<name>.pushurl when set and only otherwise its url, so
// reading the plain url (as GetRemoteURL does) can name a different repository
// than the one being pushed to.
//
// When several push URLs are configured this returns the FIRST, which is not an
// arbitrary pick: resolveRefsPushDestination sends checkpoint refs to exactly
// that URL, so the URL this derives transport and (origin-less) ownership from
// is the URL the checkpoints land in.
func GetPushURL(ctx context.Context, remoteName string) (string, error) {
	urls, err := GetPushURLs(ctx, remoteName)
	if err != nil {
		return "", err
	}
	return urls[0], nil
}

// checkpointRemoteIsInherited reports whether the configured checkpoint_remote
// looks like it belongs to an upstream project rather than to this developer,
// along with a short reason for logging.
//
// checkpoint_remote is normally committed in .entire/settings.json, so anyone who
// forks or clones the project inherits it. Honoring it blindly would push a
// contributor's session data into the upstream project's checkpoint repo — which
// they typically cannot write to and should not be writing to. This is the check
// that guards against that (added in e8b589835 as "fork detection").
//
// Ownership is decided from local signals only, no network:
//
//  1. A checkpoint_remote in .entire/settings.local.json is gitignored and
//     per-clone, so it cannot have been inherited — it is always ours.
//  2. Otherwise EVERY remote that identifies this repo must be owned by the
//     checkpoint repo's owner: origin (which project am I working in) and the
//     push destination (which repo am I actually writing to). A single
//     mismatch means inherited.
//
// Requiring both is what closes the "cloned the base repo, added my fork"
// topology. There are two ways to contribute to someone else's project:
//
//	fork first, clone the fork  -> origin <fork>/app          -> origin mismatches
//	clone the base, add a fork  -> origin <upstream>/app       -> origin MATCHES
//
// In the second, origin belongs to upstream rather than to us, so an
// origin-only check reads an inherited setting as ours and aims a
// contributor's transcripts at the upstream project's checkpoint repo. The push
// destination is the signal that disagrees: we are writing to <fork>/app.
//
// The cost, accepted deliberately: a repo of our own with a differently-owned
// secondary remote (a backup in another org) no longer routes checkpoints to
// our checkpoint repo on pushes to THAT remote — the two topologies are
// indistinguishable from local git config alone, since both are
// origin=<owner>/app plus a push destination owned by somebody else, and only
// the network could say which repo is ours. Pushes to the elected sync remote
// (normally origin) are unaffected, and ENT-1451's pre-push gate independently
// blocks checkpoint sync on pushes to a non-elected remote, so this costs
// nothing that the gate was not already stopping. Erring toward "not ours" is
// the safe direction: it never writes session transcripts to a repository we
// cannot confirm we own.
//
// On a remote with several push URLs only the FIRST is checked, matching the
// destination resolveRefsPushDestination sends checkpoint refs to. The
// git-branch backend still fans out to every push URL, so a multi-URL remote
// whose later URLs are owned by someone else is not covered here.
//
// An identity whose owner cannot be determined (no remote at all, or a
// non-forge URL such as a bare local path) counts as inherited: for a committed
// setting we cannot confirm ownership, and falling back preserves the previous
// behavior for those repos. settings.local.json is the escape hatch when the
// checkpoint repo is genuinely ours but owned by a different account or org.
func checkpointRemoteIsInherited(ctx context.Context, config *settings.CheckpointRemoteConfig, originURL, pushRemoteURL string) (bool, string) {
	checkpointOwner := config.Owner()
	if checkpointOwner == "" {
		// No owner to compare (malformed repo field). Matches the predecessor,
		// which skipped the check rather than blocking on it.
		return false, ""
	}
	if settings.CheckpointRemoteIsLocalOnly(ctx) {
		return false, ""
	}

	// Both identities when both exist. A repo with no origin has exactly one
	// identity — its push remote — and must not lose its configured checkpoint
	// remote just because nothing is named "origin".
	identities := make([]struct{ url, source string }, 0, 2)
	if originURL != "" {
		identities = append(identities, struct{ url, source string }{originURL, "origin"})
	}
	if pushRemoteURL != "" && pushRemoteURL != originURL {
		identities = append(identities, struct{ url, source string }{pushRemoteURL, "push remote"})
	}
	if len(identities) == 0 {
		return true, "no remote to establish ownership"
	}

	for _, id := range identities {
		info, err := ParseURL(id.url)
		if err != nil || info.Owner == "" {
			return true, id.source + " URL owner could not be determined"
		}
		if !strings.EqualFold(info.Owner, checkpointOwner) {
			return true, fmt.Sprintf("%s owner %q differs from checkpoint owner %q", id.source, info.Owner, checkpointOwner)
		}
	}
	return false, ""
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

// isDirectGitTransport reports whether the protocol talks to the git host
// directly over ssh/https (where the host is a usable HTTPS host for token
// auth), as opposed to a remote helper scheme like entire:// or a local
// file://.
func isDirectGitTransport(protocol string) bool {
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
	case ProtocolEntire:
		// entire:// push-through mirrors are cluster-scoped: keep the cluster
		// host and forge segment, swap in the checkpoint repo. Only derivable
		// when the forge maps back to the configured provider's host, so a
		// github checkpoint_remote never routes through another forge's mirror.
		host, ok := providerHost(config.Provider)
		if !ok || !strings.EqualFold(info.CanonicalHost(), host) {
			return "", fmt.Errorf("entire:// remote forge %q does not match checkpoint provider %q", info.Forge, config.Provider)
		}
		return fmt.Sprintf("entire://%s/%s/%s", info.HostPort(), info.Forge, config.Repo), nil
	default:
		return "", fmt.Errorf("unsupported protocol %q in remote URL", info.Protocol)
	}
}

// resolveProviderCheckpointURL builds the checkpoint URL for the configured
// provider, choosing the transport from what's already configured for that
// endpoint. It is the fallback used when the push/origin remote's protocol can't
// be mapped to a git transport (e.g. entire://, file://): the configured
// checkpoint_remote names a concrete provider, so checkpoints go there rather
// than being misrouted to the origin remote.
//
// Transport precedence:
//  1. ENTIRE_CHECKPOINT_TOKEN set -> HTTPS on the provider host (the token is
//     the credential).
//  2. An existing remote already targets the provider host -> reuse its scheme,
//     so checkpoints use the same auth the user already has for that endpoint.
//  3. Otherwise SSH on the provider host.
//
// Returns ok=false when no transport can be determined (unknown provider with no
// usable signal), in which case the caller falls back to the origin remote.
func resolveProviderCheckpointURL(ctx context.Context, config *settings.CheckpointRemoteConfig, dir string) (string, bool) {
	repo, err := openRepoAt(ctx, dir)
	if err != nil {
		repo = nil // Fall back to env/provider-only signals.
	}

	info, ok := pickProviderTransport(repo, config)
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
func pickProviderTransport(repo *git.Repository, config *settings.CheckpointRemoteConfig) (*Info, bool) {
	host, hostOK := providerHost(config.Provider)

	// 1. Explicit token -> HTTPS on the provider host.
	if hostOK && strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)) != "" {
		return &Info{Protocol: ProtocolHTTPS, Host: host}, true
	}

	// 2. An existing remote already targeting the provider host -> reuse scheme.
	if hostOK && repo != nil {
		if info, ok := findRemoteInfoForHost(repo, host); ok {
			return &Info{Protocol: info.Protocol, Host: info.Host, Port: info.Port}, true
		}
	}

	// 3. Default to SSH on the provider host.
	if hostOK {
		return &Info{Protocol: ProtocolSSH, Host: host}, true
	}

	return nil, false
}

// openRepoAt opens the git repository at dir (current directory when dir is
// empty). It routes through gitrepo, the single reftable-aware opener, so a
// reftable repository is opened via the git-CLI storer rather than rejected by
// go-git's extension check. gitrepo.OpenCurrent resolves the worktree root from
// the current directory (the walk-up equivalent of the previous DetectDotGit
// open) when no explicit root is given.
func openRepoAt(ctx context.Context, dir string) (*git.Repository, error) {
	if dir == "" {
		repo, err := gitrepo.OpenCurrent(ctx)
		if err != nil {
			return nil, fmt.Errorf("open git repository: %w", err)
		}
		return repo, nil
	}
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		return nil, fmt.Errorf("open git repository: %w", err)
	}
	return repo, nil
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
			if strings.EqualFold(info.Host, host) && isDirectGitTransport(info.Protocol) {
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

// RedactURLOrPath is RedactURL for values that may be a remote name or a local
// path rather than a URL. See gitremote.RedactURLOrPath.
func RedactURLOrPath(target string) string {
	return gitremote.RedactURLOrPath(target)
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
