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
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// setupDiscoveryDir creates a temp directory containing a mock entire-agent-<name> binary.
// Returns the directory path.
func setupDiscoveryDir(t *testing.T, agentName, infoJSON string) string {
	t.Helper()

	dir := t.TempDir()
	binName := binaryPrefix + agentName
	binPath := filepath.Join(dir, binName)

	script := mockInfoScript(infoJSON)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	return dir
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

// NOTE: Tests in this file modify process-global state (os.Setenv, os.Chdir, agent registry)
// and therefore cannot use t.Parallel().

// externalAgentsRepo writes .entire/settings.json with the given external_agents
// value and returns a context that names the directory as the worktree root.
//
// The context is what makes this deterministic. settings.Load short-circuits on
// a worktree root carried in the context and never consults entiredir's anchor,
// which resolves through `git rev-parse` — and every test here sets PATH to just
// its own mock-binary directory, so git is not on it. The previous version
// mkdir'd an empty .git and relied on Load falling back to a cwd-relative
// ".entire/settings.json" when the anchor failed. That fallback is gone
// (resolving a repository is fail-closed now), and it was papering over a repo
// the test never actually created.
//
// Production is unaffected by the difference: every DiscoverAndRegister call
// site sits inside a command's RunE, downstream of the root pre-run, which
// already refuses to run when the worktree root will not resolve.
func externalAgentsRepo(t *testing.T, enabled bool) context.Context {
	t.Helper()
	tmpDir := t.TempDir()
	// Settings/discovery anchor process-wide os.Roots on this directory
	// (osroot.Shared), which are never closed. Windows cannot remove a
	// directory while a handle to it is open, so close the registry before
	// t.TempDir's RemoveAll — t.Cleanup is LIFO and TempDir registered first.
	t.Cleanup(osroot.ResetShared)
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	body := fmt.Sprintf(`{"enabled":true,"external_agents":%t}`, enabled)
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Chdir(tmpDir)
	return settings.WithWorktreeRoot(context.Background(), tmpDir)
}

// enableExternalAgents is externalAgentsRepo with the setting on.
func enableExternalAgents(t *testing.T) context.Context {
	t.Helper()
	return externalAgentsRepo(t, true)
}

func TestDiscoverAndRegister_FindsAgent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := enableExternalAgents(t)

	name := "disc-find"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered, got error: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

func TestDiscoverAndRegister_Deduplication(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := enableExternalAgents(t)

	name := "disc-dedup"
	dir1 := setupDiscoveryDir(t, name, makeInfoJSON(name))
	dir2 := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir1+string(os.PathListSeparator)+dir2)

	DiscoverAndRegister(ctx)

	// Verify the agent is registered (just once — no panic or error from double registration).
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

func TestDiscoverAndRegister_SkipsNameConflict(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := enableExternalAgents(t)

	name := "disc-conflict"
	// Pre-register an agent with the same name.
	agent.Register(types.AgentName(name), func() agent.Agent {
		return nil // placeholder
	})

	// Create an external binary with the conflicting name, using a different type.
	conflictJSON := `{
  "protocol_version": 1,
  "name": "` + name + `",
  "type": "Different Type",
  "description": "Should be skipped",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {}
}`
	dir := setupDiscoveryDir(t, name, conflictJSON)
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

	// The pre-registered placeholder should still be there (returns nil).
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("agent should still be registered: %v", err)
	}
	// The placeholder factory returns nil, so it wasn't replaced by the external one.
	if ag != nil {
		t.Errorf("expected placeholder (nil) agent, got %v", ag)
	}
}

func TestDiscoverAndRegister_SkipsNonExecutable(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := enableExternalAgents(t)

	name := "disc-noexec"
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name)

	// Write a valid script but WITHOUT executable permission.
	script := mockInfoScript(makeInfoJSON(name))
	if err := os.WriteFile(binPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

	_, err := agent.Get(types.AgentName(name))
	if err == nil {
		t.Error("expected non-executable file to be skipped, but agent was registered")
	}
}

