package agent

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func TestRegistryOperations(t *testing.T) {
	// Save original registry state and restore after test
	originalRegistry := make(map[types.AgentName]entry)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	// Clear registry for testing
	registry = make(map[types.AgentName]entry)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	t.Run("Register and Get", func(t *testing.T) {
		Register(types.AgentName("test-agent"), func() Agent {
			return &mockAgent{}
		})

		agent, err := Get(types.AgentName("test-agent"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if agent.Name() != mockAgentName {
			t.Errorf("expected Name() %q, got %q", mockAgentName, agent.Name())
		}
	})

	t.Run("Get unknown agent returns error", func(t *testing.T) {
		_, err := Get(types.AgentName("nonexistent-agent"))
		if err == nil {
			t.Error("expected error for unknown agent")
		}
		if !strings.Contains(err.Error(), "unknown agent") {
			t.Errorf("expected 'unknown agent' in error, got: %v", err)
		}
	})

	t.Run("List returns registered agents", func(t *testing.T) {
		// Clear and register fresh
		registryMu.Lock()
		registry = make(map[types.AgentName]entry)
		registryMu.Unlock()

		Register(types.AgentName("agent-b"), func() Agent { return &mockAgent{} })
		Register(types.AgentName("agent-a"), func() Agent { return &mockAgent{} })

		names := List()
		if len(names) != 2 {
			t.Errorf("expected 2 agents, got %d", len(names))
		}
		// List should return sorted
		if names[0] != types.AgentName("agent-a") || names[1] != types.AgentName("agent-b") {
			t.Errorf("expected sorted list [agent-a, agent-b], got %v", names)
		}
	})
}

// sessionDirAgent is a mock with a configurable session dir, for path-prefix tests.
type sessionDirAgent struct {
	mockAgent

	name       types.AgentName
	agentType  types.AgentType
	sessionDir string
}

func (s *sessionDirAgent) Name() types.AgentName                  { return s.name }
func (s *sessionDirAgent) Type() types.AgentType                  { return s.agentType }
func (s *sessionDirAgent) GetSessionDir(_ string) (string, error) { return s.sessionDir, nil }

func TestAgentForTranscriptPath(t *testing.T) {
	originalRegistry := make(map[types.AgentName]entry)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	registry = make(map[types.AgentName]entry)
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	})

	cursor := &sessionDirAgent{
		name:       types.AgentName("cursor"),
		agentType:  types.AgentType("Cursor"),
		sessionDir: "/home/u/.cursor/projects/repo/agent-transcripts",
	}
	claude := &sessionDirAgent{
		name:       types.AgentName("claude-code"),
		agentType:  types.AgentType("Claude Code"),
		sessionDir: "/home/u/.claude/projects/repo",
	}
	Register(cursor.name, func() Agent { return cursor })
	Register(claude.name, func() Agent { return claude })

	cases := []struct {
		name       string
		transcript string
		wantAgent  types.AgentType
		wantOK     bool
	}{
		{
			name:       "cursor IDE nested layout",
			transcript: "/home/u/.cursor/projects/repo/agent-transcripts/abc/abc.jsonl",
			wantAgent:  cursor.Type(),
			wantOK:     true,
		},
		{
			name:       "cursor CLI flat layout",
			transcript: "/home/u/.cursor/projects/repo/agent-transcripts/abc.jsonl",
			wantAgent:  cursor.Type(),
			wantOK:     true,
		},
		{
			name:       "claude code transcript",
			transcript: "/home/u/.claude/projects/repo/abc.jsonl",
			wantAgent:  claude.Type(),
			wantOK:     true,
		},
		{
			name:       "empty transcript path returns false",
			transcript: "",
			wantOK:     false,
		},
		{
			name:       "unrelated path returns false",
			transcript: "/home/u/somewhere/else/transcript.jsonl",
			wantOK:     false,
		},
		{
			name: "directory-prefix collision is rejected",
			// Without a separator-aware prefix check, this would erroneously
			// match an agent rooted at /home/u/.cursor/projects/rep.
			transcript: "/home/u/.cursor/projects/repository/agent-transcripts/x.jsonl",
			wantOK:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag, ok := AgentForTranscriptPath(tc.transcript, "/repo")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if ag.Type() != tc.wantAgent {
				t.Errorf("agent = %q, want %q", ag.Type(), tc.wantAgent)
			}
		})
	}
}

