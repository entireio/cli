package repopolicy

import (
	"path/filepath"
	"testing"
)

func TestRepositorySelection(t *testing.T) {
	// Changes process environment and working directory.
	current, currentIdentity := newPolicyRepo(t)
	target, targetIdentity := newPolicyRepo(t)
	runPolicyGit(t, target, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	t.Chdir(target)
	t.Setenv("GIT_DIR", filepath.Join(current, ".git"))
	t.Setenv("GIT_WORK_TREE", current)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(current, ".git", "index"))
	setPolicyGlobal(t, `{"global":{"enabled":true,"exclude_origins":["github.com/acme/widgets"]}}`)

	got, err := ResolveRepository(t.Context())
	if err != nil || got != currentIdentity {
		t.Fatalf("current identity = %+v, %v; want %+v", got, err, currentIdentity)
	}
	got, err = ResolveRepositoryAt(t.Context(), target)
	if err != nil || got != targetIdentity {
		t.Errorf("target identity = %+v, %v; want %+v", got, err, targetIdentity)
	}
	policy, err := ClassifyRepoPolicy(t.Context())
	if err != nil || policy.WorktreeRoot != currentIdentity.WorktreeRoot || !policy.Active {
		t.Errorf("current policy = %+v, %v", policy, err)
	}
	policy, err = ClassifyActivationAt(t.Context(), target)
	if err != nil || policy.WorktreeRoot != targetIdentity.WorktreeRoot || policy.Active || policy.InactiveReason != InactiveReasonGlobalExcluded {
		t.Errorf("target policy = %+v, %v", policy, err)
	}
}
