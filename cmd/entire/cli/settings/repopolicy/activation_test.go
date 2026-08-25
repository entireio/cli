package repopolicy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/gitpath"
)

const windowsGOOS = "windows"

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
	if runtime.GOOS == windowsGOOS {
		return
	}
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

func TestEnsureRegistryDir_SyncsEveryPublishedDirectory(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	var synced []string
	if err := ensureRegistryDirWithSync(repository, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		repository.GitCommonDir,
		filepath.Join(repository.GitCommonDir, "entire"),
		filepath.Join(repository.GitCommonDir, "entire", "worktree"),
	}
	if !reflectStringSlicesEqual(synced, want) {
		t.Fatalf("synced = %q, want %q", synced, want)
	}
}

func TestEnsureRegistryDir_SyncFailureFailsClosed(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	wantErr := errors.New("sync failed")
	if err := ensureRegistryDirWithSync(repository, func(string) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("ensureRegistryDirWithSync error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(filepath.Join(repository.GitCommonDir, "entire", "worktree")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry creation continued after sync failure: %v", err)
	}
}

func reflectStringSlicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if filepath.Clean(got[i]) != filepath.Clean(want[i]) {
			return false
		}
	}
	return true
}

func TestLegacyMetadataEvidence_LoadsTrackedSetOnceAndHonorsScanBudget(t *testing.T) {
	t.Parallel()
	t.Run("one tracked query for many candidates", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		for i := range 64 {
			writePolicyFile(t, root, filepath.Join(".entire", "metadata", fmt.Sprintf("session-%03d", i), "prompt.txt"), "prompt")
		}
		calls := 0
		options := defaultLegacyMetadataOptions()
		options.scanBudget = time.Minute
		options.loadTracked = func(context.Context, Repository) (map[string]struct{}, error) {
			calls++
			tracked := make(map[string]struct{}, 64)
			for i := range 64 {
				path := filepath.ToSlash(filepath.Join(".entire", "metadata", fmt.Sprintf("session-%03d", i), "prompt.txt"))
				tracked[gitpath.CanonicalKey(path)] = struct{}{}
			}
			return tracked, nil
		}
		found, err := hasRecognizedMetadataEvidenceWithOptions(t.Context(), repository, options)
		if err != nil || found {
			t.Fatalf("evidence = %v, %v; want none", found, err)
		}
		if calls != 1 {
			t.Fatalf("tracked metadata queries = %d, want 1", calls)
		}
	})

	t.Run("expired filesystem budget fails closed", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/metadata/session-1/prompt.txt", "prompt")
		options := defaultLegacyMetadataOptions()
		base := time.Now()
		calls := 0
		options.now = func() time.Time {
			calls++
			if calls == 1 {
				return base
			}
			return base.Add(options.scanBudget + time.Second)
		}
		found, err := hasRecognizedMetadataEvidenceWithOptions(t.Context(), repository, options)
		if found || !errors.Is(err, errLegacyMetadataBudget) {
			t.Fatalf("evidence = %v, %v; want budget failure", found, err)
		}
	})

	t.Run("canceled context stops cooperative scan", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		writePolicyFile(t, root, ".entire/metadata/session-1/prompt.txt", "prompt")
		options := defaultLegacyMetadataOptions()
		options.loadTracked = func(context.Context, Repository) (map[string]struct{}, error) {
			return map[string]struct{}{}, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		found, err := hasRecognizedMetadataEvidenceWithOptions(ctx, repository, options)
		if found || !errors.Is(err, context.Canceled) {
			t.Fatalf("evidence = %v, %v; want canceled cooperative scan", found, err)
		}
	})
}

func TestLegacyEvidence_RejectsDeterministicFileSwaps(t *testing.T) {
	t.Parallel()
	t.Run("session record", func(t *testing.T) {
		t.Parallel()
		_, repository := newPolicyRepo(t)
		rel := "entire-sessions/session-1.json"
		writePolicyFile(t, repository.GitCommonDir, rel, `{"phase":"active","worktree_path":"/not-this-worktree","worktree_id":"wrong"}`)
		found, err := hasExactSessionEvidenceWithHooks(repository, func(string) verifiedReadHooks {
			return verifiedReadHooks{afterOpen: func() {
				path := filepath.Join(repository.GitCommonDir, filepath.FromSlash(rel))
				if err := os.Rename(path, path+".validated"); err != nil {
					t.Fatal(err)
				}
				body := `{"phase":"active","worktree_path":` + quoteJSON(repository.WorktreeRoot) + `,"worktree_id":` + quoteJSON(repository.WorktreeID) + `}`
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}}
		})
		if err != nil || found {
			t.Fatalf("session evidence = %v, %v; swapped record must not be accepted", found, err)
		}
	})

	t.Run("metadata file", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		rel := ".entire/metadata/session-1/prompt.txt"
		writePolicyFile(t, root, rel, "validated")
		options := defaultLegacyMetadataOptions()
		options.readHooks = func(string) verifiedReadHooks {
			return verifiedReadHooks{afterOpen: func() {
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.Rename(path, path+".validated"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("swapped"), 0o600); err != nil {
					t.Fatal(err)
				}
			}}
		}
		found, err := hasRecognizedMetadataEvidenceWithOptions(t.Context(), repository, options)
		if err != nil || found {
			t.Fatalf("metadata evidence = %v, %v; swapped file must not be accepted", found, err)
		}
	})
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