// TestPathHasDirPrefix_CaseSensitivity verifies the platform-dependent
// case-handling of pathHasDirPrefix. On Windows, NTFS/ReFS are case-
// insensitive and filepath.Abs preserves whatever casing the input had, so
// the transcript-path override must match across casing differences. On Unix
// the comparison stays case-sensitive (different cases are different files).
func TestPathHasDirPrefix_CaseSensitivity(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		// Mixed-case paths that refer to the same NTFS location must match.
		if !pathHasDirPrefix(`C:\Users\Bob\.cursor\projects\repo\agent-transcripts\abc.jsonl`,
			`c:\users\bob\.cursor\projects\repo\agent-transcripts`) {
			t.Errorf("expected case-insensitive match on Windows for mixed-case prefix")
		}
		// Equality with different casing should also match.
		if !pathHasDirPrefix(`C:\Users\Bob\.cursor\projects\repo`,
			`c:\users\bob\.cursor\projects\repo`) {
			t.Errorf("expected case-insensitive equality match on Windows")
		}
		return
	}

	// Unix: case-sensitive — different casing means different files.
	if pathHasDirPrefix("/Home/u/.cursor/projects/repo/x.jsonl",
		"/home/u/.cursor/projects/repo") {
		t.Errorf("expected case-sensitive comparison on %s", runtime.GOOS)
	}
}

func TestAgentNameConstants(t *testing.T) {
	if AgentNameClaudeCode != "claude-code" {
		t.Errorf("expected AgentNameClaudeCode %q, got %q", "claude-code", AgentNameClaudeCode)
	}
	if AgentNameGemini != "gemini" {
		t.Errorf("expected AgentNameGemini %q, got %q", "gemini", AgentNameGemini)
	}
}

func TestDefaultAgentName(t *testing.T) {
	// DefaultAgentName is for the `entire enable` setup flow when no agent is
	// detected. It is NOT used for agent attribution fallbacks — those use
	// AgentTypeUnknown ("Unknown") or "Unknown" in the DB.
	if DefaultAgentName != AgentNameClaudeCode {
		t.Errorf("expected DefaultAgentName %q, got %q", AgentNameClaudeCode, DefaultAgentName)
	}
}

func TestDefault(t *testing.T) {
	// Default() returns nil if default agent is not registered
	// This test verifies the function doesn't panic
	originalRegistry := make(map[types.AgentName]entry)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	registry = make(map[types.AgentName]entry)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	agent := Default()
	if agent != nil {
		t.Error("expected nil when default agent not registered")
	}

	// Register the default agent
	Register(DefaultAgentName, func() Agent {
		return &mockAgent{}
	})

	agent = Default()
	if agent == nil {
		t.Error("expected non-nil agent after registering default")
	}
}

func TestAllProtectedDirs(t *testing.T) {
	// Save original registry state
	originalRegistry := make(map[types.AgentName]entry)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	registry = make(map[types.AgentName]entry)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	t.Run("empty registry returns empty", func(t *testing.T) {
		dirs := AllProtectedDirs()
		if len(dirs) != 0 {
			t.Errorf("expected empty dirs, got %v", dirs)
		}
	})

	t.Run("collects dirs from registered agents", func(t *testing.T) {
		registryMu.Lock()
		registry = make(map[types.AgentName]entry)
		registryMu.Unlock()

		Register(types.AgentName("agent-a"), func() Agent {
			return &protectedDirAgent{dirs: []string{".agent-a"}}
		})
		Register(types.AgentName("agent-b"), func() Agent {
			return &protectedDirAgent{dirs: []string{".agent-b", ".shared"}}
		})

		dirs := AllProtectedDirs()
		if len(dirs) != 3 {
			t.Fatalf("expected 3 dirs, got %d: %v", len(dirs), dirs)
		}
		// AllProtectedDirs returns sorted
		expected := []string{".agent-a", ".agent-b", ".shared"}
		for i, d := range dirs {
			if d != expected[i] {
				t.Errorf("dirs[%d] = %q, want %q", i, d, expected[i])
			}
		}
	})

	t.Run("deduplicates across agents", func(t *testing.T) {
		registryMu.Lock()
		registry = make(map[types.AgentName]entry)
		registryMu.Unlock()

		Register(types.AgentName("agent-x"), func() Agent {
			return &protectedDirAgent{dirs: []string{".shared"}}
		})
		Register(types.AgentName("agent-y"), func() Agent {
			return &protectedDirAgent{dirs: []string{".shared"}}
		})

		dirs := AllProtectedDirs()
		if len(dirs) != 1 {
			t.Errorf("expected 1 dir (deduplicated), got %d: %v", len(dirs), dirs)
		}
	})
}

