package review_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	cli "github.com/entireio/cli/cmd/entire/cli"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// setupCmdTestRepo initialises a temp git repo with one commit and chdirs into it.
func setupCmdTestRepo(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
}

// installHooksForCmdTest installs the given agent's hooks into the CWD-relative repo.
func installHooksForCmdTest(t *testing.T, agentName types.AgentName) {
	t.Helper()
	ag, err := agent.Get(agentName)
	if err != nil {
		t.Fatalf("agent.Get(%q): %v", agentName, err)
	}
	hs, ok := agent.AsHookSupport(ag)
	if !ok {
		t.Fatalf("agent %q does not support hooks", agentName)
	}
	if _, err := hs.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks(%q): %v", agentName, err)
	}
}

// TestReviewCmd_Help verifies `entire review --help` contains the expected
// flags and subcommands without panicking.
func TestReviewCmd_Help(t *testing.T) {
	t.Parallel()
	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "--help"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"review", "--edit", "--findings", "--fix", "--all", "--agent", "attach", "Labs entry"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q: %s", want, out)
		}
	}
	// --track-only was intentionally dropped by PR #1009.
	if strings.Contains(out, "track-only") {
		t.Error("--help output should NOT contain track-only flag (dropped in #1009)")
	}
}

// TestNewReviewCmd_NoHiddenFlags ensures the removed internal flags are gone.
func TestNewReviewCmd_NoHiddenFlags(t *testing.T) {
	t.Parallel()
	rootCmd := cli.NewRootCmd()
	reviewCmd, _, err := rootCmd.Find([]string{"review"})
	if err != nil || reviewCmd == nil {
		t.Fatal("review subcommand not found")
	}
	for _, name := range []string{"postreview", "finalize", "session", "track-only"} {
		if reviewCmd.Flags().Lookup(name) != nil {
			t.Errorf("found removed flag: --%s", name)
		}
	}
}

func TestReviewFindings_NotGitRepoReturnsSilentError(t *testing.T) {
	t.Chdir(t.TempDir())

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "--findings"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error outside a git repo")
	}
	var silentErr *cli.SilentError
	if !errors.As(err, &silentErr) {
		t.Fatalf("expected SilentError, got %T: %v", err, err)
	}
	if got := strings.Count(errBuf.String(), "Not a git repository"); got != 1 {
		t.Fatalf("not-git message count = %d, want 1; stderr:\n%s", got, errBuf.String())
	}
}

func TestReviewFix_NotGitRepoReturnsSilentError(t *testing.T) {
	t.Chdir(t.TempDir())

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "--fix", "review-session"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error outside a git repo")
	}
	var silentErr *cli.SilentError
	if !errors.As(err, &silentErr) {
		t.Fatalf("expected SilentError, got %T: %v", err, err)
	}
	if got := strings.Count(errBuf.String(), "Not a git repository"); got != 1 {
		t.Fatalf("not-git message count = %d, want 1; stderr:\n%s", got, errBuf.String())
	}
}

// TestRunReview_MissingHooksAborts verifies that `entire review` aborts with a
// clear error when the configured agent has no lifecycle hooks installed.
func TestRunReview_MissingHooksAborts(t *testing.T) {
	setupCmdTestRepo(t)

	// Save config but don't install hooks.
	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{testReviewSkill}},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when hooks are not installed")
	}
	if !strings.Contains(errBuf.String(), "Hooks are not installed") {
		t.Errorf("expected 'Hooks are not installed' in stderr, got: %s", errBuf.String())
	}

	_, ok, readErr := review.ReadPendingReviewMarker(context.Background())
	if readErr != nil || ok {
		t.Errorf("marker should not exist when hooks are missing: ok=%v err=%v", ok, readErr)
	}
}

