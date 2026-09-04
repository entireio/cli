package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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
			trailRefreshAPIClient = func(context.Context, bool, string, string, string) (*api.Client, error) {
				if tc.clientErr != nil {
					return nil, tc.clientErr
				}
				return &api.Client{}, nil
			}
			t.Cleanup(func() { trailRefreshAPIClient = previous })

			client, notOnboarded, err := trailsCellClient(context.Background(), false, "gh", "acme", "widget")

			if notOnboarded != tc.wantNotOnboard {
				t.Errorf("notOnboarded = %v, want %v", notOnboarded, tc.wantNotOnboard)
			}
			if (client != nil) != tc.wantClient {
				t.Errorf("client != nil = %v, want %v", client != nil, tc.wantClient)
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
