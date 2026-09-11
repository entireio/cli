package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	var gotForge, gotOwner, gotRepo string
	wantErr := errors.New("cell client unavailable")
	trailRefreshAPIClient = func(_ context.Context, _ bool, forge, owner, repo string) (*api.Client, error) {
		gotForge, gotOwner, gotRepo = forge, owner, repo
		return nil, wantErr
	}
	t.Cleanup(func() { trailRefreshAPIClient = previous })

	info := &gitremote.Info{Forge: "gh", Owner: "acme", Repo: "widget"}
	probeAndCacheTrailsEnablement(t.Context(), false, info)

	if gotForge+"/"+gotOwner+"/"+gotRepo != "gh/acme/widget" {
		t.Fatalf("trailRefreshAPIClient repo = (%q,%q,%q), want (gh,acme,widget)", gotForge, gotOwner, gotRepo)
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
	trailRefreshAPIClient = func(_ context.Context, _ bool, _, _, _ string) (*api.Client, error) {
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
	trailRefreshAPIClient = func(context.Context, bool, string, string, string) (*api.Client, error) {
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

// The probe must apply its OWN deadline rather than inheriting whatever the
// enable report left behind: it now costs ~4 sequential round trips since it
// moved onto the repo's cell, and sharing one budget let a slow enable report
// starve it to nothing.
// Not parallel: changes the process working directory.
func TestProbeAndCacheTrailsEnablement_AppliesItsOwnBudget(t *testing.T) {
	t.Chdir(t.TempDir())

	previous := trailRefreshAPIClient
	var deadline time.Time
	var hasDeadline bool
	trailRefreshAPIClient = func(ctx context.Context, _ bool, _, _, _ string) (*api.Client, error) {
		deadline, hasDeadline = ctx.Deadline()
		return nil, errors.New("stop before any network call")
	}
	t.Cleanup(func() { trailRefreshAPIClient = previous })

	// A parent with a far-off deadline: the probe must still narrow to its own
	// budget, so the bound cannot be something the caller happened to grant.
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()

	probeAndCacheTrailsEnablement(ctx, false, &gitremote.Info{Forge: "gh", Owner: "acme", Repo: "widget"})

	if !hasDeadline {
		t.Fatal("probe ran with no deadline; a hung control plane would stall `entire enable` after success")
	}
	if remaining := time.Until(deadline); remaining > enableTrailsProbeBudget {
		t.Fatalf("probe deadline is %s out, want at most enableTrailsProbeBudget (%s)", remaining, enableTrailsProbeBudget)
	}
}

// The cache write is the point of the probe, so it must not be gated on the
// probe's own budget: a probe that resolved an answer and then failed to record
// it leaves the cache unknown and re-forks a refresh child next SessionStart.
// Not parallel: changes the process working directory and env.
func TestProbeAndCacheTrailsEnablement_CachesEvenWhenTheParentIsDone(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	previous := trailRefreshAPIClient
	trailRefreshAPIClient = func(context.Context, bool, string, string, string) (*api.Client, error) {
		return nil, fmt.Errorf("resolve the Entire cell for acme/widget: %w", errRepoNotOnboarded)
	}
	t.Cleanup(func() { trailRefreshAPIClient = previous })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	probeAndCacheTrailsEnablement(ctx, false, &gitremote.Info{Forge: "gh", Owner: "acme", Repo: "widget"})

	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if prefs.TrailsEnabled == nil || *prefs.TrailsEnabled {
		t.Fatalf("cached trails-enabled decision = %v, want false recorded despite the cancelled parent", prefs.TrailsEnabled)
	}
}

// The two budgets are a split of one ceiling, not independent knobs: raising
// either without the other in mind lengthens how long `entire enable` sits
// silent after it has already printed success.
func TestEnableBudgetsSumToTheSynchronousCeiling(t *testing.T) {
	t.Parallel()

	if total := enableReportBudget + enableTrailsProbeBudget; total > 5*time.Second {
		t.Errorf("enableReportBudget + enableTrailsProbeBudget = %s, want at most the 5s this path used to spend", total)
	}
	if enableTrailsProbeBudget <= enableReportBudget {
		t.Errorf("probe budget %s <= report budget %s; the probe is the step that grew to ~4 round trips and needs the larger share",
			enableTrailsProbeBudget, enableReportBudget)
	}
}