// TestRunReview_NonLaunchableAgentPreservesMarker verifies that the pending
// marker is NOT cleared when a non-launchable agent is selected. Uses cursor
// because it has HookSupport but no Launcher.
//
// Regression: previously the cleanup defer was registered before the
// LauncherFor check, so the marker was wiped on the !ok path, breaking
// the hand-off message.
func TestRunReview_NonLaunchableAgentPreservesMarker(t *testing.T) {
	setupCmdTestRepo(t)

	const nonLaunchableAgent = "cursor"
	installHooksForCmdTest(t, types.AgentName(nonLaunchableAgent))

	// Confirm cursor has no Launcher; skip if a future change adds one.
	if _, hasLauncher := agent.LauncherFor(types.AgentName(nonLaunchableAgent)); hasLauncher {
		t.Skipf("%s now implements Launcher; pick another non-launchable agent", nonLaunchableAgent)
	}

	// Use prompt-only config: cursor has no curated built-ins, so a Skills
	// value would trip the installed-skill guard before reaching this path.
	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		nonLaunchableAgent: {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Marker written") {
		t.Errorf("expected marker-written message, got: %s", out)
	}

	m, ok, err := review.ReadPendingReviewMarker(context.Background())
	if err != nil {
		t.Fatalf("ReadPendingReviewMarker: %v", err)
	}
	if !ok {
		t.Fatal("marker was cleared — hand-off is broken")
	}
	if m.AgentName != nonLaunchableAgent {
		t.Errorf("AgentName = %q, want %s", m.AgentName, nonLaunchableAgent)
	}
}

// TestRunReview_MissingConfiguredSkillAbortsBeforeMarker verifies that a
// bogus configured skill aborts before writing the pending marker.
func TestRunReview_MissingConfiguredSkillAbortsBeforeMarker(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "claude-code")

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{"/bogus:skill-does-not-exist"}},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when configured skill not installed")
	}
	if !strings.Contains(errBuf.String(), "not installed") {
		t.Errorf("stderr should mention 'not installed', got: %s", errBuf.String())
	}
	_, markerExists, markerErr := review.ReadPendingReviewMarker(context.Background())
	if markerErr != nil {
		t.Fatalf("ReadPendingReviewMarker: %v", markerErr)
	}
	if markerExists {
		t.Error("pending marker should not exist when verification fails")
	}
}

// TestRunReview_PromptOnlyConfigSkipsVerification verifies that a prompt-only
// config (no Skills) skips the installed-skill guard and writes the marker.
func TestRunReview_PromptOnlyConfigSkipsVerification(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor")

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor": {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, markerExists, markerErr := review.ReadPendingReviewMarker(context.Background())
	if markerErr != nil {
		t.Fatalf("ReadPendingReviewMarker: %v", markerErr)
	}
	if !markerExists {
		t.Error("marker should exist for prompt-only config")
	}
}

// TestRunReview_FlagOverrideSkipsPicker verifies that --agent flag bypasses
// the interactive picker even when multiple eligible agents are configured.
func TestRunReview_FlagOverrideSkipsPicker(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor")
	installHooksForCmdTest(t, "opencode")

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor":   {Prompt: "review the diff"},
		"opencode": {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "--agent", "opencode"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m, ok, err := review.ReadPendingReviewMarker(context.Background())
	if err != nil || !ok {
		t.Fatalf("marker should be written: ok=%v err=%v", ok, err)
	}
	if m.AgentName != "opencode" {
		t.Errorf("AgentName = %q, want opencode", m.AgentName)
	}
}

// TestRunReview_FlagOverrideMustBeEligibleAgent verifies that --agent with an
// agent that has no hooks installed gives a clear error.
func TestRunReview_FlagOverrideMustBeEligibleAgent(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor")
	// opencode has no hooks installed

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor":   {Prompt: "review the diff"},
		"opencode": {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "--agent", "opencode"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when --agent points at hookless agent")
	}
	if !strings.Contains(errBuf.String(), "Hooks are not installed") {
		t.Errorf("stderr should mention 'Hooks are not installed', got: %s", errBuf.String())
	}
}

// --- Dispatch fork tests (CU8) ---
//
// These tests exercise the dispatch fork added in CU8 using a minimal Deps
// struct with injected stubs instead of the full cli.NewRootCmd() path. This
// avoids needing real hooks or agent binaries.

// newDispatchTestDeps builds a Deps stub suitable for dispatch fork tests.
// Agents in launchableAgents get a non-nil ReviewerFor; others return nil.
func newDispatchTestDeps(
	t *testing.T,
	installed []types.AgentName,
	launchableAgents []string,
) review.Deps {
	t.Helper()
	launchableSet := make(map[string]struct{}, len(launchableAgents))
	for _, name := range launchableAgents {
		launchableSet[name] = struct{}{}
	}
	return review.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return installed
		},
		NewSilentError: func(err error) error { return err },
		HeadHasReviewCheckpoint: func(_ context.Context) (bool, string) {
			return false, "" // no review guard
		},
		ReviewerFor: func(agentName string) reviewtypes.AgentReviewer {
			if _, ok := launchableSet[agentName]; ok {
				return &stubDispatchReviewer{name: agentName}
			}
			return nil
		},
	}
}

