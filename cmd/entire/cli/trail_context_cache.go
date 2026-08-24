package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/validation"

	"github.com/spf13/cobra"
)

const (
	trailEnablementCacheTTL                   = time.Hour
	agentHelpTrailsRefreshFailureBackoff      = 5 * time.Minute
	trailEnablementSessionStartRefreshTimeout = time.Second
	// trailEnablementRefreshTimeout bounds a full enablement refresh. Since
	// the probe moved onto the repo's own cell it covers ~4 sequential round
	// trips (repos index, cluster catalog, identity-token exchange,
	// TrailsEnabled), so requiredCellResolveTimeout's 15s inner budget is
	// inert underneath it — this is the effective bound.
	//
	// Kept at 3s rather than grown to match: expiry is soft everywhere it
	// applies (agent-help falls back to agentHelpTrailsRefreshFailureBackoff
	// and reports trails unavailable; the detached SessionStart child just
	// leaves the cache unknown for the next turn), whereas raising it makes
	// every offline invocation wait longer for the same answer.
	trailEnablementRefreshTimeout = 3 * time.Second
)

type trailEnablementCacheStatus int

const (
	trailEnablementCacheUnknown trailEnablementCacheStatus = iota
	trailEnablementCacheEnabled
	trailEnablementCacheDisabled
)