func TestAllProtectedFiles(t *testing.T) {
	// Save original registry state
	originalRegistry := make(map[types.AgentName]entry)
	registryMu.Lock()
	for k, v := range registry {
		originalRegistry[k] = v
	}
	registry = make(map[types.AgentName]entry)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		registry = originalRegistry
		registryMu.Unlock()
	}()

	t.Run("empty registry returns empty", func(t *testing.T) {
		files := AllProtectedFiles()
		if len(files) != 0 {
			t.Errorf("expected empty files, got %v", files)
		}
	})

	t.Run("collects files from registered agents", func(t *testing.T) {
		registryMu.Lock()
		registry = make(map[types.AgentName]entry)
		registryMu.Unlock()

		Register(types.AgentName("agent-no-files"), func() Agent {
			return &mockAgent{}
		})
		Register(types.AgentName("agent-a"), func() Agent {
			return &protectedDirAgent{files: []string{"a.json"}}
		})
		Register(types.AgentName("agent-b"), func() Agent {
			return &protectedDirAgent{files: []string{"b.json", "shared.json"}}
		})

		files := AllProtectedFiles()
		expected := []string{"a.json", "b.json", "shared.json"}
		if len(files) != len(expected) {
			t.Fatalf("expected %d files, got %d: %v", len(expected), len(files), files)
		}
		for i, file := range files {
			if file != expected[i] {
				t.Errorf("files[%d] = %q, want %q", i, file, expected[i])
			}
		}
	})

	t.Run("deduplicates across agents", func(t *testing.T) {
		registryMu.Lock()
		registry = make(map[types.AgentName]entry)
		registryMu.Unlock()

		Register(types.AgentName("agent-x"), func() Agent {
			return &protectedDirAgent{files: []string{"shared.json"}}
		})
		Register(types.AgentName("agent-y"), func() Agent {
			return &protectedDirAgent{files: []string{"shared.json"}}
		})

		files := AllProtectedFiles()
		if len(files) != 1 {
			t.Errorf("expected 1 file (deduplicated), got %d: %v", len(files), files)
		}
	})
}

// protectedDirAgent is a mock that returns configurable protected dirs.
type protectedDirAgent struct {
	mockAgent

	dirs  []string
	files []string
}

func (p *protectedDirAgent) ProtectedDirs() []string  { return p.dirs }
func (p *protectedDirAgent) ProtectedFiles() []string { return p.files }

func TestLauncherFor(t *testing.T) {
	t.Parallel()
	// Claude Code should be found. (claudecode init() registers it via the blank
	// import in generate_external_test.go — but registry_test.go is package agent,
	// so we register a launcher directly here.)
	Register(types.AgentName("launcher-test-agent"), func() Agent {
		return &mockLauncherAgent{}
	})
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, types.AgentName("launcher-test-agent"))
		registryMu.Unlock()
	})

	l, ok := LauncherFor(types.AgentName("launcher-test-agent"))
	if !ok {
		t.Fatal("expected launcher-test-agent to implement Launcher")
	}
	if l == nil {
		t.Fatal("expected non-nil Launcher")
	}
	// A non-existent agent should return false.
	l2, ok2 := LauncherFor(types.AgentName("does-not-exist"))
	if ok2 {
		t.Error("expected ok=false for unknown agent")
	}
	if l2 != nil {
		t.Error("expected nil Launcher for unknown agent")
	}
}

// mockLauncherAgent implements Agent and Launcher for testing.
type mockLauncherAgent struct {
	mockAgent
}

//nolint:unparam // error is always nil in this mock; satisfies the Launcher interface.
func (m *mockLauncherAgent) LaunchCmd(ctx context.Context, _ string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "true"), nil
}

// snapshotRegistry replaces the registry with an empty one for the duration of
// the test and restores it afterwards.
func snapshotRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	original := make(map[types.AgentName]entry, len(registry))
	for k, v := range registry {
		original[k] = v
	}
	registry = make(map[types.AgentName]entry)
	registryMu.Unlock()

	t.Cleanup(func() {
		registryMu.Lock()
		registry = original
		registryMu.Unlock()
	})
}

func TestGet_FailedExternalReturnsItsLoadError(t *testing.T) {
	snapshotRegistry(t)

	loadErr := errors.New("info: invalid JSON: unexpected end of input")
	RegisterExternalFailure("broken-ext", "/usr/local/bin/entire-agent-broken-ext", loadErr)

	_, err := Get("broken-ext")
	if !errors.Is(err, loadErr) {
		t.Fatalf("Get() error = %v, want the recorded load error", err)
	}
	if !strings.Contains(err.Error(), "/usr/local/bin/entire-agent-broken-ext") {
		t.Errorf("Get() error = %q, want the binary path so the user can find it", err)
	}
}

