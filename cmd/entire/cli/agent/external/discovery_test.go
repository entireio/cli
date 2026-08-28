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

// NOTE: Tests in this file modify process-global state (os.Setenv, os.Chdir, agent registry)
// and therefore cannot use t.Parallel().

// discoveryTest isolates one discovery test.
//
// It clears external registry entries, because both the registry and the
// probe memo built from it are process-wide: without this, results leak
// between tests and the memo makes failures depend on test order.
//
// It also raises the info budget so behavioral tests do not depend on machine
// load. Timeout tests set their own smaller budget. Fixtures are never warmed,
// so call-site tests still exercise the cold execution path users get.
func discoveryTest(t *testing.T) {
	t.Helper()
	agent.ResetExternalsForTesting()
	t.Cleanup(agent.ResetExternalsForTesting)
	setInfoTimeout(t, 15*time.Second)
}

func setInfoTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := infoTimeout
	infoTimeout = d
	t.Cleanup(func() { infoTimeout = original })
}

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

// writeAgentBinary writes an executable entire-agent-<name> into dir with the
// given shell body and returns its path.
func writeAgentBinary(t *testing.T, dir, name, body string) string {
	t.Helper()
	binPath := filepath.Join(dir, binaryPrefix+name)
	if err := os.WriteFile(binPath, []byte(body), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	return binPath
}

// setupDiscoveryDir creates a temp directory containing a mock entire-agent-<name> binary.
// Returns the directory path.
func setupDiscoveryDir(t *testing.T, agentName, infoJSON string) string {
	t.Helper()
	dir := t.TempDir()
	writeAgentBinary(t, dir, agentName, mockInfoScript(infoJSON))
	return dir
}

func makeInfoJSON(name string) string {
	return makeInfoJSONWithType(name, name+" Agent")
}

func makeInfoJSONWithType(name, agentType string) string {
	return `{
  "protocol_version": 1,
  "name": "` + name + `",
  "type": "` + agentType + `",
  "description": "Agent ` + name + `",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {}
}`
}

// stalledScript returns a script body that never answers, so it always breaches
// the info budget.
func stalledScript(t *testing.T) string {
	t.Helper()
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	return fmt.Sprintf("#!/bin/sh\nexec %q 60\n", sleepPath)
}

// touchOnRunScript answers info correctly but also touches marker, so a test
// can prove whether the binary was executed at all.
func touchOnRunScript(name, marker string) string {
	return "#!/bin/sh\n: > " + marker + "\n" +
		"case \"$1\" in\n  info)\n    echo '" + makeInfoJSON(name) + "'\n    ;;\nesac\n"
}

// countOnRunScript appends a line to counter on every invocation, so a test can
// count executions across repeated discovery calls. reply is the info output.
func countOnRunScript(counter, reply string) string {
	return "#!/bin/sh\necho x >> " + counter + "\n" +
		"case \"$1\" in\n  info)\n    echo '" + reply + "'\n    ;;\nesac\n"
}

func runCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return len(strings.Fields(string(data)))
}

// failureFor returns the recorded failure for name, failing the test if absent.
func failureFor(t *testing.T, name string) agent.ExternalFailure {
	t.Helper()
	for _, f := range agent.ExternalFailures() {
		if f.Name == types.AgentName(name) {
			return f
		}
	}
	t.Fatalf("agent %q was not recorded as a failure; failures = %v", name, agent.ExternalFailures())
	return agent.ExternalFailure{}
}

// enableExternalAgents creates a temp repo with external_agents enabled in settings
// and chdir's into it so that settings.Load can find the config.
func enableExternalAgents(t *testing.T) {
	t.Helper()
	writeRepoSettings(t, `{"enabled":true,"external_agents":true}`)
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

func TestDiscoverAndRegister_FindsAgent(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-find"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered, got error: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

func TestDiscoverAndRegister_Deduplication(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
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

// TestDiscoverAndRegister_DedupesOnDerivedAgentName pins which binary wins when
// two directories offer the same agent: the earlier $PATH entry, as $PATH itself
// resolves. The two are told apart by the type they report.
func TestDiscoverAndRegister_DedupesOnDerivedAgentName(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-dedup-order"
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	writeAgentBinary(t, firstDir, name, mockInfoScript(makeInfoJSONWithType(name, "First On Path")))
	writeAgentBinary(t, secondDir, name, mockInfoScript(makeInfoJSONWithType(name, "Second On Path")))
	t.Setenv("PATH", firstDir+string(os.PathListSeparator)+secondDir)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered: %v", name, err)
	}
	if got := string(ag.Type()); got != "First On Path" {
		t.Errorf("agent Type() = %q, want the binary from the first PATH dir", got)
	}
}

// TestDiscoverAndRegister_InvalidEarlierPathEntryDoesNotHideValidBinary pins
// normal PATH lookup behavior: a directory with an executable-looking name is
// not a command and must not hide a runnable binary in a later PATH directory.
func TestDiscoverAndRegister_InvalidEarlierPathEntryDoesNotHideValidBinary(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-invalid-before-valid"
	invalidDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(invalidDir, binaryPrefix+name), 0o755); err != nil {
		t.Fatalf("create invalid PATH entry: %v", err)
	}
	validDir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", invalidDir+string(os.PathListSeparator)+validDir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err != nil {
		t.Fatalf("valid later-PATH agent %q was hidden by a directory: %v", name, err)
	}
	for _, failure := range agent.ExternalFailures() {
		if failure.Name == types.AgentName(name) {
			t.Errorf("usable agent %q remained recorded as broken: %v", name, failure.Err)
		}
	}
}

