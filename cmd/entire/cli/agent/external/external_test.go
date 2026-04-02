package external

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// testBinaryDir creates a temp directory with a mock entire-agent-test binary.
// The binary is a shell script implementing the protocol.
func testBinaryDir(t *testing.T, script string) string {
	t.Helper()

	dir := t.TempDir()

	name := "entire-agent-test"
	if runtime.GOOS == osWindows {
		name += ".bat"
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock binary: %v", err)
	}

	return path
}

// newExternalAgent creates an ExternalAgent with retry to handle ETXTBSY.
// On heavily loaded CI machines, the kernel may briefly report "text file busy"
// when executing a just-written shell script.
func newExternalAgent(t *testing.T, binPath string) *Agent {
	t.Helper()
	var ea *Agent
	var err error
	for range 3 {
		ea, err = New(context.Background(), binPath)
		if err == nil {
			return ea
		}
		if !errors.Is(err, syscall.ETXTBSY) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("New: %v", err)
	return nil
}

// mockInfoScript returns a shell script that responds to "info" with the given JSON.
func mockInfoScript(infoJSON string) string {
	return `#!/bin/sh
case "$1" in
  info)
    echo '` + infoJSON + `'
    ;;
  detect)
    echo '{"present": true}'
    ;;
  get-session-dir)
    echo '{"session_dir": "/tmp/sessions"}'
    ;;
  resolve-session-file)
    echo '{"session_file": "/tmp/sessions/test.jsonl"}'
    ;;
  get-session-id)
    echo '{"session_id": "test-session-123"}'
    ;;
  read-session)
    echo '{"session_id":"s1","agent_name":"test","repo_path":"/repo","session_ref":"ref"}'
    ;;
  write-session)
    exit 0
    ;;
  format-resume-command)
    echo '{"command": "test-agent resume '$3'"}'
    ;;
  read-transcript)
    echo 'raw transcript data'
    ;;
  chunk-transcript)
    echo '{"chunks": ["Y2h1bms="]}'
    ;;
  reassemble-transcript)
    cat
    ;;
  parse-hook)
    echo 'null'
    ;;
  install-hooks)
    echo '{"hooks_installed": 2}'
    ;;
  uninstall-hooks)
    exit 0
    ;;
  are-hooks-installed)
    echo '{"installed": true}'
    ;;
  get-transcript-position)
    echo '{"position": 42}'
    ;;
  extract-modified-files)
    echo '{"files": ["a.go", "b.go"], "current_position": 10}'
    ;;
  extract-prompts)
    echo '{"prompts": ["hello", "world"]}'
    ;;
  extract-summary)
    echo '{"summary": "test summary", "has_summary": true}'
    ;;
  *)
    echo "unknown subcommand: $1" >&2
    exit 1
    ;;
esac
`
}

const validInfoJSON = `{
  "protocol_version": 1,
  "name": "test",
  "type": "Test Agent",
  "description": "A test agent",
  "is_preview": true,
  "protected_dirs": [".test"],
  "hook_names": ["session-start", "stop"],
  "capabilities": {
    "hooks": true,
    "transcript_analyzer": true,
    "transcript_preparer": false,
    "token_calculator": false,
    "text_generator": false,
    "hook_response_writer": false,
    "subagent_aware_extractor": false
  }
}`

func TestRun_AppliesTimeoutWhenNoDeadline(t *testing.T) {
	// Not parallel: mutates package-level defaultRunTimeout.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	script := `#!/bin/sh
case "$1" in
  info)
    echo '` + validInfoJSON + `'
    ;;
  slow)
    exec sleep 60
    ;;
esac
`
	binPath := testBinaryDir(t, script)
	ea := newExternalAgent(t, binPath)

	// Temporarily override the default timeout to keep the test fast.
	orig := defaultRunTimeout
	defaultRunTimeout = 200 * time.Millisecond
	t.Cleanup(func() { defaultRunTimeout = orig })

	start := time.Now()
	_, err := ea.run(context.Background(), nil, "slow")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Should be killed around 200ms, not 60s.
	if elapsed >= 4*time.Second {
		t.Errorf("run() took %v; default timeout was not applied", elapsed)
	}
}

func TestRun_RespectsExistingDeadline(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	script := `#!/bin/sh
case "$1" in
  info)
    echo '` + validInfoJSON + `'
    ;;
  slow)
    exec sleep 60
    ;;
