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
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

const (
	trailEnablementCacheTTL                   = time.Hour
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

func refreshTrailsEnabledCacheIfStaleForScope(ctx context.Context, scope trailEnablementScope) error {
	if cachedTrailsEnablementForScope(ctx, scope, time.Now()) != trailEnablementCacheUnknown {
		return nil
	}
	if !scope.Supported {
		return saveTrailsEnabledForScope(ctx, scope, false, time.Now())
	}
	client, err := NewAuthenticatedAPIClient(ctx, false)
	if err != nil {
		return err
	}
	_, err = refreshTrailsEnabledCacheForScope(ctx, client, scope)
	return err
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

// runAuthenticatedTrailAPI runs fn against the data API as the current user.
// repoOverride is the raw --repo flag: when non-empty the trails-enablement
// cache is skipped, because that cache is keyed to the local clone's origin and
// a cross-repo query says nothing about the local clone (recording it would
// mis-attribute enablement to the wrong repo).
func runAuthenticatedTrailAPI(ctx context.Context, errW io.Writer, insecureHTTP bool, repoOverride string, fn func(context.Context, *api.Client) error) error {
	return runAuthenticatedDataAPI(ctx, errW, insecureHTTP, func(ctx context.Context, client *api.Client) error {
		err := fn(ctx, client)
		if repoOverride == "" {
			noteTrailCommandEnablement(ctx, client, err)
		}
		return err
	})
}