// TestDiscoverAndRegister_SkipsNameConflict covers the rule that a built-in
// always wins: the colliding binary must not even be executed, because the
// gated discovery path runs inside the git and agent hook trees.
func TestDiscoverAndRegister_SkipsNameConflict(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-conflict"
	agent.Register(types.AgentName(name), func() agent.Agent {
		return nil // placeholder
	})

	dir := t.TempDir()
	marker := filepath.Join(dir, "was-executed")
	writeAgentBinary(t, dir, name, touchOnRunScript(name, marker))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("agent should still be registered: %v", err)
	}
	// The placeholder factory returns nil, so it wasn't replaced.
	if ag != nil {
		t.Errorf("expected placeholder (nil) agent, got %v", ag)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("shadowing binary was executed; a colliding name must be skipped without running it")
	}
	if len(agent.ExternalFailures()) != 0 {
		t.Errorf("a name collision is not a broken agent, got failures = %v", agent.ExternalFailures())
	}
}

// TestDiscoverAndRegister_RecordsFailedAgent is the point of the refactor: a
// binary that exists but cannot be loaded keeps its reason instead of
// disappearing into a debug log, and does not stop a healthy binary loading.
func TestDiscoverAndRegister_RecordsFailedAgent(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	badName := "disc-bad-json"
	goodName := "disc-good-json"
	dir := t.TempDir()
	badPath := writeAgentBinary(t, dir, badName, "#!/bin/sh\necho 'not json'\n")
	writeAgentBinary(t, dir, goodName, mockInfoScript(makeInfoJSON(goodName)))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(goodName)); err != nil {
		t.Fatalf("healthy agent %q was not registered alongside a broken one: %v", goodName, err)
	}
	for _, name := range agent.List() {
		if name == types.AgentName(badName) {
			t.Errorf("broken agent %q is listed as usable", badName)
		}
	}

	failure := failureFor(t, badName)
	if failure.Binary != badPath {
		t.Errorf("failure.Binary = %q, want %q", failure.Binary, badPath)
	}
	if !strings.Contains(failure.Err.Error(), "invalid JSON") {
		t.Errorf("failure.Err = %q, want the parse failure to be diagnostic", failure.Err)
	}

	// Resolving it by name explains the problem rather than claiming it is unknown.
	_, err := agent.Get(types.AgentName(badName))
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("agent.Get(%q) error = %v, want the recorded load failure", badName, err)
	}
}

// TestDiscoverAndRegister_ParallelProbesDoNotSerialize is the regression this
// refactor exists to prevent: before it, one stalled binary consumed the whole
// discovery budget and every later binary was dropped.
//
// Four stalled binaries rather than one: with a single slow binary, serial and
// concurrent execution differ by only one budget, which is not a gap that can
// be asserted on reliably.
func TestDiscoverAndRegister_ParallelProbesDoNotSerialize(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	const budget = 10 * time.Second
	setInfoTimeout(t, budget)

	dir := t.TempDir()
	stalled := stalledScript(t)
	stalledNames := []string{"disc-par-slow1", "disc-par-slow2", "disc-par-slow3", "disc-par-slow4"}
	for _, name := range stalledNames {
		writeAgentBinary(t, dir, name, stalled)
	}
	goodName := "disc-par-good"
	writeAgentBinary(t, dir, goodName, mockInfoScript(makeInfoJSON(goodName)))
	t.Setenv("PATH", dir)

	started := time.Now()
	DiscoverAndRegister(context.Background())
	elapsed := time.Since(started)

	// Serial probing would cost at least 4 budgets; concurrent costs about one
	// plus the healthy binary's cold start.
	if elapsed >= time.Duration(len(stalledNames))*budget {
		t.Errorf("discovery took %v with %d stalled binaries at a %v budget; probes are serialized",
			elapsed, len(stalledNames), budget)
	}
	if _, err := agent.Get(types.AgentName(goodName)); err != nil {
		t.Errorf("healthy agent %q was not registered despite stalled neighbours: %v", goodName, err)
	}
	for _, name := range stalledNames {
		failureFor(t, name)
	}
}

