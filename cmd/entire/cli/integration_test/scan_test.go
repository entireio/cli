//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// scanReport mirrors `entire scan --json`. Declared here rather than imported
// so a change to the CLI's internal struct that breaks the published JSON
// contract shows up as a test failure.
type scanReport struct {
	ScannedDirs []string `json:"scanned_dirs"`
	Repos       []struct {
		Path                   string   `json:"path"`
		SetUp                  bool     `json:"set_up"`
		Enabled                bool     `json:"enabled"`
		GitHooksInstalled      bool     `json:"git_hooks_installed"`
		AgentsHooked           []string `json:"agents_hooked"`
		AgentsDetectedUnhooked []string `json:"agents_detected_unhooked"`
		LinkedWorktree         bool     `json:"linked_worktree"`
		Error                  string   `json:"error"`
	} `json:"repos"`
	Summary struct {
		Total          int `json:"total"`
		SetUp          int `json:"set_up"`
		Enabled        int `json:"enabled"`
		NeedsAttention int `json:"needs_attention"`
	} `json:"summary"`
}

func (r scanReport) repo(t *testing.T, path string) int {
	t.Helper()
	for i, repo := range r.Repos {
		if repo.Path == path {
			return i
		}
	}
	t.Fatalf("repo %s not in scan report: %+v", path, r.Repos)
	return -1
}

// TestScan_ReportsAndFixesAFolderOfRepos exercises the whole command against a
// real dev folder: an already-enabled repo, a repo that clearly uses Claude
// Code without Entire hooks, and a plain repo. It then fixes them with the
// non-interactive path and asserts the hooks actually landed.
func TestScan_ReportsAndFixesAFolderOfRepos(t *testing.T) {
	t.Parallel()

	dev := scanTempDir(t)
	enabled := scanFixtureRepo(t, dev, "enabled")
	unhooked := scanFixtureRepo(t, dev, "unhooked")
	plain := scanFixtureRepo(t, dev, "plain")

	// "unhooked" looks like a Claude Code project but was never enabled.
	if err := os.MkdirAll(filepath.Join(unhooked, ".claude"), 0o755); err != nil {
		t.Fatalf("creating .claude dir: %v", err)
	}
	runScanCLI(t, enabled, "enable", "--agent", agentClaudeCode, "--telemetry=false")

	report := runScanJSON(t, dev, "scan", "--json", dev)

	if got := report.ScannedDirs; len(got) != 1 || got[0] != dev {
		t.Fatalf("scanned_dirs = %v, want [%s]", got, dev)
	}
	if report.Summary.Total != 3 {
		t.Fatalf("summary.total = %d, want 3 (report: %+v)", report.Summary.Total, report.Repos)
	}

	enabledRepo := report.Repos[report.repo(t, enabled)]
	if !enabledRepo.SetUp || !enabledRepo.Enabled || !enabledRepo.GitHooksInstalled {
		t.Fatalf("enabled repo misreported: %+v", enabledRepo)
	}
	if !slices.Contains(enabledRepo.AgentsHooked, agentClaudeCode) {
		t.Fatalf("enabled repo should list claude-code as hooked: %+v", enabledRepo)
	}

	unhookedRepo := report.Repos[report.repo(t, unhooked)]
	if unhookedRepo.SetUp {
		t.Fatalf("unhooked repo should not be set up: %+v", unhookedRepo)
	}
	if !slices.Contains(unhookedRepo.AgentsDetectedUnhooked, agentClaudeCode) {
		t.Fatalf("unhooked repo should detect claude-code without hooks: %+v", unhookedRepo)
	}

	plainRepo := report.Repos[report.repo(t, plain)]
	if plainRepo.SetUp || len(plainRepo.AgentsDetectedUnhooked) != 0 {
		t.Fatalf("plain repo should be untouched and undetected: %+v", plainRepo)
	}
	if report.Summary.NeedsAttention != 1 {
		t.Fatalf("summary.needs_attention = %d, want 1", report.Summary.NeedsAttention)
	}

	// Fix, naming the agent explicitly so it applies to every scanned repo.
	out := runScanCLI(t, dev, "scan", "--fix", "--yes", "--agent", agentClaudeCode, dev)
	if !strings.Contains(out, "entire enable --agent "+agentClaudeCode) {
		t.Fatalf("fix output should show the enable invocations, got:\n%s", out)
	}

	after := runScanJSON(t, dev, "scan", "--json", dev)
	for _, path := range []string{enabled, unhooked, plain} {
		repo := after.Repos[after.repo(t, path)]
		if !repo.Enabled {
			t.Fatalf("%s should be enabled after --fix: %+v", path, repo)
		}
		if !repo.GitHooksInstalled {
			t.Fatalf("%s should have git hooks after --fix: %+v", path, repo)
		}
		if !slices.Contains(repo.AgentsHooked, agentClaudeCode) {
			t.Fatalf("%s should have claude-code hooks after --fix: %+v", path, repo)
		}
	}
	if after.Summary.NeedsAttention != 0 {
		t.Fatalf("summary.needs_attention = %d after --fix, want 0", after.Summary.NeedsAttention)
	}
}

// TestScan_FixWithoutYesIsRefusedWithoutATerminal is the agent-safety guard on
// the real binary: no TTY and no --yes must fail loudly, not silently rewrite
// every repository found.
func TestScan_FixWithoutYesIsRefusedWithoutATerminal(t *testing.T) {
	t.Parallel()

	dev := scanTempDir(t)
	unhooked := scanFixtureRepo(t, dev, "unhooked")
	if err := os.MkdirAll(filepath.Join(unhooked, ".claude"), 0o755); err != nil {
		t.Fatalf("creating .claude dir: %v", err)
	}

	out, err := runScanCLIWithError(t, dev, "scan", "--fix", dev)
	if err == nil {
		t.Fatalf("expected `scan --fix` without a TTY to fail, got:\n%s", out)
	}
	if !strings.Contains(out, "pass --yes to fix non-interactively") {
		t.Fatalf("error should point at --yes, got:\n%s", out)
	}
}

func scanTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	return resolved
}

// scanFixtureRepo creates a committed git repo named name inside parent.
func scanFixtureRepo(t *testing.T, parent, name string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("creating %s: %v", repo, err)
	}
	testutil.InitRepo(t, repo)
	testutil.WriteFile(t, repo, "README.md", "# "+name+"\n")
	testutil.GitAdd(t, repo, "README.md")
	testutil.GitCommit(t, repo, "initial commit")
	return repo
}

func runScanCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runScanCLIWithError(t, dir, args...)
	if err != nil {
		t.Fatalf("entire %v failed: %v\nOutput: %s", args, err, out)
	}
	return out
}

func runScanCLIWithError(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runScanJSON(t *testing.T, dir string, args ...string) scanReport {
	t.Helper()
	out := runScanCLI(t, dir, args...)
	var report scanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parsing scan JSON: %v\nOutput: %s", err, out)
	}
	return report
}