func TestDiscoverAndRegister_SkipsDirectory(t *testing.T) {
	ctx := enableExternalAgents(t)

	name := "disc-dir"
	dir := t.TempDir()

	// Create a directory (not a file) matching the prefix.
	if err := os.Mkdir(filepath.Join(dir, binaryPrefix+name), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

	_, err := agent.Get(types.AgentName(name))
	if err == nil {
		t.Error("expected directory to be skipped, but agent was registered")
	}
}

func TestDiscoverAndRegister_SkipsWhenDisabled(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// A repo whose settings are readable and say external_agents is off. The
	// distinction matters: when settings could not be loaded at all this test
	// also passed, because the disabled path and the unreadable path both end
	// at IsExternalAgentsEnabled returning false.
	ctx := externalAgentsRepo(t, false)

	name := "disc-disabled"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

	// Agent should NOT be registered because external_agents is false
	_, err := agent.Get(types.AgentName(name))
	if err == nil {
		t.Error("expected agent to NOT be registered when external_agents is disabled")
	}
}

func TestDiscoverAndRegister_EmptyPATH(t *testing.T) {
	ctx := enableExternalAgents(t)
	t.Setenv("PATH", "")

	// Should return without error or panic.
	DiscoverAndRegister(ctx)
}

func TestDiscoverAndRegister_UnreadableDir(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := enableExternalAgents(t)

	name := "disc-unread"
	goodDir := setupDiscoveryDir(t, name, makeInfoJSON(name))

	// Include a non-existent directory in PATH — it should be silently skipped.
	t.Setenv("PATH", "/nonexistent/path"+string(os.PathListSeparator)+goodDir)

	DiscoverAndRegister(ctx)

	_, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered despite unreadable dir in PATH: %v", name, err)
	}
}

func TestDiscoverAndRegisterAlways_FindsAgentWithoutSettings(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// Set up a repo WITHOUT external_agents enabled (default false).
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Chdir(tmpDir)

	name := "disc-always"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	// DiscoverAndRegisterAlways should find the agent even without external_agents enabled.
	DiscoverAndRegisterAlways(context.Background())

	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatalf("expected agent %q to be registered by DiscoverAndRegisterAlways, got error: %v", name, err)
	}
	if string(ag.Name()) != name {
		t.Errorf("agent Name() = %q, want %q", ag.Name(), name)
	}
}

func TestDiscoverAndRegisterNamedAlways_TimesOutStalledInfo(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}

	name := types.AgentName("disc-named-timeout")
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+string(name))
	script := fmt.Sprintf("#!/bin/sh\nexec %q 60\n", sleepPath)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stalled mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	err = DiscoverAndRegisterNamedAlways(ctx, name)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("named discovery took %v, want cancellation near context deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want context deadline exceeded", err)
	}
	if _, err := agent.Get(name); err == nil {
		t.Fatal("stalled external agent was registered")
	}
}

func TestDiscoverAndRegisterNamedAlways_CanceledContext(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

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
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

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
}

func TestDiscoverAndRegisterNamedAlways_MissingHelper(t *testing.T) {
	name := types.AgentName("disc-named-missing")
	t.Setenv("PATH", t.TempDir())

	if err := DiscoverAndRegisterNamedAlways(context.Background(), name); err != nil {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want nil for missing helper", err)
	}
}

func TestDiscoverAndRegisterNamedAlways_RejectsPathSeparators(t *testing.T) {
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

func TestDiscoverAndRegisterNamedAlways_DeadlineWhileLookingUpMissingHelper(t *testing.T) {
	name := types.AgentName("disc-named-lookup-deadline")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	originalLookPath := lookPathExternalAgent
	t.Cleanup(func() { lookPathExternalAgent = originalLookPath })
	lookPathExternalAgent = func(string) (string, error) {
		<-ctx.Done()
		return "", exec.ErrNotFound
	}

	err := DiscoverAndRegisterNamedAlways(ctx, name)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want context deadline exceeded", err)
	}
}

func TestDiscoverAndRegisterNamedAlways_HelperDisappearsAfterLookup(t *testing.T) {
	name := types.AgentName("disc-named-helper-disappeared")
	binPath := filepath.Join(t.TempDir(), binaryPrefix+string(name))

	originalLookPath := lookPathExternalAgent
	originalStat := statExternalAgent
	t.Cleanup(func() {
		lookPathExternalAgent = originalLookPath
		statExternalAgent = originalStat
	})
	lookPathExternalAgent = func(string) (string, error) { return binPath, nil }
	statExternalAgent = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	err := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if err == nil {
		t.Fatal("DiscoverAndRegisterNamedAlways() error = nil, want helper-disappeared error")
	}
	if !strings.Contains(err.Error(), string(name)) || !strings.Contains(err.Error(), binPath) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %q, want agent and binary context", err)
	}
	if !strings.Contains(err.Error(), "was found but could not be registered") {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %q, want actionable registration context", err)
	}
}

func TestDiscoverAndRegisterNamedAlways_StatError(t *testing.T) {
	name := types.AgentName("disc-named-stat-error")
	binPath := filepath.Join(t.TempDir(), binaryPrefix+string(name))
	wantErr := errors.New("stat failed")

	originalLookPath := lookPathExternalAgent
	originalStat := statExternalAgent
	t.Cleanup(func() {
		lookPathExternalAgent = originalLookPath
		statExternalAgent = originalStat
	})
	lookPathExternalAgent = func(string) (string, error) { return binPath, nil }
	statExternalAgent = func(string) (os.FileInfo, error) { return nil, wantErr }

	err := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if !errors.Is(err, wantErr) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want stat error", err)
	}
	if !strings.Contains(err.Error(), string(name)) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %q, want agent name %q", err, name)
	}
}

