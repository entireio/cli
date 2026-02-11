package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestInstallHooks_CreatesManagedScaffold(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	count, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("InstallHooks() count = %d, want 1", count)
	}

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("failed to read scaffold: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, managedMarker) {
		t.Fatalf("managed marker missing from scaffold")
	}
	if !strings.Contains(content, "hooks pi session-start") {
		t.Fatalf("expected hook signature in scaffold")
	}

	if _, err := os.Stat(filepath.Join(repoRoot, ".pi", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .pi/settings.json mutation")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	if _, err := ag.InstallHooks(false, false); err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	count, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("second InstallHooks() count = %d, want 0", count)
	}
}

func TestInstallHooks_PreservesUserOwnedFileWithoutForce(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	if err := os.MkdirAll(filepath.Dir(entryPath), 0o755); err != nil {
		t.Fatalf("failed to create extension dir: %v", err)
	}
	const custom = "// user-owned extension\nexport default function() {}\n"
	if err := os.WriteFile(entryPath, []byte(custom), 0o644); err != nil {
		t.Fatalf("failed to write custom extension: %v", err)
	}

	ag := &PiAgent{}
	count, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("InstallHooks() count = %d, want 0", count)
	}

	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("failed to read extension: %v", err)
	}
	if string(data) != custom {
		t.Fatalf("user-owned extension should be preserved")
	}
}

func TestInstallHooks_ForceOverwritesUserOwnedFile(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	if err := os.MkdirAll(filepath.Dir(entryPath), 0o755); err != nil {
		t.Fatalf("failed to create extension dir: %v", err)
	}
	if err := os.WriteFile(entryPath, []byte("// user file\n"), 0o644); err != nil {
		t.Fatalf("failed to write extension: %v", err)
	}

	ag := &PiAgent{}
	count, err := ag.InstallHooks(false, true)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("InstallHooks() count = %d, want 1", count)
	}

	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("failed to read extension: %v", err)
	}
	if !strings.Contains(string(data), managedMarker) {
		t.Fatalf("expected managed scaffold after force install")
	}
}

func TestInstallHooks_LocalDevUsesAbsoluteRepoRootPath(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	if _, err := ag.InstallHooks(true, false); err != nil {
		t.Fatalf("InstallHooks(localDev=true) error = %v", err)
	}

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("failed to read scaffold: %v", err)
	}

	content := string(data)
	expectedMainPath := filepath.Join(repoRoot, "cmd", "entire", "main.go")
	if !strings.Contains(content, expectedMainPath) {
		t.Fatalf("expected local-dev scaffold to include absolute go main path %q", expectedMainPath)
	}
}

func TestUninstallHooks_RemovesManagedScaffoldOnly(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	if _, err := ag.InstallHooks(false, false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if err := ag.UninstallHooks(); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	if _, err := os.Stat(entryPath); !os.IsNotExist(err) {
		t.Fatalf("managed scaffold should be removed")
	}
}

func TestUninstallHooks_PreservesUserOwnedExtension(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	if err := os.MkdirAll(filepath.Dir(entryPath), 0o755); err != nil {
		t.Fatalf("failed to create extension dir: %v", err)
	}
	const custom = "// unmanaged\nexport default function() {}\n"
	if err := os.WriteFile(entryPath, []byte(custom), 0o644); err != nil {
		t.Fatalf("failed to write custom extension: %v", err)
	}

	ag := &PiAgent{}
	if err := ag.UninstallHooks(); err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("expected user-owned extension to remain: %v", err)
	}
	if string(data) != custom {
		t.Fatalf("user-owned extension should be preserved")
	}
}

func TestAreHooksInstalled_DetectsManagedScaffold(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	if _, err := ag.InstallHooks(false, false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if !ag.AreHooksInstalled() {
		t.Fatalf("AreHooksInstalled() = false, want true")
	}
}

func TestAreHooksInstalled_DetectsSignatureMatchedUserExtension(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	customPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, "custom.ts")
	if err := os.MkdirAll(filepath.Dir(customPath), 0o755); err != nil {
		t.Fatalf("failed to create extension dir: %v", err)
	}

	content := `// user extension forwarding to Entire
// hooks pi session-start
// hooks pi user-prompt-submit
// hooks pi before-tool
// hooks pi after-tool
// hooks pi stop
// hooks pi session-end
export default function() {}
`
	if err := os.WriteFile(customPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write custom extension: %v", err)
	}

	ag := &PiAgent{}
	if !ag.AreHooksInstalled() {
		t.Fatalf("AreHooksInstalled() = false, want true for signature-matched user extension")
	}
}

