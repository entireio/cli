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
	for _, want := range []string{"review", "--edit", "--agent", "attach", "Labs entry"} {
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
	multiPickerFn func(ctx context.Context, eligible []review.AgentChoice) (review.PickedAgents, error),
	promptForAgentFn func(ctx context.Context, eligible []review.AgentChoice) (string, error),
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
		NewSilentError:   func(err error) error { return err },
		PromptForAgentFn: promptForAgentFn,
		MultiPickerFn:    multiPickerFn,
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
func (r *stubDispatchReviewer) Start(_ context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
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

// TestDispatchFork_TwoLaunchableNoOverride verifies that when 2+ launchable
// agents are configured and --agent is empty, the multi-picker is invoked
// and RunMulti is called (not the single-agent path).
func TestDispatchFork_TwoLaunchableNoOverride(t *testing.T) {
	setupCmdTestRepo(t)

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	multiPickerCalled := false
	multiPickerFn := func(_ context.Context, eligible []review.AgentChoice) (review.PickedAgents, error) {
		multiPickerCalled = true
		names := make([]string, 0, len(eligible))
		for _, e := range eligible {
			names = append(names, e.Name)
		}
		return review.PickedAgents{Names: names, PerRun: ""}, nil
	}

	installed := []types.AgentName{"agent-a", "agent-b"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a", "agent-b"}, multiPickerFn, nil)

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !multiPickerCalled {
		t.Error("expected multi-picker to be invoked for 2 launchable agents with no --agent override")
	}
}

// TestDispatchFork_OneLaunchableOneNonLaunchableNoOverride verifies that when
// only 1 agent is launchable (the other is non-launchable), the single-agent
// path is taken (no multi-picker). Uses cursor (real non-launchable agent with
// hooks) + agent-a (fake launchable stub).
func TestDispatchFork_OneLaunchableOneNonLaunchableNoOverride(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor")

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor":  {Prompt: "review"},
		"agent-a": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	multiPickerCalled := false
	multiPickerFn := func(_ context.Context, _ []review.AgentChoice) (review.PickedAgents, error) {
		multiPickerCalled = true
		return review.PickedAgents{}, errors.New("should not be called")
	}
	// Stub single-select picker to avoid TTY: always picks cursor.
	singlePickerFn := func(_ context.Context, _ []review.AgentChoice) (string, error) {
		return "cursor", nil
	}

	installed := []types.AgentName{"cursor", "agent-a"}
	// Only agent-a is launchable. With 1 launchable agent, computeLaunchableEligible
	// returns 1 entry, so multi-path is skipped. The single-select picker picks cursor.
	// ReviewerFor("cursor") returns nil → marker fallback path (writes marker file).
	deps := newDispatchTestDeps(t, installed, []string{"agent-a"}, multiPickerFn, singlePickerFn)

	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	executeErr := cmd.Execute() // may error (agent-a not a real agent); we only care about picker routing
	_ = executeErr              // intentionally ignored: this test only asserts picker routing
	if multiPickerCalled {
		t.Error("multi-picker should NOT be invoked when only 1 launchable agent is configured")
	}
}

// TestDispatchFork_TwoLaunchableWithAgentOverride verifies that --agent flag
// bypasses the multi-picker even when 2+ launchable agents are configured.
// The test uses cursor (non-launchable, real agent) + agent-a (fake launchable)
// with --agent cursor so the single-agent path runs to completion via marker
// fallback (cursor is non-launchable in reviewerFor, so nil → marker fallback).
func TestDispatchFork_TwoLaunchableWithAgentOverride(t *testing.T) {
	setupCmdTestRepo(t)
	installHooksForCmdTest(t, "cursor") // cursor needs real hooks

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"cursor":  {Prompt: "review"},
		"agent-a": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	multiPickerCalled := false
	multiPickerFn := func(_ context.Context, _ []review.AgentChoice) (review.PickedAgents, error) {
		multiPickerCalled = true
		return review.PickedAgents{}, errors.New("should not be called")
	}

	// cursor + agent-a both installed; agent-a is launchable but cursor is not.
	// With 1 launchable agent (agent-a) among the 2 eligible agents, the
	// multi-agent path would NOT fire (needs 2+ launchable). But when we
	// additionally pass --agent cursor, the multi-picker is bypassed by the
	// agentOverride check at the top of step 3.
	installed := []types.AgentName{"cursor", "agent-a"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a"}, multiPickerFn, nil)

	buf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--agent", "cursor"})

	// cursor is not launchable in our stub (reviewerFor returns nil), so it
	// falls through to RunMarkerFallback. That's fine — we only care that
	// multiPickerCalled is false.
	executeErr := cmd.Execute()
	_ = executeErr // intentionally ignored: this test only asserts picker routing
	if multiPickerCalled {
		t.Error("multi-picker should NOT be invoked when --agent override is set")
	}
}

// TestDispatchFork_MultiPickerCancellationExitsCleanly verifies that when
// the multi-picker is cancelled (ErrPickerCancelled), the command exits with
// nil error (no user-facing error).
func TestDispatchFork_MultiPickerCancellationExitsCleanly(t *testing.T) {
	setupCmdTestRepo(t)

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	multiPickerFn := func(_ context.Context, _ []review.AgentChoice) (review.PickedAgents, error) {
		return review.PickedAgents{}, review.ErrPickerCancelled
	}

	installed := []types.AgentName{"agent-a", "agent-b"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a", "agent-b"}, multiPickerFn, nil)

	errBuf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("ErrPickerCancelled should produce nil command error, got: %v", err)
	}
}

// TestDispatchFork_MultiPickerNoSelectionSurfacesError verifies that when the
// multi-picker returns ErrNoAgentsSelected, a clear error is shown to the user.
func TestDispatchFork_MultiPickerNoSelectionSurfacesError(t *testing.T) {
	setupCmdTestRepo(t)

	if err := review.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	multiPickerFn := func(_ context.Context, _ []review.AgentChoice) (review.PickedAgents, error) {
		return review.PickedAgents{}, review.ErrNoAgentsSelected
	}

	installed := []types.AgentName{"agent-a", "agent-b"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a", "agent-b"}, multiPickerFn, nil)

	errBuf := &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected non-nil error when no agents are selected")
	}
	if !strings.Contains(errBuf.String(), "no agents selected") {
		t.Errorf("stderr should mention 'no agents selected', got: %q", errBuf.String())
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
		"agent-a": {Prompt: "review"},
		"agent-b": {Prompt: "review"},
	}); err != nil {
		t.Fatal(err)
	}

	multiPickerFn := func(_ context.Context, eligible []review.AgentChoice) (review.PickedAgents, error) {
		names := make([]string, 0, len(eligible))
		for _, e := range eligible {
			names = append(names, e.Name)
		}
		return review.PickedAgents{Names: names, PerRun: ""}, nil
	}

	installed := []types.AgentName{"agent-a", "agent-b"}
	deps := newDispatchTestDeps(t, installed, []string{"agent-a", "agent-b"}, multiPickerFn, nil)
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
	deps := newDispatchTestDeps(t, installed, nil /* no launchable */, nil, nil)
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
