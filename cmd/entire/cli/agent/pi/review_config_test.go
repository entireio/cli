package pi

import (
	"os"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The checkout's extensions are dropped; Entire's own and the profile's load.
func TestPreparePiReviewConfig(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "x")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	cfg := reviewtypes.RunConfig{AgentConfig: &reviewtypes.AgentConfig{Extensions: []string{"/opt/pi/ext.ts"}}}
	got, cleanup, err := preparePiReviewConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got.ExtraArgs[0] != "--no-extensions" || got.ExtraArgs[1] != "--extension" {
		t.Fatalf("ExtraArgs = %v", got.ExtraArgs)
	}
	entireExt, err := os.ReadFile(got.ExtraArgs[2])
	if err != nil || !IsEntireExtension(entireExt) {
		t.Fatalf("Entire extension not written (%v)", err)
	}
	if !slices.Contains(got.ExtraArgs, "/opt/pi/ext.ts") {
		t.Errorf("profile extension missing: %v", got.ExtraArgs)
	}
}
