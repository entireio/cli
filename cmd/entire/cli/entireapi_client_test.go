package cli

import (
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// setupRecapRepo chdirs into a fresh repo whose origin is originURL.
func setupRecapRepo(t *testing.T, originURL string) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", originURL)
	t.Chdir(dir)
}

// Not parallel: chdirs and swaps the package-level core client seam.
func TestCurrentRepoRef_UsesProcessingPlacement(t *testing.T) {
	setupRecapRepo(t, "https://github.com/entirehq/entire.io.git")
	// The processing placement is not the first one listed, which is the one
	// the old first-active-mirror lookup returned.
	withFakeCellCore(t, &fakeCellCore{
		repos:    reposOutput(fourRegionFixtureProcessingInMiddle()),
		clusters: clustersWithSlugs(),
	})

	repoID, slug, _ := currentRepoRef(t.Context())
	if repoID != "mirror-us" || slug != "entirehq/entire.io" {
		t.Errorf("currentRepoRef = (%q, %q), want (mirror-us, entirehq/entire.io)", repoID, slug)
	}
}

// Not parallel: chdirs and swaps the package-level core client seam.
func TestCurrentRepoRef_LookupFailureIsBestEffort(t *testing.T) {
	setupRecapRepo(t, "https://github.com/entirehq/entire.io.git")
	withFakeCellCore(t, &fakeCellCore{reposErr: errors.New("control plane down")})

	if repoID, slug, _ := currentRepoRef(t.Context()); repoID != "" || slug != "" {
		t.Errorf("currentRepoRef = (%q, %q), want empty so recap shows the personal side only", repoID, slug)
	}
}