esac
`
	binPath := testBinaryDir(t, script)
	ea := newExternalAgent(t, binPath)

	// Provide a context with a short deadline. run() should respect it
	// and NOT override with its own (longer) timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ea.run(ctx, nil, "slow")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed >= 4*time.Second {
		t.Errorf("run() took %v; caller's deadline was not respected", elapsed)
	}
}

func TestNew_Valid(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	binPath := testBinaryDir(t, mockInfoScript(validInfoJSON))
	ea := newExternalAgent(t, binPath)
	if ea.info.Name != "test" {
		t.Errorf("Name = %q, want %q", ea.info.Name, "test")
	}
	if ea.info.ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion = %d, want 1", ea.info.ProtocolVersion)
	}
}

func TestNew_WrongProtocolVersion(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	script := `#!/bin/sh
echo '{"protocol_version": 99, "name": "bad"}'
`
	binPath := testBinaryDir(t, script)
	_, err := New(context.Background(), binPath)
	if err == nil {
		t.Fatal("expected error for wrong protocol version")
	}
}

func TestNew_BinaryNotFound(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), "/nonexistent/entire-agent-nope")
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestNew_InvalidJSON(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	script := `#!/bin/sh
echo 'not json'
`
	binPath := testBinaryDir(t, script)
	_, err := New(context.Background(), binPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestExternalAgent_Identity(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	binPath := testBinaryDir(t, mockInfoScript(validInfoJSON))
	ea := newExternalAgent(t, binPath)

	if string(ea.Name()) != "test" {
		t.Errorf("Name() = %q, want %q", ea.Name(), "test")
	}
	if string(ea.Type()) != "Test Agent" {
		t.Errorf("Type() = %q, want %q", ea.Type(), "Test Agent")
	}
	if ea.Description() != "A test agent" {
		t.Errorf("Description() = %q, want %q", ea.Description(), "A test agent")
	}
	if !ea.IsPreview() {
		t.Error("IsPreview() = false, want true")
	}
	dirs := ea.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".test" {
		t.Errorf("ProtectedDirs() = %v, want [.test]", dirs)
	}
}

func TestExternalAgent_DetectPresence(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	binPath := testBinaryDir(t, mockInfoScript(validInfoJSON))
	ea := newExternalAgent(t, binPath)

	present, err := ea.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence: %v", err)
	}
	if !present {
		t.Error("DetectPresence() = false, want true")
	}
}

func TestExternalAgent_GetSessionDir(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	binPath := testBinaryDir(t, mockInfoScript(validInfoJSON))
	ea := newExternalAgent(t, binPath)

	dir, err := ea.GetSessionDir("/repo")
	if err != nil {
		t.Fatalf("GetSessionDir: %v", err)
	}
	if dir != "/tmp/sessions" {
		t.Errorf("GetSessionDir() = %q, want /tmp/sessions", dir)
	}
}

func TestExternalAgent_TranscriptAnalyzer(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	binPath := testBinaryDir(t, mockInfoScript(validInfoJSON))
	ea := newExternalAgent(t, binPath)

	pos, err := ea.GetTranscriptPosition("/some/path")
	if err != nil {
		t.Fatalf("GetTranscriptPosition: %v", err)
	}
	if pos != 42 {
		t.Errorf("GetTranscriptPosition() = %d, want 42", pos)
	}

	files, curPos, err := ea.ExtractModifiedFilesFromOffset("/path", 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset: %v", err)
	}
	if len(files) != 2 || files[0] != "a.go" {
		t.Errorf("ExtractModifiedFilesFromOffset files = %v, want [a.go b.go]", files)
	}
	if curPos != 10 {
		t.Errorf("ExtractModifiedFilesFromOffset pos = %d, want 10", curPos)
	}

	prompts, err := ea.ExtractPrompts("/path", 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 2 || prompts[0] != "hello" {
		t.Errorf("ExtractPrompts() = %v, want [hello world]", prompts)
	}

	summary, err := ea.ExtractSummary("/path")
	if err != nil {
		t.Fatalf("ExtractSummary: %v", err)
	}
	if summary != "test summary" {
		t.Errorf("ExtractSummary() = %q, want 'test summary'", summary)
	}
}

func TestExternalAgent_HookSupport(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	binPath := testBinaryDir(t, mockInfoScript(validInfoJSON))
	ea := newExternalAgent(t, binPath)

	names := ea.HookNames()
	if len(names) != 2 {
		t.Errorf("HookNames() = %v, want 2 names", names)
	}

	installed, err := ea.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if installed != 2 {
		t.Errorf("InstallHooks() = %d, want 2", installed)
	}

	if !ea.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled() = false, want true")
	}
}

func TestExternalAgent_ErrorOnStderr(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	script := `#!/bin/sh
case "$1" in
  info)
    echo '` + validInfoJSON + `'
    ;;
  detect)
    echo "agent not available" >&2
    exit 1
    ;;
