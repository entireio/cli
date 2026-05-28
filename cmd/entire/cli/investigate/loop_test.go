package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
)

// fakeSpawner is a minimal Spawner used by the loop tests. The constructor
// returns an exec.Cmd that rewrites the run's state.json file to set
// PendingTurn — the loop reads that on return to record the stance.
type fakeSpawner struct {
	name       string
	onBuildCmd func(ctx context.Context, env []string, prompt string) *exec.Cmd
}

func (s *fakeSpawner) Name() string { return s.name }

func (s *fakeSpawner) BuildCmd(ctx context.Context, env []string, prompt string) *exec.Cmd {
	return s.onBuildCmd(ctx, env, prompt)
}

// shellCmd builds an exec.Cmd that runs a /bin/sh script with the supplied
// env. We use /bin/sh for portability; the scripts in this file only use
// POSIX features.
func shellCmd(ctx context.Context, env []string, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = env
	return cmd
}

// pendingTurnScript writes a fresh state.json (copied from the path in
// $ENTIRE_INVESTIGATE_STATE_DOC) with PendingTurn set. We use a tiny
// helper Go binary at runtime to avoid embedding a JSON parser in
// /bin/sh. Simplest: use jq if it exists, otherwise just do a here-doc
// rewrite that preserves the schema fields the loop already wrote.
//
// In practice the loop has already written state.json once before
// spawning, so the file always exists. We append/overwrite it with a
// Python or jq-flavoured rewrite — neither is universally installed in
// CI, so we cheat by using a Go test helper that calls
// writePendingTurnFromEnv() directly.
//
// However, the spawner is /bin/sh in these tests. To keep the script
// simple and dependency-free, we rewrite the file via a heredoc using
// the in-process helper bin available to the test process via
// $TEST_PENDING_HELPER. The helper accepts (state-path, stance, note)
// args, reads the file, merges PendingTurn, writes it back atomically.
//
// Since we don't have a separate helper binary, we instead instruct the
// loop tests to call setPendingTurn() directly between the call to
// BuildCmd and the agent process exit. The fake spawner does that via
// the onBuildCmd closure.

// failScript exits non-zero without touching state.json.
const failScript = `exit 1`

// noopScript exits 0 without touching state.json.
const noopScript = `exit 0`

// makeLoopFiles seeds a findings file in t.TempDir for a loop test, and
// returns its absolute path along with the state-store directory. The
// store directory is empty; the loop will create the per-run subdir on
// first Save.
func makeLoopFiles(t *testing.T) (findings, storeDir string) {
	t.Helper()
	dir := t.TempDir()
	findings = filepath.Join(dir, "findings.md")
	if err := os.WriteFile(findings, []byte("# Findings\n"), 0o600); err != nil {
		t.Fatalf("write findings: %v", err)
	}
	return findings, t.TempDir()
}

// writePendingTurn rewrites the state.json file at path so its PendingTurn
// field equals {stance, note}, preserving the rest of the file. Used by
// the fake spawner to simulate an agent writing its stance back to disk.
func writePendingTurn(t *testing.T, path, stance, note string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state for pending-turn write: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	raw["pending_turn"] = map[string]string{"stance": stance, "note": note}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// stableSpawner returns a SpawnerFor that runs scripts[agent] as the agent
// process, then (via the onBuildCmd wrapper) writes a PendingTurn into
// the state.json file at $ENTIRE_INVESTIGATE_STATE_DOC.
func stableSpawner(t *testing.T, scripts map[string]string, stances map[string]string) func(string) spawn.Spawner {
	return func(agent string) spawn.Spawner {
		script, ok := scripts[agent]
		if !ok {
			return nil
		}
		stance := stances[agent]
		return &fakeSpawner{
			name: agent,
			onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
				// If the agent has a stance to write, do it BEFORE the
				// shell script runs — the loop reads state.json AFTER
				// the shell process exits, so the ordering between
				// PendingTurn write and exec is only constrained by
				// "before the loop reads it back".
				if stance != "" {
					stateDoc := stateDocFromEnv(env)
					if stateDoc != "" {
						writePendingTurn(t, stateDoc, stance, "")
					}
				}
				return shellCmd(ctx, env, script)
			},
		}
	}
}

// stateDocFromEnv returns the value of $ENTIRE_INVESTIGATE_STATE_DOC in a
// KEY=VALUE env slice, or "" when absent. Mirrors helpers used in other
// test files.
func stateDocFromEnv(env []string) string {
	prefix := EnvStateDoc + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("loop tests rely on /bin/sh; skipping on Windows")
	}
}

