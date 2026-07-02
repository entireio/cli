package investigate_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// stubSpawner is a minimal spawn.Spawner used in tests. It returns a cmd
// that always succeeds, so production loop code can run without spawning a
// real agent.
type stubSpawner struct{ name string }

func (s stubSpawner) Name() string { return s.name }
func (s stubSpawner) BuildCmd(ctx context.Context, env []string, _ string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "true")
	cmd.Env = env
	return cmd
}

// silentPassthrough returns the same error unchanged. Mirrors review's
// test helper.
func silentPassthrough(err error) error { return err }

// setupInvestigateRepo creates a fresh git repo with one commit and chdirs
// into it. Mirrors review's setupCmdTestRepo.
func setupInvestigateRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	return tmp
}

// seedArg writes a temp markdown seed file with the given topic body and
// returns its absolute path. Tests that just need any valid topic input
// pass the return value as the positional [seed-doc] arg.
func seedArg(t *testing.T, topic string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.md")
	if err := os.WriteFile(path, []byte("# "+topic+"\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	return path
}

// captureLoopRun returns a LoopRun stub that records the LoopInput it was
// given. Useful for tests that want to assert flag plumbing without
// spawning real agents.
func captureLoopRun() (capture *investigate.LoopInput, fn func(ctx context.Context, in investigate.LoopInput, ldeps investigate.LoopDeps) (investigate.LoopResult, error)) {
	return captureLoopRunWithOutcome(investigate.OutcomeQuorum)
}

// captureLoopRunWithOutcome is captureLoopRun parameterised by the
// terminal outcome the stub returns. Used by the manifest-capture /
// per-run-dir cleanup tests which need to exercise both terminal
// (Quorum/Stalled) and resumable (Paused/Cancelled) branches.
func captureLoopRunWithOutcome(outcome investigate.LoopOutcome) (capture *investigate.LoopInput, fn func(ctx context.Context, in investigate.LoopInput, ldeps investigate.LoopDeps) (investigate.LoopResult, error)) {
	captured := &investigate.LoopInput{}
	return captured, func(_ context.Context, in investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
		*captured = in
		return investigate.LoopResult{
			Outcome: outcome,
			State:   nil,
		}, nil
	}
}

// newTestDeps builds a Deps wired with passthrough silent error and
// stub spawners for the named agents.
func newTestDeps(t *testing.T, installed []types.AgentName, launchable []string) investigate.Deps {
	t.Helper()
	launchableSet := make(map[string]struct{}, len(launchable))
	for _, n := range launchable {
		launchableSet[n] = struct{}{}
	}
	return investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return installed },
		NewSilentError:              silentPassthrough,
		SpawnerFor: func(name string) spawn.Spawner {
			if _, ok := launchableSet[name]; ok {
				return stubSpawner{name: name}
			}
			return nil
		},
		LaunchFix: func(_ context.Context, _ string, _ string) error { return nil },
	}
}

func TestNewCommand_RejectsConflictingInputs(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)

	// Validation runs before any I/O, so the seed path doesn't have to
	// exist on disk to exercise the [seed-doc]+--issue-link conflict.
	cmd.SetArgs([]string{"some-seed.md", "--issue-link=https://example.com/i/1"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when [seed-doc] and --issue-link are both set")
	}
}

func TestNewCommand_RejectsContinueWithSeed(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"some-seed.md", "--continue=abcdef012345"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when [seed-doc] and --continue are both set")
	}
}

func TestNewCommand_RejectsEditWithFindings(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"--edit", "--findings"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --edit and --findings are both set")
	}
}

func TestNewCommand_FixSubcommand_Help(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"fix", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "Launch a coding agent") {
		t.Errorf("--help output missing fix description: %s", out.String())
	}
}

func TestNewCommand_NotInGitRepoReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())

	deps := newTestDeps(t, nil, nil)
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{seedArg(t, "foo")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error outside a git repo")
	}
	if !strings.Contains(errBuf.String(), "Not a git repository") {
		t.Errorf("stderr should mention 'Not a git repository', got: %s", errBuf.String())
	}
}