// stubDispatchReviewer is a minimal AgentReviewer that immediately finishes
// successfully — used in dispatch fork tests to verify routing without
// running real agent logic.
type stubDispatchReviewer struct {
	name string
}

func (r *stubDispatchReviewer) Name() string { return r.name }
func (r *stubDispatchReviewer) Start(context.Context, reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return &stubDispatchProcess{}, nil
}

type stubDispatchProcess struct{}

func (p *stubDispatchProcess) Events() <-chan reviewtypes.Event {
	ch := make(chan reviewtypes.Event, 2)
	ch <- reviewtypes.Started{}
	ch <- reviewtypes.Finished{Success: true}
	close(ch)
	return ch
}

func (p *stubDispatchProcess) Wait() error { return nil }

// Compile-time interface check.
var _ reviewtypes.AgentReviewer = (*stubDispatchReviewer)(nil)
var _ reviewtypes.Process = (*stubDispatchProcess)(nil)

type captureRunConfigReviewer struct {
	name   string
	called bool
	got    reviewtypes.RunConfig
}

func (r *captureRunConfigReviewer) Name() string { return r.name }
func (r *captureRunConfigReviewer) Start(_ context.Context, cfg reviewtypes.RunConfig) (reviewtypes.Process, error) {
	r.called = true
	r.got = cfg
	return &stubDispatchProcess{}, nil
}

func TestRunReview_ConfigPromptAugmentsSelectedSkills(t *testing.T) {
	setupCmdTestRepo(t)

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {
			Skills: []string{"/review"},
			Prompt: "Focus on auth regressions.",
		},
	}); err != nil {
		t.Fatal(err)
	}

	reviewer := &captureRunConfigReviewer{name: "claude-code"}
	deps := review.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{"claude-code"}
		},
		NewSilentError: func(err error) error { return err },
		HeadHasReviewCheckpoint: func(_ context.Context) (bool, string) {
			return false, ""
		},
		ReviewerFor: func(agentName string) reviewtypes.AgentReviewer {
			if agentName == "claude-code" {
				return reviewer
			}
			return nil
		},
	}

	out := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewer.got.PromptOverride != "" {
		t.Fatalf("PromptOverride = %q, want empty so skills still run", reviewer.got.PromptOverride)
	}
	if reviewer.got.AlwaysPrompt != "Focus on auth regressions." {
		t.Fatalf("AlwaysPrompt = %q, want saved prompt as additional instructions", reviewer.got.AlwaysPrompt)
	}
	if len(reviewer.got.Skills) != 1 || reviewer.got.Skills[0] != "/review" {
		t.Fatalf("Skills = %v, want [/review]", reviewer.got.Skills)
	}
	if !strings.Contains(out.String(), "Running review with claude-code...") {
		t.Fatalf("output missing running line:\n%s", out.String())
	}
}

