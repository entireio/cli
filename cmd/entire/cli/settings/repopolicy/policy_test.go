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