func TestNewCommand_AgentsFlagOverrideUsed(t *testing.T) {
	setupInvestigateRepo(t)

	// Persist a settings file with two agents; --agents flag must override.
	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"agent-default-1", "agent-default-2"},
		MaxTurns: 3,
	}); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"override-a", "override-b"}, []string{"override-a", "override-b"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--agents=override-a,override-b",
		seedArg(t, "test investigation"),
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got, want := captured.Agents, []string{"override-a", "override-b"}; !equalStringSlices(got, want) {
		t.Errorf("LoopInput.Agents = %v, want %v", got, want)
	}
}

func TestNewCommand_FindingsBranchListsManifests(t *testing.T) {
	setupInvestigateRepo(t)

	deps := newTestDeps(t, nil, nil)
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--findings"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Empty store → "No local investigations found." message.
	if !strings.Contains(out.String(), "No local investigations found") {
		t.Errorf("stdout should report empty list, got: %s", out.String())
	}
}

// TestNewCommand_FreshRunWritesManifest exercises the end-to-end fresh-run
// path with a stub LoopRun. On the default OutcomeQuorum branch it
// verifies:
//   - the manifest file is written and the footer hint is printed
//   - findings.md content is embedded into the manifest's FindingsContent
//   - the per-run directory is cleaned up after capture
func TestNewCommand_FreshRunWritesManifest(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"stub-agent"},
		MaxTurns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"stub-agent"}, []string{"stub-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{seedArg(t, "test investigation")})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, errBuf.String())
	}
	if captured.RunID == "" {
		t.Fatal("LoopInput.RunID was empty — fresh-run path didn't generate one")
	}
	// Manifest should mention how to run fix.
	if !strings.Contains(out.String(), "entire investigate fix") {
		t.Errorf("expected fix hint in output, got:\n%s", out.String())
	}
	// Footer should embed the findings body (rendered via mdrender;
	// out is a bytes.Buffer so mdrender falls back to raw markdown,
	// and the scaffold's `# Investigation:` header is a stable anchor).
	if !strings.Contains(out.String(), "Investigation:") {
		t.Errorf("expected footer to embed findings body, got:\n%s", out.String())
	}

	// Manifest should have captured the findings body.
	manifestStore := investigate.NewLocalManifestStoreWithDir(
		filepath.Join(tmp, ".git", "entire-investigations", "manifests"),
	)
	m, ok, err := manifestStore.FindByRunID(context.Background(), captured.RunID)
	if err != nil {
		t.Fatalf("FindByRunID: %v", err)
	}
	if !ok {
		t.Fatal("manifest not written for run")
	}
	if m.FindingsContent == "" {
		t.Error("FindingsContent should be populated on Quorum outcome")
	}
	if !strings.Contains(m.FindingsContent, "# Investigation: test investigation") {
		t.Errorf("FindingsContent should embed the scaffold body, got: %q", m.FindingsContent)
	}

	// Per-run dir should be cleaned up.
	runDir := filepath.Join(tmp, ".git", "entire-investigations", captured.RunID)
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Errorf("per-run dir should be cleaned up on Quorum, but exists: %s (err=%v)", runDir, statErr)
	}
}

