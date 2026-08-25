package repopolicy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsProvenance_RejectsTrackedLocalAndSymlinkTraversal(t *testing.T) {
	t.Parallel()
	t.Run("ordinary project and untracked local are verified", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{}`)
		writePolicyFile(t, root, ".entire/settings.local.json", `{}`)
		provenance, err := VerifySettingsProvenance(t.Context(), repository)
		if err != nil {
			t.Fatal(err)
		}
		if !provenance.ProjectVerified || !provenance.LocalVerified {
			t.Fatalf("provenance = %+v, want both verified", provenance)
		}
	})

	t.Run("tracked local is rejected", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.local.json", `{}`)
		runPolicyGit(t, root, "add", "-f", ".entire/settings.local.json")
		provenance, err := VerifySettingsProvenance(t.Context(), repository)
		if err != nil {
			t.Fatal(err)
		}
		if provenance.LocalVerified {
			t.Fatalf("provenance = %+v, tracked local must be rejected", provenance)
		}
	})

	t.Run("symlinked Entire directory is rejected", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		outside := t.TempDir()
		writePolicyFile(t, outside, "settings.json", `{}`)
		if err := os.Symlink(outside, filepath.Join(root, ".entire")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		provenance, err := VerifySettingsProvenance(t.Context(), repository)
		if err != nil {
			t.Fatal(err)
		}
		if provenance.ProjectVerified || provenance.LocalVerified {
			t.Fatalf("provenance = %+v, symlink traversal must be rejected", provenance)
		}
	})
}