// TestRunReview_IncludingContextBanner_SingleAgent verifies the
// transparency banner: the single-agent path always prints a line
// describing the session/checkpoint-context state directly below the scope
// banner. The line includes ACTUAL counts (number of checkpoints + number
// of sessions) when context is present; the empty variant is preserved for
// no-history branches. The line is never omitted — it surfaces what
// `entire review` does for the user vs. running a review skill manually.
func TestRunReview_IncludingContextBanner_SingleAgent(t *testing.T) {
	const emptyLine = "No prior session or checkpoint context for this branch yet."
	tests := []struct {
		name     string
		result   review.ContextResult
		wantLine string
	}{
		{
			name: "itemised checkpoint bullet",
			result: review.ContextResult{
				Prompt: "ctx", Checkpoints: 2, Sessions: 1,
				CheckpointItems: []review.CheckpointScopeItem{
					{ID: "a1b2c3d4", Summary: "feat: add thing"},
					{ID: "b2c3d4e5", Summary: "fix: the bug"},
				},
				SessionItems: []review.SessionScopeItem{{ID: "ac3d5c6e", Agent: "Claude Code"}},
			},
			wantLine: "  • a1b2c3d4  feat: add thing",
		},
		{
			name: "checkpoint header shows count",
			result: review.ContextResult{
				Prompt: "ctx", Checkpoints: 2,
				CheckpointItems: []review.CheckpointScopeItem{
					{ID: "a1b2c3d4", Summary: "feat: add thing"},
					{ID: "b2c3d4e5", Summary: "fix: the bug"},
				},
			},
			wantLine: "Checkpoints in scope (2):",
		},
		{
			name: "session header and bullet",
			result: review.ContextResult{
				Prompt: "ctx", Sessions: 1,
				SessionItems: []review.SessionScopeItem{{ID: "3d4c9f88", Agent: "Codex"}},
			},
			wantLine: "In-progress sessions (1):",
		},
		{
			name:     "empty context advertised as absent",
			result:   review.ContextResult{},
			wantLine: emptyLine,
		},
	}
	for _, tc := range tests {
		// Cannot t.Parallel — t.Chdir in setupCmdTestRepo.
		t.Run(tc.name, func(t *testing.T) {
			setupCmdTestRepo(t)
			if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
				testAgentName: {Skills: []string{"/review"}},
			}); err != nil {
				t.Fatal(err)
			}
			reviewer := &captureRunConfigReviewer{name: testAgentName}
			deps := review.Deps{
				GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
					return []types.AgentName{testAgentName}
				},
				NewSilentError:          func(err error) error { return err },
				HeadHasReviewCheckpoint: func(_ context.Context) (bool, string) { return false, "" },
				ReviewCheckpointContext: func(_ context.Context, _ string, _ string) review.ContextResult {
					return tc.result
				},
				ReviewerFor: func(agentName string) reviewtypes.AgentReviewer {
					if agentName == testAgentName {
						return reviewer
					}
					return nil
				},
			}
			out := &bytes.Buffer{}
			cmd := review.NewCommand(deps)
			cmd.SetOut(out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if !strings.Contains(out.String(), tc.wantLine) {
				t.Fatalf("expected output to contain %q; output:\n%s", tc.wantLine, out.String())
			}
			// The line must appear AFTER the scope banner and BEFORE any
			// agent output. We don't have a strict agent-table marker on
			// the non-TTY path, but we can at least require the scope
			// banner to come first.
			bannerIdx := strings.Index(out.String(), "Reviewing ")
			ctxIdx := strings.Index(out.String(), tc.wantLine)
			if bannerIdx == -1 || ctxIdx == -1 || ctxIdx < bannerIdx {
				t.Errorf("transparency line must follow the scope banner; output:\n%s", out.String())
			}
			// Sanity: the RunConfig threaded to the reviewer carries the
			// composed prompt — the banner change only affects printing,
			// it must not change the data plumbing.
			if reviewer.got.CheckpointContext != tc.result.Prompt {
				t.Errorf("CheckpointContext on RunConfig = %q, want %q",
					reviewer.got.CheckpointContext, tc.result.Prompt)
			}
		})
	}
}

// TestRunReview_RolesDispatchToMultiAgent verifies that when 2+ reviewers are
// configured (post-Chunk 2 role model), the multi-agent path is taken
// directly — no spawn-time picker is shown.
func TestRunReview_RolesDispatchToMultiAgent(t *testing.T) {
	setupCmdTestRepo(t)

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {
			Role:   settings.RoleReviewer,
			Skills: []string{"/review"},
			Prompt: "Claude saved prompt.",
		},
		testCodexAgent: {
			Role:   settings.RoleBoth,
			Skills: []string{"/review"},
			Prompt: "Codex saved prompt.",
		},
	}); err != nil {
		t.Fatal(err)
	}

	claudeReviewer := &captureRunConfigReviewer{name: "claude-code"}
	codexReviewer := &captureRunConfigReviewer{name: testCodexAgent}

	deps := review.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{"claude-code", testCodexAgent}
		},
		NewSilentError: func(err error) error { return err },
		HeadHasReviewCheckpoint: func(_ context.Context) (bool, string) {
			return false, ""
		},
		ReviewerFor: func(agentName string) reviewtypes.AgentReviewer {
			switch agentName {
			case "claude-code":
				return claudeReviewer
			case testCodexAgent:
				return codexReviewer
			default:
				return nil
			}
		},
	}

	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, tc := range []struct {
		name       string
		reviewer   *captureRunConfigReviewer
		wantPrompt string
	}{
		{name: "claude-code", reviewer: claudeReviewer, wantPrompt: "Claude saved prompt."},
		{name: "codex", reviewer: codexReviewer, wantPrompt: "Codex saved prompt."},
	} {
		if !tc.reviewer.called {
			t.Fatalf("%s reviewer was not started", tc.name)
		}
		if got := tc.reviewer.got.Skills; len(got) != 1 || got[0] != "/review" {
			t.Fatalf("%s Skills = %v, want [/review]", tc.name, got)
		}
		if tc.reviewer.got.AlwaysPrompt != tc.wantPrompt {
			t.Fatalf("%s AlwaysPrompt = %q, want %q", tc.name, tc.reviewer.got.AlwaysPrompt, tc.wantPrompt)
		}
		if tc.reviewer.got.StartingSHA == "" {
			t.Fatalf("%s StartingSHA is empty", tc.name)
		}
	}
}

