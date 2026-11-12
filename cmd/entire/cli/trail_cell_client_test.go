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

	"github.com/stretchr/testify/require"
)

// trailsCellClient exists so its three callers do not each re-derive which
// client-build failure is a definitive negative. Its contract is that err
// ALWAYS describes the client build — never a cache save — so a caller can log
// it without knowing which of the two it got; notOnboarded is the flag callers
// switch on.
func TestTrailsCellClient_Contract(t *testing.T) {
	sentinel := fmt.Errorf("resolve processing placement for acme/widget: %w", errRepoNotOnboarded)
	transient := errors.New("control plane unavailable")

	for _, tc := range []struct {
		name           string
		clientErr      error
		wantNotOnboard bool
		wantClient     bool
	}{
		{name: "client builds", wantClient: true},
		{name: "not onboarded is flagged", clientErr: sentinel, wantNotOnboard: true},
		{name: "any other failure is not flagged", clientErr: transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := trailRefreshAPIClient
			trailRefreshAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
				if tc.clientErr != nil {
					return nil, "", tc.clientErr
				}
				return &api.Client{}, "repo-placement-id", nil
			}
			t.Cleanup(func() { trailRefreshAPIClient = previous })

			client, repoID, notOnboarded, err := trailsCellClient(context.Background(), false, "gh", "acme", "widget")

			if notOnboarded != tc.wantNotOnboard {
				t.Errorf("notOnboarded = %v, want %v", notOnboarded, tc.wantNotOnboard)
			}
			if (client != nil) != tc.wantClient {
				t.Errorf("client != nil = %v, want %v", client != nil, tc.wantClient)
			}
			wantRepoID := ""
			if tc.wantClient {
				wantRepoID = "repo-placement-id"
			}
			if repoID != wantRepoID {
				t.Errorf("repoID = %q, want %q", repoID, wantRepoID)
			}
			// err mirrors the client build in every branch, including the
			// not-onboarded one where the caller ignores it in favour of the flag.
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

// Not parallel: each entry changes cwd and the authenticated-client seam.
func TestTrailEnablementRefresh_ResolvedRepoIDAndCacheScope(t *testing.T) {
	for _, refresh := range []struct {
		name string
		run  func(context.Context, *api.Client, string, trailEnablementScope) error
	}{
		{"setup", func(ctx context.Context, _ *api.Client, _ string, scope trailEnablementScope) error {
			probeAndCacheTrailsEnablement(ctx, false, &gitremote.Info{Forge: scope.Forge, Owner: scope.Owner, Repo: scope.Repo})
			return nil
		}},
		{"agent help", func(ctx context.Context, _ *api.Client, _ string, scope trailEnablementScope) error {
			return refreshAgentHelpTrailsEnabledCacheIfStaleForScope(ctx, scope)
		}},
		{"detached refresh", func(ctx context.Context, _ *api.Client, _ string, _ trailEnablementScope) error {
			return runTrailEnablementRefresh(ctx)
		}},
		{"failed command", func(ctx context.Context, client *api.Client, repoID string, _ trailEnablementScope) error {
			noteTrailCommandEnablement(ctx, client, repoID, errors.New("command failed"))
			return nil
		}},
	} {
		t.Run(refresh.name, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				repoID string
				status int
				want   trailEnablementCacheStatus
			}{
				{"enabled", " placement/id? ", http.StatusOK, trailEnablementCacheEnabled},
				{"disabled", " placement/id? ", http.StatusForbidden, trailEnablementCacheDisabled},
				{"not found", " placement/id? ", http.StatusNotFound, trailEnablementCacheDisabled},
				{"gone", " placement/id? ", http.StatusGone, trailEnablementCacheDisabled},
				{"unauthorized", " placement/id? ", http.StatusUnauthorized, trailEnablementCacheUnknown},
				{"transient error", " placement/id? ", http.StatusInternalServerError, trailEnablementCacheUnknown},
				{"missing ID", " \t", http.StatusOK, trailEnablementCacheUnknown},
			} {
				t.Run(tc.name, func(t *testing.T) {
					setupStopTestRepo(t)
					runGitInDir(t, ".", "remote", "add", "origin", "https://github.com/acme/widget.git")
					scope, err := currentTrailEnablementScope(t.Context())
					require.NoError(t, err)

					requests := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						if r.RequestURI != "/api/v1/repos/placement%2Fid%3F/changes?per_page=1" {
							t.Errorf("unexpected enablement probe: %s", r.RequestURI)
						}
						w.WriteHeader(tc.status)
					}))
					t.Cleanup(srv.Close)
					client := api.NewClientWithBaseURL("tok", srv.URL)
					previous := trailRefreshAPIClient
					trailRefreshAPIClient = func(_ context.Context, _ bool, forge, owner, repo string) (*api.Client, string, error) {
						require.Equal(t, scope.RepoKey, trailEnablementRepoKey(forge, owner, repo))
						return client, tc.repoID, nil
					}
					t.Cleanup(func() { trailRefreshAPIClient = previous })

					err = refresh.run(t.Context(), client, tc.repoID, scope)
					if refresh.name == "agent help" && tc.want == trailEnablementCacheUnknown {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}

					wantRequests := 1
					if tc.name == "missing ID" {
						wantRequests = 0
					}
					require.Equal(t, wantRequests, requests)
					require.Equal(t, tc.want, cachedTrailsEnablementForScope(t.Context(), scope, time.Now()))
					for _, other := range []trailEnablementScope{
						{RepoKey: "gh/other/repo", APIBase: scope.APIBase, AuthKey: scope.AuthKey},
						{RepoKey: scope.RepoKey, APIBase: "https://other.example", AuthKey: scope.AuthKey},
						{RepoKey: scope.RepoKey, APIBase: scope.APIBase, AuthKey: "other-identity"},
					} {
						require.Equal(t, trailEnablementCacheUnknown, cachedTrailsEnablementForScope(t.Context(), other, time.Now()))
					}
				})
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