// --- loop integration tests ----------------------------------------------

func TestRunInvestigateLoop_QuorumReachedFirstRound(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	store := NewStateStoreWithDir(storeDir)

	in := LoopInput{
		RunID:       "111111111111",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex", "gemini-cli"},
		MaxTurns:    3,
		Quorum:      0, // default to len(Agents)
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(t,
			map[string]string{
				"claude-code": noopScript,
				"codex":       noopScript,
				"gemini-cli":  noopScript,
			},
			map[string]string{
				"claude-code": "approve",
				"codex":       "approve",
				"gemini-cli":  "approve",
			},
		),
		States: store,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeQuorum {
		t.Fatalf("Outcome = %s, want quorum (state.Stances=%+v)", res.Outcome, res.State.Stances)
	}
	if len(res.State.Stances) != 3 {
		t.Errorf("stances = %d, want 3", len(res.State.Stances))
	}
	for i, s := range res.State.Stances {
		if s.Stance != stanceApprove {
			t.Errorf("stance[%d] = %q, want approve", i, s.Stance)
		}
	}
	if res.State.PendingTurn != nil {
		t.Errorf("PendingTurn = %+v, want nil after loop end", res.State.PendingTurn)
	}
}

func TestRunInvestigateLoop_QuorumDefault(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "222222222222",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		Quorum:      0, // → default to 2
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(t,
			map[string]string{"claude-code": noopScript, "codex": noopScript},
			map[string]string{"claude-code": "approve", "codex": "approve"},
		),
		States: NewStateStoreWithDir(storeDir),
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeQuorum {
		t.Fatalf("Outcome = %s, want quorum", res.Outcome)
	}
	if res.State.Quorum != 2 {
		t.Errorf("Quorum = %d, want 2 (default to len(Agents))", res.State.Quorum)
	}
}

func TestRunInvestigateLoop_Stalled(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "333333333333",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    2, // 4 overall turns, never reaching approve quorum
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(t,
			map[string]string{"claude-code": noopScript, "codex": noopScript},
			map[string]string{"claude-code": "request-changes", "codex": "request-changes"},
		),
		States: NewStateStoreWithDir(storeDir),
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %s, want stalled", res.Outcome)
	}
	if res.State.Turn != 4 {
		t.Errorf("Turn = %d, want 4", res.State.Turn)
	}
	if got := len(res.State.Stances); got != 4 {
		t.Errorf("Stances = %d, want 4", got)
	}
}

func TestRunInvestigateLoop_PausedOnTwoFailures(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "444444444444",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    3,
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(t,
			map[string]string{"claude-code": failScript, "codex": failScript},
			// No stances written — agents fail before they could.
			map[string]string{},
		),
		States: NewStateStoreWithDir(storeDir),
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomePaused {
		t.Fatalf("Outcome = %s, want paused", res.Outcome)
	}
	if res.Err == nil {
		t.Errorf("res.Err = nil, want underlying error")
	}
	// Both turns ran an executable that spawned successfully and exited
	// non-zero, so classifyRunErr should tag them as exit errors (not
	// spawn errors) — that distinction is what tells the operator the
	// agent's installed but misbehaving versus missing from PATH.
	if len(res.State.Stances) != 2 {
		t.Errorf("Stances = %d, want 2", len(res.State.Stances))
	}
	for i, s := range res.State.Stances {
		if s.Stance != stanceUnknown {
			t.Errorf("stance[%d] = %q, want unknown", i, s.Stance)
		}
		if !strings.Contains(s.Note, "exit error") {
			t.Errorf("stance[%d].Note = %q, want exit-error description", i, s.Note)
		}
	}
}

func TestRunInvestigateLoop_UnknownStanceWhenPendingTurnMissing(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "555555555555",
		Topic:       "test",
		Agents:      []string{"claude-code"},
		MaxTurns:    1, // 1 overall turn, no quorum possible
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(t,
			map[string]string{"claude-code": noopScript},
			// No stance — agent exits 0 without writing PendingTurn.
			map[string]string{},
		),
		States: NewStateStoreWithDir(storeDir),
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	// With one agent, default Quorum=1, but the only stance is "unknown" so
	// no quorum is reached → Stalled at end of turn budget.
	if res.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %s, want stalled", res.Outcome)
	}
	if len(res.State.Stances) != 1 {
		t.Fatalf("Stances = %d, want 1", len(res.State.Stances))
	}
	if got := res.State.Stances[0].Stance; got != stanceUnknown {
		t.Errorf("stance = %q, want unknown", got)
	}
}

