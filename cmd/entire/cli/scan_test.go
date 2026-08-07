package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// cannedScanReport is the fixture the rendering tests assert against: one repo
// per interesting state.
func cannedScanReport() repoScanReport {
	return newRepoScanReport(
		[]string{"/dev"},
		[]repoScanEntry{
			{
				Path:              "/dev/enabled",
				SetUp:             true,
				Enabled:           true,
				GitHooksInstalled: true,
				AgentsHooked:      []string{"claude-code"},
			},
			{
				Path:                   "/dev/unhooked",
				AgentsDetectedUnhooked: []string{"claude-code", "cursor"},
			},
			{
				Path:              "/dev/disabled",
				SetUp:             true,
				GitHooksInstalled: true,
				AgentsHooked:      []string{"claude-code"},
			},
			{
				Path:              "/dev/stale",
				SetUp:             true,
				Enabled:           true,
				GitHooksInstalled: true,
				AgentsHooked:      []string{"claude-code"},
				HooksOutdated:     []string{"claude-code"},
				CodexTrustGaps:    []string{"session_start"},
			},
			{
				Path:           "/dev/plain",
				LinkedWorktree: true,
			},
		},
	)
}

func TestNewRepoScanReport_Summary(t *testing.T) {
	t.Parallel()

	report := cannedScanReport()

	require.Equal(t, repoScanSummary{Total: 5, SetUp: 3, Enabled: 2, NeedsAttention: 3}, report.Summary,
		"unhooked, disabled and stale need attention; enabled and plain do not")
}

func TestWriteRepoScanTable_RendersEveryState(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeRepoScanTable(&buf, cannedScanReport(), false)
	out := buf.String()

	require.Contains(t, out, "REPO")
	require.Contains(t, out, "GIT HOOKS")
	require.Contains(t, out, "PRESENT")
	require.Regexp(t, `/dev/enabled\s+enabled\s+yes\s+claude-code\s+-`, out)
	require.Regexp(t, `/dev/unhooked\s+not set up\s+no\s+-\s+claude-code,cursor`, out)
	require.Regexp(t, `/dev/disabled\s+disabled\s+yes`, out)
	require.Contains(t, out, "/dev/plain (worktree)", "linked worktrees are marked")
	require.Contains(t, out, "hook config outdated for claude-code")
	require.Contains(t, out, "codex hooks awaiting trust: session_start")
	require.Contains(t, out, "5 repositories scanned: 3 set up, 2 enabled, 3 need attention.")
	require.Contains(t, out, scanHint)
}

func TestWriteRepoScanTable_SuppressesHintWhenFixing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeRepoScanTable(&buf, cannedScanReport(), true)

	require.NotContains(t, buf.String(), scanHint, "--fix is already doing what the hint suggests")
}

func TestWriteRepoScanTable_NoRepos(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeRepoScanTable(&buf, newRepoScanReport([]string{"/dev"}, nil), false)

	require.Contains(t, buf.String(), "No git repositories found.")
}

func TestWriteRepoScanJSON_RoundTrips(t *testing.T) {
	t.Parallel()

	want := cannedScanReport()
	var buf bytes.Buffer
	require.NoError(t, writeRepoScanJSON(&buf, want))

	var got repoScanReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, want, got)

	// The optional fields must be omitted, not rendered as nulls, for the four
	// repos that have nothing to report.
	require.Equal(t, 1, strings.Count(buf.String(), `"hooks_outdated"`))
	require.Equal(t, 1, strings.Count(buf.String(), `"codex_trust_gaps"`))
	require.Equal(t, 1, strings.Count(buf.String(), `"linked_worktree"`))
	require.NotContains(t, buf.String(), `"error"`)
}

func TestWriteRepoScanJSON_EmptyReposIsAnArray(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeRepoScanJSON(&buf, newRepoScanReport([]string{"/dev"}, nil)))

	require.Contains(t, buf.String(), `"repos": []`, "consumers should not have to handle null")
}

func TestPlanScanFixes_DerivesActionsFromDetection(t *testing.T) {
	t.Parallel()

	got := planScanFixes(cannedScanReport().Repos, "")

	require.Equal(t, []scanFixAction{
		{RepoRoot: "/dev/unhooked", AgentName: "claude-code"},
		{RepoRoot: "/dev/unhooked", AgentName: "cursor"},
		{RepoRoot: "/dev/disabled"},
		{RepoRoot: "/dev/stale", AgentName: "claude-code", Force: true},
	}, got)
}