func TestListAndStringList_ExcludeFailedExternals(t *testing.T) {
	snapshotRegistry(t)

	Register("built-in", func() Agent { return &protectedDirAgent{} })
	RegisterExternal("ready-ext", "/bin/entire-agent-ready-ext", func() Agent { return &protectedDirAgent{} })
	RegisterExternalFailure("broken-ext", "/bin/entire-agent-broken-ext", errors.New("boom"))

	want := []types.AgentName{"built-in", "ready-ext"}
	if got := List(); !slices.Equal(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}
	if got := StringList(); !slices.Equal(got, []string{"built-in", "ready-ext"}) {
		t.Errorf("StringList() = %v, want built-in and ready-ext", got)
	}
}

func TestExternalFailures_SortedSnapshot(t *testing.T) {
	snapshotRegistry(t)

	RegisterExternalFailure("zed", "/bin/entire-agent-zed", errors.New("zed failed"))
	RegisterExternalFailure("abe", "/bin/entire-agent-abe", errors.New("abe failed"))
	RegisterExternal("ready", "/bin/entire-agent-ready", func() Agent { return &protectedDirAgent{} })

	failures := ExternalFailures()
	if len(failures) != 2 {
		t.Fatalf("ExternalFailures() returned %d entries, want 2", len(failures))
	}
	if failures[0].Name != "abe" || failures[1].Name != "zed" {
		t.Errorf("ExternalFailures() = %v, want sorted by name", failures)
	}
	if failures[0].Binary != "/bin/entire-agent-abe" {
		t.Errorf("ExternalFailures()[0].Binary = %q, want the discovered path", failures[0].Binary)
	}
}

func TestProbedExternalBinaries_CoversReadyAndFailed(t *testing.T) {
	snapshotRegistry(t)

	Register("built-in", func() Agent { return &protectedDirAgent{} })
	RegisterExternal("ready", "/bin/entire-agent-ready", func() Agent { return &protectedDirAgent{} })
	RegisterExternalFailure("broken", "/bin/entire-agent-broken", errors.New("boom"))

	probed := ProbedExternalBinaries()
	for _, want := range []string{"/bin/entire-agent-ready", "/bin/entire-agent-broken"} {
		if _, ok := probed[want]; !ok {
			t.Errorf("ProbedExternalBinaries() missing %q", want)
		}
	}
	if len(probed) != 2 {
		t.Errorf("ProbedExternalBinaries() = %v, want only external binaries", probed)
	}
}

func TestResetExternalsForTesting_KeepsBuiltIns(t *testing.T) {
	snapshotRegistry(t)

	Register("built-in", func() Agent { return &protectedDirAgent{} })
	RegisterExternal("ready", "/bin/entire-agent-ready", func() Agent { return &protectedDirAgent{} })
	RegisterExternalFailure("broken", "/bin/entire-agent-broken", errors.New("boom"))

	ResetExternalsForTesting()

	if got := List(); !slices.Equal(got, []types.AgentName{"built-in"}) {
		t.Errorf("List() after reset = %v, want only the built-in", got)
	}
	if got := ExternalFailures(); len(got) != 0 {
		t.Errorf("ExternalFailures() after reset = %v, want none", got)
	}
	if got := ProbedExternalBinaries(); len(got) != 0 {
		t.Errorf("ProbedExternalBinaries() after reset = %v, want none", got)
	}
}

func TestFailedExternalDoesNotBreakRegistryWalks(t *testing.T) {
	snapshotRegistry(t)

	RegisterExternalFailure("broken", "/bin/entire-agent-broken", errors.New("boom"))
	Register("built-in", func() Agent { return &protectedDirAgent{dirs: []string{".built-in"}} })

	// A failed entry has no factory. Every registry walk must skip it rather
	// than call through a nil factory.
	if _, err := GetByAgentType("nope"); err == nil {
		t.Error("GetByAgentType() error = nil, want unknown agent type")
	}
	if got := DetectAll(context.Background()); len(got) != 0 {
		t.Errorf("DetectAll() = %v, want none detected", got)
	}
	if got := AllProtectedDirs(); !slices.Equal(got, []string{".built-in"}) {
		t.Errorf("AllProtectedDirs() = %v, want the built-in's dirs", got)
	}
	if got := AllProtectedFiles(); len(got) != 0 {
		t.Errorf("AllProtectedFiles() = %v, want none", got)
	}
}