// TestNewCommand_FreshRunPausedKeepsPerRunDir verifies that resumable
// outcomes (Paused/Cancelled) leave the per-run directory in place so
// `entire investigate --continue` has files to read, and the manifest
// records the path with empty FindingsContent.
func TestNewCommand_FreshRunPausedKeepsPerRunDir(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"stub-agent"},
		MaxTurns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRunWithOutcome(investigate.OutcomePaused)
	deps := newTestDeps(t, []types.AgentName{"stub-agent"}, []string{"stub-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{seedArg(t, "paused investigation")})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, errBuf.String())
	}

	manifestStore := investigate.NewLocalManifestStoreWithDir(
		filepath.Join(tmp, ".git", "entire-investigations", "manifests"),
	)
	m, ok, err := manifestStore.FindByRunID(context.Background(), captured.RunID)
	if err != nil {
		t.Fatalf("FindByRunID: %v", err)
	}
	if !ok {
		t.Fatal("manifest not written for paused run")
	}
	if m.FindingsContent != "" {
		t.Errorf("FindingsContent should be empty on Paused, got %q", m.FindingsContent)
	}
	if m.FindingsDoc == "" {
		t.Error("FindingsDoc should still be recorded on Paused")
	}

	// Per-run dir must remain so --continue can resume.
	runDir := filepath.Join(tmp, ".git", "entire-investigations", captured.RunID)
	if _, statErr := os.Stat(runDir); statErr != nil {
		t.Errorf("per-run dir should remain on Paused, but stat failed: %v", statErr)
	}
	if _, statErr := os.Stat(m.FindingsDoc); statErr != nil {
		t.Errorf("findings.md should remain on Paused, but stat failed: %v", statErr)
	}

	// Footer should still embed the findings body — for paused outcomes
	// we read it from the on-disk file (the per-run dir is preserved).
	if !strings.Contains(out.String(), "Investigation:") {
		t.Errorf("expected footer to embed findings body on Paused, got:\n%s", out.String())
	}
}

// TestNewCommand_FreshRunRejectsNonLaunchableAgent verifies the spawner
// guard fires before the bootstrap step.
func TestNewCommand_FreshRunRejectsNonLaunchableAgent(t *testing.T) {
	setupInvestigateRepo(t)

	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"missing-spawner"},
		MaxTurns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	deps := newTestDeps(t, []types.AgentName{"missing-spawner"}, nil) // installed but not launchable
	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{seedArg(t, "foo")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when configured agent has no spawner")
	}
	if !strings.Contains(errBuf.String(), "spawner missing") {
		t.Errorf("stderr should mention 'spawner missing', got: %s", errBuf.String())
	}
}

func TestNewCommand_FreshRunRejectsAgentWithoutHooks(t *testing.T) {
	setupInvestigateRepo(t)

	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"hookless"},
		MaxTurns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Spawner exists but agent isn't in the installed list.
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return nil },
		NewSilentError:              silentPassthrough,
		SpawnerFor:                  func(_ string) spawn.Spawner { return stubSpawner{name: "hookless"} },
		LaunchFix:                   func(_ context.Context, _ string, _ string) error { return nil },
	}
	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{seedArg(t, "foo")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when configured agent has no hooks")
	}
	if !strings.Contains(errBuf.String(), "entire configure --agent") {
		t.Errorf("stderr should hint at `entire configure --agent`, got: %s", errBuf.String())
	}
}

func TestNewCommand_ContinueLoadsExistingState(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	// Create a state file in the conventional location.
	stateDir := filepath.Join(tmp, ".git", "entire-investigations")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "abcdef012345"
	st := &investigate.RunState{
		RunID:       runID,
		Topic:       "resumed topic",
		Agents:      []string{"resumed-agent"},
		MaxTurns:    2,
		FindingsDoc: filepath.Join(tmp, "find.md"),
		StartingSHA: "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"resumed-agent"}, []string{"resumed-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", runID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, errBuf.String())
	}
	if captured.RunID != runID {
		t.Errorf("LoopInput.RunID = %q, want %q", captured.RunID, runID)
	}
	if captured.Topic != "resumed topic" {
		t.Errorf("LoopInput.Topic = %q, want %q", captured.Topic, "resumed topic")
	}
	if !strings.Contains(out.String(), "Resuming investigation") {
		t.Errorf("expected 'Resuming investigation' banner, got: %s", out.String())
	}
}