func TestPlanScanFixes_ReEnableRunsBeforeAgentHooks(t *testing.T) {
	t.Parallel()

	entry := repoScanEntry{
		Path:                   "/dev/x",
		SetUp:                  true,
		AgentsDetectedUnhooked: []string{"claude-code"},
	}

	got := planScanFixes([]repoScanEntry{entry}, "")

	require.Equal(t, []scanFixAction{
		{RepoRoot: "/dev/x"},
		{RepoRoot: "/dev/x", AgentName: "claude-code"},
	}, got, "the repo must be enabled before per-agent hooks are installed")
}

func TestPlanScanFixes_AgentOverrideAppliesToEveryRepo(t *testing.T) {
	t.Parallel()

	got := planScanFixes(cannedScanReport().Repos, "claude-code")

	require.Equal(t, []scanFixAction{
		{RepoRoot: "/dev/enabled", AgentName: "claude-code"},
		{RepoRoot: "/dev/unhooked", AgentName: "claude-code"},
		{RepoRoot: "/dev/disabled", AgentName: "claude-code"},
		{RepoRoot: "/dev/stale", AgentName: "claude-code", Force: true},
		{RepoRoot: "/dev/plain", AgentName: "claude-code"},
	}, got, "--agent names the agent explicitly, including where nothing was detected")
}

func TestFixableScanRepos(t *testing.T) {
	t.Parallel()

	repos := cannedScanReport().Repos

	require.Equal(t, []string{"/dev/unhooked", "/dev/disabled", "/dev/stale"},
		fixableScanRepos(repos, ""))
	require.Equal(t, []string{"/dev/enabled", "/dev/unhooked", "/dev/disabled", "/dev/stale", "/dev/plain"},
		fixableScanRepos(repos, "claude-code"))
}

func TestScanFixAction_Describe(t *testing.T) {
	t.Parallel()

	require.Equal(t, "entire enable", scanFixAction{RepoRoot: "/x"}.describe())
	require.Equal(t, "entire enable --agent cursor", scanFixAction{RepoRoot: "/x", AgentName: "cursor"}.describe())
	require.Equal(t, "entire enable --agent codex --force",
		scanFixAction{RepoRoot: "/x", AgentName: "codex", Force: true}.describe())
}

// recordingScanFixRunner captures the actions a fix run would execute.
type recordingScanFixRunner struct {
	mu      sync.Mutex
	actions []scanFixAction
	failOn  string
}

