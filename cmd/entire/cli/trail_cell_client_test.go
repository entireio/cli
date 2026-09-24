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
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// trailsCellClient exists so its three callers do not each re-derive which
// client-build failure is a definitive negative. Its contract is that err
// ALWAYS describes the client build — never a cache save — so a caller can log
// it without knowing which of the two it got; definitiveNegative is the flag
// callers switch on.
func TestTrailsCellClient_Contract(t *testing.T) {
	sentinel := fmt.Errorf("resolve processing placement for acme/widget: %w", errRepoNotOnboarded)
	crossSite := fmt.Errorf("refusing to send a entire.io login: %w", auth.ErrCellSiteMismatch)
	transient := errors.New("control plane unavailable")

	for _, tc := range []struct {
		name         string
		clientErr    error
		wantNegative bool
		wantClient   bool
	}{
		{name: "client builds", wantClient: true},
		{name: "not onboarded is flagged", clientErr: sentinel, wantNegative: true},
		{name: "cross-site cell is flagged", clientErr: crossSite, wantNegative: true},
		{name: "any other failure is not flagged", clientErr: transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := trailRefreshAPIClient
			trailRefreshAPIClient = func(context.Context, bool, string, string, string) (*api.Client, error) {
				if tc.clientErr != nil {
					return nil, tc.clientErr
				}
				return &api.Client{}, nil
			}
			t.Cleanup(func() { trailRefreshAPIClient = previous })

			client, definitiveNegative, err := trailsCellClient(context.Background(), false, "gh", "acme", "widget")

			if definitiveNegative != tc.wantNegative {
				t.Errorf("definitiveNegative = %v, want %v", definitiveNegative, tc.wantNegative)
			}
			if (client != nil) != tc.wantClient {
				t.Errorf("client != nil = %v, want %v", client != nil, tc.wantClient)
			}
			// err mirrors the client build in every branch, including the
			// definitive-negative ones where the caller ignores it in favour of the flag.
			if tc.clientErr == nil {
				if err != nil {
					t.Errorf("err = %v, want nil when the client builds", err)
				}
				return
			}
			if !errors.Is(err, tc.clientErr) {
				t.Errorf("err = %v, want it to wrap the client-build failure %v", err, tc.clientErr)
			}
		})
	}
}

// A spent refresh deadline must not cost us the answer it just bought.
// saveTrailsEnabledForScope resolves the git common dir with `git rev-parse`
// under the passed ctx, so before this guarantee moved into the single writer a
// refresh that answered at 2.9s of its 3s budget could fail to record the
// result — leaving the cache "unknown" and re-forking a refresh child on every
// SessionStart, the exact outcome the not-onboarded branch exists to prevent.
// Not parallel: changes the process working directory and env.
func TestSaveTrailsEnabledForScope_SurvivesASpentDeadline(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)
	runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/acme/widget.git")

	scope, err := currentTrailEnablementScope(t.Context())
	if err != nil {
		t.Fatalf("resolve trail scope: %v", err)
	}

	// Exactly the state a refresh is in when its budget ran out resolving the
	// answer it is about to store.
	spent, cancel := context.WithCancel(t.Context())
	cancel()

	if err := saveTrailsEnabledForScope(spent, scope, false, time.Now()); err != nil {
		t.Fatalf("save with a cancelled context: %v", err)
	}

	prefs, err := settings.LoadClonePreferences(t.Context())
	if err != nil {
		t.Fatalf("load clone preferences: %v", err)
	}
	if prefs.TrailsEnabled == nil || *prefs.TrailsEnabled {
		t.Fatalf("cached decision = %v, want false actually recorded", prefs.TrailsEnabled)
	}
}

// A 401 from the enablement probe means this login is not accepted by the cell
// (e.g. a login from another Entire environment). It will not fix itself on
// retry, so it is cached as unavailable like a 403: leaving the cache unknown
// re-probed on every SessionStart and raised a security alarm server-side each
// time (ENT-2573).
// Not parallel: changes the process working directory and env.
func TestRefreshTrailsEnabledCacheForScope_CachesUnauthorizedAsDisabled(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)
	runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/acme/widget.git")

	scope, err := currentTrailEnablementScope(t.Context())
	if err != nil {
		t.Fatalf("resolve trail scope: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	enabled, err := refreshTrailsEnabledCacheForScope(t.Context(), api.NewClientWithBaseURL("tok", server.URL), scope)
	if err != nil {
		t.Fatalf("refresh: %v, want a 401 to be a definitive answer", err)
	}
	if enabled {
		t.Fatal("enabled = true, want false")
	}
	if got := cachedTrailsEnablementForScope(t.Context(), scope, time.Now()); got != trailEnablementCacheDisabled {
		t.Fatalf("cache = %v, want disabled", got)
	}
}