type trailEnablementScope struct {
	Forge     string `json:"forge"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	RepoKey   string `json:"repo_key"`
	APIBase   string `json:"api_base"`
	AuthKey   string `json:"auth_key"`
	Supported bool   `json:"supported"`
}

func cachedTrailsEnablementForScope(ctx context.Context, scope trailEnablementScope, now time.Time) trailEnablementCacheStatus {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil || prefs.TrailsEnabled == nil || prefs.TrailsEnabledCheckedAt == nil {
		return trailEnablementCacheUnknown
	}
	if !trailEnablementCacheMatchesScope(prefs, scope) || trailEnablementCacheExpired(*prefs.TrailsEnabledCheckedAt, now) {
		return trailEnablementCacheUnknown
	}
	if *prefs.TrailsEnabled {
		return trailEnablementCacheEnabled
	}
	return trailEnablementCacheDisabled
}

func trailEnablementCacheMatchesScope(prefs *settings.ClonePreferences, scope trailEnablementScope) bool {
	return prefs.TrailsEnabledRepoKey == scope.RepoKey &&
		prefs.TrailsEnabledAPIBase == scope.APIBase &&
		prefs.TrailsEnabledAuthKey == scope.AuthKey
}

func trailEnablementCacheExpired(checkedAt time.Time, now time.Time) bool {
	if checkedAt.IsZero() {
		return true
	}
	if now.Before(checkedAt) {
		return true
	}
	return now.Sub(checkedAt) > trailEnablementCacheTTL
}

func currentTrailEnablementScope(ctx context.Context) (trailEnablementScope, error) {
	rawURL, err := gitremote.GetRemoteURL(ctx, "origin")
	if err != nil {
		return trailEnablementScope{}, fmt.Errorf("get origin remote: %w", err)
	}
	if strings.TrimSpace(rawURL) == "" {
		return trailEnablementScope{}, errors.New("get origin remote: empty URL")
	}
	info, err := gitremote.ParseURL(rawURL)
	if err != nil {
		return trailEnablementScope{}, fmt.Errorf("parse origin remote: %w", err)
	}
	authKey, err := auth.LocalIdentityCacheKey()
	if err != nil {
		return trailEnablementScope{}, fmt.Errorf("resolve auth cache key: %w", err)
	}
	return trailEnablementScope{
		Forge:     info.Forge,
		Owner:     info.Owner,
		Repo:      info.Repo,
		RepoKey:   trailEnablementRepoKey(info.Forge, info.Owner, info.Repo),
		APIBase:   api.BaseURL(),
		AuthKey:   authKey,
		Supported: info.Forge != "",
	}, nil
}

func trailEnablementRepoKey(forge, owner, repo string) string {
	return strings.Join([]string{forge, owner, repo}, "/")
}

func saveTrailEnablementScopeHint(ctx context.Context, sessionID string, scope trailEnablementScope) error {
	path, err := trailEnablementScopeHintPath(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create session state dir: %w", err)
	}
	data, err := jsonutil.MarshalIndentWithNewline(scope, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trail scope hint: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write trail scope hint: %w", err)
	}
	return nil
}

func loadTrailEnablementScopeHint(ctx context.Context, sessionID string) (trailEnablementScope, bool, error) {
	path, err := trailEnablementScopeHintPath(ctx, sessionID)
	if err != nil {
		return trailEnablementScope{}, false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from validated session ID
	if err != nil {
		if os.IsNotExist(err) {
			return trailEnablementScope{}, false, nil
		}
		return trailEnablementScope{}, false, fmt.Errorf("read trail scope hint: %w", err)
	}
	var scope trailEnablementScope
	if err := json.Unmarshal(data, &scope); err != nil {
		return trailEnablementScope{}, false, fmt.Errorf("parse trail scope hint: %w", err)
	}
	return scope, true, nil
}

func trailEnablementScopeHintPath(ctx context.Context, sessionID string) (string, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID: %w", err)
	}
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Join(commonDir, session.SessionStateDirName, sessionID+".trail-scope.json"), nil
}

func saveTrailsEnabledForRepo(ctx context.Context, enabled bool) error {
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil {
		return err
	}
	return saveTrailsEnabledForScope(ctx, scope, enabled, time.Now())
}

func saveTrailsEnabledForRemote(ctx context.Context, forge, owner, repo string, enabled bool) error {
	authKey, err := auth.LocalIdentityCacheKey()
	if err != nil {
		return fmt.Errorf("resolve auth cache key: %w", err)
	}
	scope := trailEnablementScope{
		Forge:     forge,
		Owner:     owner,
		Repo:      repo,
		RepoKey:   trailEnablementRepoKey(forge, owner, repo),
		APIBase:   api.BaseURL(),
		AuthKey:   authKey,
		Supported: forge != "",
	}
	return saveTrailsEnabledForScope(ctx, scope, enabled, time.Now())
}

// saveTrailsEnabledForScope is the single writer behind every trails-enablement
// cache save (saveTrailsEnabledForRepo/ForRemote both funnel here), which is why
// the outlives-the-deadline guarantee lives at this one point rather than at
// each call site.
//
// The write is not purely local: ClonePreferencesPath resolves the git common
// dir with `git rev-parse` under the passed ctx (session.getGitCommonDir). So a
// refresh that answers the question at 2.9s of a 3s budget could then fail to
// STORE the answer, leaving the cache "unknown" — precisely the state the
// callers' branches exist to escape, and the one that makes SessionStart
// re-fork a refresh child on every invocation. Losing the answer is strictly
// worse than spending a few extra milliseconds past the deadline to keep it.
func saveTrailsEnabledForScope(ctx context.Context, scope trailEnablementScope, enabled bool, checkedAt time.Time) error {
	ctx = context.WithoutCancel(ctx)
	enabledCopy := enabled
	checkedAtUTC := checkedAt.UTC()
	if err := settings.ModifyClonePreferences(ctx, func(prefs *settings.ClonePreferences) error {
		prefs.TrailsEnabled = &enabledCopy
		prefs.TrailsEnabledCheckedAt = &checkedAtUTC
		prefs.TrailsEnabledRepoKey = scope.RepoKey
		prefs.TrailsEnabledAPIBase = scope.APIBase
		prefs.TrailsEnabledAuthKey = scope.AuthKey
		return nil
	}); err != nil {
		return fmt.Errorf("save clone preferences: %w", err)
	}
	return nil
}

// recentAgentHelpTrailsRefreshFailure reports whether agent-help should back off
// after a failed availability refresh for this exact repo/API/auth scope. This
// marker is deliberately separate from TrailsEnabled: lifecycle SessionStart
// must still perform its authoritative probe and decide context injection.
func recentAgentHelpTrailsRefreshFailure(ctx context.Context, scope trailEnablementScope, now time.Time) bool {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil || prefs.TrailsAgentHelpRefreshFailedAt == nil {
		return false
	}
	if prefs.TrailsAgentHelpFailureRepoKey != scope.RepoKey ||
		prefs.TrailsAgentHelpFailureAPIBase != scope.APIBase ||
		prefs.TrailsAgentHelpFailureAuthKey != scope.AuthKey {
		return false
	}
	failedAt := *prefs.TrailsAgentHelpRefreshFailedAt
	if failedAt.IsZero() || now.Before(failedAt) {
		return false
	}
	return now.Sub(failedAt) <= agentHelpTrailsRefreshFailureBackoff
}

func saveAgentHelpTrailsRefreshFailure(ctx context.Context, scope trailEnablementScope, failedAt time.Time) error {
	failedAtUTC := failedAt.UTC()
	if err := settings.ModifyClonePreferences(ctx, func(prefs *settings.ClonePreferences) error {
		prefs.TrailsAgentHelpRefreshFailedAt = &failedAtUTC
		prefs.TrailsAgentHelpFailureRepoKey = scope.RepoKey
		prefs.TrailsAgentHelpFailureAPIBase = scope.APIBase
		prefs.TrailsAgentHelpFailureAuthKey = scope.AuthKey
		return nil
	}); err != nil {
		return fmt.Errorf("save clone preferences: %w", err)
	}
	return nil
}

// refreshTrailsEnabledCacheIfStaleForScope refreshes the trails-enablement
// cache when it's unknown/expired for scope. Callers on hot, latency-sensitive
// paths (SessionStart) must not block on this: resolving the API token and
// dialing TrailsEnabled can stall for seconds when the host is slow or
// unreachable (VPN, firewall, offline). Instead of doing that
// network work inline, hand it off to a detached `__refresh_change_enablement`
// subprocess and return immediately; a later SessionStart will observe the
// freshly written cache once the subprocess completes. The "not supported"
// case is answered locally (no network) since it's free.
func refreshTrailsEnabledCacheIfStaleForScope(ctx context.Context, scope trailEnablementScope) error {
	if cachedTrailsEnablementForScope(ctx, scope, time.Now()) != trailEnablementCacheUnknown {
		return nil
	}
	if !scope.Supported {
		return saveTrailsEnabledForScope(ctx, scope, false, time.Now())
	}
	spawnDetachedTrailEnablementRefresh(ctx)
	return nil
}

// trailRefreshAPIClient is the authenticated-client seam used by
// runTrailEnablementRefresh, refreshAgentHelpTrailsEnabledCacheIfStaleForScope,
// and probeAndCacheTrailsEnablement, swapped in tests so they can force the
// refreshTrailsEnabledCacheForScope error branch (e.g. a broken API host)
// without a real login context. Production code always uses the repo-routed
// entire-api cell client (newChangeAPIClient) rather than the generic
// data-API/BFF client: the BFF does not proxy /api/v1/changes/... for bearer
// callers (COR-666), so probing it via the generic client silently misreads a
// supported repo as disabled.
var trailRefreshAPIClient = func(ctx context.Context, insecureHTTP bool, fullName string) (*api.Client, error) {
	return newChangeAPIClient(ctx, insecureHTTP, fullName)
}

// trailsCellClient resolves the entire-api cell client for ownerRepo via
// trailRefreshAPIClient, classifying the one failure every caller must treat
// specially instead of leaving each to re-derive it.
//
// notOnboarded reports errRepoNotOnboarded (cell_target.go): a DEFINITIVE, not
// transient, negative — the repo has no processing placement — which callers
// must PERSIST rather than treat as an ordinary client-construction failure,
// because leaving the cache unknown means every future refresh re-attempts
// (and re-fails) the same client build forever.
//
// The save itself stays with the caller because its key differs (scope- vs
// remote-keyed), and err always describes the client build — never a save — so
// a caller can log it without having to know which of the two it got. When
// notOnboarded is true, err is the sentinel-wrapping error, kept for logging;
// callers must act on notOnboarded, not propagate err.
func trailsCellClient(ctx context.Context, insecureHTTP bool, ownerRepo string) (client *api.Client, notOnboarded bool, err error) {
	client, err = trailRefreshAPIClient(ctx, insecureHTTP, ownerRepo)
	switch {
	case err == nil:
		return client, false, nil
	case errors.Is(err, errRepoNotOnboarded):
		return nil, true, err
	default:
		return nil, false, err
	}
}

// runTrailEnablementRefresh performs the actual (potentially slow) network
// refresh. It is invoked from the detached `__refresh_change_enablement`
// subprocess spawned by refreshTrailsEnabledCacheIfStaleForScope, never
// synchronously from a hook path.
func runTrailEnablementRefresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, trailEnablementRefreshTimeout)
	defer cancel()

	// This runs detached with stdout/stderr discarded, so log at debug to the
	// repo's .entire/logs/entire.log (initialized by newRefreshTrailEnablementCmd).
	// Without this, an unreachable/failing host would leave the background
	// refresh silently failing with no diagnostic trail.
	logCtx := logging.WithComponent(ctx, "trail-refresh")

	scope, err := currentTrailEnablementScope(ctx)
	if err != nil {
		logging.Debug(logCtx, "trails enablement refresh skipped: scope unresolved", "error", err.Error())
		return nil
	}
	// Another process (e.g. a fast-following SessionStart, or a concurrent
	// refresh already in flight) may have populated the cache first.
	if cachedTrailsEnablementForScope(ctx, scope, time.Now()) != trailEnablementCacheUnknown {
		return nil
	}
	if !scope.Supported {
		if err := saveTrailsEnabledForScope(ctx, scope, false, time.Now()); err != nil {
			logging.Debug(logCtx, "trails enablement refresh failed to save unsupported scope", "error", err.Error())
		}
		return nil
	}
	client, notOnboarded, err := trailsCellClient(ctx, false, scope.Owner+"/"+scope.Repo)
	if notOnboarded {
		// A definitive, permanent negative: without this, every SessionStart
		// re-forks a refresh child for this repo forever (see
		// trailRefreshSpawnThrottle above), because the cache is never
		// written and so never leaves "unknown".
		if saveErr := saveTrailsEnabledForScope(ctx, scope, false, time.Now()); saveErr != nil {
			logging.Debug(logCtx, "trails enablement refresh failed to save not-onboarded scope", "error", saveErr.Error())
		}
		return nil
	}
	if err != nil {
		logging.Debug(logCtx, "trails enablement refresh skipped: authenticated client unavailable", "error", err.Error())
		return nil
	}
	// Best-effort: this runs from the detached __refresh_change_enablement
	// subprocess (stdout/stderr discarded, see newRefreshTrailEnablementCmd),
	// so a transient network/API failure here must not surface as a non-zero
	// process exit — there's no one watching it and no user-visible benefit,
	// only a spurious failure signal. The failure is still diagnosable via the
	// debug log above.
	if _, err := refreshTrailsEnabledCacheForScope(ctx, client, scope); err != nil {
		logging.Debug(logCtx, "trails enablement refresh failed", "error", err.Error())
		return nil
	}
	logging.Debug(logCtx, "trails enablement refresh completed", "enabled_repo_key", scope.RepoKey)
	return nil
}

// trailRefreshSpawn is the process-spawn seam used by
// spawnDetachedTrailEnablementRefresh. Swapped in tests so they can assert
// SessionStart never blocks on it without forking a real subprocess (a real
// `go test` binary doesn't understand `__refresh_change_enablement` as an
// argument). Production code always uses spawnDetachedTrailRefreshProcess.
var trailRefreshSpawn = spawnDetachedTrailRefreshProcess

// spawnDetachedTrailRefreshProcess starts `entire __refresh_change_enablement`
// as a detached child so the trails-enablement network refresh can't add
// latency to the SessionStart hook that spawned it. The child runs from the
// worktree root because the refresh resolves the origin remote and
// git-common-dir for cache storage from its working directory.
func spawnDetachedTrailRefreshProcess(worktreeRoot string) {
	execx.SpawnDetached(worktreeRoot, "__refresh_change_enablement")
}

// trailRefreshSpawnThrottle bounds how often SessionStart forks a detached
// refresh child for a given repo. When the API host is unreachable the refresh
// never writes the cache, so cachedTrailsEnablementForScope stays unknown and
// the hourly TTL never starts — without this guard every SessionStart (and
// every concurrent worktree) would fork a fresh child that re-opens the repo,
// re-resolves auth, and re-dials the dead host. Tying the window to the child's
// own timeout collapses a burst of hooks to roughly one child per window while
// still retrying promptly once the host recovers.
const trailRefreshSpawnThrottle = trailEnablementRefreshTimeout

// spawnDetachedTrailEnablementRefresh starts a detached child process that
// runs runTrailEnablementRefresh in the background. Best-effort: if the
// worktree root can't be resolved or the subprocess can't be spawned, the
// cache simply stays unknown and the next SessionStart tries again. A recent
// spawn for the same repo short-circuits so a burst of hooks doesn't fork a
// herd of redundant refresh children (see trailRefreshRecentlySpawned).
func spawnDetachedTrailEnablementRefresh(ctx context.Context) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	if commonDir, err := session.GetGitCommonDir(ctx); err == nil &&
		trailRefreshRecentlySpawned(commonDir, time.Now()) {
		return
	}
	trailRefreshSpawn(worktreeRoot)
}

// trailRefreshRecentlySpawned reports whether a detached refresh was spawned for
// this repo within trailRefreshSpawnThrottle and, when it wasn't, records now as
// the most recent spawn. The read-and-record is serialized with a flock keyed to
// the shared git-common-dir (so every worktree of the repo agrees), collapsing a
// burst of concurrent SessionStart hooks to a single child rather than one per
// hook. Best-effort: any error resolving, locking, or writing the marker falls
// through to spawning — never worse than before this guard existed.
func trailRefreshRecentlySpawned(commonDir string, now time.Time) bool {
	return recentlySpawnedMarker(commonDir, "trail-refresh-spawn", trailRefreshSpawnThrottle, now)
}

// recentlySpawnedMarker reports whether the named spawn marker under the
// shared git-common-dir was refreshed within ttl and, when it wasn't, records
// now as the most recent spawn. The read-and-record is serialized with a flock
// keyed to the marker (so every worktree of the repo agrees), collapsing a
// burst of concurrent hooks to a single detached child rather than one per
// hook. Best-effort: any error resolving, locking, or writing the marker falls
// through to spawning — never worse than having no guard at all. Shared by the
// trail-enablement refresh and the zombie-session sweep, each with its own
// marker name and ttl.
func recentlySpawnedMarker(commonDir, marker string, ttl time.Duration, now time.Time) bool {
	dir := filepath.Join(commonDir, "entire")
	// Create the directory before acquiring the lock: flock.Acquire opens the
	// lock file, which fails if its parent doesn't exist yet (mirrors
	// ModifyClonePreferences, which MkdirAlls before locking).
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return false
	}
	markerPath := filepath.Join(dir, marker)
	release, err := flock.Acquire(markerPath + ".lock")
	if err != nil {
		return false
	}
	defer release()

	if data, readErr := os.ReadFile(markerPath); readErr == nil { //nolint:gosec // markerPath is derived from the trusted git-common-dir, not user input
		if last, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); parseErr == nil &&
			now.After(last) && now.Sub(last) < ttl {
			return true
		}
	}
	//nolint:errcheck // best-effort marker; a failed write just means the next hook re-spawns
	_ = os.WriteFile(markerPath, []byte(now.UTC().Format(time.RFC3339Nano)), 0o600)
	return false
}

// newRefreshTrailEnablementCmd creates the hidden command that performs the
// (potentially slow) trails-enablement network refresh out of band. It is
// invoked by spawnDetachedTrailEnablementRefresh from a detached subprocess
// and should not be called directly.
func newRefreshTrailEnablementCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__refresh_change_enablement",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Detached child with discarded stdout/stderr: the root
			// PersistentPreRun has already routed logging into
			// .entire/logs/entire.log, so a failing background refresh (e.g.
			// an unreachable host) is diagnosable there rather than vanishing.
			ctx := cmd.Context()
			return runTrailEnablementRefresh(ctx)
		},
	}
}

func refreshTrailsEnabledCache(ctx context.Context, client *api.Client) (bool, error) {
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil {
		return false, err
	}
	return refreshTrailsEnabledCacheForScope(ctx, client, scope)
}

func refreshTrailsEnabledCacheForScope(ctx context.Context, client *api.Client, scope trailEnablementScope) (bool, error) {
	if !scope.Supported {
		if err := saveTrailsEnabledForScope(ctx, scope, false, time.Now()); err != nil {
			return false, err
		}
		return false, nil
	}
	enabled, err := client.TrailsEnabled(ctx, scope.Forge, scope.Owner, scope.Repo)
	if err != nil {
		return false, fmt.Errorf("check trails enablement: %w", err)
	}
	if err := saveTrailsEnabledForScope(ctx, scope, enabled, time.Now()); err != nil {
		return false, err
	}
	return enabled, nil
}

func saveTrailsEnabledForRepoBestEffort(ctx context.Context, enabled bool) {
	if err := saveTrailsEnabledForRepo(ctx, enabled); err != nil {
		logging.Debug(ctx, "failed to cache trails enablement", "error", err)
	}
}

func saveTrailsEnabledForRemoteBestEffort(ctx context.Context, forge, owner, repo string, enabled bool) {
	if err := saveTrailsEnabledForRemote(ctx, forge, owner, repo, enabled); err != nil {
		logging.Debug(ctx, "failed to cache trails enablement", "error", err)
	}
}

func refreshTrailsEnabledCacheBestEffort(ctx context.Context, client *api.Client) {
	refreshCtx, cancel := context.WithTimeout(ctx, trailEnablementRefreshTimeout)
	defer cancel()
	if _, err := refreshTrailsEnabledCache(refreshCtx, client); err != nil {
		logging.Debug(ctx, "trails enablement refresh skipped", "error", err)
	}
}

func noteChangeCommandEnablement(ctx context.Context, client *api.Client, commandErr error) {
	if commandErr == nil {
		saveTrailsEnabledForRepoBestEffort(ctx, true)
		return
	}
	refreshTrailsEnabledCacheBestEffort(ctx, client)
}

// runAuthenticatedChangeAPI runs fn against the entire-api cell that owns the
// target repository. repoOverride is the raw --repo flag: when non-empty the
// local clone's enablement cache is skipped because the result belongs to a
// different repository.
func runAuthenticatedChangeAPI(ctx context.Context, errW io.Writer, insecureHTTP bool, repoOverride string, fn func(context.Context, *api.Client) error) error {
	_, owner, repo, err := resolveChangeRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return err
	}
	client, err := newChangeAPIClient(ctx, insecureHTTP, owner+"/"+repo)
	if err != nil {
		return renderDataAPIAuthError(ctx, errW, owner+"/"+repo, err)
	}
	err = fn(ctx, client)
	if repoOverride == "" {
		noteChangeCommandEnablement(ctx, client, err)
	}
	return err
}