func (r *recordingScanFixRunner) run(_ context.Context, action scanFixAction, out io.Writer) error {
	r.mu.Lock()
	r.actions = append(r.actions, action)
	r.mu.Unlock()
	_, _ = io.WriteString(out, "enabling\n")
	if r.failOn != "" && action.RepoRoot == r.failOn {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func TestRunScan_FixWithYesAppliesEveryFixableRepo(t *testing.T) {
	t.Parallel()

	runner := &recordingScanFixRunner{}
	report := cannedScanReport()
	var buf bytes.Buffer

	err := applyScanFixes(context.Background(), &buf, report,
		scanOptions{Fix: true, Yes: true}, runner.run)

	require.NoError(t, err)
	require.Equal(t, []scanFixAction{
		{RepoRoot: "/dev/unhooked", AgentName: "claude-code"},
		{RepoRoot: "/dev/unhooked", AgentName: "cursor"},
		{RepoRoot: "/dev/disabled"},
		{RepoRoot: "/dev/stale", AgentName: "claude-code", Force: true},
	}, runner.actions)
	require.Contains(t, buf.String(), "Ran 4 enable commands.")
	require.Contains(t, buf.String(), "| enabling", "subprocess output is prefixed with the repo")
}

func TestRunScan_FixWithAgentSelectsThatAgentEverywhere(t *testing.T) {
	t.Parallel()

	runner := &recordingScanFixRunner{}
	var buf bytes.Buffer

	err := applyScanFixes(context.Background(), &buf, cannedScanReport(),
		scanOptions{Fix: true, Yes: true, AgentName: "cursor"}, runner.run)

	require.NoError(t, err)
	require.Len(t, runner.actions, 5)
	for _, action := range runner.actions {
		require.Equal(t, "cursor", action.AgentName)
	}
}

func TestRunScan_FixReportsFailuresWithoutStopping(t *testing.T) {
	t.Parallel()

	runner := &recordingScanFixRunner{failOn: "/dev/disabled"}
	var buf bytes.Buffer

	err := applyScanFixes(context.Background(), &buf, cannedScanReport(),
		scanOptions{Fix: true, Yes: true}, runner.run)

	require.Error(t, err)
	require.Contains(t, err.Error(), "1 of 4 enable commands failed")
	require.Len(t, runner.actions, 4, "a failing repo does not abort the remaining fixes")
}

// TestSelectScanFixRepos_NonInteractiveWithoutYes is the agent-safety guard:
// without a terminal the command must say what to do instead of silently
// rewriting every repository it found.
func TestSelectScanFixRepos_NonInteractiveWithoutYes(t *testing.T) {
	t.Parallel()

	// go test makes interactive.CanPromptInteractively() false by default.
	_, err := selectScanFixRepos([]string{"/dev/a"}, false)

	require.Error(t, err)
	require.Contains(t, err.Error(), "pass --yes to fix non-interactively")
}

func TestSelectScanFixRepos_YesSelectsEverything(t *testing.T) {
	t.Parallel()

	got, err := selectScanFixRepos([]string{"/dev/a", "/dev/b"}, true)

	require.NoError(t, err)
	require.Equal(t, []string{"/dev/a", "/dev/b"}, got)
}

func TestSelectScanFixRepos_NothingFixable(t *testing.T) {
	t.Parallel()

	got, err := selectScanFixRepos(nil, false)

	require.NoError(t, err, "no candidates means no prompt and no error")
	require.Empty(t, got)
}

func TestRunScanFixes_SkipsUnselectedRepos(t *testing.T) {
	t.Parallel()

	runner := &recordingScanFixRunner{}
	actions := []scanFixAction{
		{RepoRoot: "/dev/a", AgentName: "claude-code"},
		{RepoRoot: "/dev/b", AgentName: "claude-code"},
	}
	var buf bytes.Buffer

	require.NoError(t, runScanFixes(context.Background(), &buf, actions, []string{"/dev/b"}, runner.run))
	require.Equal(t, []scanFixAction{{RepoRoot: "/dev/b", AgentName: "claude-code"}}, runner.actions)
}

func TestRunScan_RejectsInvalidDepth(t *testing.T) {
	t.Parallel()

	err := runScan(context.Background(), io.Discard, []string{t.TempDir()}, scanOptions{Depth: 0}, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "--depth must be at least 1")
}

func TestRunScan_RejectsUnknownAgent(t *testing.T) {
	t.Parallel()

	err := runScan(context.Background(), io.Discard, []string{t.TempDir()},
		scanOptions{Depth: 2, AgentName: "not-a-real-agent"}, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown agent "not-a-real-agent"`)
}

func TestRunScan_JSONOverRealRepos(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	repo := newScanTestRepoIn(t, dev, "alpha")
	writeScanSettingsFixture(t, repo, `{"enabled": true}`)

	var buf bytes.Buffer
	require.NoError(t, runScan(context.Background(), &buf, []string{dev},
		scanOptions{Depth: 2, JSON: true}, nil))

	var report repoScanReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &report))
	require.Equal(t, []string{dev}, report.ScannedDirs)
	require.Len(t, report.Repos, 1)
	require.Equal(t, repo, report.Repos[0].Path)
	require.True(t, report.Repos[0].SetUp)
	require.True(t, report.Repos[0].Enabled)
	require.Equal(t, 1, report.Summary.Total)
}

func TestLinePrefixWriter_PrefixesLinesAndFlushesRemainder(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newLinePrefixWriter(&buf, "> ")
	_, err := io.WriteString(w, "one\ntw")
	require.NoError(t, err)
	_, err = io.WriteString(w, "o\nthree")
	require.NoError(t, err)
	w.Flush()

	require.Equal(t, "> one\n> two\n> three\n", buf.String())
}

func TestAbbreviateHomePath(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	t.Setenv("HOME", "/home/tester")
	require.Equal(t, "~", abbreviateHomePath("/home/tester"))
	require.Equal(t, "~/dev/repo", abbreviateHomePath("/home/tester/dev/repo"))
	require.Equal(t, "/srv/repo", abbreviateHomePath("/srv/repo"), "paths outside home stay absolute")
}
