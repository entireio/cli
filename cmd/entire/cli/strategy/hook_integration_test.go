package strategy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestCheckGitHookIntegration(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, hooksDir string)
		want  GitHookIntegrationHealth
	}{
		{
			name:  "no hooks uses absent native integration",
			setup: func(*testing.T, string) {},
			want: GitHookIntegrationHealth{
				Mode:       GitHookIntegrationNative,
				State:      GitHookIntegrationAbsent,
				ReasonCode: "native_hooks_absent",
				Reason:     "Entire Git hooks are not installed.",
			},
		},
		{
			name: "detected Lefthook without artifacts is outdated",
			setup: func(t *testing.T, hooksDir string) {
				repoDir := filepath.Dir(filepath.Dir(hooksDir))
				if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: GitHookIntegrationHealth{
				Mode:       GitHookIntegrationLefthook,
				State:      GitHookIntegrationOutdated,
				Manager:    lefthookManagerName,
				ReasonCode: "lefthook_artifacts_outdated",
				Reason:     "Entire's Lefthook artifacts are missing or outdated.",
			},
		},
		{
			name: "current wrappers use current native integration",
			setup: func(t *testing.T, hooksDir string) {
				writeCurrentManagedHooks(t, hooksDir)
			},
			want: GitHookIntegrationHealth{
				Mode:  GitHookIntegrationNative,
				State: GitHookIntegrationCurrent,
			},
		},
		{
			name: "one stale wrapper uses outdated native integration",
			setup: func(t *testing.T, hooksDir string) {
				writeCurrentManagedHooks(t, hooksDir)
				legacy := "#!/bin/sh\n# " + entireHookMarker + "\n./scripts/entire-dev hooks git pre-push\n"
				if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(legacy), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: GitHookIntegrationHealth{
				Mode:       GitHookIntegrationNative,
				State:      GitHookIntegrationOutdated,
				ReasonCode: "native_hooks_outdated",
				Reason:     "Entire Git hooks are outdated or not executable.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			clearGlobalHooksPath(t, repoDir)
			hooksDir := filepath.Join(repoDir, ".git", "hooks")
			if err := os.MkdirAll(hooksDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, hooksDir)
			t.Chdir(repoDir)

			got := CheckGitHookIntegration(context.Background())
			if got != tt.want {
				t.Errorf("CheckGitHookIntegration() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCheckGitHookIntegration_Lefthook(t *testing.T) {
	setup := func(t *testing.T) (string, string) {
		t.Helper()
		repoDir := t.TempDir()
		testutil.InitRepo(t, repoDir)
		clearGlobalHooksPath(t, repoDir)
		if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		hooksDir := filepath.Join(repoDir, ".git", "hooks")
		if err := os.MkdirAll(hooksDir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(repoDir)
		return repoDir, hooksDir
	}

	t.Run("current manager owned hooks", func(t *testing.T) {
		repoDir, hooksDir := setup(t)
		if _, err := installLefthookFiles(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		writeRealLefthookHooks(t, hooksDir)
		got := checkGitHookIntegrationInDir(t.Context(), repoDir)
		if got.Mode != GitHookIntegrationLefthook || got.State != GitHookIntegrationCurrent || got.Manager != lefthookManagerName {
			t.Errorf("health = %+v, want current Lefthook", got)
		}
	})

	t.Run("current native bridge needs migration", func(t *testing.T) {
		repoDir, hooksDir := setup(t)
		writeCurrentManagedHooks(t, hooksDir)
		got := checkGitHookIntegrationInDir(t.Context(), repoDir)
		if got.Mode != GitHookIntegrationLefthook || got.State != GitHookIntegrationDegraded || got.ReasonCode != "lefthook_native_bridge" {
			t.Errorf("health = %+v, want degraded Lefthook bridge", got)
		}
	})

	t.Run("ambiguity with working native hooks is degraded native", func(t *testing.T) {
		repoDir, hooksDir := setup(t)
		writeCurrentManagedHooks(t, hooksDir)
		if err := os.WriteFile(filepath.Join(repoDir, ".lefthook.yaml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got := checkGitHookIntegrationInDir(t.Context(), repoDir)
		if got.Mode != GitHookIntegrationNative || got.State != GitHookIntegrationDegraded || got.ReasonCode != "hook_manager_ambiguous" {
			t.Errorf("health = %+v, want degraded native", got)
		}
	})

	t.Run("outdated artifacts", func(t *testing.T) {
		repoDir, _ := setup(t)
		if _, err := installLefthookFiles(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, ".lefthook-local", "pre-push", lefthookScriptName), []byte("# stale\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := checkGitHookIntegrationInDir(t.Context(), repoDir)
		if got.Mode != GitHookIntegrationLefthook || got.State != GitHookIntegrationOutdated || got.ReasonCode != "lefthook_artifacts_outdated" {
			t.Errorf("health = %+v, want outdated Lefthook", got)
		}
	})

	t.Run("non executable artifact is outdated", func(t *testing.T) {
		repoDir, _ := setup(t)
		if _, err := installLefthookFiles(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(repoDir, ".lefthook-local", "pre-push", lefthookScriptName), 0o644); err != nil {
			t.Fatal(err)
		}
		got := checkGitHookIntegrationInDir(t.Context(), repoDir)
		if got.Mode != GitHookIntegrationLefthook || got.State != GitHookIntegrationOutdated {
			t.Errorf("health = %+v, want outdated Lefthook", got)
		}
	})

	t.Run("ambiguity without native hooks is error", func(t *testing.T) {
		repoDir, _ := setup(t)
		if err := os.WriteFile(filepath.Join(repoDir, ".lefthook.yaml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got := checkGitHookIntegrationInDir(t.Context(), repoDir)
		if got.Mode != GitHookIntegrationNative || got.State != GitHookIntegrationError || got.ReasonCode != "hook_manager_ambiguous_no_native" {
			t.Errorf("health = %+v, want ambiguity error", got)
		}
	})
}

func TestLegacyGitHookStateRemainsNativeOnlyWithCurrentLefthook(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	if _, err := installLefthookFiles(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	writeRealLefthookHooks(t, filepath.Join(repoDir, ".git", "hooks"))

	health := CheckGitHookIntegration(t.Context())
	if health.Mode != GitHookIntegrationLefthook || health.State != GitHookIntegrationCurrent {
		t.Fatalf("CheckGitHookIntegration() = %+v, want current Lefthook", health)
	}
	if got := CheckGitHookState(t.Context()); got != GitHooksAbsent {
		t.Errorf("CheckGitHookState() = %v, want native-only GitHooksAbsent", got)
	}
	if got := CheckGitHookStateInDir(t.Context(), repoDir); got != GitHooksAbsent {
		t.Errorf("CheckGitHookStateInDir() = %v, want native-only GitHooksAbsent", got)
	}
}

func TestCheckGitHookIntegration_InspectionError(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCurrentManagedHooks(t, hooksDir)

	brokenHook := filepath.Join(hooksDir, "pre-push")
	if err := os.Remove(brokenHook); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(brokenHook, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	got := CheckGitHookIntegration(context.Background())
	if got.Mode != GitHookIntegrationNative {
		t.Errorf("Mode = %q, want %q", got.Mode, GitHookIntegrationNative)
	}
	if got.State != GitHookIntegrationError {
		t.Errorf("State = %q, want %q", got.State, GitHookIntegrationError)
	}
	if got.ReasonCode != "native_hooks_inspection_error" {
		t.Errorf("ReasonCode = %q, want native_hooks_inspection_error", got.ReasonCode)
	}
	if !strings.Contains(got.Reason, "pre-push") {
		t.Errorf("Reason = %q, want managed hook path", got.Reason)
	}
	if legacy := CheckGitHookState(context.Background()); legacy != GitHooksAbsent {
		t.Errorf("CheckGitHookState() = %v, want GitHooksAbsent compatibility mapping", legacy)
	}
}

func TestCheckGitHookIntegration_MarkerOnlyNativeHookIsNotCurrent(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCurrentManagedHooks(t, hooksDir)
	markerOnly := "#!/bin/sh\n# " + entireHookMarker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(markerOnly), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	if got := CheckGitHookIntegration(t.Context()); got.State == GitHookIntegrationCurrent {
		t.Errorf("health = %+v, marker-only native hook must not be current", got)
	}
}

func TestCheckGitHookIntegration_MarkerOnlyLefthookBridgeIsNotCurrent(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), []byte("pre_commit: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	writeRealLefthookHooks(t, hooksDir)
	t.Chdir(repoDir)
	if _, err := installLefthookFiles(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	markerOnly := "#!/bin/sh\n# " + entireHookMarker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(markerOnly), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := CheckGitHookIntegration(t.Context()); got.State == GitHookIntegrationCurrent {
		t.Errorf("health = %+v, marker-only bridge must not deliver Entire", got)
	}
}

func TestEnsureGitHookIntegration_SupportsDottedRootYAMLConfigs(t *testing.T) {
	for _, name := range []string{".lefthook.yml", ".lefthook.yaml"} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			clearGlobalHooksPath(t, repoDir)
			if err := os.WriteFile(filepath.Join(repoDir, name), []byte("pre_commit: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repoDir)
			ClearHooksDirCache()

			if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
				t.Fatalf("EnsureGitHookIntegration() for %s error = %v", name, err)
			}
			if got := CheckGitHookIntegration(t.Context()); got.Mode != GitHookIntegrationLefthook || got.State != GitHookIntegrationCurrent {
				t.Errorf("health for %s = %+v, want current Lefthook", name, got)
			}
		})
	}
}

func TestEnsureGitHookIntegration_UnsupportedLefthookLayoutKeepsNativeDelivery(t *testing.T) {
	for _, configName := range []string{"lefthook.toml", "lefthook.jsonc", ".config/lefthook.yml"} {
		t.Run(configName, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			clearGlobalHooksPath(t, repoDir)
			configPath := filepath.Join(repoDir, configName)
			if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
				t.Fatal(err)
			}
			original := []byte("user-owned config\n")
			if err := os.WriteFile(configPath, original, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repoDir)
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)

			if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
				t.Fatalf("EnsureGitHookIntegration() for %s error = %v", configName, err)
			}
			got := CheckGitHookIntegration(t.Context())
			if got.Mode != GitHookIntegrationLefthook || got.State != GitHookIntegrationDegraded || got.ReasonCode != "lefthook_unsupported_native_bridge" {
				t.Fatalf("health = %+v, want degraded unsupported-layout native bridge", got)
			}
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, original) {
				t.Fatalf("unsupported config was modified: got %q, want %q", data, original)
			}
		})
	}
}

func TestCheckGitHookIntegration_ManagerCandidateDetectionError(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, ".husky"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	got := CheckGitHookIntegration(t.Context())
	if got.State != GitHookIntegrationError {
		t.Errorf("State = %q, want %q", got.State, GitHookIntegrationError)
	}
	if got.ReasonCode != "hook_manager_detection_error" {
		t.Errorf("ReasonCode = %q, want hook_manager_detection_error", got.ReasonCode)
	}
	if !strings.Contains(got.Reason, "Could not detect hook-manager ownership safely") {
		t.Errorf("Reason = %q, want generic manager detection wording", got.Reason)
	}
}

func TestCheckGitHookStateCompatibilityMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state GitHookIntegrationState
		want  GitHookState
	}{
		{state: GitHookIntegrationCurrent, want: GitHooksCurrent},
		{state: GitHookIntegrationOutdated, want: GitHooksOutdated},
		{state: GitHookIntegrationDegraded, want: GitHooksAbsent},
		{state: GitHookIntegrationError, want: GitHooksAbsent},
	}

	for _, tt := range tests {
		if got := legacyGitHookState(GitHookIntegrationHealth{State: tt.state}); got != tt.want {
			t.Errorf("legacyGitHookState(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestEnsureGitHookIntegration_InstallsLefthookArtifacts(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), []byte("pre_commit:\n  commands:\n    lint:\n      run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	written, err := EnsureGitHookIntegration(t.Context(), false)
	if err != nil {
		t.Fatalf("EnsureGitHookIntegration() error = %v", err)
	}
	if written == 0 {
		t.Fatal("EnsureGitHookIntegration() should write Lefthook artifacts")
	}
	if got := CheckGitHookIntegration(t.Context()); got.Mode != GitHookIntegrationLefthook || got.State != GitHookIntegrationCurrent {
		t.Errorf("health = %+v, want current Lefthook", got)
	}
}

func TestEnsureGitHookIntegration_LefthookWithoutActiveHooksInstallsNativeBridge(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), []byte("pre_commit: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
		t.Fatalf("EnsureGitHookIntegration() error = %v", err)
	}
	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(repoDir, ".git", "hooks", hook))
		if err != nil {
			t.Fatalf("active %s hook missing: %v", hook, err)
		}
		if !strings.Contains(string(data), entireHookMarker) {
			t.Errorf("active %s hook does not contain Entire native bridge", hook)
		}
	}
	if got := CheckGitHookIntegration(t.Context()); got.State != GitHookIntegrationCurrent {
		t.Errorf("health = %+v, want current only after active bridges exist", got)
	}
}

func TestEnsureGitHookIntegration_RestoresProvenSavedLefthookHook(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := realLefthookWrapper("pre-push")
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(saved), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	if _, err := InstallGitHook(t.Context(), true, false); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
		t.Fatalf("EnsureGitHookIntegration() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(hooksDir, "pre-push"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != saved {
		t.Errorf("active hook = %q, want saved Lefthook hook", got)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "pre-push"+backupSuffix)); !os.IsNotExist(err) {
		t.Errorf("saved hook backup still exists, stat error = %v", err)
	}
}

func TestEnsureGitHookIntegration_DoesNotRestoreNoOpSavedLefthookHook(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := noOpLefthookWrapper("pre-push")
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(saved), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	if _, err := InstallGitHook(t.Context(), true, false); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
		t.Fatalf("EnsureGitHookIntegration() error = %v", err)
	}
	active, err := os.ReadFile(filepath.Join(hooksDir, "pre-push"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(active), entireHookMarker) {
		t.Errorf("no-op saved hook replaced the active Entire bridge:\n%s", active)
	}
	backup, err := os.ReadFile(filepath.Join(hooksDir, "pre-push"+backupSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != saved {
		t.Errorf("saved no-op hook changed = %q, want %q", backup, saved)
	}
}

func TestCheckGitHookIntegration_NoOpLefthookWrappersAreNotCurrent(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	for _, hook := range gitHookNames {
		if err := os.WriteFile(filepath.Join(hooksDir, hook), []byte(noOpLefthookWrapper(hook)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if got := CheckGitHookIntegration(t.Context()); got.State == GitHookIntegrationCurrent {
		t.Errorf("health = %+v, no-op Lefthook wrappers must not be current", got)
	}
}

func TestRemoveGitHookIntegration_PreservesUnrelatedLefthookContent(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	mainConfig := "# keep me\npre_commit:\n  commands:\n    lint:\n      run: true\n"
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), []byte(mainConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveGitHookIntegration(t.Context())
	if err != nil {
		t.Fatalf("RemoveGitHookIntegration() error = %v", err)
	}
	if removed == 0 {
		t.Fatal("RemoveGitHookIntegration() should remove owned artifacts")
	}
	data, err := os.ReadFile(filepath.Join(repoDir, "lefthook.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != mainConfig {
		t.Errorf("main config changed = %q", data)
	}
	local, err := os.ReadFile(filepath.Join(repoDir, lefthookLocalConfigName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(local), lefthookOwnedMarker) {
		t.Errorf("local config retains Entire-owned entries: %s", local)
	}
}

func TestLefthookIntegration_RoundTripPreservesCompatibleUserSourceDirLocal(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(repoDir, lefthookLocalConfigName)
	const userConfig = "# user config\nsource_dir_local: .lefthook-local # user-selected shared directory\nuser_key: keep-me\n"
	if err := os.WriteFile(localPath, []byte(userConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
		t.Fatalf("EnsureGitHookIntegration() error = %v", err)
	}
	installed, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	installedRoot, err := parseYAMLMapping(installed, lefthookLocalConfigName)
	if err != nil {
		t.Fatal(err)
	}
	sourceValue, found, duplicate := mappingValueCount(installedRoot, "source_dir_local")
	sourceKey := mappingKey(installedRoot, "source_dir_local")
	if !found || duplicate || sourceValue == nil {
		t.Fatalf("installed source_dir_local = found %v duplicate %v value %#v", found, duplicate, sourceValue)
	}
	if sourceValue.Value != lefthookLocalDir {
		t.Fatalf("installed source_dir_local = %q, want %q", sourceValue.Value, lefthookLocalDir)
	}
	if lefthookNodeOwned(sourceKey, sourceValue) {
		t.Fatal("install claimed the compatible user source_dir_local")
	}
	if !strings.Contains(string(installed), "# user-selected shared directory") {
		t.Errorf("install lost user source_dir_local comment:\n%s", installed)
	}

	if _, err := RemoveGitHookIntegration(t.Context()); err != nil {
		t.Fatalf("RemoveGitHookIntegration() error = %v", err)
	}
	uninstalled, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"source_dir_local: .lefthook-local", "# user-selected shared directory", "user_key: keep-me"} {
		if !strings.Contains(string(uninstalled), want) {
			t.Errorf("uninstall lost %q:\n%s", want, uninstalled)
		}
	}
	if strings.Contains(string(uninstalled), lefthookOwnedMarker) {
		t.Errorf("uninstall retained Entire-owned YAML:\n%s", uninstalled)
	}
}

func TestRemoveGitHookIntegration_RollsBackAfterOwnedScriptObstruction(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	if _, err := EnsureGitHookIntegration(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repoDir, lefthookLocalConfigName)
	excludePath := filepath.Join(repoDir, ".git", "info", "exclude")
	wantConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantExclude, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	obstruction := filepath.Join(repoDir, filepath.FromSlash(lefthookScriptPath("pre-push")))
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(obstruction, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := RemoveGitHookIntegration(t.Context()); err == nil {
		t.Fatal("RemoveGitHookIntegration() should fail on an owned script directory obstruction")
	} else if !strings.Contains(err.Error(), "inspect owned Lefthook script pre-push") {
		t.Fatalf("RemoveGitHookIntegration() error = %v, want failure after staged config changes", err)
	}
	if got, err := os.ReadFile(configPath); err != nil || string(got) != string(wantConfig) {
		t.Errorf("local config not rolled back: got %q err %v", got, err)
	}
	if got, err := os.ReadFile(excludePath); err != nil || string(got) != string(wantExclude) {
		t.Errorf("exclude not rolled back: got %q err %v", got, err)
	}

	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveGitHookIntegration(t.Context()); err != nil {
		t.Fatalf("retry RemoveGitHookIntegration() error = %v", err)
	}
}

func TestEnsureGitHookIntegration_FinalVerificationRollbackRestoresBytesAndMode(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repoDir, lefthookLocalConfigName)
	want := []byte("# user local config\npre_commit:\n  commands:\n    lint:\n      run: true\n")
	if err := os.WriteFile(configPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	hookIntegrationFault = func(stage, _ string) error {
		if stage == "final-verification" {
			return errors.New("injected verification failure")
		}
		return nil
	}
	t.Cleanup(func() { hookIntegrationFault = nil })

	if _, err := EnsureGitHookIntegration(t.Context(), false); err == nil {
		t.Fatal("EnsureGitHookIntegration() should report injected verification failure")
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("config bytes = %q, want %q", got, want)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Errorf("config mode = %o, want 600", gotMode)
	}
}

func TestEnsureGitHookIntegration_VerifiesArtifactsBeforeSavedHookRestore(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), []byte("pre_commit: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(realLefthookWrapper("pre-push")), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	if _, err := InstallGitHook(t.Context(), true, false); err != nil {
		t.Fatal(err)
	}
	restoreObserved := false
	hookIntegrationFault = func(stage, _ string) error {
		if stage == "saved-hook-restore" {
			restoreObserved = true
		}
		if stage == "artifact-verification" {
			return errors.New("injected artifact verification failure")
		}
		return nil
	}
	t.Cleanup(func() { hookIntegrationFault = nil })

	if _, err := EnsureGitHookIntegration(t.Context(), false); err == nil {
		t.Fatal("EnsureGitHookIntegration() should report artifact verification failure")
	}
	if restoreObserved {
		t.Fatal("saved-hook restoration ran before artifact verification succeeded")
	}
}

func TestLefthookGeneratedHookProvenance(t *testing.T) {
	t.Parallel()
	if !looksLikeLefthookHook([]byte(realLefthookWrapper("pre-push")), "pre-push") {
		t.Fatal("real Lefthook v2.1.12 wrapper should be recognized")
	}
	mentionOnly := []byte("#!/bin/sh\n# custom hook mentioning lefthook\nexit 0\n")
	if looksLikeLefthookHook(mentionOnly, "pre-push") {
		t.Fatal("arbitrary mention of Lefthook must not prove generated-hook provenance")
	}
	if looksLikeLefthookHook([]byte(noOpLefthookWrapper("pre-push")), "pre-push") {
		t.Fatal("a shaped wrapper whose call_lefthook body is a no-op must not prove delivery")
	}
}

// A Lefthook release that adds a package-manager probe must still read as
// Lefthook's launcher. Byte-matching the generated body pinned every existing
// probe branch, so the next release read as a foreign hook — and Entire then
// wrote native wrappers over Lefthook's own hooks, which is #1349 in reverse.
func TestLooksLikeLefthookHook_ToleratesTemplateDrift(t *testing.T) {
	t.Parallel()
	drifted := "#!/bin/sh\n\n" +
		"if [ \"$LEFTHOOK_VERBOSE\" = \"1\" -o \"$LEFTHOOK_VERBOSE\" = \"true\" ]; then\n  set -x\nfi\n\n" +
		"if [ \"$LEFTHOOK\" = \"0\" ]; then\n  exit 0\nfi\n\n" +
		"call_lefthook()\n{\n" +
		"  if some_new_package_manager run lefthook -h >/dev/null 2>&1\n" +
		"  then\n    some_new_package_manager run lefthook \"$@\"\n" +
		"  else\n    lefthook \"$@\"\n  fi\n" +
		"}\n\n" +
		"call_lefthook run \"pre-push\" \"$@\"\n"
	if !looksLikeLefthookHook([]byte(drifted), "pre-push") {
		t.Error("a Lefthook launcher with an unrecognized probe branch must still be recognized")
	}
}

func noOpLefthookWrapper(hook string) string {
	return "#!/bin/sh\n\n" +
		"if [ \"$LEFTHOOK_VERBOSE\" = \"1\" -o \"$LEFTHOOK_VERBOSE\" = \"true\" ]; then\n  set -x\nfi\n\n" +
		"if [ \"$LEFTHOOK\" = \"0\" ]; then\n  exit 0\nfi\n\n" +
		"call_lefthook()\n{\n  :\n}\n\n" +
		"call_lefthook run \"" + hook + "\" \"$@\"\n"
}

func realLefthookWrapper(hook string) string {
	return "#!/bin/sh\n\n" +
		"if [ \"$LEFTHOOK_VERBOSE\" = \"1\" -o \"$LEFTHOOK_VERBOSE\" = \"true\" ]; then\n  set -x\nfi\n\n" +
		"if [ \"$LEFTHOOK\" = \"0\" ]; then\n  exit 0\nfi\n\n" +
		"call_lefthook()\n{\n  lefthook \"$@\"\n}\n\n" +
		"call_lefthook run \"" + hook + "\" \"$@\"\n"
}

func writeRealLefthookHooks(t *testing.T, hooksDir string) {
	t.Helper()
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range gitHookNames {
		if err := os.WriteFile(filepath.Join(hooksDir, hook), []byte(realLefthookWrapper(hook)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveGitHookIntegration_RejectsSymlinkedLefthookRoot(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	external := t.TempDir()
	externalScript := filepath.Join(external, "pre-push", lefthookScriptName)
	if err := os.MkdirAll(filepath.Dir(externalScript), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\n# " + lefthookOwnedMarker + "\nexit 0\n"
	if err := os.WriteFile(externalScript, []byte(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(repoDir, lefthookLocalDir)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	if _, err := RemoveGitHookIntegration(t.Context()); err == nil {
		t.Fatal("RemoveGitHookIntegration() should reject symlinked Lefthook root")
	}
	got, err := os.ReadFile(externalScript)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("external script changed = %q, want %q", got, want)
	}
}

func TestRemoveGitHookIntegration_RejectsSymlinkedHookSubdirectory(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	external := t.TempDir()
	externalScript := filepath.Join(external, lefthookScriptName)
	want := "#!/bin/sh\n# " + lefthookOwnedMarker + "\nexit 0\n"
	if err := os.WriteFile(externalScript, []byte(want), 0o755); err != nil {
		t.Fatal(err)
	}
	localRoot := filepath.Join(repoDir, lefthookLocalDir)
	if err := os.Mkdir(localRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(localRoot, "pre-push")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	if _, err := RemoveGitHookIntegration(t.Context()); err == nil {
		t.Fatal("RemoveGitHookIntegration() should reject symlinked hook subdirectory")
	}
	got, err := os.ReadFile(externalScript)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("external script changed = %q, want %q", got, want)
	}
}

func TestRemoveGitHookIntegration_RejectsSymlinkedEffectiveHooksDirectory(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	testutil.RunGit(t, repoDir, "config", "core.hooksPath", "managed-hooks")
	external := t.TempDir()
	externalHook := filepath.Join(external, "pre-push")
	want := "#!/bin/sh\n# " + entireHookMarker + "\nexit 0\n"
	if err := os.WriteFile(externalHook, []byte(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(repoDir, "managed-hooks")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	ClearHooksDirCache()

	if _, err := RemoveGitHookIntegration(t.Context()); err == nil {
		t.Fatal("RemoveGitHookIntegration() should reject a symlinked effective hooks directory")
	}
	got, err := os.ReadFile(externalHook)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("external native hook changed = %q, want %q", got, want)
	}
}

func TestRemoveGitHookIntegration_RejectsSymlinkedNativeHookAndBackup(t *testing.T) {
	for _, leaf := range []string{"pre-push", "pre-push" + backupSuffix} {
		t.Run(leaf, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			clearGlobalHooksPath(t, repoDir)
			if err := os.MkdirAll(filepath.Join(repoDir, ".git", "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), "external-hook")
			want := "#!/bin/sh\n# " + entireHookMarker + "\nexit 0\n"
			if err := os.WriteFile(external, []byte(want), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, filepath.Join(repoDir, ".git", "hooks", leaf)); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repoDir)
			ClearHooksDirCache()

			if _, err := RemoveGitHookIntegration(t.Context()); err == nil {
				t.Fatalf("RemoveGitHookIntegration() should reject symlinked %s", leaf)
			}
			got, err := os.ReadFile(external)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("external hook changed = %q, want %q", got, want)
			}
		})
	}
}

func TestGitHookIntegrationIgnoresOwnershipMarkerInUnrelatedYAML(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	want := []byte("user_setting: value # " + lefthookOwnedMarker + "\n")
	if err := os.WriteFile(filepath.Join(repoDir, lefthookLocalConfigName), want, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	if AnyGitHookIntegrationInstalled(t.Context()) {
		t.Fatal("unrelated YAML marker must not claim Entire ownership")
	}
	if _, err := RemoveGitHookIntegration(t.Context()); err != nil {
		t.Fatalf("RemoveGitHookIntegration() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoDir, lefthookLocalConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("unrelated YAML changed = %q, want %q", got, want)
	}
}