// TestNewCommand_ContinueWritesTerminalManifest verifies that resuming a
// paused run and reaching a terminal outcome (quorum/stalled) rewrites the
// manifest with the new outcome and findings content. Without this the
// --findings / show / fix subcommands would still see "paused" + empty
// FindingsContent after a successful continuation.
func TestNewCommand_ContinueWritesTerminalManifest(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	stateDir := filepath.Join(tmp, ".git", "entire-investigations")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "112233445566"
	// Findings doc must live in the per-run subdir so the terminal-outcome
	// cleanup (os.RemoveAll(filepath.Dir(findingsDoc))) only nukes that
	// subdir, not the sibling manifests/ directory.
	runDir := filepath.Join(stateDir, runID)
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		t.Fatal(err)
	}
	findingsPath := filepath.Join(runDir, "findings.md")
	if err := os.WriteFile(findingsPath, []byte("# resumed findings body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	st := &investigate.RunState{
		RunID:       runID,
		Topic:       "resumed topic",
		Agents:      []string{"resumed-agent"},
		MaxTurns:    2,
		Quorum:      1,
		FindingsDoc: findingsPath,
		StartingSHA: "deadbeef",
		StartedAt:   startedAt,
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	// Seed the manifest dir with a paused-state record at the same StartedAt.
	manifestStore, err := investigate.NewLocalManifestStore(context.Background())
	if err != nil {
		t.Fatalf("manifest store: %v", err)
	}
	pausedManifest := investigate.LocalManifest{
		RunID:       runID,
		Topic:       "resumed topic",
		StartingSHA: "deadbeef",
		FindingsDoc: findingsPath,
		Agents:      []string{"resumed-agent"},
		Outcome:     string(investigate.OutcomePaused),
		StartedAt:   startedAt,
		EndedAt:     startedAt.Add(time.Minute),
	}
	if err := manifestStore.Write(context.Background(), pausedManifest); err != nil {
		t.Fatalf("seed paused manifest: %v", err)
	}

	// Stub the loop to terminate with Quorum so runContinue takes the
	// "terminal" branch in writeRunManifest.
	_, runFn := captureLoopRunWithOutcome(investigate.OutcomeQuorum)
	deps := newTestDeps(t, []types.AgentName{"resumed-agent"}, []string{"resumed-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--continue", runID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got, ok, err := manifestStore.FindByRunID(context.Background(), runID)
	if err != nil {
		t.Fatalf("FindByRunID: %v", err)
	}
	if !ok {
		t.Fatal("manifest disappeared after --continue")
	}
	if got.Outcome != string(investigate.OutcomeQuorum) {
		t.Errorf("manifest.Outcome = %q, want %q (paused -> quorum overwrite)", got.Outcome, investigate.OutcomeQuorum)
	}
	if got.FindingsContent == "" {
		t.Errorf("manifest.FindingsContent empty; expected findings to be captured on terminal outcome")
	}
}

// TestNewCommand_ContinueLoadsAlwaysPromptFromSettings verifies that the
// configured settings.Investigate.AlwaysPrompt is reloaded on resume —
// without this, a Ctrl+C plus --continue silently loses the user's
// "be skeptical, cite line numbers"-style preamble.
func TestNewCommand_ContinueLoadsAlwaysPromptFromSettings(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	const wantPrompt = "Be skeptical and cite line numbers."
	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:       []string{"resumed-agent"},
		MaxTurns:     2,
		AlwaysPrompt: wantPrompt,
	}); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(tmp, ".git", "entire-investigations")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "fedcba654321"
	st := &investigate.RunState{
		RunID:       runID,
		Topic:       "resumed topic",
		Agents:      []string{"resumed-agent"},
		MaxTurns:    2,
		FindingsDoc: filepath.Join(tmp, "find.md"),
		StartingSHA: "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"resumed-agent"}, []string{"resumed-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--continue", runID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if captured.AlwaysPrompt != wantPrompt {
		t.Errorf("LoopInput.AlwaysPrompt = %q, want %q (must survive --continue)", captured.AlwaysPrompt, wantPrompt)
	}
}

// TestNewCommand_ContinueRejectsAgentShrink verifies that resuming with a
// `--agents` override shorter than the persisted NextAgentIdx is refused
// with an actionable error rather than crashing the loop with index-out-
// of-range. Adversarial input (hand-edited state file or careless
// --agents) must not panic.
func TestNewCommand_ContinueRejectsAgentShrink(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	stateDir := filepath.Join(tmp, ".git", "entire-investigations")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "ababababcdcd"
	st := &investigate.RunState{
		RunID:        runID,
		Topic:        "shrink test",
		Agents:       []string{"a", "b", "c", "d"},
		NextAgentIdx: 3, // points at "d" in the persisted list
		MaxTurns:     2,
		FindingsDoc:  filepath.Join(tmp, "find.md"),
		StartingSHA:  "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	deps := newTestDeps(t, []types.AgentName{"a", "b"}, []string{"a", "b"})
	// LoopRun MUST NOT be invoked — we expect the bounds check to short-
	// circuit before reaching the loop.
	deps.LoopRun = func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
		t.Fatal("LoopRun must not run when persisted NextAgentIdx exceeds available agents")
		return investigate.LoopResult{}, nil
	}

	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", runID, "--agents", "a,b"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for agent-shrink resume")
	}
	if !strings.Contains(errBuf.String(), "exceeds available agents") {
		t.Errorf("stderr should explain the bounds violation; got: %s", errBuf.String())
	}
}

