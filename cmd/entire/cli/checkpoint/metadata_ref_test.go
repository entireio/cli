package checkpoint_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func writeSettings(t *testing.T, repoDir string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoDir, paths.EntireDir), 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, paths.EntireDir, paths.SettingsFileName), []byte(contents), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func TestMetadataRef_LegacyVsCustom(t *testing.T) {
	// Not t.Parallel() — uses t.Chdir for settings.Load CWD resolution.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	// No settings: legacy branch ref.
	if got := checkpoint.MetadataRef(context.Background()); got != plumbing.NewBranchReferenceName(paths.MetadataBranchName) {
		t.Fatalf("default = %s; want legacy branch ref", got)
	}

	// checkpoints_version: 1.1 → custom ref.
	writeSettings(t, dir, `{"strategy_options":{"checkpoints_version":"1.1"}}`)
	if got := checkpoint.MetadataRef(context.Background()); got != plumbing.ReferenceName(paths.MetadataRefName) {
		t.Fatalf("1.1 = %s; want %s", got, paths.MetadataRefName)
	}

	// checkpoints_version: 1 → legacy.
	writeSettings(t, dir, `{"strategy_options":{"checkpoints_version":1}}`)
	if got := checkpoint.MetadataRef(context.Background()); got != plumbing.NewBranchReferenceName(paths.MetadataBranchName) {
		t.Fatalf("v1 = %s; want legacy branch ref", got)
	}
}

func TestRefDisplayName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   plumbing.ReferenceName
		want string
	}{
		{"legacy v1 branch", plumbing.NewBranchReferenceName(paths.MetadataBranchName), "entire/checkpoints/v1"},
		{"v1.1 custom ref", plumbing.ReferenceName(paths.MetadataRefName), "checkpoints/v1.1"},
		{"v1.1 tracking ref", plumbing.ReferenceName(paths.MetadataTrackingRefName), "remotes/origin/checkpoints/v1.1"},
		{"unrecognized prefix returned verbatim", plumbing.ReferenceName("refs/tags/v1"), "refs/tags/v1"},
		{"empty", plumbing.ReferenceName(""), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := checkpoint.RefDisplayName(tc.in); got != tc.want {
				t.Fatalf("RefDisplayName(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMetadataTrackingRef_LegacyVsCustom(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	if got := checkpoint.MetadataTrackingRef(context.Background()); got != plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName) {
		t.Fatalf("default tracking = %s; want legacy tracking", got)
	}

	writeSettings(t, dir, `{"strategy_options":{"checkpoints_version":1.1}}`)
	if got := checkpoint.MetadataTrackingRef(context.Background()); got != plumbing.ReferenceName(paths.MetadataTrackingRefName) {
		t.Fatalf("1.1 tracking = %s; want %s", got, paths.MetadataTrackingRefName)
	}
}

func TestMetadataTrackingRefForRemote_UsesActualRemoteName(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	// Legacy v1 with a non-origin remote.
	got := checkpoint.MetadataTrackingRefForRemote(context.Background(), "upstream")
	want := plumbing.NewRemoteReferenceName("upstream", paths.MetadataBranchName)
	if got != want {
		t.Fatalf("v1 tracking for upstream = %s; want %s", got, want)
	}

	// 1.1 with a non-origin remote.
	writeSettings(t, dir, `{"strategy_options":{"checkpoints_version":"1.1"}}`)
	got = checkpoint.MetadataTrackingRefForRemote(context.Background(), "upstream")
	want = plumbing.ReferenceName("refs/entire/remotes/upstream/checkpoints/v1.1")
	if got != want {
		t.Fatalf("1.1 tracking for upstream = %s; want %s", got, want)
	}

	// 1.1 with origin matches the default helper.
	got = checkpoint.MetadataTrackingRefForRemote(context.Background(), "origin")
	if got != plumbing.ReferenceName(paths.MetadataTrackingRefName) {
		t.Fatalf("1.1 tracking for origin = %s; want %s (the documented default)", got, paths.MetadataTrackingRefName)
	}
}