// TestRunReview_RecursionGuard verifies that ENTIRE_REVIEW_SESSION in env
// refuses a nested review (prevents fan-out loops).
func TestRunReview_RecursionGuard(t *testing.T) {
	setupCmdTestRepo(t)
	t.Setenv("ENTIRE_REVIEW_SESSION", "outer-session-id")

	installed := []types.AgentName{"claude-code"}
	deps := newDispatchTestDeps(t, installed, []string{"claude-code"})

	errBuf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !strings.Contains(errBuf.String(), "Already in a review session") {
		t.Errorf("expected recursion-guard message, got:\n%s", errBuf.String())
	}
}

// TestRunReview_ReviewersFlagSingleAgent verifies that --reviewers <agent>
// runs review with that agent only and ignores saved config.
func TestRunReview_ReviewersFlagSingleAgent(t *testing.T) {
	setupCmdTestRepo(t)

	// Save a different agent in config — should be ignored.
	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"codex": {Role: settings.RoleReviewer, Skills: []string{"/review"}},
	}); err != nil {
		t.Fatal(err)
	}

	claudeReviewer := &captureRunConfigReviewer{name: testAgentName}
	deps := review.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{testAgentName, "codex"}
		},
		NewSilentError:          func(err error) error { return err },
		HeadHasReviewCheckpoint: func(_ context.Context) (bool, string) { return false, "" },
		ReviewerFor: func(agentName string) reviewtypes.AgentReviewer {
			if agentName == testAgentName {
				return claudeReviewer
			}
			return nil
		},
	}
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--reviewers", "claude-code"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claudeReviewer.called {
		t.Fatal("claude-code reviewer should have been started")
	}
}

// TestRunReview_ReviewersFlagUnknownAgentErrors verifies that --reviewers
// with an uninstalled agent name produces a clear error.
func TestRunReview_ReviewersFlagUnknownAgentErrors(t *testing.T) {
	setupCmdTestRepo(t)

	deps := newDispatchTestDeps(t, []types.AgentName{"claude-code"}, []string{"claude-code"})

	errBuf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--reviewers", "no-such-agent"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(errBuf.String(), "no-such-agent") {
		t.Errorf("error should name the unknown agent, got: %q", errBuf.String())
	}
}

// TestRunReview_FixerOnlyFlagErrorsAtReviewLayer verifies that
// `entire review --fixer X` (without --reviewers) errors at the
// review-command layer with a helpful message. The check lives in
// runReview rather than resolveRolesFromFlags so that
// `entire review fix --fixer X` (which only needs a Fixer) is not
// rejected by the shared flag-resolution path.
func TestRunReview_FixerOnlyFlagErrorsAtReviewLayer(t *testing.T) {
	setupCmdTestRepo(t)

	deps := newDispatchTestDeps(t,
		[]types.AgentName{"claude-code", "codex"},
		[]string{"claude-code", "codex"},
	)

	errBuf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--fixer", "codex"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --fixer is set without --reviewers, got nil")
	}
	stderr := errBuf.String()
	if !strings.Contains(stderr, "needs at least one") {
		t.Errorf("expected error to mention needing at least one reviewer, got: %s", stderr)
	}
	if !strings.Contains(stderr, "--reviewers") {
		t.Errorf("expected error to mention --reviewers as the fix, got: %s", stderr)
	}
	if !strings.Contains(stderr, "entire review fix") {
		t.Errorf("expected error to mention `entire review fix` as the alternative, got: %s", stderr)
	}
}

