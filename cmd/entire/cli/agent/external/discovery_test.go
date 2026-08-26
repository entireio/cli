package external

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// NOTE: Tests in this file modify process-global state (os.Setenv, t.Chdir, the
// agent registry, and the external ready/broken registries) and therefore cannot
// use t.Parallel().
//
// Every mock agent is written into a fresh t.TempDir(), so every exec in this
// file is a COLD one — see the infoTimeout doc comment for why that costs
// hundreds of milliseconds rather than the ~10ms a warm plugin does, and why on
// a saturated machine it has been seen to exceed two seconds.
//
// That is why no test here runs against the production budget. Tests that need
// a binary to SUCCEED raise it via withInfoTimeout so they can't flake when the
// machine is busy; tests that need one to TIME OUT lower it so they stay fast
// and deterministic. A happy-path assertion that depends on how loaded the CI
// box is would be worse than no assertion at all.

// setupDiscoveryTest clears the external ready/broken registries and installs a
// budget generous enough that no healthy binary can time out.
//
// The reset matters because the idempotency skip in scanPath would otherwise let
// one test's verdicts leak into the next and make failures depend on test order.
// The agent registry itself is never reset; tests use unique agent names.
//
// The budget matters because every mock here is a cold exec (see the file
// comment). Tests that specifically exercise the timeout path opt back down with
// withInfoTimeout(t, shortInfoTimeout).
func setupDiscoveryTest(t *testing.T) {
	t.Helper()
	reset := func() {
		discoveryMu.Lock()
		defer discoveryMu.Unlock()
		readyAgents = make(map[types.AgentName]agent.Agent)
		brokenAgents = make(map[types.AgentName]BrokenAgent)
	}
	reset()
	t.Cleanup(reset)
	withInfoTimeout(t, generousInfoTimeout)
}

// setupDiscoveryDir creates a temp directory containing a mock
// entire-agent-<name> binary and returns the directory path.
func setupDiscoveryDir(t *testing.T, agentName, infoJSON string) string {
	t.Helper()

	dir := t.TempDir()
	writeMockAgent(t, dir, agentName, mockInfoScript(infoJSON))
	return dir
}

// writeMockAgent writes an executable entire-agent-<name> script into dir.
func writeMockAgent(t *testing.T, dir, agentName, script string) {
	t.Helper()

	binPath := filepath.Join(dir, binaryPrefix+agentName)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
}

// writeStalledAgent writes an agent binary that never answers "info", so its
// load has to be cut off by the per-binary budget.
func writeStalledAgent(t *testing.T, dir, agentName string) {
	t.Helper()

	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	writeMockAgent(t, dir, agentName, fmt.Sprintf("#!/bin/sh\nexec %q 60\n", sleepPath))
}

// writeMarkerAgent writes an agent binary that appends a line to markerPath on
// every invocation, so a test can count how many times it was actually executed.
// body runs after the marker write (valid info JSON, garbage, whatever).
func writeMarkerAgent(t *testing.T, dir, agentName, markerPath, body string) {
	t.Helper()

	writeMockAgent(t, dir, agentName, fmt.Sprintf("#!/bin/sh\necho run >> %q\n%s\n", markerPath, body))
}

// markerRuns counts how many times a marker-writing agent binary was executed.
func markerRuns(t *testing.T, markerPath string) int {
	t.Helper()

	data, err := os.ReadFile(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read marker %q: %v", markerPath, err)
	}
	return len(strings.Fields(string(data)))
}

func makeInfoJSON(name string) string {
	return `{
  "protocol_version": 1,
  "name": "` + name + `",
  "type": "` + name + ` Agent",
  "description": "Agent ` + name + `",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {}
}`
}

// echoInfo is a shell body that answers "info" with valid metadata.
func echoInfo(name string) string {
	return "echo '" + makeInfoJSON(name) + "'"
}

// brokenEntry returns the broken-registry entry for name, failing if absent.
func brokenEntry(t *testing.T, name string) BrokenAgent {
	t.Helper()

	for _, b := range BrokenAgents() {
		if b.Name == types.AgentName(name) {
			return b
		}
	}
	t.Fatalf("agent %q not recorded as broken; broken = %v", name, BrokenAgents())
	return BrokenAgent{}
}