// TestRunInvestigateLoop_MissingPendingTurnPausesAfterTwo verifies that an
// agent that exits cleanly but writes no PendingTurn counts as a soft
// failure: two consecutive missing PendingTurns trip pause-on-failure
// rather than burning the whole turn budget silently.
func TestRunInvestigateLoop_MissingPendingTurnPausesAfterTwo(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "777777777777",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    3, // 6 overall turns; pause should fire on turn 2
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(t,
			map[string]string{"claude-code": noopScript, "codex": noopScript},
			map[string]string{}, // No stances
		),
		States: NewStateStoreWithDir(storeDir),
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomePaused {
		t.Fatalf("Outcome = %s, want paused (two consecutive missing-PendingTurn failures should pause)", res.Outcome)
	}
	if got := len(res.State.Stances); got != 2 {
		t.Fatalf("Stances = %d, want 2 (loop should pause after the second consecutive failure)", got)
	}
}

func TestRunInvestigateLoop_PersistsStateEachTurn(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "666666666666",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    1, // 2 overall turns, request-changes → Stalled
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}

	var counter int32
	stances := map[string]string{"claude-code": "request-changes", "codex": "request-changes"}
	// Wrap stableSpawner so the test can observe a fresh load between turns.
	spawnerFor := func(agent string) spawn.Spawner {
		return &fakeSpawner{
			name: agent,
			onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
				stateDoc := stateDocFromEnv(env)
				if stateDoc != "" {
					writePendingTurn(t, stateDoc, stances[agent], "")
				}
				atomic.AddInt32(&counter, 1)
				return shellCmd(ctx, env, noopScript)
			},
		}
	}

	deps := LoopDeps{
		SpawnerFor: spawnerFor,
		States:     NewStateStoreWithDir(storeDir),
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %s, want stalled", res.Outcome)
	}

	// A fresh StateStore in the same dir should see all stances.
	fresh := NewStateStoreWithDir(storeDir)
	loaded, err := fresh.Load(context.Background(), in.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil")
	}
	if len(loaded.Stances) != 2 {
		t.Errorf("loaded stances = %d, want 2", len(loaded.Stances))
	}
	if loaded.Turn != 2 {
		t.Errorf("Turn = %d, want 2", loaded.Turn)
	}
}

func TestRunInvestigateLoop_Resume(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	store := NewStateStoreWithDir(storeDir)

	// Pre-existing state: agent[0] has already gone in turn 1 and approved.
	resumeState := &RunState{
		RunID:           "777777777777",
		Topic:           "test",
		Agents:          []string{"claude-code", "codex"},
		MaxTurns:        1, // Already used by claude-code, so codex's only turn closes round 1
		Quorum:          2,
		CompletedRounds: 0,
		Turn:            1,
		NextAgentIdx:    1, // Next is codex.
		Stances: []TurnStance{
			{Round: 1, Turn: 1, Agent: "claude-code", Stance: stanceApprove},
		},
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
		StartedAt:   time.Now().Add(-time.Hour),
	}

	in := LoopInput{
		RunID:       resumeState.RunID,
		Topic:       resumeState.Topic,
		Agents:      resumeState.Agents,
		MaxTurns:    1,
		Quorum:      2,
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
		Resume:      resumeState,
	}

	var observedAgent string
	spawnerFor := func(agent string) spawn.Spawner {
		return &fakeSpawner{
			name: agent,
			onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
				observedAgent = agent
				stateDoc := stateDocFromEnv(env)
				if stateDoc != "" {
					writePendingTurn(t, stateDoc, "approve", "")
				}
				return shellCmd(ctx, env, noopScript)
			},
		}
	}

	deps := LoopDeps{
		SpawnerFor: spawnerFor,
		States:     store,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if observedAgent != "codex" {
		t.Errorf("first spawned agent on resume = %q, want codex", observedAgent)
	}
	if res.Outcome != OutcomeQuorum {
		t.Errorf("Outcome = %s, want quorum (resume completed round)", res.Outcome)
	}
}