// TestReviewFixCmd_FixerOnlyFlagAccepted verifies that
// `entire review fix --fixer codex` is NOT rejected by the shared
// flag-resolution path (the scope-bug fix). It may still fail later for
// unrelated reasons in this test repo (no findings, no review session),
// but the "needs at least one reviewer" check must NOT fire.
func TestReviewFixCmd_FixerOnlyFlagAccepted(t *testing.T) {
	setupCmdTestRepo(t)

	deps := newDispatchTestDeps(t,
		[]types.AgentName{"claude-code", "codex"},
		[]string{"claude-code", "codex"},
	)

	errBuf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"fix", "--fixer", "codex"})

	// We don't care whether the overall command succeeds — only that the
	// failure isn't the "needs at least one reviewer" check from
	// resolveRolesFromFlags.
	if err := cmd.Execute(); err != nil {
		t.Logf("entire review fix exited with error (expected — test repo has no findings): %v", err)
	}
	stderr := errBuf.String()
	if strings.Contains(stderr, "needs at least one") {
		t.Errorf("entire review fix --fixer X should not be rejected by the reviewer-required check; got: %s", stderr)
	}
	if strings.Contains(stderr, "Reviewer or Both") {
		t.Errorf("entire review fix --fixer X should not be rejected by the Reviewer-or-Both check; got: %s", stderr)
	}
}

// --- Synthesis sink dispatch tests (CU10) ---

// stubCmdSynthesisProvider is a minimal SynthesisProvider for cmd-level tests.
type stubCmdSynthesisProvider struct {
	called bool
}

func (s *stubCmdSynthesisProvider) Synthesize(_ context.Context, _ string) (string, error) {
	s.called = true
	return "synthesis verdict", nil
}

// TestComposeMultiAgentSinks exercises the sink-composition helper directly
// with explicit isTTY/canPrompt values, so we get real coverage of the TTY
// branch without depending on os.Stdout being a terminal during `go test`.
func TestComposeMultiAgentSinks(t *testing.T) {
	t.Parallel()

	provider := &stubCmdSynthesisProvider{}
	noopCancel := func() {}

	tests := []struct {
		name      string
		isTTY     bool
		canPrompt bool
		provider  review.SynthesisProvider
		wantTUI   bool
		wantDump  bool
		wantSynth bool
		wantTotal int
	}{
		{
			name:      "non-tty omits tui and synth",
			isTTY:     false,
			canPrompt: false,
			provider:  provider,
			wantDump:  true,
			wantTotal: 1,
		},
		{
			name:      "tty with provider and prompt appends synth",
			isTTY:     true,
			canPrompt: true,
			provider:  provider,
			wantTUI:   true,
			wantDump:  true,
			wantSynth: true,
			wantTotal: 3,
		},
		{
			name:      "tty without provider skips synth",
			isTTY:     true,
			canPrompt: true,
			provider:  nil,
			wantTUI:   true,
			wantDump:  true,
			wantTotal: 2,
		},
		{
			name:      "tty without prompt skips synth even with provider",
			isTTY:     true,
			canPrompt: false,
			provider:  provider,
			wantTUI:   true,
			wantDump:  true,
			wantTotal: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sinks := review.ExposedComposeMultiAgentSinks(review.SinkComposeInputs{
				Out:               &bytes.Buffer{},
				IsTTY:             tt.isTTY,
				CanPrompt:         tt.canPrompt,
				AgentNames:        []string{"a", "b"},
				CancelRun:         noopCancel,
				SynthesisProvider: tt.provider,
			})
			if got := len(sinks); got != tt.wantTotal {
				t.Fatalf("len(sinks)=%d, want %d", got, tt.wantTotal)
			}
			_, hasTUI := review.ExposedFindTUISink(sinks)
			if hasTUI != tt.wantTUI {
				t.Errorf("findTUISink found=%v, want %v", hasTUI, tt.wantTUI)
			}
			var hasDump, hasSynth bool
			for _, s := range sinks {
				switch s.(type) {
				case review.DumpSink:
					hasDump = true
				case review.SynthesisSink:
					hasSynth = true
				}
			}
			if hasDump != tt.wantDump {
				t.Errorf("DumpSink present=%v, want %v", hasDump, tt.wantDump)
			}
			if hasSynth != tt.wantSynth {
				t.Errorf("SynthesisSink present=%v, want %v", hasSynth, tt.wantSynth)
			}
		})
	}
}

