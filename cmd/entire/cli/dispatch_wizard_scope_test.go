package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

func TestNewDispatchWizardScope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		repos       []string
		placements  map[string][]string
		home        string
		wantJuris   string
		wantDefault string
		wantOptions string // labels in render order
		reposIn     map[string]string
	}{
		{
			name:  "home wins when eligible; unplaced repos fall into home",
			repos: []string{"acme/us-only", "acme/both", "acme/eu-only", "acme/unplaced"},
			placements: map[string][]string{
				"acme/us-only": {"us"}, "acme/both": {"eu", "us"}, "acme/eu-only": {"eu"},
			},
			home:        "au",
			wantJuris:   "au,eu,us",
			wantDefault: "au",
			wantOptions: "AU (home),EU,US",
			reposIn:     map[string]string{"us": "acme/us-only,acme/both", "au": "acme/unplaced", "": "", dispatchWizardJurisdictionHome: ""},
		},
		{
			name:        "busiest wins when home has no repos",
			repos:       []string{"a/one", "a/two", "a/three"},
			placements:  map[string][]string{"a/one": {"us"}, "a/two": {"eu"}, "a/three": {"eu"}},
			home:        "au",
			wantJuris:   "eu,us",
			wantDefault: "eu",
			wantOptions: "EU,US",
		},
		{
			name:        "no placement data and home unknown: nothing to pick, route home",
			repos:       []string{"a/one"},
			wantJuris:   "",
			wantDefault: "",
			wantOptions: "Home",
			reposIn:     map[string]string{"": "a/one"},
		},
		{
			name:        "home unknown: unplaced repos stay selectable under Home",
			repos:       []string{"a/placed", "a/unplaced"},
			placements:  map[string][]string{"a/placed": {"us"}},
			wantJuris:   "us",
			wantDefault: "us",
			wantOptions: "US,Home",
			reposIn:     map[string]string{"us": "a/placed", "": "a/unplaced", dispatchWizardJurisdictionHome: "a/unplaced"},
		},
		{
			name:        "single jurisdiction is the default",
			repos:       []string{"a/one"},
			placements:  map[string][]string{"a/one": {"us"}},
			wantJuris:   "us",
			wantDefault: "us",
			wantOptions: "US",
		},
		{
			name:        "placement keys are lowercased slugs",
			repos:       []string{"Acme/Widget"},
			placements:  map[string][]string{"acme/widget": {"eu"}},
			wantJuris:   "eu",
			wantDefault: "eu",
			wantOptions: "EU",
			reposIn:     map[string]string{"eu": "Acme/Widget"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scope := newDispatchWizardScope(tc.repos, tc.placements, tc.home)
			if got := strings.Join(scope.jurisdictions, ","); got != tc.wantJuris {
				t.Fatalf("jurisdictions = %q, want %q", got, tc.wantJuris)
			}
			if scope.defaultJurisdiction != tc.wantDefault {
				t.Fatalf("default = %q, want %q", scope.defaultJurisdiction, tc.wantDefault)
			}
			labels := make([]string, 0)
			for _, option := range scope.options() {
				labels = append(labels, option.Key)
				if option.Value == "" {
					t.Fatalf("option %q has the empty value, which huh would pre-select over the default", option.Key)
				}
			}
			if got := strings.Join(labels, ","); got != tc.wantOptions {
				t.Fatalf("options = %q, want %q (default must render first)", got, tc.wantOptions)
			}
			for j, want := range tc.reposIn {
				if got := strings.Join(scope.reposIn(j), ","); got != want {
					t.Fatalf("reposIn(%q) = %q, want %q", j, got, want)
				}
			}
		})
	}
}

func TestDefaultListDispatchWizardPlacements_ReadyOnlyKeyedBySlug(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{repos: &coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{
		{FullName: "Acme/Widget", Placements: []coreapi.RepoPlacement{
			{ID: "p1", Jurisdiction: "US", Status: coreapi.RepoPlacementStatusReady},
			{ID: "p2", Jurisdiction: "eu", Status: coreapi.RepoPlacementStatusProcessing},
		}},
		{FullName: "", Placements: []coreapi.RepoPlacement{{ID: "p3", Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady}}},
	}}})

	got, err := defaultListDispatchWizardPlacements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Join(got["acme/widget"], ",") != "us" {
		t.Fatalf("expected ready placements keyed by lowercased slug, got %v", got)
	}
}

func TestLoadDispatchWizardScope_DegradesPerSource(t *testing.T) {
	stubDispatchWizardScopeSources(t, []string{"a/one"}, errors.New("core down"), "au")

	scope := loadDispatchWizardScope(context.Background(), t.TempDir())
	if strings.Join(scope.repos, ",") != "a/one" || scope.home != "au" {
		t.Fatalf("expected repos and home with placements degraded, got %+v", scope)
	}
	// Placements unavailable: every repo is attributed to the known home.
	if strings.Join(scope.jurisdictions, ",") != "au" || scope.defaultJurisdiction != "au" {
		t.Fatalf("expected the home-only scope, got %+v", scope)
	}
}

// stubDispatchWizardScopeSources swaps the wizard's catalogue seams (no
// placement data). Not parallel-safe: the seams are package globals.
func stubDispatchWizardScopeSources(t *testing.T, repos []string, placementsErr error, home string) {
	t.Helper()
	oldRepos, oldPlacements, oldHome := listDispatchWizardRepos, listDispatchWizardPlacements, resolveDispatchWizardHome
	listDispatchWizardRepos = func(context.Context) ([]string, error) { return repos, nil }
	listDispatchWizardPlacements = func(context.Context) (map[string][]string, error) { return nil, placementsErr }
	resolveDispatchWizardHome = func(context.Context) string { return home }
	t.Cleanup(func() {
		listDispatchWizardRepos, listDispatchWizardPlacements, resolveDispatchWizardHome = oldRepos, oldPlacements, oldHome
	})
}
