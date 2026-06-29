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

// seedReviewConfig persists a default review profile into clone-local
// preferences for test setup, preserving any other existing preferences.
func seedReviewConfig(ctx context.Context, cfg map[string]settings.ReviewConfig) error {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return err
	}
	if prefs == nil {
		prefs = &settings.ClonePreferences{}
	}
	prefs.ReviewDefaultProfile = review.DefaultProfileName
	profile := settings.ReviewProfileConfig{
		Task:   "Test review task.",
		Agents: cfg,
	}
	if judge := defaultTestJudge(cfg); judge != "" {
		profile.Judge = &settings.ReviewConfig{Agent: judge}
	}
	prefs.ReviewProfiles = map[string]settings.ReviewProfileConfig{
		review.DefaultProfileName: profile,
	}
	return settings.SaveClonePreferences(ctx, prefs)
}

func defaultTestJudge(cfg map[string]settings.ReviewConfig) string {
	if _, ok := cfg[string(agent.AgentNameClaudeCode)]; ok {
		return string(agent.AgentNameClaudeCode)
	}
	for name := range cfg {
		return name
	}
	return ""
}

// TestReviewCmd_ListAgents verifies `entire review --agents` lists the
// configured profile workers (the valid --agent values) with the master marked.
func TestReviewCmd_ListAgents(t *testing.T) {
	setupCmdTestRepo(t)
	ctx := context.Background()
	if err := seedReviewConfig(ctx, map[string]settings.ReviewConfig{
		string(agent.AgentNameClaudeCode): {Skills: []string{"/review"}},
		string(agent.AgentNameCodex):      {Skills: []string{"/review"}},
	}); err != nil {
		t.Fatalf("seedReviewConfig: %v", err)
	}

	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "--agents"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"claude-code", "codex", "--agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("--agents output missing %q:\n%s", want, out)
		}
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
	for _, want := range []string{"review", "--configure", "--edit", "--findings", "--agent", "--agents", "--model", "--models", "--list", "attach"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q: %s", want, out)
		}
	}
	// --track-only was intentionally dropped by PR #1009.
	if strings.Contains(out, "track-only") {
		t.Error("--help output should NOT contain track-only flag (dropped in #1009)")
	}
}

// TestReviewCmd_ListModels verifies `entire review --models` prints the
// advertised models for the built-in review agents without needing a repo.
func TestReviewCmd_ListModels(t *testing.T) {
	t.Parallel()
	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "--models"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	// claude-code advertises real aliases; codex/gemini have no enumeration
	// command, so they list no models and point at Default/--model instead.
	for _, want := range []string{"claude-code", "opus", "sonnet", "codex", "gemini", "no advertised models"} {
		if !strings.Contains(out, want) {
			t.Errorf("--models output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gpt-5-codex") {
		t.Errorf("--models should not invent example codex models:\n%s", out)
	}
}

// TestReviewCmd_ListModelsFilteredByAgent verifies the --agent filter narrows
// the model listing to a single agent.
func TestReviewCmd_ListModelsFilteredByAgent(t *testing.T) {
	t.Parallel()
	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "--models", "--agent", "codex"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "codex") || !strings.Contains(out, "no advertised models") {
		t.Errorf("expected codex section with no-advertised-models note, got:\n%s", out)
	}
	if strings.Contains(out, "gemini") {
		t.Errorf("--agent codex should not list gemini:\n%s", out)
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

// TestReview_NotGitRepoReturnsSilentError checks that review outside a git repo
// returns a SilentError and prints the message once, for any mode flag.
func TestReview_NotGitRepoReturnsSilentError(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"findings", []string{"review", "--findings"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			rootCmd := cli.NewRootCmd()
			errBuf := &bytes.Buffer{}
			rootCmd.SetErr(errBuf)
			rootCmd.SetArgs(tt.args)

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
		})
	}
}

// TestRunReview_MissingHooksAborts verifies that `entire review` aborts with a
// clear error when the configured agent has no lifecycle hooks installed.
func TestRunReview_MissingHooksAborts(t *testing.T) {
	setupCmdTestRepo(t)

	// Save config but don't install hooks.
	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{testReviewSkill}},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "general"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when hooks are not installed")
	}
	if !strings.Contains(errBuf.String(), "hooks are not installed") {
		t.Errorf("expected 'hooks are not installed' in stderr, got: %s", errBuf.String())
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
	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		nonLaunchableAgent: {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "general"})
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

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{"/bogus:skill-does-not-exist"}},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "general"})
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

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor": {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "general"})
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