func TestInstallHooks_DoesNotInstallWhenUserExtensionAlreadyForwardsHooks(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	customPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, "custom.ts")
	if err := os.MkdirAll(filepath.Dir(customPath), 0o755); err != nil {
		t.Fatalf("failed to create extension dir: %v", err)
	}

	content := `// user extension forwarding to Entire
// hooks pi session-start
// hooks pi user-prompt-submit
// hooks pi before-tool
// hooks pi after-tool
// hooks pi stop
// hooks pi session-end
export default function() {}
`
	if err := os.WriteFile(customPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write custom extension: %v", err)
	}

	ag := &PiAgent{}
	count, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("InstallHooks() count = %d, want 0 when user extension already installs hooks", count)
	}

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	if _, err := os.Stat(entryPath); !os.IsNotExist(err) {
		t.Fatalf("managed scaffold should not be created when user extension already forwards hooks")
	}
}

func TestInstallHooks_TemplateContainsCanonicalLifecycleMappings(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	if _, err := ag.InstallHooks(false, false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	entryPath := filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("failed to read scaffold: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `pi.on("agent_end"`) {
		t.Fatalf("template must map agent_end lifecycle")
	}
	if !strings.Contains(content, `runHook("stop"`) {
		t.Fatalf("template must map stop hook")
	}
	if !strings.Contains(content, `pi.on("session_switch"`) {
		t.Fatalf("template must handle session_switch")
	}
	if !strings.Contains(content, `pi.on("session_fork"`) {
		t.Fatalf("template must handle session_fork")
	}
	if !strings.Contains(content, `pi.on("session_tree"`) {
		t.Fatalf("template must handle session_tree")
	}
	if !strings.Contains(content, `leaf_id: activeLeafId`) {
		t.Fatalf("template must include active leaf id in stop payload")
	}
	if !strings.Contains(content, `pi.on("tool_call"`) || !strings.Contains(content, `runHook("before-tool"`) {
		t.Fatalf("template must map tool_call to before-tool")
	}
	if !strings.Contains(content, `pi.on("tool_result"`) || !strings.Contains(content, `runHook("after-tool"`) {
		t.Fatalf("template must map tool_result to after-tool")
	}

	switchIdx := strings.Index(content, `pi.on("session_switch"`)
	if switchIdx == -1 {
		t.Fatalf("missing session_switch handler")
	}
	switchBlock := content[switchIdx:]
	endIdx := strings.Index(switchBlock, `runHook("session-end"`)
	startIdx := strings.Index(switchBlock, `runHook("session-start"`)
	if endIdx == -1 || startIdx == -1 || endIdx > startIdx {
		t.Fatalf("session_switch mapping must call session-end before session-start")
	}

	forkIdx := strings.Index(content, `pi.on("session_fork"`)
	if forkIdx == -1 {
		t.Fatalf("missing session_fork handler")
	}
	forkBlock := content[forkIdx:]
	forkEndIdx := strings.Index(forkBlock, `runHook("session-end"`)
	forkStartIdx := strings.Index(forkBlock, `runHook("session-start"`)
	if forkEndIdx == -1 || forkStartIdx == -1 || forkEndIdx > forkStartIdx {
		t.Fatalf("session_fork mapping must call session-end before session-start")
	}
}

func TestAreHooksInstalled_FalseWithoutScaffoldOrSignatures(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	if ag.AreHooksInstalled() {
		t.Fatalf("AreHooksInstalled() = true, want false")
	}
}

func TestGetHookNames(t *testing.T) {
	ag := &PiAgent{}
	names := ag.GetHookNames()

	if len(names) != 6 {
		t.Fatalf("GetHookNames() returned %d names, want 6", len(names))
	}

	expected := []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNameBeforeTool,
		HookNameAfterTool,
	}

	for i, name := range expected {
		if names[i] != name {
			t.Fatalf("GetHookNames()[%d] = %q, want %q", i, names[i], name)
		}
	}
}

func TestGetSupportedHooks(t *testing.T) {
	ag := &PiAgent{}
	hooks := ag.GetSupportedHooks()

	if len(hooks) != 6 {
		t.Fatalf("GetSupportedHooks() returned %d hooks, want 6", len(hooks))
	}

	expected := map[agent.HookType]bool{
		agent.HookSessionStart:     false,
		agent.HookSessionEnd:       false,
		agent.HookUserPromptSubmit: false,
		agent.HookPreToolUse:       false,
		agent.HookPostToolUse:      false,
		agent.HookStop:             false,
	}

	for _, hook := range hooks {
		if _, ok := expected[hook]; ok {
			expected[hook] = true
		}
	}

	for hook, found := range expected {
		if !found {
			t.Fatalf("GetSupportedHooks() missing %q", hook)
		}
	}
}

func TestUninstallHooks_NoExtensionFile(t *testing.T) {
	repoRoot := t.TempDir()
	t.Chdir(repoRoot)

	ag := &PiAgent{}
	if err := ag.UninstallHooks(); err != nil {
		t.Fatalf("UninstallHooks() should not error when file doesn't exist: %v", err)
	}
}