esac
`
	binPath := testBinaryDir(t, script)
	ea := newExternalAgent(t, binPath)

	_, err := ea.DetectPresence(context.Background())
	if err == nil {
		t.Fatal("expected error from stderr")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestWrap_HooksAndAnalyzer(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	binPath := testBinaryDir(t, mockInfoScript(validInfoJSON))
	ea := newExternalAgent(t, binPath)

	wrapped, err := Wrap(ea)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	// Should satisfy HookSupport
	if _, ok := agent.AsHookSupport(wrapped); !ok {
		t.Error("Wrap() should return HookSupport when hooks=true")
	}

	// Should satisfy TranscriptAnalyzer
	if _, ok := agent.AsTranscriptAnalyzer(wrapped); !ok {
		t.Error("Wrap() should return TranscriptAnalyzer when transcript_analyzer=true")
	}

	// Should NOT satisfy TokenCalculator
	if _, ok := agent.AsTokenCalculator(wrapped); ok {
		t.Error("Wrap() should not return TokenCalculator when token_calculator=false")
	}

	// Should NOT satisfy TranscriptPreparer (transcript_preparer=false in validInfoJSON)
	if _, ok := agent.AsTranscriptPreparer(wrapped); ok {
		t.Error("Wrap() should not return TranscriptPreparer when transcript_preparer=false")
	}
}

func TestWrap_NoCapabilities(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	noCapJSON := `{
  "protocol_version": 1,
  "name": "minimal",
  "type": "Minimal",
  "description": "Minimal agent",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {}
}`

	binPath := testBinaryDir(t, mockInfoScript(noCapJSON))
	ea := newExternalAgent(t, binPath)

	wrapped, err := Wrap(ea)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	if _, ok := agent.AsHookSupport(wrapped); ok {
		t.Error("Wrap() should not return HookSupport when hooks=false")
	}
	if _, ok := agent.AsTranscriptAnalyzer(wrapped); ok {
		t.Error("Wrap() should not return TranscriptAnalyzer when transcript_analyzer=false")
	}
}

func TestWrap_HooksOnly(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	hooksOnlyJSON := `{
  "protocol_version": 1,
  "name": "hooks-only",
  "type": "Hooks Only",
  "description": "Agent with hooks only",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": ["stop"],
  "capabilities": {"hooks": true}
}`

	binPath := testBinaryDir(t, mockInfoScript(hooksOnlyJSON))
	ea := newExternalAgent(t, binPath)

	wrapped, err := Wrap(ea)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	if _, ok := agent.AsHookSupport(wrapped); !ok {
		t.Error("Wrap() should return HookSupport when hooks=true")
	}
	if _, ok := agent.AsTranscriptAnalyzer(wrapped); ok {
		t.Error("Wrap() should not return TranscriptAnalyzer when transcript_analyzer=false")
	}
}

func TestWrap_PreparerOnly(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	infoJSON := `{
  "protocol_version": 1,
  "name": "preparer-only",
  "type": "Preparer Only",
  "description": "Agent with preparer only",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {"transcript_preparer": true}
}`

	binPath := testBinaryDir(t, mockInfoScript(infoJSON))
	ea := newExternalAgent(t, binPath)

	wrapped, err := Wrap(ea)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	if _, ok := agent.AsTranscriptPreparer(wrapped); !ok {
		t.Error("Wrap() should return TranscriptPreparer when transcript_preparer=true")
	}
	if _, ok := agent.AsHookSupport(wrapped); ok {
		t.Error("Wrap() should not return HookSupport when hooks=false")
	}
	if _, ok := agent.AsTranscriptAnalyzer(wrapped); ok {
		t.Error("Wrap() should not return TranscriptAnalyzer when transcript_analyzer=false")
	}
}

func TestWrap_AnalyzerAndPreparer(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	infoJSON := `{
  "protocol_version": 1,
  "name": "analyzer-preparer",
  "type": "Analyzer Preparer",
  "description": "Agent with analyzer and preparer",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {"transcript_analyzer": true, "transcript_preparer": true}
}`

	binPath := testBinaryDir(t, mockInfoScript(infoJSON))
	ea := newExternalAgent(t, binPath)

	wrapped, err := Wrap(ea)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	if _, ok := agent.AsTranscriptAnalyzer(wrapped); !ok {
		t.Error("Wrap() should return TranscriptAnalyzer when transcript_analyzer=true")
	}
	if _, ok := agent.AsTranscriptPreparer(wrapped); !ok {
		t.Error("Wrap() should return TranscriptPreparer when transcript_preparer=true")
	}
	if _, ok := agent.AsHookSupport(wrapped); ok {
		t.Error("Wrap() should not return HookSupport when hooks=false")
	}
}

func TestWrap_HooksAnalyzerPreparer(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	infoJSON := `{
  "protocol_version": 1,
  "name": "hooks-analyzer-preparer",
  "type": "Hooks Analyzer Preparer",
  "description": "Agent with hooks, analyzer and preparer",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": ["stop"],
  "capabilities": {"hooks": true, "transcript_analyzer": true, "transcript_preparer": true}
}`

	binPath := testBinaryDir(t, mockInfoScript(infoJSON))
	ea := newExternalAgent(t, binPath)

	wrapped, err := Wrap(ea)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	if _, ok := agent.AsHookSupport(wrapped); !ok {
		t.Error("Wrap() should return HookSupport when hooks=true")
	}
	if _, ok := agent.AsTranscriptAnalyzer(wrapped); !ok {
		t.Error("Wrap() should return TranscriptAnalyzer when transcript_analyzer=true")
	}
	if _, ok := agent.AsTranscriptPreparer(wrapped); !ok {
		t.Error("Wrap() should return TranscriptPreparer when transcript_preparer=true")
	}
	if _, ok := agent.AsTokenCalculator(wrapped); ok {
		t.Error("Wrap() should not return TokenCalculator when token_calculator=false")
	}
}

func TestStripExeExt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "exe lowercase", in: "entire-agent-test.exe", want: "entire-agent-test"},
		{name: "bat lowercase", in: "entire-agent-test.bat", want: "entire-agent-test"},
		{name: "cmd lowercase", in: "entire-agent-test.cmd", want: "entire-agent-test"},
		{name: "exe uppercase", in: "entire-agent-test.EXE", want: "entire-agent-test"},
		{name: "bat mixed case", in: "entire-agent-test.Bat", want: "entire-agent-test"},
		{name: "cmd mixed case", in: "entire-agent-test.CmD", want: "entire-agent-test"},
		{name: "no extension", in: "entire-agent-test", want: "entire-agent-test"},
		{name: "unrelated extension", in: "entire-agent-test.sh", want: "entire-agent-test.sh"},
		{name: "dot only", in: "entire-agent-test.", want: "entire-agent-test."},
		{name: "empty string", in: "", want: ""},
		{name: "exe in middle", in: "entire-agent-exe-test", want: "entire-agent-exe-test"},
		{name: "double extension", in: "entire-agent-test.tar.exe", want: "entire-agent-test.tar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stripExeExt(tt.in); got != tt.want {
				t.Errorf("stripExeExt(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
