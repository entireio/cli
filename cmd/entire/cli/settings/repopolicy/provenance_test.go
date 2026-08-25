package repopolicy

import (
	"errors"
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

func TestReadVerifiedRegular_RejectsDeterministicPathSwaps(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		hook func(t *testing.T, root string) verifiedReadHooks
	}{
		{
			name: "entire directory swapped after validation",
			hook: func(t *testing.T, root string) verifiedReadHooks {
				t.Helper()
				outside := t.TempDir()
				writePolicyFile(t, outside, "settings.json", `{"enabled":false}`)
				return verifiedReadHooks{afterPrecheck: func() {
					if err := os.RemoveAll(filepath.Join(root, ".entire")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, filepath.Join(root, ".entire")); err != nil {
						t.Fatal(err)
					}
				}}
			},
		},
		{
			name: "settings file swapped after open",
			hook: func(t *testing.T, root string) verifiedReadHooks {
				t.Helper()
				return verifiedReadHooks{afterOpen: func() {
					path := filepath.Join(root, ".entire", "settings.json")
					if err := os.Rename(path, path+".validated"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(`{"enabled":false}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root, _ := newPolicyRepo(t)
			writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
			rootHandle, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer rootHandle.Close()

			data, err := readVerifiedRegular(rootHandle, projectSettingsRelative, nil, tt.hook(t, root))
			if err == nil || !errors.Is(err, errUnverifiedPath) {
				t.Fatalf("readVerifiedRegular = %q, %v; want unverified-path rejection", data, err)
			}
		})
	}
}

func TestVerifySettingsProvenance_ReturnsBoundContent(t *testing.T) {
	t.Parallel()
	root, repository := newPolicyRepo(t)
	writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
	writePolicyFile(t, root, ".entire/settings.local.json", `{"log_level":"debug"}`)

	provenance, err := VerifySettingsProvenance(t.Context(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !provenance.ProjectVerified || string(provenance.ProjectData) != `{"enabled":true}` {
		t.Fatalf("project provenance = %+v", provenance)
	}
	if !provenance.LocalVerified || string(provenance.LocalData) != `{"log_level":"debug"}` {
		t.Fatalf("local provenance = %+v", provenance)
	}
}

func TestVerifySettingsProvenance_RejectsFilesystemEquivalentTrackedLocalNames(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{".Entire/Settings.Local.json", ".entire/settings.local.json."} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			root, repository := newPolicyRepo(t)
			writePolicyFile(t, root, rel, `{}`)
			runPolicyGit(t, root, "add", "-f", rel)
			writePolicyFile(t, root, ".entire/settings.local.json", `{"redaction":{"openai_privacy_filter":{"command":"/trusted/opf"}}}`)

			provenance, err := VerifySettingsProvenance(t.Context(), repository)
			if err != nil {
				t.Fatal(err)
			}
			if provenance.LocalVerified || provenance.LocalOPFVerified {
				t.Fatalf("filesystem-equivalent tracked path %q was trusted: %+v", rel, provenance)
			}
		})
	}
}

func TestVerifySettingsProvenance_UnbornHEADCanVerifyUntrackedOPFCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initPolicyRepo(t, root)
	writePolicyFile(t, root, ".entire/settings.local.json", `{"redaction":{"openai_privacy_filter":{"command":"/trusted/opf"}}}`)
	repository, err := ResolveRepositoryAt(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}

	provenance, err := VerifySettingsProvenance(t.Context(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !provenance.LocalVerified || !provenance.LocalOPFVerified {
		t.Fatalf("untracked local command in unborn repository was rejected: %+v", provenance)
	}
}