// requireNotBroken asserts name was not recorded in the broken registry.
func requireNotBroken(t *testing.T, name string) {
	t.Helper()

	for _, b := range BrokenAgents() {
		if b.Name == types.AgentName(name) {
			t.Fatalf("agent %q unexpectedly recorded as broken: %v", name, b.Err)
		}
	}
}

// enableExternalAgents creates a temp repo with external_agents enabled and
// chdir's into it so settings.Load can find the config.
func enableExternalAgents(t *testing.T) {
	t.Helper()
	writeRepoSettings(t, `{"enabled":true,"external_agents":true}`)
}

// disableExternalAgents creates a temp repo with external_agents absent (the
// default, i.e. disabled) and chdir's into it.
func disableExternalAgents(t *testing.T) {
	t.Helper()
	writeRepoSettings(t, `{"enabled":true}`)
}

func writeRepoSettings(t *testing.T, settingsJSON string) {
	t.Helper()

	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Chdir(tmpDir)
}

func requireSh(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

// generousInfoTimeout is used by tests that expect a binary to load. It is far
// above any legitimate cold-start cost, so these tests assert behaviour rather
// than machine speed.
const generousInfoTimeout = 60 * time.Second

// shortInfoTimeout is used by tests that expect the budget to expire. It is long
// enough that a healthy binary would still answer, so a timeout here means the
// binary really never responded.
const shortInfoTimeout = 250 * time.Millisecond

// withInfoTimeout overrides the per-binary budget for one test.
func withInfoTimeout(t *testing.T, d time.Duration) {
	t.Helper()

	original := infoTimeout
	infoTimeout = d
	t.Cleanup(func() { infoTimeout = original })
}

// warmUpBinary runs a mock agent once and discards the result, so the exec the
// test actually measures is a warm one (~10ms) rather than a cold one (hundreds
// of ms). Only for tests that must run a healthy binary under a short budget.
func warmUpBinary(t *testing.T, binPath string) {
	t.Helper()

	if err := exec.CommandContext(t.Context(), binPath, "info").Run(); err != nil {
		t.Fatalf("warm up %q: %v", binPath, err)
	}
}

// --- Scan and register: agents that load ---

func TestDiscoverAndRegister_FindsAgent(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-find"
	t.Setenv("PATH", setupDiscoveryDir(t, name, makeInfoJSON(name)))

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered, got error: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
	requireNotBroken(t, name)
}

func TestDiscoverAndRegister_Deduplication(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-dedup"
	dir1 := setupDiscoveryDir(t, name, makeInfoJSON(name))
	dir2 := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir1+string(os.PathListSeparator)+dir2)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

// TestDiscoverAndRegister_FirstPathDirWins pins that the earlier $PATH entry
// wins a duplicate name, which is what keeps registration deterministic even
// though the binaries are loaded concurrently.
func TestDiscoverAndRegister_FirstPathDirWins(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-firstwins"
	firstDir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	// The second copy on $PATH is broken; it must never be consulted.
	secondDir := t.TempDir()
	writeMockAgent(t, secondDir, name, "#!/bin/sh\necho 'not json'\n")
	t.Setenv("PATH", firstDir+string(os.PathListSeparator)+secondDir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err != nil {
		t.Fatalf("expected first-$PATH agent %q to be registered: %v", name, err)
	}
	requireNotBroken(t, name)
}

func TestDiscoverAndRegisterAlways_FindsAgentWithoutSettings(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	disableExternalAgents(t)

	name := "disc-always"
	t.Setenv("PATH", setupDiscoveryDir(t, name, makeInfoJSON(name)))

	DiscoverAndRegisterAlways(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered by DiscoverAndRegisterAlways: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

func TestDiscoverAndRegister_SkipsWhenDisabled(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	disableExternalAgents(t)

	name := "disc-disabled"
	marker := filepath.Join(t.TempDir(), "runs")
	dir := t.TempDir()
	writeMarkerAgent(t, dir, name, marker, echoInfo(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err == nil {
		t.Error("expected agent to NOT be registered when external_agents is disabled")
	}
	// The gate short-circuits before scanning, so nothing is recorded either.
	requireNotBroken(t, name)
	if runs := markerRuns(t, marker); runs != 0 {
		t.Errorf("disabled discovery executed the binary %d time(s), want 0", runs)
	}
}

func TestDiscoverAndRegister_EmptyPATH(t *testing.T) {
	setupDiscoveryTest(t)
	enableExternalAgents(t)
	t.Setenv("PATH", "")

	DiscoverAndRegister(context.Background()) // must not panic

	if got := BrokenAgents(); len(got) != 0 {
		t.Errorf("BrokenAgents() = %v, want empty for an empty PATH", got)
	}
}

func TestDiscoverAndRegister_UnreadableDir(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-unread"
	goodDir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", "/nonexistent/path"+string(os.PathListSeparator)+goodDir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err != nil {
		t.Fatalf("expected agent %q to be registered despite an unreadable dir in PATH: %v", name, err)
	}
}

// --- Scan and register: candidates that are not usable agents ---

func TestDiscoverAndRegister_SkipsDirectory(t *testing.T) {
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-dir"
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, binaryPrefix+name), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err == nil {
		t.Error("expected directory to be skipped, but agent was registered")
	}
	// A directory is not a plugin at all, so it must not be reported as broken.
	requireNotBroken(t, name)
}

// TestDiscoverAndRegister_NonExecutableIsBroken covers the forgot-chmod case.
// It used to be an invisible skip; surfacing it is why the broken registry exists.
func TestDiscoverAndRegister_NonExecutableIsBroken(t *testing.T) {
	if runtime.GOOS == osWindows {
		t.Skip("Windows does not use exec bits")
	}
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-noexec"
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name)
	if err := os.WriteFile(binPath, []byte(mockInfoScript(makeInfoJSON(name))), 0o644); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err == nil {
		t.Error("expected non-executable file to not be registered")
	}
	entry := brokenEntry(t, name)
	if !errors.Is(entry.Err, ErrNotExecutable) {
		t.Errorf("broken entry error = %v, want ErrNotExecutable", entry.Err)
	}
	if entry.BinaryPath != binPath {
		t.Errorf("broken entry BinaryPath = %q, want %q", entry.BinaryPath, binPath)
	}
}

// TestDiscoverAndRegister_RegisteredAgentWinsNameConflict pins the anti-hijack
// rule: an external binary never replaces an already-registered agent, and is
// not even executed. Without this, dropping entire-agent-claude-code anywhere on
// $PATH would take over transcript reading and checkpoint writing on every hook,
// because agent.Register overwrites process-wide and the gated scan runs in hooks.
func TestDiscoverAndRegister_RegisteredAgentWinsNameConflict(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-conflict"
	agent.Register(types.AgentName(name), func() agent.Agent {
		return nil // sentinel: a nil agent proves the original factory survived
	})

	marker := filepath.Join(t.TempDir(), "runs")
	dir := t.TempDir()
	writeMarkerAgent(t, dir, name, marker, echoInfo(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("agent should still be registered: %v", err)
	}
	if ag != nil {
		t.Errorf("external agent replaced the registered one: got %v, want the nil sentinel", ag)
	}
	if runs := markerRuns(t, marker); runs != 0 {
		t.Errorf("shadowing binary was executed %d time(s), want 0", runs)
	}
	// A shadowed binary is not a broken agent.
	requireNotBroken(t, name)
}

// --- Scan and register: failures are recorded, not swallowed ---

func TestDiscoverAndRegister_InvalidInfoIsBroken(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-badjson"
	dir := t.TempDir()
	writeMockAgent(t, dir, name, "#!/bin/sh\necho 'not json'\n")
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err == nil {
		t.Error("expected agent with bad info to not be registered")
	}
	entry := brokenEntry(t, name)
	if !strings.Contains(entry.Err.Error(), "invalid JSON") {
		t.Errorf("broken entry error = %v, want invalid JSON context", entry.Err)
	}
	if errors.Is(entry.Err, ErrInfoTimeout) {
		t.Errorf("broken entry error = %v, want a load failure, not a timeout", entry.Err)
	}
}

func TestDiscoverAndRegister_ContinuesAfterFailure(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	badName := "disc-scan-a-invalid"
	badDir := t.TempDir()
	writeMockAgent(t, badDir, badName, "#!/bin/sh\necho 'not json'\n")
	goodName := "disc-scan-z-valid"
	goodDir := setupDiscoveryDir(t, goodName, makeInfoJSON(goodName))
	t.Setenv("PATH", badDir+string(os.PathListSeparator)+goodDir)

	DiscoverAndRegisterAlways(context.Background())

	if _, err := agent.Get(types.AgentName(badName)); err == nil {
		t.Fatalf("invalid external agent %q was registered", badName)
	}
	if _, err := agent.Get(types.AgentName(goodName)); err != nil {
		t.Fatalf("valid external agent %q was not registered after a failure: %v", goodName, err)
	}
}

// TestDiscoverAndRegister_SlowBinariesDoNotBlockFastOnes is the regression this
// refactor exists to prevent. Serially, four stalled binaries would burn
// 4*infoTimeout before the valid one was even attempted — and under the old
// shared budget it would have been dropped entirely. Concurrently the whole scan
// costs roughly one infoTimeout.
func TestDiscoverAndRegister_SlowBinariesDoNotBlockFastOnes(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	// Short enough that the stalled binaries are cut off quickly, long enough that
	// the (warmed) fast one comfortably answers.
	withInfoTimeout(t, time.Second)
	enableExternalAgents(t)

	const stalledCount = 4
	dir := t.TempDir()
	for i := range stalledCount {
		writeStalledAgent(t, dir, fmt.Sprintf("disc-par-slow-%d", i))
	}
	fastName := "disc-par-fast"
	writeMockAgent(t, dir, fastName, mockInfoScript(makeInfoJSON(fastName)))
	// The fast binary must beat the budget on merit, not on cold-start luck.
	warmUpBinary(t, filepath.Join(dir, binaryPrefix+fastName))
	t.Setenv("PATH", dir)

	started := time.Now()
	DiscoverAndRegisterAlways(context.Background())
	elapsed := time.Since(started)

	if _, err := agent.Get(types.AgentName(fastName)); err != nil {
		t.Fatalf("fast agent %q was not registered alongside stalled ones: %v", fastName, err)
	}
	for i := range stalledCount {
		stalled := fmt.Sprintf("disc-par-slow-%d", i)
		entry := brokenEntry(t, stalled)
		if !errors.Is(entry.Err, ErrInfoTimeout) {
			t.Errorf("stalled agent %q error = %v, want ErrInfoTimeout", stalled, entry.Err)
		}
		if !errors.Is(entry.Err, context.DeadlineExceeded) {
			t.Errorf("stalled agent %q error = %v, want context.DeadlineExceeded", stalled, entry.Err)
		}
	}
	// Serial execution cannot finish in under stalledCount*infoTimeout, because
	// each stalled binary burns the whole budget in turn. Concurrent execution
	// needs ~infoTimeout plus cold-start overhead, so the threshold sits between
	// the two. The gap is what the assertion rests on, not absolute speed: the
	// budget is deliberately short here, and a cold start that eats the slack
	// would have to eat more than a whole extra budget to produce a false failure.
	if maxElapsed := stalledCount * infoTimeout / 2; elapsed > maxElapsed {
		t.Errorf("discovery took %v, want < %v (binaries loaded serially?)", elapsed, maxElapsed)
	}
}

// TestDiscoverAndRegister_CanceledCallerRecordsCandidates pins the agreed
// behaviour for a dead caller: the $PATH scan still runs (it is cheap and has no
// side effects) so the plugins found stay visible, but nothing is executed and
// the failure is attributed to cancellation rather than to the plugins.
func TestDiscoverAndRegister_CanceledCallerRecordsCandidates(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-canceled-caller"
	marker := filepath.Join(t.TempDir(), "runs")
	dir := t.TempDir()
	writeMarkerAgent(t, dir, name, marker, echoInfo(name))
	t.Setenv("PATH", dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	DiscoverAndRegisterAlways(ctx)

	entry := brokenEntry(t, name)
	if !errors.Is(entry.Err, context.Canceled) {
		t.Errorf("broken entry error = %v, want context.Canceled", entry.Err)
	}
	if errors.Is(entry.Err, ErrInfoTimeout) {
		t.Errorf("broken entry error = %v, want cancellation, not a timeout", entry.Err)
	}
	if runs := markerRuns(t, marker); runs != 0 {
		t.Errorf("binary executed %d time(s) under a canceled caller, want 0", runs)
	}
}

// TestDiscoverAndRegister_IdempotentAcrossCalls matters because setup.go reaches
// discovery up to three times per process from independently reachable entry
// points. A name with a verdict must not be re-executed, so the redundant calls
// cost a $PATH glob and nothing more.
func TestDiscoverAndRegister_IdempotentAcrossCalls(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	brokenName := "disc-idem-broken"
	brokenMarker := filepath.Join(t.TempDir(), "broken-runs")
	readyName := "disc-idem-ready"
	readyMarker := filepath.Join(t.TempDir(), "ready-runs")

	dir := t.TempDir()
	writeMarkerAgent(t, dir, brokenName, brokenMarker, "echo 'not json'")
	writeMarkerAgent(t, dir, readyName, readyMarker, echoInfo(readyName))
	t.Setenv("PATH", dir)

	DiscoverAndRegisterAlways(context.Background())
	firstBroken, firstReady := len(BrokenAgents()), len(ReadyAgents())

	DiscoverAndRegisterAlways(context.Background())

	if runs := markerRuns(t, brokenMarker); runs != 1 {
		t.Errorf("broken binary executed %d time(s) across two calls, want 1", runs)
	}
	if runs := markerRuns(t, readyMarker); runs != 1 {
		t.Errorf("ready binary executed %d time(s) across two calls, want 1", runs)
	}
	if got := len(BrokenAgents()); got != firstBroken {
		t.Errorf("BrokenAgents() len = %d after second call, want %d", got, firstBroken)
	}
	if got := len(ReadyAgents()); got != firstReady {
		t.Errorf("ReadyAgents() len = %d after second call, want %d", got, firstReady)
	}
}

// --- Get ---

func TestGet(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	readyName := "disc-get-ready"
	brokenName := "disc-get-broken"
	dir := t.TempDir()
	writeMockAgent(t, dir, readyName, mockInfoScript(makeInfoJSON(readyName)))
	writeMockAgent(t, dir, brokenName, "#!/bin/sh\necho 'not json'\n")
	t.Setenv("PATH", dir)

	DiscoverAndRegisterAlways(context.Background())

	ag, err := Get(types.AgentName(readyName))
	if err != nil {
		t.Fatalf("Get(%q) error = %v, want nil", readyName, err)
	}
	if string(ag.Name()) != readyName {
		t.Errorf("Get(%q).Name() = %q, want %q", readyName, ag.Name(), readyName)
	}

	_, err = Get(types.AgentName(brokenName))
	if err == nil {
		t.Fatalf("Get(%q) error = nil, want the recorded load failure", brokenName)
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("Get(%q) error = %v, want the recorded load failure", brokenName, err)
	}

	if _, err := Get(types.AgentName("disc-get-unknown")); err == nil {
		t.Error("Get(unknown) error = nil, want a not-found error")
	}
}

// --- Named discovery (shares the load path and the budget) ---

func TestDiscoverAndRegisterNamedAlways_Registers(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)

	name := types.AgentName("disc-named-ok")
	t.Setenv("PATH", setupDiscoveryDir(t, string(name), makeInfoJSON(string(name))))

	if err := DiscoverAndRegisterNamedAlways(context.Background(), name); err != nil {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v", err)
	}
	if _, err := agent.Get(name); err != nil {
		t.Fatalf("expected named agent %q to be registered: %v", name, err)
	}
}

// TestDiscoverAndRegisterNamedAlways_MissingHelper pins that an absent binary is
// "no such plugin, fall through", not a broken agent. The --summarize-provider
// override in explain_summary_provider.go depends on the nil error.
func TestDiscoverAndRegisterNamedAlways_MissingHelper(t *testing.T) {
	setupDiscoveryTest(t)

	name := types.AgentName("disc-named-missing")
	t.Setenv("PATH", t.TempDir())

	if err := DiscoverAndRegisterNamedAlways(context.Background(), name); err != nil {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want nil for a missing helper", err)
	}
	requireNotBroken(t, string(name))
}

func TestDiscoverAndRegisterNamedAlways_InvalidInfo(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)

	name := types.AgentName("disc-named-invalid-info")
	t.Setenv("PATH", setupDiscoveryDir(t, string(name), "not json"))

	err := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if err == nil {
		t.Fatal("DiscoverAndRegisterNamedAlways() error = nil, want an invalid info error")
	}
	if !strings.Contains(err.Error(), string(name)) {
		t.Errorf("error = %q, want agent name %q", err, name)
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("error = %q, want invalid info context", err)
	}
	// The failure is both returned to the caller and recorded for listing.
	brokenEntry(t, string(name))
}

// TestDiscoverAndRegisterNamedAlways_TimesOutStalledInfo pins that named
// discovery shares the strict per-binary budget rather than the old 10s one.
func TestDiscoverAndRegisterNamedAlways_TimesOutStalledInfo(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	withInfoTimeout(t, shortInfoTimeout)

	name := types.AgentName("disc-named-timeout")
	dir := t.TempDir()
	writeStalledAgent(t, dir, string(name))
	t.Setenv("PATH", dir)

	started := time.Now()
	err := DiscoverAndRegisterNamedAlways(context.Background(), name)
	elapsed := time.Since(started)

	if !errors.Is(err, ErrInfoTimeout) {
		t.Fatalf("error = %v, want ErrInfoTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	// Generous multiple of the budget: this asserts the call is bounded at all,
	// not how fast the machine is.
	if maxElapsed := 20 * shortInfoTimeout; elapsed > maxElapsed {
		t.Errorf("named discovery took %v, want < %v", elapsed, maxElapsed)
	}
	if _, err := agent.Get(name); err == nil {
		t.Error("stalled external agent was registered")
	}
}

func TestDiscoverAndRegisterNamedAlways_CanceledContext(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)

	name := types.AgentName("disc-named-canceled")
	t.Setenv("PATH", setupDiscoveryDir(t, string(name), makeInfoJSON(string(name))))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := DiscoverAndRegisterNamedAlways(ctx, name)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrInfoTimeout) {
		t.Errorf("error = %v, want cancellation, not a timeout", err)
	}
}

func TestDiscoverAndRegisterNamedAlways_RejectsPathSeparators(t *testing.T) {
	setupDiscoveryTest(t)

	originalLookPath := lookPathExternalAgent
	t.Cleanup(func() { lookPathExternalAgent = originalLookPath })

	for _, name := range []types.AgentName{"foo/../../agent", `foo\bar`} {
		lookedUp := false
		lookPathExternalAgent = func(string) (string, error) {
			lookedUp = true
			return "", exec.ErrNotFound
		}

		err := DiscoverAndRegisterNamedAlways(context.Background(), name)
		if err == nil || !strings.Contains(err.Error(), "path separators") {
			t.Errorf("DiscoverAndRegisterNamedAlways(%q) error = %v, want a path separator error", name, err)
		}
		if lookedUp {
			t.Errorf("DiscoverAndRegisterNamedAlways(%q) called exec.LookPath for an invalid name", name)
		}
	}
}

// --- Windows binary extensions ---

// TestDiscoverAndRegister_RegistersBatOnWindows verifies that a .bat agent
// binary is discovered and registered on Windows, with the file extension
// stripped from the agent name. .cmd and .exe follow the same code path.
func TestDiscoverAndRegister_RegistersBatOnWindows(t *testing.T) {
	if runtime.GOOS != osWindows {
		t.Skip("this test only applies on Windows")
	}
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-bat"
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name+".bat")
	if err := os.WriteFile(binPath, []byte(windowsInfoBat(name)), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered after stripping .bat: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

// TestDiscoverAndRegisterNamedAlways_RegistersBatOnWindows covers the explicit
// named-discovery path, which uses exec.LookPath and therefore depends on
// Windows PATHEXT handling rather than the scan-all filepath.Glob path above.
func TestDiscoverAndRegisterNamedAlways_RegistersBatOnWindows(t *testing.T) {
	if runtime.GOOS != osWindows {
		t.Skip("this test only applies on Windows")
	}
	setupDiscoveryTest(t)

	name := types.AgentName("disc-named-bat")
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+string(name)+".bat")
	if err := os.WriteFile(binPath, []byte(windowsInfoBat(string(name))), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	if err := DiscoverAndRegisterNamedAlways(context.Background(), name); err != nil {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v", err)
	}
	ag, err := agent.Get(name)
	if err != nil {
		t.Fatalf("expected named .bat agent %q to be registered: %v", name, err)
	}
	if ag.Name() != name {
		t.Fatalf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

func windowsInfoBat(name string) string {
	infoJSON := `{"protocol_version":1,"name":"` + name + `","type":"` + name + ` Agent","description":"Agent ` + name + `","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{}}`
	return "@echo off\r\nif not \"%1\"==\"info\" goto :notinfo\r\necho " + infoJSON +
		"\r\ngoto :eof\r\n:notinfo\r\necho unknown subcommand: %1 1>&2\r\nexit /b 1\r\n"
}

// --- IsExternal ---

func TestIsExternal_WrappedAgent(t *testing.T) {
	requireSh(t)
	setupDiscoveryTest(t)
	enableExternalAgents(t)

	name := "disc-isext"
	t.Setenv("PATH", setupDiscoveryDir(t, name, makeInfoJSON(name)))

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered: %v", name, err)
	}
	if !IsExternal(ag) {
		t.Error("IsExternal should return true for a wrapped external agent")
	}
}

func TestIsExternal_BuiltInAgent(t *testing.T) {
	name := "disc-builtin"
	builtIn := &fakeBuiltInAgent{name: types.AgentName(name)}
	agent.Register(types.AgentName(name), func() agent.Agent {
		return builtIn
	})

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered: %v", name, err)
	}
	if IsExternal(ag) {
		t.Error("IsExternal should return false for a built-in agent")
	}
}

// fakeBuiltInAgent is a minimal agent.Agent stub for testing IsExternal.
type fakeBuiltInAgent struct {
	name types.AgentName
}

func (f *fakeBuiltInAgent) Name() types.AgentName                        { return f.name }
func (f *fakeBuiltInAgent) Type() types.AgentType                        { return "fake" }
func (f *fakeBuiltInAgent) Description() string                          { return "fake" }
func (f *fakeBuiltInAgent) IsPreview() bool                              { return false }
func (f *fakeBuiltInAgent) DetectPresence(context.Context) (bool, error) { return false, nil }
func (f *fakeBuiltInAgent) ProtectedDirs() []string                      { return nil }
func (f *fakeBuiltInAgent) ReadTranscript(string) ([]byte, error)        { return nil, nil }
func (f *fakeBuiltInAgent) ChunkTranscript(context.Context, []byte, int) ([][]byte, error) {
	return nil, nil
}
func (f *fakeBuiltInAgent) ReassembleTranscript([][]byte) ([]byte, error) { return nil, nil }
func (f *fakeBuiltInAgent) GetSessionID(*agent.HookInput) string          { return "" }
func (f *fakeBuiltInAgent) GetSessionDir(string) (string, error)          { return "", nil }
func (f *fakeBuiltInAgent) ResolveSessionFile(string, string) string      { return "" }
func (f *fakeBuiltInAgent) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // test fake — no session is a valid state
}
func (f *fakeBuiltInAgent) WriteSession(context.Context, *agent.AgentSession) error { return nil }
func (f *fakeBuiltInAgent) FormatResumeCommand(string) string                       { return "" }
