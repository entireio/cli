package cli

import (
	"bytes"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
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
		{name: "change id", raw: "01JABCDEF", want: "01JABCDEF"},
		{name: "change URL number", raw: "https://entire.io/gh/entireio/cli/changes/604/review-target", want: "604", wantURL: true},
		{name: "change URL id", raw: "https://app.entire.io/gh/entireio/cli/changes/01JABCDEF", want: "01JABCDEF", wantURL: true},
		{name: "wrong repo", raw: "https://entire.io/gh/acme/other/changes/7/topic", wantURL: true, wantErr: true},
		{name: "non Entire URL", raw: "https://example.com/gh/entireio/cli/changes/7", wantURL: true, wantErr: true},
		{name: "malformed change URL", raw: "https://entire.io/gh/entireio/cli/changes", wantURL: true, wantErr: true},
		// Legacy "/trails/" spelling: old links (GitHub PR bodies, chat
		// history, ...) name it that way forever. entire.io redirects
		// /trails/ to /changes/ only in the browser, never through this
		// parser, so both spellings must keep working here.
		{name: "legacy trail URL number", raw: "https://entire.io/gh/entireio/cli/trails/604/review-target", want: "604", wantURL: true},
		{name: "legacy trail URL id", raw: "https://app.entire.io/gh/entireio/cli/trails/01JABCDEF", want: "01JABCDEF", wantURL: true},
		{name: "legacy trail URL wrong repo", raw: "https://entire.io/gh/acme/other/trails/7/topic", wantURL: true, wantErr: true},
		{name: "legacy trail URL malformed", raw: "https://entire.io/gh/entireio/cli/trails", wantURL: true, wantErr: true},
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

// The URL the CLI itself prints (trailReviewWebURL, review_bridge.go) must be
// accepted back by the --target parser it feeds into: a user copy-pasting the
// CLI's own "View the change" link should never hit "invalid Entire change
// URL". This is the round trip the "changes" vs "trails" split above doesn't
// pin on its own, since it only exercises hand-written fixtures.
func TestNormalizeReviewTargetSelector_AcceptsGeneratedChangeURL(t *testing.T) {
	t.Parallel()

	target := changeReviewTarget{
		Host:  "gh",
		Owner: "entireio",
		Repo:  "cli",
		Change: api.ChangeResource{
			Number: 604,
			Branch: "review-target",
		},
	}
	generated := trailReviewWebURL(target)
	if generated == "" {
		t.Fatal("trailReviewWebURL returned empty URL for a valid target")
	}

	got, gotURL, err := normalizeReviewTargetSelector(generated, target.Host, target.Owner, target.Repo)
	if err != nil {
		t.Fatalf("normalizeReviewTargetSelector(%q) unexpected error: %v", generated, err)
	}
	if !gotURL {
		t.Fatalf("normalizeReviewTargetSelector(%q) gotURL = false, want true", generated)
	}
	if want := "604"; got != want {
		t.Fatalf("normalizeReviewTargetSelector(%q) = %q, want %q", generated, got, want)
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
