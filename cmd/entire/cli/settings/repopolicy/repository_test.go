package repopolicy

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyGlobalConfig_DisabledDoesNotResolveRepository(t *testing.T) {
	t.Parallel()
	resolver := func(context.Context) (Repository, error) {
		t.Fatal("disabled or unconfigured global policy must not resolve the repository")
		return Repository{}, errors.New("unexpected repository resolution")
	}
	for _, config := range []*GlobalConfig{nil, {Enabled: false}} {
		policy, err := ClassifyGlobalConfig(t.Context(), config, resolver)
		if err != nil {
			t.Fatalf("ClassifyGlobalConfig: %v", err)
		}
		if policy.Active || policy.ActivationSource != ActivationInactive || policy.InactiveReason != InactiveReasonGlobalOff {
			t.Fatalf("policy = %+v, want inactive global-off", policy)
		}
	}
}

func TestRuntimeLayoutValues(t *testing.T) {
	t.Parallel()

	if RuntimeUnknown != "unknown" {
		t.Fatalf("RuntimeUnknown = %q, want %q", RuntimeUnknown, "unknown")
	}
}

func TestResolveRepository_MainAndLinkedWorktree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initPolicyRepo(t, root)
	writePolicyFile(t, root, "README.md", "test\n")
	runPolicyGit(t, root, "add", "README.md")
	runPolicyGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")

	mainRepo, err := ResolveRepositoryAt(t.Context(), root)
	if err != nil {
		t.Fatalf("ResolveRepositoryAt(main): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if mainRepo.WorktreeRoot != wantRoot {
		t.Fatalf("main worktree root = %q, want %q", mainRepo.WorktreeRoot, wantRoot)
	}
	if mainRepo.WorktreeID != "" || mainRepo.WorktreeKey != HashWorktreeID("") {
		t.Fatalf("main repository identity = %+v", mainRepo)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	cmd := exec.CommandContext(t.Context(), "git", "worktree", "add", "-b", "linked-test", linked)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	linkedRepo, err := ResolveRepositoryAt(t.Context(), linked)
	if err != nil {
		t.Fatalf("ResolveRepositoryAt(linked): %v", err)
	}
	if linkedRepo.WorktreeID == "" {
		t.Fatalf("linked worktree identity = %+v, want non-empty ID", linkedRepo)
	}
	if linkedRepo.WorktreeKey != HashWorktreeID(linkedRepo.WorktreeID) {
		t.Fatalf("linked worktree key = %q, want hash of %q", linkedRepo.WorktreeKey, linkedRepo.WorktreeID)
	}
	if linkedRepo.GitCommonDir != mainRepo.GitCommonDir {
		t.Fatalf("linked common dir = %q, want %q", linkedRepo.GitCommonDir, mainRepo.GitCommonDir)
	}
}

func TestClassifyGlobalConfig_UnnormalizableOriginDoesNotLeakCredentials(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initPolicyRepo(t, root)
	const (
		secret = "super-secret"
		origin = "https://user:super-secret@example.com"
	)
	cmd := exec.CommandContext(t.Context(), "git", "remote", "add", "origin", origin)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}

	policy, err := ClassifyGlobalConfig(t.Context(), &GlobalConfig{
		Enabled:        true,
		ExcludeOrigins: []string{"github.com/**"},
	}, func(context.Context) (Repository, error) {
		return Repository{WorktreeRoot: root}, nil
	})
	if err == nil {
		t.Fatal("unnormalizable origin must return an error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), origin) {
		t.Fatalf("error leaks origin credentials: %v", err)
	}
	if policy.Active || policy.InactiveReason != InactiveReasonGlobalExcluded {
		t.Fatalf("policy = %+v, want inactive global exclusion", policy)
	}
}
