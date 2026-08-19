package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// probeAndCacheTrailsEnablement must route the repo-scoped trails-enablement
// probe through the entire-api cell that hosts THIS repo (trailRefreshAPIClient),
// never through the generic data-API/BFF client — the BFF does not proxy
// /api/v1/trails/... for bearer callers (COR-666).
// probeAndCacheTrailsEnablement performs no cache lookup of its own — this
// isolates saveTrailsEnabledForRemote/debug logging below from the developer's
// real repo, since the probe always runs regardless of cwd.
// Not parallel: changes the process working directory.
func TestProbeAndCacheTrailsEnablement_RoutesThroughRepoCell(t *testing.T) {
	t.Chdir(t.TempDir())

	previous := trailRefreshAPIClient
	var gotFullName string
	wantErr := errors.New("cell client unavailable")
	trailRefreshAPIClient = func(_ context.Context, _ bool, fullName string) (*api.Client, error) {
		gotFullName = fullName
		return nil, wantErr
	}
	t.Cleanup(func() { trailRefreshAPIClient = previous })

	info := &gitremote.Info{Forge: "gh", Owner: "acme", Repo: "widget"}
	probeAndCacheTrailsEnablement(t.Context(), false, info)

	if gotFullName != testTrailCellRoutedFullName {
		t.Fatalf("trailRefreshAPIClient fullName = %q, want %s", gotFullName, testTrailCellRoutedFullName)
	}
}

// When the repo-routed client resolves and the probe succeeds, the decision is
// cached exactly like the pre-split reportRepoEnabled did.
// Not parallel: changes the process working directory and env.
func TestProbeAndCacheTrailsEnablement_CachesEnabledDecision(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	previous := trailRefreshAPIClient
	trailRefreshAPIClient = func(_ context.Context, _ bool, _ string) (*api.Client, error) {
		return api.NewClientWithBaseURL("tok", srv.URL), nil
	}
	t.Cleanup(func() { trailRefreshAPIClient = previous })

	info := &gitremote.Info{Forge: "gh", Owner: "acme", Repo: "widget"}
	probeAndCacheTrailsEnablement(t.Context(), false, info)

	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if prefs.TrailsEnabled == nil || !*prefs.TrailsEnabled {
		t.Fatalf("cached trails-enabled decision = %v, want true", prefs.TrailsEnabled)
	}
	wantRepoKey := trailEnablementRepoKey("gh", "acme", "widget")
	if prefs.TrailsEnabledRepoKey != wantRepoKey {
		t.Fatalf("cached repo key = %q, want %q", prefs.TrailsEnabledRepoKey, wantRepoKey)
	}
}

// probeAndCacheTrailsEnablement must persist a definitive negative when
// trailRefreshAPIClient fails because the repo has no processing placement
// (errRepoNotOnboarded), matching runTrailEnablementRefresh's handling of the
// same sentinel. Without this, the cache is left unknown forever, and a repo
// that was later de-onboarded could keep a stale cached `true` from a prior
// enable until the hourly TTL expires.
// Not parallel: changes the process working directory and env.
func TestProbeAndCacheTrailsEnablement_NotOnboardedSavesDisabledCache(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	previous := trailRefreshAPIClient
	trailRefreshAPIClient = func(context.Context, bool, string) (*api.Client, error) {
		return nil, fmt.Errorf("resolve the Entire cell for acme/widget: %w", errRepoNotOnboarded)
	}
	t.Cleanup(func() { trailRefreshAPIClient = previous })

	info := &gitremote.Info{Forge: "gh", Owner: "acme", Repo: "widget"}
	probeAndCacheTrailsEnablement(t.Context(), false, info)

	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if prefs.TrailsEnabled == nil || *prefs.TrailsEnabled {
		t.Fatalf("cached trails-enabled decision = %v, want false (not left unknown)", prefs.TrailsEnabled)
	}
	wantRepoKey := trailEnablementRepoKey("gh", "acme", "widget")
	if prefs.TrailsEnabledRepoKey != wantRepoKey {
		t.Fatalf("cached repo key = %q, want %q", prefs.TrailsEnabledRepoKey, wantRepoKey)
	}
}
