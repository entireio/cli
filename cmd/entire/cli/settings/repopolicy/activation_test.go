package repopolicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func newPolicyRepo(t *testing.T) (string, Repository) {
	t.Helper()
	root := t.TempDir()
	initPolicyRepo(t, root)
	writePolicyFile(t, root, "README.md", "test\n")
	runPolicyGit(t, root, "add", "README.md")
	runPolicyGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")
	repository, err := ResolveRepositoryAt(t.Context(), root)
	if err != nil {
		t.Fatalf("ResolveRepositoryAt: %v", err)
	}
	return root, repository
}

func policyAt(t *testing.T, root string) RepoPolicy {
	t.Helper()
	t.Chdir(root)
	ClearGlobalModeCache()
	t.Cleanup(ClearGlobalModeCache)
	policy, err := ClassifyRepoPolicy(t.Context())
	if err != nil {
		t.Fatalf("ClassifyRepoPolicy: %v", err)
	}
	return policy
}

func setPolicyGlobal(t *testing.T, body string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if body != "" {
		writePolicyFile(t, configDir, UserSettingsFileName, body)
	}
	ClearGlobalModeCache()
	t.Cleanup(ClearGlobalModeCache)
}

func TestClassifyRepoPolicy_CommittedEnabledSettingDoesNotActivateFreshClone(t *testing.T) {
	root, _ := newPolicyRepo(t)
	setPolicyGlobal(t, "")
	writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
	runPolicyGit(t, root, "add", ".entire/settings.json")
	runPolicyGit(t, root, "commit", "--no-gpg-sign", "-m", "tracked settings")

	policy := policyAt(t, root)
	if policy.Active || policy.ActivationSource != ActivationInactive {
		t.Fatalf("policy = %+v, want inactive fresh clone", policy)
	}
}

func TestClassifyRepoPolicy_LocalMarkerActivatesOnlyItsWorktree(t *testing.T) {
	root, mainRepo := newPolicyRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runPolicyGit(t, root, "worktree", "add", "-b", "linked-policy", linked)
	linkedRepo, err := ResolveRepositoryAt(t.Context(), linked)
	if err != nil {
		t.Fatal(err)
	}
	setPolicyGlobal(t, "")

	if err := SetLocalActivationForRepository(mainRepo, ActivationEnabled); err != nil {
		t.Fatalf("SetLocalActivationForRepository: %v", err)
	}
	assertActivationRecordModes(t, mainRepo)
	mainPolicy := policyAt(t, root)
	if !mainPolicy.Active || mainPolicy.ActivationSource != ActivationLocal {
		t.Fatalf("main policy = %+v, want local activation", mainPolicy)
	}
	linkedPolicy := policyAt(t, linked)
	if linkedPolicy.Active || linkedPolicy.ActivationSource != ActivationInactive {
		t.Fatalf("linked policy = %+v, marker from %+v must not apply", linkedPolicy, mainRepo)
	}
	if mainRepo.WorktreeKey == linkedRepo.WorktreeKey {
		t.Fatal("test setup produced identical worktree keys")
	}
}

func assertActivationRecordModes(t *testing.T, repository Repository) {
	t.Helper()
	dirInfo, err := os.Stat(registryDir(repository))
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(activationPath(repository))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("registry mode = %o, want 700", got)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("activation mode = %o, want 600", got)
	}
}

func TestClassifyRepoPolicy_DisabledMarkerVetoesGlobalActivation(t *testing.T) {
	root, repository := newPolicyRepo(t)
	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	if err := SetLocalActivationForRepository(repository, ActivationDisabled); err != nil {
		t.Fatal(err)
	}

	policy := policyAt(t, root)
	if policy.Active || policy.ActivationSource != ActivationInactive || policy.InactiveReason != InactiveReasonRepoDisabled {
		t.Fatalf("policy = %+v, want local disabled veto", policy)
	}
}

