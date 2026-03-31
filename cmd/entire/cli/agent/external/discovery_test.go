package external

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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

// enableExternalAgents creates a temp repo with external_agents enabled in settings
// and chdir's into it so that settings.Load can find the config.
func enableExternalAgents(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{"enabled":true,"external_agents":true}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Chdir(tmpDir)
}

func TestDiscoverAndRegister_FindsAgent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

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
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	enableExternalAgents(t)

	name := "disc-dedup"
	dir1 := setupDiscoveryDir(t, name, makeInfoJSON(name))
	dir2 := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir1+string(os.PathListSeparator)+dir2)

	DiscoverAndRegister(context.Background())

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

	enableExternalAgents(t)

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

	DiscoverAndRegister(context.Background())

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

	enableExternalAgents(t)

	name := "disc-noexec"
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name)

	// Write a valid script but WITHOUT executable permission.
	script := mockInfoScript(makeInfoJSON(name))
	if err := os.WriteFile(binPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	_, err := agent.Get(types.AgentName(name))
	if err == nil {
		t.Error("expected non-executable file to be skipped, but agent was registered")
	}
}

func TestDiscoverAndRegister_SkipsDirectory(t *testing.T) {
	enableExternalAgents(t)

	name := "disc-dir"
	dir := t.TempDir()

	// Create a directory (not a file) matching the prefix.
	if err := os.Mkdir(filepath.Join(dir, binaryPrefix+name), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	_, err := agent.Get(types.AgentName(name))
	if err == nil {
		t.Error("expected directory to be skipped, but agent was registered")
	}
}

func TestDiscoverAndRegister_SkipsWhenDisabled(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// Set up a repo WITHOUT external_agents enabled (default false)
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

	name := "disc-disabled"
	dir := setupDiscoveryDir(t, name, makeInfoJSON(name))
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

	// Agent should NOT be registered because external_agents is false
	_, err := agent.Get(types.AgentName(name))
	if err == nil {
		t.Error("expected agent to NOT be registered when external_agents is disabled")
	}
}

func TestDiscoverAndRegister_EmptyPATH(t *testing.T) {
	enableExternalAgents(t)
	t.Setenv("PATH", "")

	// Should return without error or panic.
	DiscoverAndRegister(context.Background())
}

func TestDiscoverAndRegister_UnreadableDir(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	enableExternalAgents(t)

	name := "disc-unread"
	goodDir := setupDiscoveryDir(t, name, makeInfoJSON(name))

	// Include a non-existent directory in PATH — it should be silently skipped.
	t.Setenv("PATH", "/nonexistent/path"+string(os.PathListSeparator)+goodDir)

	DiscoverAndRegister(context.Background())

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

func TestIsExternal_WrappedAgent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

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

func TestDiscoverAndRegister_SkipsInfoFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	enableExternalAgents(t)

	name := "disc-badjson"
	dir := t.TempDir()
	binPath := filepath.Join(dir, binaryPrefix+name)

	// Binary that outputs invalid JSON for "info".
	script := "#!/bin/sh\necho 'not json'\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}
	t.Setenv("PATH", dir)

	DiscoverAndRegister(context.Background())

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
