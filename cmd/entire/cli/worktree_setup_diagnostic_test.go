package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestInspectWorktreeSetup_RequiresConfiguredSibling(t *testing.T) {
	t.Run("fresh worktree missing settings and hooks", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
		enterWorktree(t, linkedRoot)

		issue := inspectWorktreeSetup(t.Context())
		if issue == nil {
			t.Fatal("inspectWorktreeSetup() = nil, want incomplete setup")
		}
		if !issue.MissingProjectSettings || !issue.MissingClaudeProjectHooks {
			t.Errorf("issue = %+v, want both settings and Claude hooks missing", issue)
		}
	})

	t.Run("current worktree configured", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
		configureClaudeWorktree(t, linkedRoot, testSettingsEnabled, true)
		enterWorktree(t, linkedRoot)

		assertNoWorktreeSetupIssue(t, "configured current worktree")
	})

	t.Run("ordinary linked worktrees", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, "", false)
		enterWorktree(t, linkedRoot)

		assertNoWorktreeSetupIssue(t, "ordinary linked worktrees")
	})

	t.Run("disabled sibling", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, testSettingsDisabled, true)
		enterWorktree(t, linkedRoot)

		assertNoWorktreeSetupIssue(t, "disabled sibling")
	})

	t.Run("sibling enabled only by local override", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, testSettingsDisabled, true)
		repoRoot := filepath.Join(filepath.Dir(linkedRoot), "repo")
		writeFile(t, filepath.Join(repoRoot, EntireSettingsLocalFile), testSettingsEnabled)
		enterWorktree(t, linkedRoot)

		assertNoWorktreeSetupIssue(t, "sibling without portable project settings")
	})

	t.Run("disabled current worktree", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
		configureClaudeWorktree(t, linkedRoot, testSettingsDisabled, false)
		enterWorktree(t, linkedRoot)

		assertNoWorktreeSetupIssue(t, "disabled current worktree")
	})

	t.Run("current worktree disabled by local override", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
		configureClaudeWorktree(t, linkedRoot, testSettingsEnabled, true)
		writeFile(t, filepath.Join(linkedRoot, EntireSettingsLocalFile), testSettingsDisabled)
		enterWorktree(t, linkedRoot)

		assertNoWorktreeSetupIssue(t, "current worktree disabled by local override")
	})

	t.Run("current hook config inspection fails", func(t *testing.T) {
		linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
		writeFile(t, filepath.Join(linkedRoot, ".claude", "settings.json"), `{`)
		enterWorktree(t, linkedRoot)

		assertNoWorktreeSetupIssue(t, "failed current hook config inspection")
	})
}

func assertNoWorktreeSetupIssue(t *testing.T, scenario string) {
	t.Helper()
	if issue := inspectWorktreeSetup(t.Context()); issue != nil {
		t.Errorf("inspectWorktreeSetup() = %+v, want nil for %s", issue, scenario)
	}
}

func TestInspectWorktreeSetup_ReportsOnlyMissingClaudeProjectHooks(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	configureClaudeWorktree(t, linkedRoot, testSettingsEnabled, false)
	enterWorktree(t, linkedRoot)

	issue := inspectWorktreeSetup(t.Context())
	if issue == nil {
		t.Fatal("inspectWorktreeSetup() = nil, want incomplete setup")
	}
	if issue.MissingProjectSettings || !issue.MissingClaudeProjectHooks {
		t.Errorf("issue = %+v, want only shared Claude project hooks missing", issue)
	}
}

func TestInspectWorktreeSetup_PreservesCurrentWorktreeVouch(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	writeFile(t, filepath.Join(linkedRoot, EntireSettingsLocalFile),
		`{"enabled":true,"allow_symlinked_agent_dirs":[".claude"]}`)
	enterWorktree(t, linkedRoot)
	t.Cleanup(func() { agentpkg.SetVouchedSymlinkedDirs("", nil) })
	currentRoot, err := paths.WorktreeRoot(t.Context())
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}

	issue := inspectWorktreeSetup(t.Context())
	if issue == nil {
		t.Fatal("inspectWorktreeSetup() = nil, want incomplete shared Claude setup")
	}
	if got := agentpkg.VouchedSymlinkedDirs(currentRoot); !slices.Equal(got, []string{".claude"}) {
		t.Errorf("current worktree vouch after sibling inspection = %v, want [.claude]", got)
	}
}