func TestDiscoverAndRegister_TimeoutIsClassifiable(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)
	setInfoTimeout(t, 200*time.Millisecond)

	name := "disc-timeout-class"
	dir := t.TempDir()
	writeAgentBinary(t, dir, name, stalledScript(t))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	err := failureFor(t, name).Err
	if !errors.Is(err, ErrInfoTimeout) {
		t.Errorf("failure = %v, want ErrInfoTimeout so a tight budget is diagnosable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("failure = %v, want it to unwrap to context.DeadlineExceeded too", err)
	}
}

func TestDiscoverAndRegister_NonExecutableIsClassifiable(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-noexec"
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name)
	if err := os.WriteFile(binPath, []byte(mockInfoScript(makeInfoJSON(name))), 0o644); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if runtime.GOOS == osWindows {
		// Windows has no execute bit, so the file is a legitimate candidate there.
		t.Skip("execute bits are not meaningful on Windows")
	}
	if _, err := agent.Get(types.AgentName(name)); err == nil {
		t.Error("non-executable file was registered as a usable agent")
	}
	if err := failureFor(t, name).Err; !errors.Is(err, ErrNotExecutable) {
		t.Errorf("failure = %v, want ErrNotExecutable", err)
	}
}

func TestDiscoverAndRegister_DirectoryRecordedAsFailure(t *testing.T) {
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-dir"
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, binaryPrefix+name), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err == nil {
		t.Error("directory was registered as a usable agent")
	}
	if err := failureFor(t, name).Err; !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("failure = %v, want it to say the path is a directory", err)
	}
}

// TestDiscoverAndRegister_AlreadyCancelledCallerExecsNothing covers a caller
// that is out of time before discovery starts. Nothing is executed, but what
// was found on $PATH stays visible rather than looking uninstalled.
func TestDiscoverAndRegister_AlreadyCancelledCallerExecsNothing(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-cancelled-caller"
	dir := t.TempDir()
	marker := filepath.Join(dir, "was-executed")
	writeAgentBinary(t, dir, name, touchOnRunScript(name, marker))
	t.Setenv("PATH", dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	DiscoverAndRegister(ctx)

	if _, err := os.Stat(marker); err == nil {
		t.Error("binary was executed despite an already-cancelled caller")
	}
	if err := failureFor(t, name).Err; !errors.Is(err, context.Canceled) {
		t.Errorf("failure = %v, want context.Canceled", err)
	}
}

// TestDiscoverAndRegister_MemoizesReadyAndFailed pins that a repeated call does
// not re-execute anything. One setup flow calls discovery several times, so a
// failing binary would otherwise pay its full budget on each pass.
func TestDiscoverAndRegister_MemoizesReadyAndFailed(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	dir := t.TempDir()
	goodName := "disc-memo-good"
	badName := "disc-memo-bad"
	goodCounter := filepath.Join(dir, "good-runs")
	badCounter := filepath.Join(dir, "bad-runs")
	writeAgentBinary(t, dir, goodName, countOnRunScript(goodCounter, makeInfoJSON(goodName)))
	writeAgentBinary(t, dir, badName, countOnRunScript(badCounter, "not json"))
	t.Setenv("PATH", dir)

	DiscoverAndRegisterAlways(context.Background())
	firstGood, firstBad := runCount(t, goodCounter), runCount(t, badCounter)
	if firstGood != 1 || firstBad != 1 {
		t.Fatalf("first pass ran good=%d bad=%d times, want 1 each", firstGood, firstBad)
	}

	DiscoverAndRegisterAlways(context.Background())

	if got := runCount(t, goodCounter); got != 1 {
		t.Errorf("healthy binary executed %d times across two discovery calls, want 1", got)
	}
	if got := runCount(t, badCounter); got != 1 {
		t.Errorf("failing binary executed %d times across two discovery calls, want 1", got)
	}
	if _, err := agent.Get(types.AgentName(goodName)); err != nil {
		t.Errorf("healthy agent lost across the second call: %v", err)
	}
	failureFor(t, badName)
}

// TestDiscoverAndRegister_NameMismatchStillRegisters pins that the registry key
// comes from the file name. Callers resolve agents by that key, so a binary
// reporting a different name still works.
func TestDiscoverAndRegister_NameMismatchStillRegisters(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	binaryName := "disc-mismatch-file"
	declaredName := "disc-mismatch-declared"
	dir := t.TempDir()
	writeAgentBinary(t, dir, binaryName, mockInfoScript(makeInfoJSON(declaredName)))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(binaryName))
	if err != nil {
		t.Fatalf("expected agent to be registered under its binary name %q: %v", binaryName, err)
	}
	if string(ag.Name()) != declaredName {
		t.Errorf("agent Name() = %q, want the declared %q", ag.Name(), declaredName)
	}
	if _, err := agent.Get(types.AgentName(declaredName)); err == nil {
		t.Errorf("agent should not be resolvable under its declared name %q", declaredName)
	}
}