func TestClassifyRepoPolicy_TrackedLocalDisableCannotVetoGlobal(t *testing.T) {
	root, _ := newPolicyRepo(t)
	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	writePolicyFile(t, root, ".entire/settings.local.json", `{"enabled":false}`)
	runPolicyGit(t, root, "add", "-f", ".entire/settings.local.json")

	policy := policyAt(t, root)
	if !policy.Active || policy.ActivationSource != ActivationGlobal {
		t.Fatalf("policy = %+v, tracked local content must not veto global activation", policy)
	}
}

func TestClassifyRepoPolicy_CorruptActivationFailsClosedUntilExplicitEnable(t *testing.T) {
	root, repository := newPolicyRepo(t)
	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	if err := ensureRegistryDir(repository); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activationPath(repository), []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	policy, err := ClassifyRepoPolicy(t.Context())
	if err == nil || policy.Active || policy.ActivationSource != ActivationInactive {
		t.Fatalf("ClassifyRepoPolicy = %+v, %v; want inactive error", policy, err)
	}
	if err := SetLocalActivationForRepository(repository, ActivationDisabled); err == nil {
		t.Fatal("explicit disable must not replace a corrupt record")
	}
	if err := SetLocalActivationForRepository(repository, ActivationEnabled); err != nil {
		t.Fatalf("explicit enable must recover corrupt record: %v", err)
	}
	policy, err = ClassifyRepoPolicy(t.Context())
	if err != nil || !policy.Active || policy.ActivationSource != ActivationLocal {
		t.Fatalf("recovered policy = %+v, %v; want local activation", policy, err)
	}
}

func TestBootstrapLegacyActivation_SingleWorktreeRequiresVerifiedEvidence(t *testing.T) {
	t.Parallel()
	t.Run("enabled settings alone are insufficient", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
		activated, err := BootstrapLegacyActivation(t.Context(), repository)
		if err != nil {
			t.Fatal(err)
		}
		if activated {
			t.Fatal("settings alone must not bootstrap local activation")
		}
	})

	t.Run("git-dir Entire hook is not activation evidence", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
		writePolicyFile(t, repository.GitCommonDir, "hooks/pre-commit", "#!/bin/sh\n# Entire CLI hooks\nentire hooks git pre-commit\n")
		activated, err := BootstrapLegacyActivation(t.Context(), repository)
		if err != nil {
			t.Fatal(err)
		}
		if activated {
			t.Fatal("Git-dir hook must not bootstrap activation")
		}
	})

	t.Run("exact active session record is accepted", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
		body := `{"session_id":"session-1","phase":"active","worktree_path":` + quoteJSON(repository.WorktreeRoot) + `,"worktree_id":` + quoteJSON(repository.WorktreeID) + `}`
		writePolicyFile(t, repository.GitCommonDir, "entire-sessions/session-1.json", body)
		activated, err := BootstrapLegacyActivation(t.Context(), repository)
		if err != nil || !activated {
			t.Fatalf("BootstrapLegacyActivation = %v, %v; want true", activated, err)
		}
	})

	t.Run("legacy active_committed session record is accepted", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
		body := `{"session_id":"session-1","phase":"active_committed","worktree_path":` + quoteJSON(repository.WorktreeRoot) + `,"worktree_id":` + quoteJSON(repository.WorktreeID) + `}`
		writePolicyFile(t, repository.GitCommonDir, "entire-sessions/session-1.json", body)
		activated, err := BootstrapLegacyActivation(t.Context(), repository)
		if err != nil || !activated {
			t.Fatalf("BootstrapLegacyActivation = %v, %v; want true", activated, err)
		}
	})

	t.Run("untracked local enabled setting plus exact evidence is accepted", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.local.json", `{"enabled":true}`)
		writePolicyFile(t, root, ".entire/metadata/session-1/prompt.txt", "prompt")
		activated, err := BootstrapLegacyActivation(t.Context(), repository)
		if err != nil || !activated {
			t.Fatalf("BootstrapLegacyActivation = %v, %v; want true", activated, err)
		}
	})
}