// TestNewCommand_ContinueWarnsOnSettingsLoadFailure verifies that a
// corrupt settings file on resume surfaces a visible warning instead of
// silently dropping the configured AlwaysPrompt. Without this, a user who
// breaks their settings.json mid-run would see the agent's behaviour
// change with no explanation.
func TestNewCommand_ContinueWarnsOnSettingsLoadFailure(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	// Write a malformed settings.json so settings.Load fails.
	if err := os.MkdirAll(filepath.Join(tmp, ".entire"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".entire", "settings.json"), []byte("{broken-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(tmp, ".git", "entire-investigations")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "cdcdcdcdcdcd"
	st := &investigate.RunState{
		RunID:       runID,
		Topic:       "warn test",
		Agents:      []string{"a"},
		MaxTurns:    1,
		FindingsDoc: filepath.Join(tmp, "find.md"),
		StartingSHA: "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"a"}, []string{"a"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", runID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "could not reload settings on --continue") {
		t.Errorf("stderr should warn about settings load failure; got: %s", errBuf.String())
	}
	if captured.AlwaysPrompt != "" {
		t.Errorf("AlwaysPrompt = %q, want empty when settings unavailable", captured.AlwaysPrompt)
	}
}

// TestNewCommand_ContinueWithMissingState surfaces an actionable error.
func TestNewCommand_ContinueWithMissingState(t *testing.T) {
	setupInvestigateRepo(t)

	deps := newTestDeps(t, nil, nil)
	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", "abcdef012345"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing run id")
	}
	if !strings.Contains(errBuf.String(), "no run state found") {
		t.Errorf("stderr should mention missing run, got: %s", errBuf.String())
	}
}

// --- helpers ---------------------------------------------------------------

// saveInvestigateSettings writes an InvestigateConfig into the CWD's
// .entire/settings.json.
func saveInvestigateSettings(cfg *settings.InvestigateConfig) error {
	ctx := context.Background()
	s, err := settings.Load(ctx)
	if err != nil {
		return err
	}
	if s == nil {
		s = &settings.EntireSettings{}
	}
	s.Investigate = cfg
	return settings.Save(ctx, s)
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunInvestigate_SoftWarnDeclinedReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)

	var loopCalled bool
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return nil },
		NewSilentError:              func(err error) error { return err },
		HeadHasInvestigateCheckpoint: func(_ context.Context) (bool, string) {
			return true, "checkpoint abc123"
		},
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			return false, nil // decline
		},
		LoopRun: func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
			loopCalled = true
			return investigate.LoopResult{}, nil
		},
	}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{seedArg(t, "foo")})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // soft-warn decline must not run the loop
	require.False(t, loopCalled, "loop must not run when user declines soft warn")
}

func TestRunFresh_SkipsMultipickerWhenAgentsFlagPresent(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".entire/settings.local.json"),
		[]byte(`{"investigate":{"agents":["claude-code","codex"]}}`), 0o644))

	var pickerCalls int
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{"claude-code", "codex"}
		},
		NewSilentError: func(err error) error { return err },
		SpawnerFor:     func(name string) spawn.Spawner { return stubSpawner{name: name} },
		InvestigateMultipicker: func(_ context.Context, _ []investigate.AgentChoice, _ bool) (investigate.PickedInvestigate, error) {
			pickerCalls++
			return investigate.PickedInvestigate{Names: []string{"claude-code"}}, nil
		},
		LoopRun: func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
			return investigate.LoopResult{Outcome: investigate.OutcomeQuorum}, nil
		},
	}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"--agents", "claude-code", seedArg(t, "foo")})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // contract is picker not invoked; downstream errors irrelevant
	require.Equal(t, 0, pickerCalls, "multipicker must not run when --agents is set")
}

