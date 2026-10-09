package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

func TestResolveReviewTargetLocalBranchDoesNotRequireRemote(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	resolved, err := resolveReviewTarget(t.Context(), &out, &errOut, currentBranchInDir(t, repoDir))
	if err != nil {
		t.Fatalf("resolveReviewTarget: %v; stderr: %s", err, errOut.String())
	}
	if normalizeWorktreePath(resolved.ExistingWorktree) != normalizeWorktreePath(repoDir) {
		t.Fatalf("resolved = %+v, want reused main worktree %s", resolved, repoDir)
	}
	if want := gitOutputInDir(t, repoDir, "rev-parse", "HEAD"); resolved.HeadSHA != want {
		t.Fatalf("HeadSHA = %q, want %q", resolved.HeadSHA, want)
	}
	target, err := checkoutReviewTarget(t.Context(), &out, &errOut, resolved, true)
	if err != nil {
		t.Fatalf("checkoutReviewTarget: %v", err)
	}
	if normalizeWorktreePath(target.Path) != normalizeWorktreePath(repoDir) || target.Created {
		t.Fatalf("target = %+v, want reused main worktree %s", target, repoDir)
	}
}

// An untrusted checkout must not run the branch's git hooks or copy
// .worktreeinclude files; a trusted one keeps today's behavior.
func TestCheckoutReviewTargetUntrustedSkipsHooksAndIncludes(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runGit(t, repoDir, "branch", "feature/untrusted")
	runGit(t, repoDir, "branch", "feature/trusted")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", ".env\n")
	testutil.WriteFile(t, repoDir, ".env", "SECRET=1\n")
	testutil.WriteFile(t, repoDir, ".gitignore", ".env\n.entire/\n")
	testutil.GitAdd(t, repoDir, ".worktreeinclude", ".gitignore")
	testutil.GitCommit(t, repoDir, "add include config")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o750); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\necho ran >> " + marker + "\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	resolved, err := resolveReviewTarget(t.Context(), &out, &errOut, "feature/untrusted")
	if err != nil {
		t.Fatalf("resolveReviewTarget: %v; stderr: %s", err, errOut.String())
	}
	if resolved.ExistingWorktree != "" {
		t.Fatalf("ExistingWorktree = %q, want none", resolved.ExistingWorktree)
	}
	target, err := checkoutReviewTarget(t.Context(), &out, &errOut, resolved, true)
	if err != nil {
		t.Fatalf("checkoutReviewTarget: %v; stderr: %s", err, errOut.String())
	}
	if !target.Created {
		t.Fatalf("target = %+v, want a new worktree", target)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("post-checkout hook ran for an untrusted checkout (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(target.Path, ".env")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf(".worktreeinclude file was copied into an untrusted checkout (stat err %v)", err)
	}

	resolved, err = resolveReviewTarget(t.Context(), &out, &errOut, "feature/trusted")
	if err != nil {
		t.Fatalf("resolveReviewTarget: %v", err)
	}
	if _, err := checkoutReviewTarget(t.Context(), &out, &errOut, resolved, false); err != nil {
		t.Fatalf("checkoutReviewTarget trusted: %v; stderr: %s", err, errOut.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post-checkout hook did not run for a trusted checkout: %v", err)
	}
}

func gitOutputInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
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