func TestDiscoverAndRegister_SkipsWhenDisabled(t *testing.T) {
	requireSh(t)
	discoveryTest(t)

	// A repo WITHOUT external_agents enabled (default false).
	writeRepoSettings(t, `{"enabled":true}`)

	name := "disc-disabled"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err == nil {
		t.Error("expected agent to NOT be registered when external_agents is disabled")
	}
	if len(agent.ExternalFailures()) != 0 {
		t.Errorf("gated-off discovery recorded failures = %v, want none", agent.ExternalFailures())
	}
}

func TestDiscoverAndRegister_EmptyPATH(t *testing.T) {
	discoveryTest(t)
	enableExternalAgents(t)
	t.Setenv("PATH", "")

	// Should return without error or panic.
	DiscoverAndRegister(context.Background())
}

func TestDiscoverAndRegister_UnreadableDir(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-unread"
	goodDir := setupDiscoveryDir(t, name, makeInfoJSON(name))

	// Include a non-existent directory in PATH — it should be silently skipped.
	t.Setenv("PATH", "/nonexistent/path"+string(os.PathListSeparator)+goodDir)

	DiscoverAndRegister(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err != nil {
		t.Fatalf("expected agent %q to be registered despite unreadable dir in PATH: %v", name, err)
	}
}

func TestDiscoverAndRegisterAlways_FindsAgentWithoutSettings(t *testing.T) {
	requireSh(t)
	discoveryTest(t)

	// Settings exist but external_agents is absent; Always bypasses the gate.
	writeRepoSettings(t, `{"enabled":true}`)

	name := "disc-always"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegisterAlways(context.Background())

	if _, err := agent.Get(types.AgentName(name)); err != nil {
		t.Fatalf("expected agent %q to be registered by DiscoverAndRegisterAlways: %v", name, err)
	}
}

func TestDiscoverAndRegister_ContinuesAfterRegistrationError(t *testing.T) {
	requireSh(t)
	discoveryTest(t)

	badName := "disc-scan-a-invalid"
	badDir := setupDiscoveryDir(t, badName, "not json")
	goodName := "disc-scan-z-valid"
	goodDir := setupDiscoveryDir(t, goodName, makeInfoJSON(goodName))
	t.Setenv("PATH", badDir+string(os.PathListSeparator)+goodDir)

	DiscoverAndRegisterAlways(context.Background())

	if _, err := agent.Get(types.AgentName(badName)); err == nil {
		t.Fatalf("invalid external agent %q was registered", badName)
	}
	if _, err := agent.Get(types.AgentName(goodName)); err != nil {
		t.Fatalf("valid external agent %q was not registered after earlier failure: %v", goodName, err)
	}
}

func TestDiscoverAndRegisterNamedAlways_TimesOutStalledInfo(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	setInfoTimeout(t, 200*time.Millisecond)

	name := types.AgentName("disc-named-timeout")
	dir := t.TempDir()
	writeAgentBinary(t, dir, string(name), stalledScript(t))
	t.Setenv("PATH", dir)

	started := time.Now()
	err := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("named discovery took %v, want cancellation near the info budget", elapsed)
	}
	if !errors.Is(err, ErrInfoTimeout) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want ErrInfoTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want context deadline exceeded", err)
	}
	if _, err := agent.Get(name); err == nil {
		t.Fatal("stalled external agent was registered")
	}
}