func TestComposeSingleAgentSinks(t *testing.T) {
	t.Parallel()

	noopCancel := func() {}

	tests := []struct {
		name       string
		isTTY      bool
		canPrompt  bool
		wantTUI    bool
		wantDump   bool
		wantTotal  int
		wantOutput string
	}{
		{
			name:       "non-tty prints running line and uses dump only",
			wantDump:   true,
			wantTotal:  1,
			wantOutput: "Running review with agent-a...",
		},
		{
			name:      "tty uses tui and dump",
			isTTY:     true,
			canPrompt: true,
			wantTUI:   true,
			wantDump:  true,
			wantTotal: 2,
		},
		{
			name:       "tty without prompt falls back to running line",
			isTTY:      true,
			canPrompt:  false,
			wantDump:   true,
			wantTotal:  1,
			wantOutput: "Running review with agent-a...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := &bytes.Buffer{}
			sinks := review.ExposedComposeSingleAgentSinks(review.SingleAgentSinkComposeInputs{
				Out:       out,
				IsTTY:     tt.isTTY,
				CanPrompt: tt.canPrompt,
				AgentName: "agent-a",
				CancelRun: noopCancel,
			})
			if got := len(sinks); got != tt.wantTotal {
				t.Fatalf("len(sinks)=%d, want %d", got, tt.wantTotal)
			}
			_, hasTUI := review.ExposedFindTUISink(sinks)
			if hasTUI != tt.wantTUI {
				t.Errorf("findTUISink found=%v, want %v", hasTUI, tt.wantTUI)
			}
			var hasDump, hasSynth bool
			for _, s := range sinks {
				switch s.(type) {
				case review.DumpSink:
					hasDump = true
				case review.SynthesisSink:
					hasSynth = true
				}
			}
			if hasDump != tt.wantDump {
				t.Errorf("DumpSink present=%v, want %v", hasDump, tt.wantDump)
			}
			if hasSynth {
				t.Error("SynthesisSink should not be present for single-agent reviews")
			}
			if tt.wantOutput != "" && !strings.Contains(out.String(), tt.wantOutput) {
				t.Errorf("output missing %q:\n%s", tt.wantOutput, out.String())
			}
			if tt.wantOutput == "" && out.Len() != 0 {
				t.Errorf("expected no pre-run output, got:\n%s", out.String())
			}
		})
	}
}

func TestComposeSinks_TUIWritersRunBeforePostRunWriters(t *testing.T) {
	t.Parallel()
	provider := &stubSynthesisProvider{}

	multi := review.ExposedComposeMultiAgentSinks(review.SinkComposeInputs{
		Out:               &bytes.Buffer{},
		IsTTY:             true,
		CanPrompt:         true,
		AgentNames:        []string{"a", "b"},
		CancelRun:         func() {},
		SynthesisProvider: provider,
	})
	if len(multi) != 3 {
		t.Fatalf("multi sinks len = %d, want 3", len(multi))
	}
	if _, ok := multi[0].(*review.TUISink); !ok {
		t.Fatalf("multi sink[0] = %T, want *TUISink", multi[0])
	}
	if _, ok := multi[1].(review.DumpSink); !ok {
		t.Fatalf("multi sink[1] = %T, want DumpSink", multi[1])
	}
	if _, ok := multi[2].(review.SynthesisSink); !ok {
		t.Fatalf("multi sink[2] = %T, want SynthesisSink", multi[2])
	}

	single := review.ExposedComposeSingleAgentSinks(review.SingleAgentSinkComposeInputs{
		Out:       &bytes.Buffer{},
		IsTTY:     true,
		CanPrompt: true,
		AgentName: "a",
		CancelRun: func() {},
	})
	if len(single) != 2 {
		t.Fatalf("single sinks len = %d, want 2", len(single))
	}
	if _, ok := single[0].(*review.TUISink); !ok {
		t.Fatalf("single sink[0] = %T, want *TUISink", single[0])
	}
	if _, ok := single[1].(review.DumpSink); !ok {
		t.Fatalf("single sink[1] = %T, want DumpSink", single[1])
	}
}