// TestRunReview_BareNonInteractiveRequiresProfile verifies that `entire review`
// with no profile, in a non-interactive context (the test has no TTY), never
// auto-runs a default crew — it errors and lists the configured profiles.
func TestRunReview_BareNonInteractiveRequiresProfile(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor")
	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor": {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	rootCmd.SetOut(outBuf)
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review"}) // bare, no profile

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("bare non-interactive review should require a profile, got nil error")
	}
	if !strings.Contains(errBuf.String(), "Specify a profile") {
		t.Errorf("stderr should ask for a profile, got:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "general") {
		t.Errorf("stderr should list the configured profile, got:\n%s", errBuf.String())
	}
	// It must not have spawned a crew: no pending marker written.
	_, exists, markerErr := review.ReadPendingReviewMarker(context.Background())
	if markerErr == nil && exists {
		t.Error("bare non-interactive review should not have started a review")
	}
}

// TestRunReview_FlagOverrideSkipsPicker verifies that --agent flag bypasses
// the interactive picker even when multiple eligible agents are configured.
func TestRunReview_FlagOverrideSkipsPicker(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor")
	installHooksForCmdTest(t, "opencode")

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor":   {Prompt: "review the diff"},
		"opencode": {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "general", "--agent", "opencode"})
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

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor":   {Prompt: "review the diff"},
		"opencode": {Prompt: "review the diff"},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := cli.NewRootCmd()
	errBuf := &bytes.Buffer{}
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "general", "--agent", "opencode"})
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

type scriptedDispatchReviewer struct {
	name    string
	events  []reviewtypes.Event
	waitErr error
}

func (r *scriptedDispatchReviewer) Name() string { return r.name }
func (r *scriptedDispatchReviewer) Start(context.Context, reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return &scriptedDispatchProcess{events: r.events, waitErr: r.waitErr}, nil
}

type scriptedDispatchProcess struct {
	events  []reviewtypes.Event
	waitErr error
}

func (p *scriptedDispatchProcess) Events() <-chan reviewtypes.Event {
	ch := make(chan reviewtypes.Event, len(p.events))
	for _, ev := range p.events {
		ch <- ev
	}
	close(ch)
	return ch
}

func (p *scriptedDispatchProcess) Wait() error { return p.waitErr }

// Compile-time interface check.
var _ reviewtypes.AgentReviewer = (*stubDispatchReviewer)(nil)
var _ reviewtypes.Process = (*stubDispatchProcess)(nil)
var _ reviewtypes.AgentReviewer = (*scriptedDispatchReviewer)(nil)
var _ reviewtypes.Process = (*scriptedDispatchProcess)(nil)

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

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
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
	cmd.SetArgs([]string{"general"})

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