func TestDiscoverAndRegisterNamedAlways_CanceledContext(t *testing.T) {
	requireSh(t)
	discoveryTest(t)

	name := types.AgentName("disc-named-canceled")
	dir := setupDiscoveryDir(t, string(name), makeInfoJSON(string(name)))
	t.Setenv("PATH", dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := DiscoverAndRegisterNamedAlways(ctx, name)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want context canceled", err)
	}
}

func TestDiscoverAndRegisterNamedAlways_InvalidInfo(t *testing.T) {
	requireSh(t)
	discoveryTest(t)

	name := types.AgentName("disc-named-invalid-info")
	dir := setupDiscoveryDir(t, string(name), "not json")
	t.Setenv("PATH", dir)

	err := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if err == nil {
		t.Fatal("DiscoverAndRegisterNamedAlways() error = nil, want invalid info error")
	}
	if !strings.Contains(err.Error(), string(name)) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %q, want agent name %q", err, name)
	}
	if !strings.Contains(err.Error(), "info: invalid JSON") {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %q, want invalid info context", err)
	}
	// The explicit caller gets the error, and it is also recorded for listing.
	failureFor(t, string(name))
}

func TestDiscoverAndRegisterNamedAlways_MemoizesFailedBinary(t *testing.T) {
	requireSh(t)
	discoveryTest(t)

	name := types.AgentName("disc-named-failure-memo")
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	writeAgentBinary(t, dir, string(name), countOnRunScript(counter, "not json"))
	t.Setenv("PATH", dir)

	firstErr := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if firstErr == nil {
		t.Fatal("first named discovery error = nil, want invalid info error")
	}
	secondErr := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if secondErr == nil {
		t.Fatal("second named discovery error = nil, want memoized invalid info error")
	}
	if got := runCount(t, counter); got != 1 {
		t.Errorf("failed named binary executed %d times, want once per process", got)
	}
}

// TestDiscoverAndRegisterNamedAlways_MissingHelper pins that a missing binary is
// not a broken agent: the nil return means "no such plugin, fall through", which
// the --summarize-provider override depends on.
func TestDiscoverAndRegisterNamedAlways_MissingHelper(t *testing.T) {
	discoveryTest(t)

	name := types.AgentName("disc-named-missing")
	t.Setenv("PATH", t.TempDir())

	if err := DiscoverAndRegisterNamedAlways(context.Background(), name); err != nil {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want nil for missing helper", err)
	}
	if len(agent.ExternalFailures()) != 0 {
		t.Errorf("a missing binary was recorded as broken: %v", agent.ExternalFailures())
	}
}

func TestDiscoverAndRegisterNamedAlways_RejectsPathSeparators(t *testing.T) {
	discoveryTest(t)

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
			t.Errorf("DiscoverAndRegisterNamedAlways(%q) error = %v, want path separator error", name, err)
		}
		if lookedUp {
			t.Errorf("DiscoverAndRegisterNamedAlways(%q) called exec.LookPath for an invalid name", name)
		}
	}
}

func TestIsExternal_WrappedAgent(t *testing.T) {
	requireSh(t)
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-isext"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

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
	// Register a non-external (built-in) agent.
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

// TestDiscoverAndRegister_RegistersBatOnWindows verifies that a .bat agent
// binary is discovered and registered on Windows, with the file extension
// stripped from the agent name. .cmd and .exe follow the same code path.
func TestDiscoverAndRegister_RegistersBatOnWindows(t *testing.T) {
	if runtime.GOOS != osWindows {
		t.Skip("this test only applies on Windows")
	}
	discoveryTest(t)
	enableExternalAgents(t)

	name := "disc-bat"
	infoJSON := `{"protocol_version":1,"name":"` + name + `","type":"` + name + ` Agent","description":"Agent ` + name + `","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{}}`
	script := "@echo off\r\nif not \"%1\"==\"info\" goto :notinfo\r\necho " + infoJSON + "\r\ngoto :eof\r\n:notinfo\r\necho unknown subcommand: %1 1>&2\r\nexit /b 1\r\n"

	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name+".bat")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered after stripping .bat, got error: %v", name, err)
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
	discoveryTest(t)

	name := types.AgentName("disc-named-bat")
	infoJSON := `{"protocol_version":1,"name":"` + string(name) + `","type":"` + string(name) + ` Agent","description":"Named Windows agent","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{}}`
	script := "@echo off\r\nif not \"%1\"==\"info\" goto :notinfo\r\necho " + infoJSON + "\r\ngoto :eof\r\n:notinfo\r\necho unknown subcommand: %1 1>&2\r\nexit /b 1\r\n"

	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+string(name)+".bat")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
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