func TestRunStatus_WorktreeSetupWarning(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	enterWorktree(t, linkedRoot)

	var human bytes.Buffer
	if err := runStatus(t.Context(), &human, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	for _, want := range []string{
		"○ not set up",
		"Claude Code worktree portability: INCOMPLETE",
		"Entire project settings, shared Claude Code hook config",
		"Without Entire settings, every Entire hook in this worktree is inactive",
		"sessions started here will not create checkpoints",
		"entire enable --agent claude-code",
	} {
		if !strings.Contains(human.String(), want) {
			t.Errorf("status output does not contain %q:\n%s", want, human.String())
		}
	}

	var detailed bytes.Buffer
	if err := runStatus(t.Context(), &detailed, true, false); err != nil {
		t.Fatalf("runStatus(--detailed) error = %v", err)
	}
	if !strings.Contains(detailed.String(), "Claude Code worktree portability: INCOMPLETE") {
		t.Errorf("detailed status does not report worktree setup gap:\n%s", detailed.String())
	}

	var machine bytes.Buffer
	if err := runStatus(t.Context(), &machine, false, true); err != nil {
		t.Fatalf("runStatus(--json) error = %v", err)
	}
	var result statusJSON
	if err := json.Unmarshal(machine.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.Error != "not set up" {
		t.Errorf("error = %q, want preserved not-set-up contract", result.Error)
	}
	if result.WorktreeSetup == nil {
		t.Fatal("worktree_setup = nil, want incomplete setup details")
	}
	if result.WorktreeSetup.State != "incomplete" || result.WorktreeSetup.Agent != claudeCodeAgentName {
		t.Errorf("worktree_setup = %+v", result.WorktreeSetup)
	}
	if !slices.Equal(result.WorktreeSetup.Missing, []string{"entire_project_settings", "claude_project_hook_config"}) {
		t.Errorf("worktree_setup.missing = %v", result.WorktreeSetup.Missing)
	}
}

func TestRunStatus_SharedClaudeHookWarningAllowsLocalCoverage(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	configureClaudeWorktree(t, linkedRoot, testSettingsEnabled, false)
	writeFile(t, filepath.Join(linkedRoot, ".claude", "settings.local.json"),
		`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"entire hooks claude-code stop"}]}]}}`)
	enterWorktree(t, linkedRoot)

	var stdout bytes.Buffer
	if err := runStatus(t.Context(), &stdout, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Claude user or local settings may still provide hooks") {
		t.Errorf("status does not acknowledge local-scope coverage:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "sessions started here will not create checkpoints") {
		t.Errorf("status makes an absolute capture claim when only shared config is absent:\n%s", stdout.String())
	}

	var machine bytes.Buffer
	if err := runStatus(t.Context(), &machine, false, true); err != nil {
		t.Fatalf("runStatus(--json) error = %v", err)
	}
	var result statusJSON
	if err := json.Unmarshal(machine.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.WorktreeSetup == nil ||
		!slices.Equal(result.WorktreeSetup.Missing, []string{"claude_project_hook_config"}) {
		t.Errorf("worktree_setup = %+v, want only missing shared Claude project config", result.WorktreeSetup)
	}
}

func TestRunStatus_LocalOnlyEntireSettingsWarnsAboutProjectPortability(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	writeFile(t, filepath.Join(linkedRoot, EntireSettingsLocalFile), testSettingsEnabled)
	configureClaudeWorktree(t, linkedRoot, "", true)
	enterWorktree(t, linkedRoot)

	var human bytes.Buffer
	if err := runStatus(t.Context(), &human, false, false); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}
	if !strings.Contains(human.String(), "Entire project settings") {
		t.Errorf("status does not report missing portable project settings:\n%s", human.String())
	}
	if strings.Contains(human.String(), "sessions started here will not create checkpoints") {
		t.Errorf("status makes an absolute capture claim for an enabled local setup:\n%s", human.String())
	}

	var machine bytes.Buffer
	if err := runStatus(t.Context(), &machine, false, true); err != nil {
		t.Fatalf("runStatus(--json) error = %v", err)
	}
	var result statusJSON
	if err := json.Unmarshal(machine.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result.WorktreeSetup == nil ||
		!slices.Equal(result.WorktreeSetup.Missing, []string{"entire_project_settings"}) {
		t.Errorf("worktree_setup = %+v, want only missing Entire project settings", result.WorktreeSetup)
	}
}

func TestInspectWorktreeSetup_LocalOverrideDoesNotMakeDisabledProjectPortable(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	configureClaudeWorktree(t, linkedRoot, testSettingsDisabled, true)
	writeFile(t, filepath.Join(linkedRoot, EntireSettingsLocalFile), testSettingsEnabled)
	enterWorktree(t, linkedRoot)

	issue := inspectWorktreeSetup(t.Context())
	if issue == nil {
		t.Fatal("inspectWorktreeSetup() = nil, want missing portable project settings")
	}
	if !issue.MissingProjectSettings || issue.MissingClaudeProjectHooks || issue.CurrentCaptureInactive {
		t.Errorf("issue = %+v, want only inactive project settings reported as non-portable", issue)
	}
}

func TestDoctor_WorktreeWithoutSettingsDoesNotReportHealthyHooks(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	enterWorktree(t, linkedRoot)
	issue := inspectWorktreeSetup(t.Context())
	if issue == nil {
		t.Fatal("inspectWorktreeSetup() = nil, want incomplete setup")
	}

	cmd, stdout := newTestCmd(t)
	if err := checkGitHooksWithWorktreeSetup(cmd, false, issue); err != nil {
		t.Fatalf("checkGitHooksWithWorktreeSetup() error = %v", err)
	}
	writeWorktreeSetupIssue(stdout, issue)
	if strings.Contains(stdout.String(), "Git hooks: OK") {
		t.Errorf("doctor reported inactive hooks as healthy:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Git hooks: INSTALLED BUT INACTIVE") {
		t.Errorf("doctor did not report inactive hooks:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Claude Code worktree portability: INCOMPLETE") {
		t.Errorf("doctor did not report worktree setup gap:\n%s", stdout.String())
	}
}

func TestDoctor_LocalOnlyEntireSettingsDoesNotReportInactiveHooks(t *testing.T) {
	linkedRoot := setupClaudeWorktrees(t, testSettingsEnabled, true)
	writeFile(t, filepath.Join(linkedRoot, EntireSettingsLocalFile), testSettingsEnabled)
	configureClaudeWorktree(t, linkedRoot, "", true)
	enterWorktree(t, linkedRoot)
	issue := inspectWorktreeSetup(t.Context())
	if issue == nil {
		t.Fatal("inspectWorktreeSetup() = nil, want project portability warning")
	}

	cmd, stdout := newTestCmd(t)
	if err := checkGitHooksWithWorktreeSetup(cmd, false, issue); err != nil {
		t.Fatalf("checkGitHooksWithWorktreeSetup() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Git hooks: OK") {
		t.Errorf("doctor did not report active shared Git hooks:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "INSTALLED BUT INACTIVE") {
		t.Errorf("doctor reported an enabled local-only setup as inactive:\n%s", stdout.String())
	}
}

func TestParseSiblingWorktrees_SkipsCurrentAndPrunable(t *testing.T) {
	t.Parallel()
	porcelain := []byte("worktree /repo/current\x00HEAD abc\x00\x00" +
		"worktree /repo/ready\x00HEAD def\x00\x00" +
		"worktree /repo/common\x00bare\x00\x00" +
		"worktree /repo/gone\x00HEAD 123\x00prunable gitdir file points to non-existent location\x00\x00")

	got := parseSiblingWorktrees(porcelain, normalizeWorktreePath("/repo/current"))
	if !slices.Equal(got, []string{"/repo/ready"}) {
		t.Errorf("parseSiblingWorktrees() = %v, want [/repo/ready]", got)
	}
}

func setupClaudeWorktrees(t *testing.T, sourceSettings string, installClaudeHooks bool) string {
	t.Helper()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	linkedRoot := filepath.Join(filepath.Dir(repoRoot), "linked")
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "initial\n")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "initial")
	testutil.RunGit(t, repoRoot, "worktree", "add", "-b", "feature", linkedRoot)
	if sourceSettings != "" || installClaudeHooks {
		configureClaudeWorktree(t, repoRoot, sourceSettings, installClaudeHooks)
	}
	enterWorktree(t, repoRoot)
	if _, err := strategy.InstallGitHook(t.Context(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	return linkedRoot
}

func configureClaudeWorktree(t *testing.T, worktreeRoot, entireSettings string, installClaudeHooks bool) {
	t.Helper()
	if entireSettings != "" {
		writeFile(t, filepath.Join(worktreeRoot, EntireSettingsFile), entireSettings)
	}
	if installClaudeHooks {
		enterWorktree(t, worktreeRoot)
		if _, err := (&claudecode.ClaudeCodeAgent{}).InstallHooks(t.Context(), false); err != nil {
			t.Fatalf("InstallHooks() error = %v", err)
		}
	}
}

func enterWorktree(t *testing.T, worktreeRoot string) {
	t.Helper()
	t.Chdir(worktreeRoot)
	paths.ClearWorktreeRootCache()
	strategy.ClearHooksDirCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(strategy.ClearHooksDirCache)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
