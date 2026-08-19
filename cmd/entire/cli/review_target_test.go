package cli

import (
	"bytes"
	"testing"
)

func TestNormalizeReviewTargetSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantURL bool
		wantErr bool
	}{
		{name: "branch", raw: "feature/review-me", want: "feature/review-me"},
		{name: "trail id", raw: "01JABCDEF", want: "01JABCDEF"},
		{name: "trail URL number", raw: "https://entire.io/gh/entireio/cli/trails/604/review-target", want: "604", wantURL: true},
		{name: "trail URL id", raw: "https://app.entire.io/gh/entireio/cli/trails/01JABCDEF", want: "01JABCDEF", wantURL: true},
		{name: "wrong repo", raw: "https://entire.io/gh/acme/other/trails/7/topic", wantURL: true, wantErr: true},
		{name: "non Entire URL", raw: "https://example.com/gh/entireio/cli/trails/7", wantURL: true, wantErr: true},
		{name: "malformed trail URL", raw: "https://entire.io/gh/entireio/cli/trails", wantURL: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, gotURL, err := normalizeReviewTargetSelector(tt.raw, "gh", "entireio", "cli")
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeReviewTargetSelector() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want || gotURL != tt.wantURL {
				t.Fatalf("normalizeReviewTargetSelector() = (%q, %v), want (%q, %v)", got, gotURL, tt.want, tt.wantURL)
			}
		})
	}
}

func TestPrepareReviewTargetLocalBranchDoesNotRequireRemote(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	target, err := prepareReviewTarget(t.Context(), &out, &errOut, currentBranchInDir(t, repoDir))
	if err != nil {
		t.Fatalf("prepareReviewTarget: %v; stderr: %s", err, errOut.String())
	}
	if normalizeWorktreePath(target.Path) != normalizeWorktreePath(repoDir) || target.Created {
		t.Fatalf("target = %+v, want reused main worktree %s", target, repoDir)
	}
}

func TestReviewTargetMayBeBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		selector string
		want     bool
	}{
		{selector: "feature/review", want: true},
		{selector: "trail-id", want: true},
		{selector: "42", want: false},
		{selector: "https://entire.io/gh/entireio/cli/trails/42", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.selector, func(t *testing.T) {
			t.Parallel()
			if got := reviewTargetMayBeBranch(tt.selector); got != tt.want {
				t.Fatalf("reviewTargetMayBeBranch(%q) = %v, want %v", tt.selector, got, tt.want)
			}
		})
	}
}

func TestDefaultReviewWorktreePathDistinguishesLossyBranchNames(t *testing.T) {
	t.Parallel()

	a := defaultReviewWorktreePath("/repo", "feature/x")
	b := defaultReviewWorktreePath("/repo", "feature-x")
	if a == b {
		t.Fatalf("lossy branch names produced the same review worktree path: %s", a)
	}
}