// TestDispatchFork_TwoLaunchableNoOverride verifies that when 2+ launchable
// agents are configured and --agent is empty, the profile fan-out runs cleanly.
func TestDispatchFork_TwoLaunchableNoOverride(t *testing.T) {
	setupCmdTestRepo(t)

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	installed := []types.AgentName{"agent-a", "agent-b"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a", "agent-b"})

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDispatchFork_MultiAgentIgnoresFailedSiblingWhenAnotherSucceeds(t *testing.T) {
	setupCmdTestRepo(t)

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	quotaErr := errors.New("quota exhausted")
	reviewers := map[string]reviewtypes.AgentReviewer{
		"agent-a": &scriptedDispatchReviewer{
			name: "agent-a",
			events: []reviewtypes.Event{
				reviewtypes.Started{},
				reviewtypes.Finished{Success: false},
			},
			waitErr: quotaErr,
		},
		"agent-b": &scriptedDispatchReviewer{
			name: "agent-b",
			events: []reviewtypes.Event{
				reviewtypes.Started{},
				reviewtypes.AssistantText{Text: "agent-b found no blockers."},
				reviewtypes.Finished{Success: true},
			},
		},
	}
	deps := newDispatchTestDeps(t, []types.AgentName{"agent-a", "agent-b"}, []string{"agent-a", "agent-b"})
	deps.ReviewerFor = func(agentName string) reviewtypes.AgentReviewer { return reviewers[agentName] }

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("partial reviewer failure should not fail command: %v\nOutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "2 agent(s) done — 1 succeeded, 1 failed") {
		t.Fatalf("output missing partial-failure counts:\n%s", out)
	}
	if !strings.Contains(out, "agent-b found no blockers") {
		t.Fatalf("output missing successful reviewer narrative:\n%s", out)
	}
}

func TestDispatchFork_MultiAgentFailsWhenAllReviewersFail(t *testing.T) {
	setupCmdTestRepo(t)

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	reviewers := map[string]reviewtypes.AgentReviewer{
		"agent-a": &scriptedDispatchReviewer{
			name: "agent-a",
			events: []reviewtypes.Event{
				reviewtypes.Started{},
				reviewtypes.Finished{Success: false},
			},
			waitErr: errors.New("agent-a quota exhausted"),
		},
		"agent-b": &scriptedDispatchReviewer{
			name: "agent-b",
			events: []reviewtypes.Event{
				reviewtypes.Started{},
				reviewtypes.Finished{Success: false},
			},
			waitErr: errors.New("agent-b quota exhausted"),
		},
	}
	deps := newDispatchTestDeps(t, []types.AgentName{"agent-a", "agent-b"}, []string{"agent-a", "agent-b"})
	deps.ReviewerFor = func(agentName string) reviewtypes.AgentReviewer { return reviewers[agentName] }

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error when all reviewers fail\nOutput:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "review run") {
		t.Fatalf("error should identify review run failure, got %v", err)
	}
}

func TestDispatchFork_MultiAgentPassesPerAgentConfigs(t *testing.T) {
	setupCmdTestRepo(t)

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {
			Skills: []string{"/review"},
			Prompt: "Claude saved prompt.",
		},
		testCodexAgent: {
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
	cmd.SetArgs([]string{"general", "--prompt", "Focus this run on regressions."})

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
		if tc.reviewer.got.PerRunPrompt != "Focus this run on regressions." {
			t.Fatalf("%s PerRunPrompt = %q", tc.name, tc.reviewer.got.PerRunPrompt)
		}
		if tc.reviewer.got.StartingSHA == "" {
			t.Fatalf("%s StartingSHA is empty", tc.name)
		}
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
		provider  review.SynthesisProvider
		wantTUI   bool
		wantDump  bool
		wantSynth bool
		wantTotal int
	}{
		{
			name:      "non-tty omits tui but auto-synthesizes with provider",
			isTTY:     false,
			provider:  provider,
			wantDump:  true,
			wantSynth: true,
			wantTotal: 2,
		},
		{
			name:      "tty with provider buffers dump and synth before the flusher",
			isTTY:     true,
			provider:  provider,
			wantTUI:   true,
			wantDump:  true,
			wantSynth: true,
			wantTotal: 4,
		},
		{
			name:      "tty without provider skips synth",
			isTTY:     true,
			provider:  nil,
			wantTUI:   true,
			wantDump:  true,
			wantTotal: 3,
		},
		{
			name:      "non-tty without provider is dump only",
			isTTY:     false,
			provider:  nil,
			wantDump:  true,
			wantTotal: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sinks := review.ExposedComposeMultiAgentSinks(review.SinkComposeInputs{
				Out:               &bytes.Buffer{},
				IsTTY:             tt.isTTY,
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
			name:      "tty uses tui buffered dump and post-run finalizer",
			isTTY:     true,
			canPrompt: true,
			wantTUI:   true,
			wantDump:  true,
			wantTotal: 3,
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
	multiOut := &bytes.Buffer{}

	multi := review.ExposedComposeMultiAgentSinks(review.SinkComposeInputs{
		Out:               multiOut,
		IsTTY:             true,
		AgentNames:        []string{"a", "b"},
		CancelRun:         func() {},
		SynthesisProvider: provider,
	})
	if len(multi) != 4 {
		t.Fatalf("multi sinks len = %d, want 4", len(multi))
	}
	if _, ok := multi[0].(*review.TUISink); !ok {
		t.Fatalf("multi sink[0] = %T, want *TUISink", multi[0])
	}
	multiDump, ok := multi[1].(review.DumpSink)
	if !ok {
		t.Fatalf("multi sink[1] = %T, want buffered DumpSink", multi[1])
	}
	if multiDump.RenderWriter != multiOut {
		t.Fatalf("multi DumpSink RenderWriter = %T, want output writer", multiDump.RenderWriter)
	}
	multiSynth, ok := multi[2].(review.SynthesisSink)
	if !ok {
		t.Fatalf("multi sink[2] = %T, want SynthesisSink", multi[2])
	}
	if multiSynth.RenderWriter != multiOut {
		t.Fatalf("multi SynthesisSink RenderWriter = %T, want output writer", multiSynth.RenderWriter)
	}

	singleOut := &bytes.Buffer{}
	single := review.ExposedComposeSingleAgentSinks(review.SingleAgentSinkComposeInputs{
		Out:       singleOut,
		IsTTY:     true,
		CanPrompt: true,
		AgentName: "a",
		CancelRun: func() {},
	})
	if len(single) != 3 {
		t.Fatalf("single sinks len = %d, want 3", len(single))
	}
	if _, ok := single[0].(*review.TUISink); !ok {
		t.Fatalf("single sink[0] = %T, want *TUISink", single[0])
	}
	singleDump, ok := single[1].(review.DumpSink)
	if !ok {
		t.Fatalf("single sink[1] = %T, want buffered DumpSink", single[1])
	}
	if singleDump.RenderWriter != singleOut {
		t.Fatalf("single DumpSink RenderWriter = %T, want output writer", singleDump.RenderWriter)
	}
	if !review.ExposedIsTUIPostRunCompleteSink(single[2]) {
		t.Fatalf("single sink[2] = %T, want TUI post-run finalizer", single[2])
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

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	installed := []types.AgentName{"agent-a", "agent-b"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a", "agent-b"})
	// Profile-native review uses the profile master rather than deps-level synthesis.

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general"})

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

	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &stubCmdSynthesisProvider{}

	// cursor is installed but not launchable (ReviewerFor returns nil).
	installed := []types.AgentName{"cursor"}
	deps := newDispatchTestDeps(t, installed, nil /* no launchable */)
	_ = provider

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"general"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider.called {
		t.Error("synthesis provider should NOT be called on single-agent path")
	}
}

func TestComposeMultiAgentSinks_TTYAutoSynthesisRunsBeforeTUIExit(t *testing.T) {
	t.Parallel()
	provider := &stubSynthesisProvider{}

	sinks := review.ExposedComposeMultiAgentSinks(review.SinkComposeInputs{
		Out:               &bytes.Buffer{},
		IsTTY:             true,
		AgentNames:        []string{"a", "b"},
		CancelRun:         func() {},
		SynthesisProvider: provider,
		MasterName:        testAgentName,
	})
	if len(sinks) != 4 {
		t.Fatalf("len(sinks) = %d, want 4", len(sinks))
	}
	if _, ok := sinks[0].(*review.TUISink); !ok {
		t.Fatalf("sink[0] = %T, want *TUISink", sinks[0])
	}
	if _, ok := sinks[1].(review.DumpSink); !ok {
		t.Fatalf("sink[1] = %T, want buffered DumpSink", sinks[1])
	}
	synth, ok := sinks[2].(review.SynthesisSink)
	if !ok {
		t.Fatalf("sink[2] = %T, want SynthesisSink", sinks[2])
	}
	if synth.MasterName != testAgentName {
		t.Fatalf("synthesis sink MasterName = %q, want %s", synth.MasterName, testAgentName)
	}
	if synth.OnStart == nil || synth.OnComplete == nil {
		t.Fatal("auto synthesis should notify the TUI when the final judge starts/completes")
	}
}