func TestRunFresh_InvokesMultipickerWhenTwoAgentsAndNoFlag(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".entire/settings.local.json"),
		[]byte(`{"investigate":{"agents":["claude-code","codex"]}}`), 0o644))

	var pickerCalled bool
	var pickerAskPrompt bool
	var receivedAgents []string
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{"claude-code", "codex"}
		},
		NewSilentError: func(err error) error { return err },
		SpawnerFor:     func(name string) spawn.Spawner { return stubSpawner{name: name} },
		InvestigateMultipicker: func(_ context.Context, choices []investigate.AgentChoice, askPrompt bool) (investigate.PickedInvestigate, error) {
			pickerCalled = true
			pickerAskPrompt = askPrompt
			require.Len(t, choices, 2)
			return investigate.PickedInvestigate{
				Names: []string{"claude-code"},
			}, nil
		},
		LoopRun: func(_ context.Context, in investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
			receivedAgents = in.Agents
			return investigate.LoopResult{Outcome: investigate.OutcomeQuorum}, nil
		},
	}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{seedArg(t, "foo")})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // contract checked via captured loop input
	require.True(t, pickerCalled, "multipicker must run when >=2 agents and no --agents flag")
	require.False(t, pickerAskPrompt, "askPrompt must be false when a seed-doc is supplied")
	require.Equal(t, []string{"claude-code"}, receivedAgents, "narrowed list must reach the loop")
}

func TestRunInvestigate_SoftWarnAcceptedRunsLoop(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".entire/settings.local.json"),
		[]byte(`{"investigate":{"agents":["claude-code"],"max_turns":1}}`), 0o644))

	var loopCalled bool
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{types.AgentName("claude-code")}
		},
		NewSilentError: func(err error) error { return err },
		SpawnerFor:     func(_ string) spawn.Spawner { return stubSpawner{name: "claude-code"} },
		HeadHasInvestigateCheckpoint: func(_ context.Context) (bool, string) {
			return true, "checkpoint xyz"
		},
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			return true, nil // accept
		},
		LoopRun: func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
			loopCalled = true
			return investigate.LoopResult{Outcome: investigate.OutcomeQuorum}, nil
		},
	}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{seedArg(t, "foo")})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // soft-warn accept proceeds; ignore downstream errors
	require.True(t, loopCalled, "loop must run when user accepts soft warn")
}

// TestRunInvestigate_SoftWarnSilentInNonInteractive verifies that when
// the user can't prompt (PromptYN is nil and CanPromptInteractively
// returns false under `go test`), the soft-warn does NOT block the loop
// — it proceeds and a single informational log line is emitted.
func TestRunInvestigate_SoftWarnSilentInNonInteractive(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".entire/settings.local.json"),
		[]byte(`{"investigate":{"agents":["claude-code"],"max_turns":1}}`), 0o644))

	var loopCalled bool
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{types.AgentName("claude-code")}
		},
		NewSilentError: func(err error) error { return err },
		SpawnerFor:     func(_ string) spawn.Spawner { return stubSpawner{name: "claude-code"} },
		HeadHasInvestigateCheckpoint: func(_ context.Context) (bool, string) {
			return true, "checkpoint nonint"
		},
		// PromptYN intentionally nil → falls back to interactive.CanPromptInteractively(),
		// which returns false under `go test` → soft-warn is silent.
		LoopRun: func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
			loopCalled = true
			return investigate.LoopResult{Outcome: investigate.OutcomeQuorum}, nil
		},
	}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{seedArg(t, "foo")})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // non-interactive path proceeds
	require.True(t, loopCalled, "loop must run when soft-warn is silent (non-interactive)")
}