// TestFindTUISink_NoTUIInSlice covers the not-found path so the caller's
// `if tuiSink, ok := findTUISink(sinks); ok` branch is exercised in both
// directions.
func TestFindTUISink_NoTUIInSlice(t *testing.T) {
	t.Parallel()
	sinks := []reviewtypes.Sink{review.DumpSink{W: &bytes.Buffer{}}}
	if tui, ok := review.ExposedFindTUISink(sinks); ok || tui != nil {
		t.Errorf("findTUISink on dump-only slice returned (%v, %v); want (nil, false)", tui, ok)
	}
}

// TestDispatchFork_SynthesisSinkNilProviderNoComposition verifies that when
// deps.SynthesisProvider is nil, the command runs without panicking and does
// not attempt to synthesize (no synthesis output appears).
func TestDispatchFork_SynthesisSinkNilProviderNoComposition(t *testing.T) {
	setupCmdTestRepo(t)

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Role: settings.RoleReviewer, Prompt: "review"},
		"agent-b": {Role: settings.RoleReviewer, Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	installed := []types.AgentName{"agent-a", "agent-b"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a", "agent-b"})
	deps.SynthesisProvider = nil // explicitly nil — synthesis unavailable

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No synthesis output expected.
	if strings.Contains(buf.String(), "synthesis") {
		t.Errorf("no synthesis output expected when provider is nil, got: %s", buf.String())
	}
}

// TestDispatchFork_SingleAgentNoSynthesis verifies that the single-agent path
// never invokes synthesis (synthesis is multi-agent only). We set a provider
// but use a single launchable agent; the command should complete without
// calling the synthesis provider.
func TestDispatchFork_SingleAgentNoSynthesis(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor")

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &stubCmdSynthesisProvider{}

	// cursor is installed but not launchable (ReviewerFor returns nil).
	installed := []types.AgentName{"cursor"}
	deps := newDispatchTestDeps(t, installed, nil /* no launchable */)
	deps.SynthesisProvider = provider

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.called {
		t.Error("synthesis provider should NOT be called on single-agent path")
	}
}

func TestRunConfigWithReviewConfig_MapsModelAndReasoningEffort(t *testing.T) {
	t.Parallel()

	// Skills branch: per-spawn knobs map through alongside Skills/AlwaysPrompt.
	got := review.ExposedRunConfigWithReviewConfig(reviewtypes.RunConfig{}, settings.ReviewConfig{
		Role:            settings.RoleReviewer,
		Skills:          []string{"/review"},
		Prompt:          "Be thorough.",
		Model:           "gpt-5-mini",
		ReasoningEffort: "low",
	})
	if got.Model != "gpt-5-mini" {
		t.Errorf("Model = %q, want %q", got.Model, "gpt-5-mini")
	}
	if got.ReasoningEffort != "low" {
		t.Errorf("ReasoningEffort = %q, want %q", got.ReasoningEffort, "low")
	}
	if got.AlwaysPrompt != "Be thorough." {
		t.Errorf("AlwaysPrompt = %q, want %q", got.AlwaysPrompt, "Be thorough.")
	}

	// Prompt-only branch (no skills): knobs still map; Prompt becomes the
	// verbatim override.
	got = review.ExposedRunConfigWithReviewConfig(reviewtypes.RunConfig{}, settings.ReviewConfig{
		Role:            settings.RoleReviewer,
		Prompt:          "Just look at the diff.",
		ReasoningEffort: "medium",
	})
	if got.ReasoningEffort != "medium" {
		t.Errorf("prompt-only: ReasoningEffort = %q, want %q", got.ReasoningEffort, "medium")
	}
	if got.PromptOverride != "Just look at the diff." {
		t.Errorf("prompt-only: PromptOverride = %q, want %q", got.PromptOverride, "Just look at the diff.")
	}

	// Empty knobs stay empty (no accidental defaulting).
	got = review.ExposedRunConfigWithReviewConfig(reviewtypes.RunConfig{}, settings.ReviewConfig{
		Skills: []string{"/review"},
	})
	if got.Model != "" || got.ReasoningEffort != "" {
		t.Errorf("empty knobs leaked: Model=%q ReasoningEffort=%q", got.Model, got.ReasoningEffort)
	}
}