func TestRunInvestigateLoop_PlanChangedFlag(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "888888888888",
		Topic:       "test",
		Agents:      []string{"claude-code"},
		MaxTurns:    1,
		Quorum:      1,
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}

	// Agent modifies the findings file AND writes PendingTurn.
	spawnerFor := func(agent string) spawn.Spawner {
		return &fakeSpawner{
			name: agent,
			onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
				stateDoc := stateDocFromEnv(env)
				if stateDoc != "" {
					writePendingTurn(t, stateDoc, "approve", "looks good")
				}
				// Mutate findings so PlanChanged is true.
				script := fmt.Sprintf(`printf '\n## edited by %s\n' >> %q`, agent, findings)
				return shellCmd(ctx, env, script)
			},
		}
	}

	deps := LoopDeps{
		SpawnerFor: spawnerFor,
		States:     NewStateStoreWithDir(storeDir),
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if len(res.State.Stances) != 1 {
		t.Fatalf("Stances = %d, want 1", len(res.State.Stances))
	}
	s := res.State.Stances[0]
	if !s.PlanChanged {
		t.Errorf("PlanChanged = false, want true (findings was edited)")
	}
	if s.Note != "looks good" {
		t.Errorf("Note = %q, want %q (round-tripped from PendingTurn.Note)", s.Note, "looks good")
	}
}

func TestRunInvestigateLoop_CancelledContext(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "999999999999",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    3,
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	// Spawner returns a script that sleeps long enough to be cancelled.
	deps := LoopDeps{
		SpawnerFor: func(agent string) spawn.Spawner {
			return &fakeSpawner{
				name: agent,
				onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
					// 30s sleep; the test cancels after 50ms.
					return shellCmd(ctx, env, "sleep 30")
				},
			}
		},
		States: NewStateStoreWithDir(storeDir),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	res, err := RunInvestigateLoop(ctx, in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	// Either the in-progress turn was aborted (recorded as failure) and the
	// outer loop saw ctx.Err on next iteration → Cancelled, or the loop
	// drained back to OutcomePaused after two consecutive ctx-driven
	// failures. Both are acceptable terminal states; we only assert that
	// the loop terminated and persisted state.
	if res.Outcome != OutcomeCancelled && res.Outcome != OutcomePaused {
		t.Errorf("Outcome = %s, want cancelled or paused", res.Outcome)
	}
	if res.State == nil {
		t.Fatalf("State is nil")
	}
}

func TestRunInvestigateLoop_RejectsInvalidInput(t *testing.T) {
	t.Parallel()
	store := NewStateStoreWithDir(t.TempDir())
	deps := LoopDeps{
		SpawnerFor: func(string) spawn.Spawner { return nil },
		States:     store,
	}
	cases := []struct {
		name string
		in   LoopInput
	}{
		{"bad_run_id", LoopInput{RunID: "not-hex", Agents: []string{"a"}, FindingsDoc: "f"}},
		{"empty_agents", LoopInput{RunID: "aaaaaaaaaaaa", Agents: nil, FindingsDoc: "f"}},
		{"empty_findings", LoopInput{RunID: "aaaaaaaaaaaa", Agents: []string{"a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := RunInvestigateLoop(context.Background(), tc.in, deps)
			if err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

// TestRunInvestigateLoop_InvalidStanceRecordedAsUnknown verifies that when
// the agent writes a PendingTurn with a stance that isn't in the
// vocabulary, the loop records it as "unknown" with a diagnostic note
// (but counts it as "has pending" so the soft-failure pause doesn't fire).
func TestRunInvestigateLoop_InvalidStanceRecordedAsUnknown(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, storeDir := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "aaaaaaaaaaaa",
		Topic:       "test",
		Agents:      []string{"claude-code"},
		MaxTurns:    1,
		Quorum:      1,
		FindingsDoc: findings,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(t,
			map[string]string{"claude-code": noopScript},
			map[string]string{"claude-code": "wibble"}, // not a valid stance
		),
		States: NewStateStoreWithDir(storeDir),
	}
	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if len(res.State.Stances) != 1 {
		t.Fatalf("Stances = %d, want 1", len(res.State.Stances))
	}
	s := res.State.Stances[0]
	if s.Stance != stanceUnknown {
		t.Errorf("stance = %q, want unknown for invalid input", s.Stance)
	}
	if !strings.Contains(s.Note, "invalid stance") {
		t.Errorf("note = %q, want diagnostic about invalid stance", s.Note)
	}
}