func TestClassifyRepoPolicy_ExcludedPushOriginVetoesGlobal(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	runPolicyGit(t, root, "remote", "set-url", "--push", "origin", "git@codeberg.org:acme/widgets.git")
	setPolicyGlobal(t, `{"global":{"enabled":true,"exclude_origins":["codeberg.org/acme/**"]}}`)

	policy := policyAt(t, root)
	if policy.Active || policy.InactiveReason != InactiveReasonGlobalExcluded {
		t.Fatalf("policy = %+v, push origin exclusion must disable global tracking", policy)
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

	for _, phase := range []string{"active", "active_committed"} {
		t.Run("session phase "+phase+" is accepted", func(t *testing.T) {
			t.Parallel()
			root, repository := newPolicyRepo(t)
			writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
			body := `{"session_id":"session-1","phase":` + quoteJSON(phase) + `,"worktree_path":` + quoteJSON(repository.WorktreeRoot) + `,"worktree_id":` + quoteJSON(repository.WorktreeID) + `}`
			writePolicyFile(t, repository.GitCommonDir, "entire-sessions/session-1.json", body)
			activated, err := BootstrapLegacyActivation(t.Context(), repository)
			if err != nil || !activated {
				t.Fatalf("BootstrapLegacyActivation = %v, %v; want true", activated, err)
			}
		})
	}

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

	for _, alias := range []string{
		".Entire/Metadata/Session-1/PROMPT.TXT",
		".entire/metadata/session-1/prompt.txt.",
	} {
		t.Run("filesystem-equivalent tracked metadata alias "+alias, func(t *testing.T) {
			t.Parallel()
			root, repository := newPolicyRepo(t)
			writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
			canonical := ".entire/metadata/session-1/prompt.txt"
			writePolicyFile(t, root, canonical, "prompt")
			blob := strings.TrimSpace(runPolicyGitOutput(t, root, "hash-object", "-w", canonical))
			runPolicyGit(t, root, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+alias)

			activated, err := BootstrapLegacyActivation(t.Context(), repository)
			if err != nil {
				t.Fatal(err)
			}
			if activated {
				t.Fatalf("tracked metadata alias %q bypassed activation consent", alias)
			}
		})
	}

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

func TestRebindMovedRepository_RequiresMovedPathAndActiveSession(t *testing.T) {
	t.Parallel()
	t.Run("moved repository rebinds identity-bound records", func(t *testing.T) {
		t.Parallel()
		root, repository := newPolicyRepo(t)
		if err := SetLocalActivationForRepository(repository, ActivationEnabled); err != nil {
			t.Fatal(err)
		}
		policy := RepoPolicy{
			Active:           true,
			ActivationSource: ActivationLocal,
			WorktreeRoot:     repository.WorktreeRoot,
			GitCommonDir:     repository.GitCommonDir,
			WorktreeKey:      repository.WorktreeKey,
			Route:            proposedRoute(repository, RuntimeWorktree),
		}
		if _, err := EnsureRuntimeRoute(t.Context(), policy); err != nil {
			t.Fatal(err)
		}
		if err := WriteSetupRecord(repository, SetupRecord{GitHooksSpec: 1, PrimaryRefSpec: 1}); err != nil {
			t.Fatal(err)
		}
		body := `{"session_id":"session-1","phase":"active","worktree_path":` + quoteJSON(repository.WorktreeRoot) + `,"worktree_id":` + quoteJSON(repository.WorktreeID) + `}`
		writePolicyFile(t, repository.GitCommonDir, "entire-sessions/session-1.json", body)

		moved := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-moved")
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(moved) })
		movedRepository, err := ResolveRepositoryAt(t.Context(), moved)
		if err != nil {
			t.Fatal(err)
		}
		rebound, err := RebindMovedRepository(t.Context(), movedRepository)
		if err != nil || !rebound {
			t.Fatalf("RebindMovedRepository = %v, %v; want true", rebound, err)
		}
		if state, err := ReadLocalActivation(movedRepository); err != nil || state != ActivationEnabled {
			t.Fatalf("activation after move = %q, %v", state, err)
		}
		if _, found, err := ReadRuntimeRoute(movedRepository); err != nil || !found {
			t.Fatalf("route after move = found %v, %v", found, err)
		}
		if _, found, err := ReadSetupRecord(movedRepository); err != nil || !found {
			t.Fatalf("setup after move = found %v, %v", found, err)
		}
	})

	t.Run("existing source path rejects copied registry", func(t *testing.T) {
		t.Parallel()
		_, repository := newPolicyRepo(t)
		if err := SetLocalActivationForRepository(repository, ActivationEnabled); err != nil {
			t.Fatal(err)
		}
		body := `{"session_id":"session-1","phase":"active","worktree_path":` + quoteJSON(repository.WorktreeRoot) + `,"worktree_id":` + quoteJSON(repository.WorktreeID) + `}`
		writePolicyFile(t, repository.GitCommonDir, "entire-sessions/session-1.json", body)
		copiedRoot := t.TempDir()
		copied := repository
		copied.WorktreeRoot = canonicalPath(copiedRoot)

		rebound, err := RebindMovedRepository(t.Context(), copied)
		if err != nil {
			t.Fatal(err)
		}
		if rebound {
			t.Fatal("copied registry must not rebind while its source worktree still exists")
		}
	})
}

func TestRelocationSourceVacatedOrSame(t *testing.T) {
	t.Parallel()

	current := t.TempDir()
	if ok, err := relocationSourceVacatedOrSame(current, current); err != nil || !ok {
		t.Fatalf("same filesystem identity = %v, %v; want true", ok, err)
	}

	copied := t.TempDir()
	if ok, err := relocationSourceVacatedOrSame(current, copied); err != nil || ok {
		t.Fatalf("different filesystem identity = %v, %v; want false", ok, err)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if ok, err := relocationSourceVacatedOrSame(missing, current); err != nil || !ok {
		t.Fatalf("missing source = %v, %v; want true", ok, err)
	}
}

func runPolicyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runPolicyGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
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