func TestBootstrapLegacyActivation_RejectsNonActiveSessionPhases(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"", "idle", "ended", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			root, repository := newPolicyRepo(t)
			writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
			body := `{"session_id":"session-1","phase":` + quoteJSON(phase) + `,"worktree_path":` + quoteJSON(repository.WorktreeRoot) + `,"worktree_id":` + quoteJSON(repository.WorktreeID) + `}`
			writePolicyFile(t, repository.GitCommonDir, "entire-sessions/session-1.json", body)

			activated, err := BootstrapLegacyActivation(t.Context(), repository)
			if err != nil {
				t.Fatal(err)
			}
			if activated {
				t.Fatalf("phase %q must not bootstrap activation", phase)
			}
		})
	}
}

func TestBootstrapLegacyActivation_LinkedWorktreeRejectsSharedHookEvidence(t *testing.T) {
	t.Parallel()
	root, mainRepo := newPolicyRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runPolicyGit(t, root, "worktree", "add", "-b", "linked-hook", linked)
	linkedRepo, err := ResolveRepositoryAt(t.Context(), linked)
	if err != nil {
		t.Fatal(err)
	}
	writePolicyFile(t, linked, ".entire/settings.json", `{"enabled":true}`)
	writePolicyFile(t, mainRepo.GitCommonDir, "hooks/pre-commit", "#!/bin/sh\n# Entire CLI hooks\n")

	activated, err := BootstrapLegacyActivation(t.Context(), linkedRepo)
	if err != nil {
		t.Fatal(err)
	}
	if activated {
		t.Fatal("shared Git hook must not bootstrap a linked worktree")
	}
}

func TestBootstrapLegacyActivation_RejectsTrackedOrSymlinkedEvidence(t *testing.T) {
	t.Parallel()
	t.Run("tracked metadata file", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
		writePolicyFile(t, root, ".entire/metadata/session-1/prompt.txt", "prompt")
		runPolicyGit(t, root, "add", "-f", ".entire/metadata/session-1/prompt.txt")
		activated, err := BootstrapLegacyActivation(t.Context(), repository)
		if err != nil {
			t.Fatal(err)
		}
		if activated {
			t.Fatal("tracked runtime evidence must not bootstrap")
		}
	})

	t.Run("symlinked metadata file", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
		outside := filepath.Join(t.TempDir(), "prompt.txt")
		if err := os.WriteFile(outside, []byte("prompt"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadataDir := filepath.Join(root, ".entire", "metadata", "session-1")
		if err := os.MkdirAll(metadataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(metadataDir, "prompt.txt")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		activated, err := BootstrapLegacyActivation(t.Context(), repository)
		if err != nil {
			t.Fatal(err)
		}
		if activated {
			t.Fatal("symlinked runtime evidence must not bootstrap")
		}
	})

	t.Run("corrupt activation record is not recovered implicitly", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
		writePolicyFile(t, root, ".entire/metadata/session-1/prompt.txt", "prompt")
		if err := ensureRegistryDir(repository); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(activationPath(repository), []byte(`{"version":999}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if activated, err := BootstrapLegacyActivation(t.Context(), repository); err == nil || activated {
			t.Fatalf("BootstrapLegacyActivation = %v, %v; corrupt marker requires explicit enable", activated, err)
		}
	})
}

func runPolicyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initPolicyRepo(t *testing.T, root string) {
	t.Helper()
	runPolicyGit(t, root, "init")
	runPolicyGit(t, root, "config", "user.name", "Entire Test")
	runPolicyGit(t, root, "config", "user.email", "test@entire.invalid")
	runPolicyGit(t, root, "config", "commit.gpgsign", "false")
}

func writePolicyFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quoteJSON(value string) string {
	return `"` + filepath.ToSlash(value) + `"`
}
