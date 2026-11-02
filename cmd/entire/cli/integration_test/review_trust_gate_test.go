//go:build integration && !windows

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The review trust gate, driven through the real binary: a branch authored by
// someone else commits agent configuration and a git hook that each write a
// marker. Nothing from the branch may run, and nothing may be checked out,
// before the user approves; after approval the checkout still runs none of the
// branch's git hooks. No agent CLI is on PATH, so no reviewer can start and no
// test here spends tokens.

type trustGateRepo struct {
	env        *TestEnv
	head       string
	hookMarker string
	gitMarker  string
}

func newTrustGateRepo(t *testing.T) *trustGateRepo {
	t.Helper()
	env := NewRepoWithCommit(t)
	testutil.RunGit(t, env.RepoDir, "branch", "-M", "main")

	prefs, err := json.Marshal(map[string]any{
		"review_default_profile": "gate",
		"review_profiles": map[string]any{
			"gate": map[string]any{
				"task":   "Review.",
				"agents": map[string]any{"claude-code": map[string]any{"skills": []string{"/review"}}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.WriteFile(filepath.Join(".git", "entire", "preferences.json"), string(prefs))

	markers := t.TempDir()
	repo := &trustGateRepo{
		env:        env,
		hookMarker: filepath.Join(markers, "agent-hook-ran"),
		gitMarker:  filepath.Join(markers, "git-hook-ran"),
	}
	testutil.RunGit(t, env.RepoDir, "checkout", "-b", "feature")
	settings := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo ran >> '` + repo.hookMarker + `'"}]}]}}`
	env.WriteFile(filepath.Join(".claude", "settings.json"), settings)
	testutil.RunGit(t, env.RepoDir, "add", ".claude/settings.json")
	testutil.RunGit(t, env.RepoDir, "commit", "-m", "add hook", "--author", "Mallory <mallory@example.com>")
	repo.head = strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "HEAD"))
	testutil.RunGit(t, env.RepoDir, "checkout", "main")

	// A git hook in the user's repository, installed after the setup checkouts:
	// the untrusted checkout must not run it, since post-checkout sees the
	// branch's files.
	hook := "#!/bin/sh\necho ran >> '" + repo.gitMarker + "'\n"
	if err := os.MkdirAll(filepath.Join(env.RepoDir, ".git", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.RepoDir, ".git", "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	// Keep agent CLIs off PATH and clear every variable that marks an agent
	// caller, so the developer's own session does not leak in.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	env.ExtraEnv = append(env.ExtraEnv, "PATH="+bin+":/bin")
	for _, name := range review.AgentCallerEnvVars() {
		env.ExtraEnv = append(env.ExtraEnv, name+"=")
	}
	return repo
}

func (r *trustGateRepo) reviewWorktrees(t *testing.T) []string {
	t.Helper()
	out := testutil.RunGit(t, r.env.RepoDir, "worktree", "list", "--porcelain")
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && strings.Contains(p, filepath.Join(".entire", "worktrees")) {
			paths = append(paths, p)
		}
	}
	return paths
}

func (r *trustGateRepo) assertNothingRan(t *testing.T) {
	t.Helper()
	for _, marker := range []string{r.hookMarker, r.gitMarker} {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s exists: something from the branch ran", filepath.Base(marker))
		}
	}
}

func TestReviewTrustGate_RefusesWithoutApproval(t *testing.T) {
	t.Parallel()
	r := newTrustGateRepo(t)

	out, err := r.env.RunCLIWithError("review", "gate", "--target", "feature")
	if err == nil {
		t.Fatalf("review of someone else's branch ran without approval:\n%s", out)
	}
	for _, want := range []string{
		"Not run: this review needs the user's approval.",
		"--trust-target " + r.head,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Mallory") || strings.Contains(out, "echo ran") {
		t.Errorf("refusal shows author-controlled text:\n%s", out)
	}
	if wts := r.reviewWorktrees(t); len(wts) != 0 {
		t.Fatalf("worktree created before approval: %v", wts)
	}
	r.assertNothingRan(t)
}

func TestReviewTrustGate_ShowConfigListsBranchHook(t *testing.T) {
	t.Parallel()
	r := newTrustGateRepo(t)

	out, err := r.env.RunCLIWithError("review", "gate", "--target", "feature", "--show-config", "--json")
	if err != nil {
		t.Fatalf("--show-config: %v\n%s", err, out)
	}
	var cfg struct {
		Head    string `json:"head"`
		Yours   bool   `json:"yours"`
		Entries []struct {
			Kind    string `json:"kind"`
			Command string `json:"command"`
			Entire  bool   `json:"entire"`
		} `json:"entries"`
	}
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in --show-config output:\n%s", out)
	}
	if err := json.Unmarshal([]byte(out[start:]), &cfg); err != nil {
		t.Fatalf("parse --show-config --json: %v\n%s", err, out)
	}
	if cfg.Head != r.head || cfg.Yours {
		t.Fatalf("head/yours = %s/%v, want %s/false", cfg.Head, cfg.Yours, r.head)
	}
	found := false
	for _, e := range cfg.Entries {
		// Matched on the file name: content redaction may replace a
		// high-entropy segment of the temp directory above it.
		if e.Kind == "hook" && strings.Contains(e.Command, filepath.Base(r.hookMarker)) && !e.Entire {
			found = true
		}
	}
	if !found {
		t.Fatalf("branch hook not listed: %+v", cfg.Entries)
	}
	if wts := r.reviewWorktrees(t); len(wts) != 0 {
		t.Fatalf("--show-config created a worktree: %v", wts)
	}
	r.assertNothingRan(t)
}

func TestReviewTrustGate_ApprovedCheckoutRunsNoGitHooks(t *testing.T) {
	t.Parallel()
	r := newTrustGateRepo(t)

	// With no agent CLI installed the review fails after the checkout, which
	// is all this test needs.
	out, _ := r.env.RunCLIWithError("review", "gate", "--target", "feature", "--trust-target", r.head) //nolint:errcheck // the review itself cannot start without agent CLIs
	if !strings.Contains(out, "as approved") {
		t.Fatalf("approval line missing:\n%s", out)
	}
	wts := r.reviewWorktrees(t)
	if len(wts) != 1 {
		t.Fatalf("review worktrees = %v, want one", wts)
	}
	if got := strings.TrimSpace(testutil.RunGit(t, wts[0], "rev-parse", "HEAD")); got != r.head {
		t.Fatalf("worktree HEAD = %s, want pinned %s", got, r.head)
	}
	if _, err := os.Stat(r.gitMarker); err == nil {
		t.Fatal("the repository's post-checkout hook ran during an untrusted checkout")
	}
	if strings.Contains(out, "needs the user's approval") {
		t.Fatalf("the re-run inside the worktree asked again:\n%s", out)
	}
}
