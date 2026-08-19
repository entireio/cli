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
	trailEnablementRefreshTimeout             = 3 * time.Second
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

func saveTrailsEnabledForScope(ctx context.Context, scope trailEnablementScope, enabled bool, checkedAt time.Time) error {
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
// network work inline, hand it off to a detached `__refresh_trail_enablement`
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
// entire-api cell client (newTrailAPIClient) rather than the generic
// data-API/BFF client: the BFF does not proxy /api/v1/trails/... for bearer
// callers (COR-666), so probing it via the generic client silently misreads a
// supported repo as disabled.
var trailRefreshAPIClient = func(ctx context.Context, insecureHTTP bool, fullName string) (*api.Client, error) {
	return newTrailAPIClient(ctx, insecureHTTP, fullName)
}

// trailsClientOrCacheNotOnboarded resolves the entire-api cell client for
// ownerRepo via trailRefreshAPIClient. errRepoNotOnboarded (cell_target.go) is
// a DEFINITIVE, not transient, negative — the repo has no processing
// placement — so every caller of this seam must persist it via save rather
// than treat it like an ordinary client-construction failure: leaving the
// cache unknown means every future refresh re-attempts (and re-fails) the
// same client build forever. handled reports whether err (construction failed
// with errRepoNotOnboarded) or the result of save; handled is false for a
// plain client-construction failure, which the caller must handle on its own
// terms (log-and-skip, backoff, etc.).
func trailsClientOrCacheNotOnboarded(ctx context.Context, insecureHTTP bool, ownerRepo string, save func() error) (client *api.Client, handled bool, err error) {
	client, err = trailRefreshAPIClient(ctx, insecureHTTP, ownerRepo)
	if err == nil {
		return client, false, nil
	}
	if !errors.Is(err, errRepoNotOnboarded) {
		return nil, false, err
	}
	return nil, true, save()
}

// runTrailEnablementRefresh performs the actual (potentially slow) network
// refresh. It is invoked from the detached `__refresh_trail_enablement`
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
	client, handled, err := trailsClientOrCacheNotOnboarded(ctx, false, scope.Owner+"/"+scope.Repo, func() error {
		return saveTrailsEnabledForScope(ctx, scope, false, time.Now())
	})
	if handled {
		// A definitive, permanent negative: without this, every SessionStart
		// re-forks a refresh child for this repo forever (see
		// trailRefreshSpawnThrottle above), because the cache is never
		// written and so never leaves "unknown".
		if err != nil {
			logging.Debug(logCtx, "trails enablement refresh failed to save not-onboarded scope", "error", err.Error())
		}
		return nil
	}
	if err != nil {
		logging.Debug(logCtx, "trails enablement refresh skipped: authenticated client unavailable", "error", err.Error())
		return nil
	}
	// Best-effort: this runs from the detached __refresh_trail_enablement
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
// `go test` binary doesn't understand `__refresh_trail_enablement` as an
// argument). Production code always uses spawnDetachedTrailRefreshProcess.
var trailRefreshSpawn = spawnDetachedTrailRefreshProcess

// spawnDetachedTrailRefreshProcess starts `entire __refresh_trail_enablement`
// as a detached child so the trails-enablement network refresh can't add
// latency to the SessionStart hook that spawned it. The child runs from the
// worktree root because the refresh resolves the origin remote and
// git-common-dir for cache storage from its working directory.
func spawnDetachedTrailRefreshProcess(worktreeRoot string) {
	execx.SpawnDetached(worktreeRoot, "__refresh_trail_enablement")
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
	dir := filepath.Join(commonDir, "entire")
	// Create the directory before acquiring the lock: flock.Acquire opens the
	// lock file, which fails if its parent doesn't exist yet (mirrors
	// ModifyClonePreferences, which MkdirAlls before locking).
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return false
	}
	markerPath := filepath.Join(dir, "trail-refresh-spawn")
	release, err := flock.Acquire(markerPath + ".lock")
	if err != nil {
		return false
	}
	defer release()

	if data, readErr := os.ReadFile(markerPath); readErr == nil { //nolint:gosec // markerPath is derived from the trusted git-common-dir, not user input
		if last, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); parseErr == nil &&
			now.After(last) && now.Sub(last) < trailRefreshSpawnThrottle {
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
		Use:    "__refresh_trail_enablement",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Detached child with discarded stdout/stderr: initialize file
			// logging so a failing background refresh (e.g. an unreachable
			// host) is diagnosable in .entire/logs/entire.log rather than
			// vanishing. Guard on WorktreeRoot first — matching resume/rewind/
			// reset/explain — so a child whose worktree was removed or relocated
			// between spawn and exec (or a manual invocation outside a repo)
			// doesn't create a stray .entire/logs/ in an arbitrary directory;
			// logging.Init falls back to cwd when WorktreeRoot fails.
			if _, err := paths.WorktreeRoot(ctx); err == nil {
				logging.SetLogLevelGetter(GetLogLevel)
				if err := logging.Init(ctx, ""); err == nil {
					defer logging.Close()
				}
			}
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

func noteTrailCommandEnablement(ctx context.Context, client *api.Client, commandErr error) {
	if commandErr == nil {
		saveTrailsEnabledForRepoBestEffort(ctx, true)
		return
	}
	refreshTrailsEnabledCacheBestEffort(ctx, client)
}

// runAuthenticatedTrailAPI runs fn against the entire-api cell that owns the
// target repository. repoOverride is the raw --repo flag: when non-empty the
// local clone's enablement cache is skipped because the result belongs to a
// different repository.
func runAuthenticatedTrailAPI(ctx context.Context, errW io.Writer, insecureHTTP bool, repoOverride string, fn func(context.Context, *api.Client) error) error {
	_, owner, repo, err := resolveTrailRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return err
	}
	client, err := newTrailAPIClient(ctx, insecureHTTP, owner+"/"+repo)
	if err != nil {
		return renderDataAPIAuthError(ctx, errW, err)
	}
	err = fn(ctx, client)
	if repoOverride == "" {
		noteTrailCommandEnablement(ctx, client, err)
	}
	return err
}