func TestDiscoverAndRegisterNamedAlways_LookPathError(t *testing.T) {
	name := types.AgentName("disc-named-lookpath-error")
	wantErr := errors.New("lookup failed")

	originalLookPath := lookPathExternalAgent
	t.Cleanup(func() { lookPathExternalAgent = originalLookPath })
	lookPathExternalAgent = func(string) (string, error) { return "", wantErr }

	err := DiscoverAndRegisterNamedAlways(context.Background(), name)
	if !errors.Is(err, wantErr) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want lookup error", err)
	}
	if !strings.Contains(err.Error(), string(name)) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %q, want agent name %q", err, name)
	}
}

func TestDiscoverAndRegister_ContinuesAfterRegistrationError(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

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

func TestIsExternal_WrappedAgent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := enableExternalAgents(t)

	name := "disc-isext"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

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

func TestDiscoverAndRegister_SkipsInfoFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx := enableExternalAgents(t)

	name := "disc-badjson"
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name)

	// Binary that outputs invalid JSON for "info".
	script := "#!/bin/sh\necho 'not json'\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

	_, err := agent.Get(types.AgentName(name))
	if err == nil {
		t.Error("expected agent with bad info to be skipped, but it was registered")
	}
}

// TestDiscoverAndRegister_RegistersBatOnWindows verifies that a .bat agent
// binary is discovered and registered on Windows, with the file extension
// stripped from the agent name. .cmd and .exe follow the same code path.
func TestDiscoverAndRegister_RegistersBatOnWindows(t *testing.T) {
	if runtime.GOOS != osWindows {
		t.Skip("this test only applies on Windows")
	}

	ctx := enableExternalAgents(t)

	name := "disc-bat"
	infoJSON := `{"protocol_version":1,"name":"` + name + `","type":"` + name + ` Agent","description":"Agent ` + name + `","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{}}`
	script := "@echo off\r\nif not \"%1\"==\"info\" goto :notinfo\r\necho " + infoJSON + "\r\ngoto :eof\r\n:notinfo\r\necho unknown subcommand: %1 1>&2\r\nexit /b 1\r\n"

	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name+".bat")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(ctx)

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

// stalledPropagationCtx is a context whose cancellation is immediately visible via
// Done and Err, but whose propagation to derived contexts is not awaited. Because it
// is not a standard context, context.WithTimeout watches it from a goroutine, so a
// derived context still reports no error for a moment after this one is cancelled.
//
// That window is real, not contrived: even for standard contexts, cancel() closes its
// own Done channel before cancelling children, so a goroutine woken by the parent can
// run before the child observes anything. This type just makes the window wide enough
// to assert on deterministically instead of relying on scheduler luck.
type stalledPropagationCtx struct {
	done chan struct{}
}

func (*stalledPropagationCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *stalledPropagationCtx) Done() <-chan struct{}     { return c.done }
func (*stalledPropagationCtx) Value(any) any               { return nil }
func (c *stalledPropagationCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// TestDiscoverAndRegisterNamedAlways_ReportsCallerDeadlineExpiredDuringLookup pins the
// contract that a caller whose context expires while the helper binary is being looked
// up gets its context error back — not a nil "no such agent" result.
//
// Before the fix this returned nil: the caller's context was shadowed by the derived
// timeout context, so the post-lookup check consulted only the derived one, which had
// not yet observed the caller's cancellation. That is the same defect that made
// TestDiscoverAndRegisterNamedAlways_DeadlineWhileLookingUpMissingHelper flaky under
// load, where the window had to be hit by chance.
func TestDiscoverAndRegisterNamedAlways_ReportsCallerDeadlineExpiredDuringLookup(t *testing.T) {
	name := types.AgentName("disc-named-caller-deadline")
	caller := &stalledPropagationCtx{done: make(chan struct{})}

	originalLookPath := lookPathExternalAgent
	t.Cleanup(func() { lookPathExternalAgent = originalLookPath })
	lookPathExternalAgent = func(string) (string, error) {
		// Expire the caller mid-lookup and return straight away, before the derived
		// context's watcher goroutine can observe it. ErrNotFound is the benign
		// "no such helper" result the deadline must take precedence over.
		close(caller.done)
		return "", exec.ErrNotFound
	}

	err := DiscoverAndRegisterNamedAlways(caller, name)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DiscoverAndRegisterNamedAlways() error = %v, want context deadline exceeded", err)
	}
}
