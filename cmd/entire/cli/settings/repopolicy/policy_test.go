package repopolicy

import (
	"path/filepath"
	"testing"
)

// TestRepoPolicy_RuntimeRoot: layout is a pure function of the activation
// source — global routes under the git common dir, everything else keeps
// main's <worktree>/.entire.
func TestRepoPolicy_RuntimeRoot(t *testing.T) {
	t.Parallel()
	base := RepoPolicy{WorktreeRoot: "/repo", GitCommonDir: "/repo/.git", WorktreeKey: "abc123"}
	tests := []struct {
		source ActivationSource
		want   string
	}{
		{ActivationGlobal, filepath.Join("/repo/.git", "entire", "worktree", "abc123")},
		{ActivationLocal, filepath.Join("/repo", ".entire")},
		{ActivationInactive, filepath.Join("/repo", ".entire")},
	}
	for _, tc := range tests {
		policy := base
		policy.ActivationSource = tc.source
		if got := policy.RuntimeRoot(); got != tc.want {
			t.Errorf("%s: RuntimeRoot = %q, want %q", tc.source, got, tc.want)
		}
	}
}

// ClassifyActivationAt is the foreign-repo question session binding asks: it
// must classify the given root, not the process cwd, cover the global tier
// (no settings files), honor exclusions and vetoes, and leave Trust untouched.
// Not parallel: uses t.Chdir().
func TestClassifyActivationAt_ForeignRootFromAnotherCwd(t *testing.T) {
	cwdRepo, _ := newPolicyRepo(t)
	cwdRepo = filepath.Clean(cwdRepo)
	foreign, repository := newPolicyRepo(t)
	runPolicyGit(t, foreign, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	t.Chdir(cwdRepo)

	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	got, err := ClassifyActivationAt(t.Context(), foreign)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Active || got.ActivationSource != ActivationGlobal || got.WorktreeRoot != repository.WorktreeRoot {
		t.Fatalf("policy = %+v, want the FOREIGN root active via the global tier", got)
	}
	if tr := got.Trust; tr.Allowed || tr.Source != "" || tr.Reason != "" || tr.Identity.OriginKeyed() || tr.Identity.Path != "" {
		t.Fatalf("trust = %+v, want untouched: activation-only must not resolve the (cwd-scoped) egress", got.Trust)
	}

	setPolicyGlobal(t, `{"global":{"enabled":true,"exclude_paths":["`+filepath.ToSlash(repository.WorktreeRoot)+`"]}}`)
	if got, err := ClassifyActivationAt(t.Context(), foreign); err != nil || got.Active || got.InactiveReason != InactiveReasonGlobalExcluded {
		t.Fatalf("policy = %+v (err %v), want excluded", got, err)
	}

	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	writePolicyFile(t, foreign, ".entire/settings.json", `{"enabled":false}`)
	if got, err := ClassifyActivationAt(t.Context(), foreign); err != nil || got.Active || got.InactiveReason != InactiveReasonRepoDisabled {
		t.Fatalf("policy = %+v (err %v), want vetoed", got, err)
	}
	writePolicyFile(t, foreign, ".entire/settings.json", `{"enabled":true}`)
	if got, err := ClassifyActivationAt(t.Context(), foreign); err != nil || !got.Active || got.ActivationSource != ActivationLocal {
		t.Fatalf("policy = %+v (err %v), want repo-enabled", got, err)
	}
}
