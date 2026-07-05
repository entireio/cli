package strategy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectHookManagers_None(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	managers := detectHookManagers(tmpDir)
	if len(managers) != 0 {
		t.Errorf("expected 0 managers, got %d", len(managers))
	}
}

func TestDetectHookManagers_Husky(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	huskyDir := filepath.Join(tmpDir, ".husky", "_")
	if err := os.MkdirAll(huskyDir, 0o755); err != nil {
		t.Fatalf("failed to create .husky/_/: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Husky" {
		t.Errorf("expected Husky, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".husky/" {
		t.Errorf("expected .husky/, got %s", managers[0].ConfigPath)
	}
	if !managers[0].OverwritesHooks {
		t.Error("Husky should have OverwritesHooks=true")
	}
}

func TestDetectHookManagers_Lefthook(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" { //nolint:goconst // test assertion, not a magic string
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "lefthook.yml" {
		t.Errorf("expected lefthook.yml, got %s", managers[0].ConfigPath)
	}
	if managers[0].OverwritesHooks {
		t.Error("Lefthook should have OverwritesHooks=false")
	}
}

func TestDetectHookManagers_LefthookDotPrefix(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .lefthook.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".lefthook.yml" {
		t.Errorf("expected .lefthook.yml, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_LefthookToml(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook.toml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook.toml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "lefthook.toml" {
		t.Errorf("expected lefthook.toml, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_LefthookLocal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook-local.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook-local.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "lefthook-local.yml" {
		t.Errorf("expected lefthook-local.yml, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_LefthookDedup(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Both lefthook.yml and .lefthook.yml present — should only report once
	if err := os.WriteFile(filepath.Join(tmpDir, "lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create lefthook.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".lefthook.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .lefthook.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager (dedup), got %d", len(managers))
	}
	if managers[0].Name != "Lefthook" {
		t.Errorf("expected Lefthook, got %s", managers[0].Name)
	}
}

func TestDetectHookManagers_PreCommit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".pre-commit-config.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .pre-commit-config.yaml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "pre-commit" {
		t.Errorf("expected pre-commit, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".pre-commit-config.yaml" {
		t.Errorf("expected .pre-commit-config.yaml, got %s", managers[0].ConfigPath)
	}
	if managers[0].OverwritesHooks {
		t.Error("pre-commit should have OverwritesHooks=false")
	}
}

func TestDetectHookManagers_Overcommit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".overcommit.yml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .overcommit.yml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "Overcommit" {
		t.Errorf("expected Overcommit, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".overcommit.yml" {
		t.Errorf("expected .overcommit.yml, got %s", managers[0].ConfigPath)
	}
	if managers[0].OverwritesHooks {
		t.Error("Overcommit should have OverwritesHooks=false")
	}
}

func TestDetectHookManagers_Hk(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "hk.pkl"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create hk.pkl: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "hk" {
		t.Errorf("expected hk, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "hk.pkl" {
		t.Errorf("expected hk.pkl, got %s", managers[0].ConfigPath)
	}
	if managers[0].OverwritesHooks {
		t.Error("hk should have OverwritesHooks=false")
	}
}

func TestDetectHookManagers_HkConfigDir(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("failed to create .config/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "hk.pkl"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .config/hk.pkl: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "hk" {
		t.Errorf("expected hk, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != ".config/hk.pkl" {
		t.Errorf("expected .config/hk.pkl, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_HkLocal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "hk.local.pkl"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create hk.local.pkl: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager, got %d", len(managers))
	}
	if managers[0].Name != "hk" {
		t.Errorf("expected hk, got %s", managers[0].Name)
	}
	if managers[0].ConfigPath != "hk.local.pkl" {
		t.Errorf("expected hk.local.pkl, got %s", managers[0].ConfigPath)
	}
}

func TestDetectHookManagers_HkDedup(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Both hk.pkl and hk.local.pkl present — should only report once
	if err := os.WriteFile(filepath.Join(tmpDir, "hk.pkl"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create hk.pkl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "hk.local.pkl"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create hk.local.pkl: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 1 {
		t.Fatalf("expected 1 manager (dedup), got %d", len(managers))
	}
	if managers[0].Name != "hk" {
		t.Errorf("expected hk, got %s", managers[0].Name)
	}
}

func TestDetectHookManagers_Multiple(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Create Husky + pre-commit
	if err := os.MkdirAll(filepath.Join(tmpDir, ".husky", "_"), 0o755); err != nil {
		t.Fatalf("failed to create .husky/_/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".pre-commit-config.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create .pre-commit-config.yaml: %v", err)
	}

	managers := detectHookManagers(tmpDir)
	if len(managers) != 2 {
		t.Fatalf("expected 2 managers, got %d", len(managers))
	}

	names := make(map[string]bool)
	for _, m := range managers {
		names[m.Name] = true
	}
	if !names["Husky"] {
		t.Error("expected Husky to be detected")
	}
	if !names["pre-commit"] {
		t.Error("expected pre-commit to be detected")
	}
}

func TestHookManagerWarning_Husky(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true},
	}

	warning := hookManagerWarning(managers, "entire")

	// Should contain all 4 hook file references
	for _, hook := range gitHookNames {
		if !strings.Contains(warning, ".husky/"+hook+":") {
			t.Errorf("warning should contain .husky/%s:", hook)
		}
	}

	// Should contain the actual command lines from buildHookSpecs
	specs := buildHookSpecs("entire")
	for _, spec := range specs {
		cmdLine := extractCommandLine(spec.content)
		if cmdLine == "" {
			t.Errorf("failed to extract command line for %s", spec.name)
			continue
		}
		if !strings.Contains(warning, cmdLine) {
			t.Errorf("warning should contain command line %q for %s", cmdLine, spec.name)
		}
	}

	// Should mention Husky by name and warn about overwriting
	if !strings.Contains(warning, "Warning: Husky detected") {
		t.Error("warning should start with 'Warning: Husky detected'")
	}
	if !strings.Contains(warning, "may overwrite hooks") {
		t.Error("warning should mention 'may overwrite hooks'")
	}
}

func TestHookManagerWarning_GitHooksManager(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Lefthook", ConfigPath: "lefthook.yml", OverwritesHooks: false},
	}

	warning := hookManagerWarning(managers, "entire")

	// Category B: should be a Note, not a Warning
	if !strings.Contains(warning, "Note: Lefthook detected") {
		t.Error("warning should contain 'Note: Lefthook detected'")
	}
	if !strings.Contains(warning, "run 'entire enable' to restore") {
		t.Error("warning should mention running 'entire enable'")
	}

	// Should NOT contain hook file copy-paste instructions
	if strings.Contains(warning, "prepare-commit-msg:") {
		t.Error("category B warning should not contain hook file instructions")
	}
}

func TestHookManagerWarning_Empty(t *testing.T) {
	t.Parallel()

	warning := hookManagerWarning(nil, "entire")
	if warning != "" {
		t.Errorf("expected empty string for nil managers, got %q", warning)
	}

	warning = hookManagerWarning([]hookManager{}, "entire")
	if warning != "" {
		t.Errorf("expected empty string for empty managers, got %q", warning)
	}
}

func TestHookManagerWarning_LocalDev(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true},
	}

	warning := hookManagerWarning(managers, localDevHookCmdPrefix)

	// Should use the local dev prefix in command lines
	if !strings.Contains(warning, localDevHookCmdPrefix+" hooks git") {
		t.Error("warning should use local dev command prefix")
	}
}

func TestHookManagerWarning_Multiple(t *testing.T) {
	t.Parallel()

	managers := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true},
		{Name: "Lefthook", ConfigPath: "lefthook.yml", OverwritesHooks: false},
	}

	warning := hookManagerWarning(managers, "entire")

	if !strings.Contains(warning, "Warning: Husky detected") {
		t.Error("should contain Husky warning")
	}
	if !strings.Contains(warning, "Note: Lefthook detected") {
		t.Error("should contain Lefthook note")
	}
}

func TestExtractCommandLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "standard hook",
			content: "#!/bin/sh\n# Entire CLI hooks\nentire hooks git post-commit 2>/dev/null || true\n",
			want:    "entire hooks git post-commit 2>/dev/null || true",
		},
		{
			name:    "multiple comments",
			content: "#!/bin/sh\n# comment 1\n# comment 2\nentire hooks git pre-push \"$1\" || true\n",
			want:    `entire hooks git pre-push "$1" || true`,
		},
		{
			name:    "empty content",
			content: "",
			want:    "",
		},
		{
			name:    "only comments",
			content: "#!/bin/sh\n# just a comment\n",
			want:    "",
		},
		{
			name:    "whitespace around command",
			content: "#!/bin/sh\n# comment\n  entire hooks git commit-msg \"$1\" || exit 1  \n",
			want:    `entire hooks git commit-msg "$1" || exit 1`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractCommandLine(tt.content)
			if got != tt.want {
				t.Errorf("extractCommandLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckAndWarnHookManagers_NoManagers(t *testing.T) {
	// Needs t.Chdir (via initHooksTestRepo), cannot be parallel
	initHooksTestRepo(t)

	var buf bytes.Buffer
	CheckAndWarnHookManagers(context.Background(), &buf, false, false)

	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}

func TestCheckAndWarnHookManagers_WithHusky(t *testing.T) {
	// Needs t.Chdir (via initHooksTestRepo), cannot be parallel
	tmpDir, _ := initHooksTestRepo(t)

	// Create .husky/_/ directory
	if err := os.MkdirAll(filepath.Join(tmpDir, ".husky", "_"), 0o755); err != nil {
		t.Fatalf("failed to create .husky/_/: %v", err)
	}

	var buf bytes.Buffer
	CheckAndWarnHookManagers(context.Background(), &buf, false, false)

	output := buf.String()
	if !strings.Contains(output, "Warning: Husky detected") {
		t.Errorf("expected warning output, got %q", output)
	}
}

func TestCheckAndWarnHookManagers_ExternalBackend_IsSilent(t *testing.T) {
	// Needs t.Chdir (via initHooksTestRepo), cannot be parallel
	tmpDir, _ := initHooksTestRepo(t)

	// Create .husky/_/ directory (Husky fingerprint that would normally trigger warning)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".husky", "_"), 0o755); err != nil {
		t.Fatalf("failed to create .husky/_/: %v", err)
	}

	// External settings: user explicitly opted into external mode, so the warning
	// is noise (user already knows about Husky and chose to integrate with it).
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "git_hooks": {"backend": "external", "external_dir": ".husky"}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	CheckAndWarnHookManagers(context.Background(), &buf, false, false)

	if buf.Len() != 0 {
		t.Errorf("expected no output in external mode, got %q", buf.String())
	}
}

// A lefthook config that wires entire into all 5 managed hooks (via
// commands) should be detected as fully installed.
func writeLefthookConfig(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

const lefthookAllHooksYAML = `prepare-commit-msg:
  commands:
    entire:
      run: entire hooks git prepare-commit-msg {1} {2}
commit-msg:
  commands:
    entire:
      run: entire hooks git commit-msg {1}
post-commit:
  commands:
    entire:
      run: entire hooks git post-commit
post-rewrite:
  commands:
    entire:
      run: entire hooks git post-rewrite {1}
pre-push:
  commands:
    entire:
      run: entire hooks git pre-push {1}
`

func TestIsEntireWiredIntoManager_Lefthook_AllHooksYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeLefthookConfig(t, dir, "lefthook.yml", lefthookAllHooksYAML)

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected wired=true when all 5 hooks call entire dispatch")
	}
}

func TestIsEntireWiredIntoManager_Lefthook_MissingOneHook(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Drop the pre-push section entirely.
	partial := strings.Replace(lefthookAllHooksYAML,
		"pre-push:\n  commands:\n    entire:\n      run: entire hooks git pre-push {1}\n", "", 1)
	writeLefthookConfig(t, dir, "lefthook.yml", partial)

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected wired=false when pre-push is not wired")
	}
}

func TestIsEntireWiredIntoManager_Lefthook_OnlyInComment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The dispatch string appears only in a comment, not in any run:.
	commented := "# entire hooks git pre-push {1}\npre-push:\n  commands:\n    lint:\n      run: npm run lint\n"
	writeLefthookConfig(t, dir, "lefthook.yml", commented)

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected wired=false when entire dispatch only appears in a comment")
	}
}

func TestIsEntireWiredIntoManager_Lefthook_JSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	json := `{
  "prepare-commit-msg": {"commands": {"entire": {"run": "entire hooks git prepare-commit-msg {1} {2}"}}},
  "commit-msg": {"commands": {"entire": {"run": "entire hooks git commit-msg {1}"}}},
  "post-commit": {"commands": {"entire": {"run": "entire hooks git post-commit"}}},
  "post-rewrite": {"commands": {"entire": {"run": "entire hooks git post-rewrite {1}"}}},
  "pre-push": {"commands": {"entire": {"run": "entire hooks git pre-push {1}"}}}
}`
	writeLefthookConfig(t, dir, "lefthook.json", json)

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected wired=true for JSON config with all hooks")
	}
}

func TestIsEntireWiredIntoManager_Lefthook_TOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	toml := `[prepare-commit-msg.commands.entire]
run = "entire hooks git prepare-commit-msg {1} {2}"
[commit-msg.commands.entire]
run = "entire hooks git commit-msg {1}"
[post-commit.commands.entire]
run = "entire hooks git post-commit"
[post-rewrite.commands.entire]
run = "entire hooks git post-rewrite {1}"
[pre-push.commands.entire]
run = "entire hooks git pre-push {1}"
`
	writeLefthookConfig(t, dir, "lefthook.toml", toml)

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected wired=true for TOML config with all hooks")
	}
}

// A scripts/-style entry (native $1/$@ rather than {1}) should also count.
func TestIsEntireWiredIntoManager_Lefthook_ScriptsForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yaml := `prepare-commit-msg:
  scripts:
    "entire.sh":
      runner: sh
commit-msg:
  commands:
    entire:
      run: entire hooks git commit-msg {1}
post-commit:
  commands:
    entire:
      run: entire hooks git post-commit
post-rewrite:
  commands:
    entire:
      run: entire hooks git post-rewrite {1}
pre-push:
  commands:
    entire:
      run: entire hooks git pre-push {1}
`
	writeLefthookConfig(t, dir, "lefthook.yml", yaml)
	// The prepare-commit-msg script file carries the dispatch call.
	scriptsDir := filepath.Join(dir, ".lefthook", "prepare-commit-msg")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "entire.sh"),
		[]byte("#!/bin/sh\nentire hooks git prepare-commit-msg \"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected wired=true when a hook is wired via a scripts/ entry")
	}
}

// lefthook-local.yml should fill in a hook missing from the main config.
func TestIsEntireWiredIntoManager_Lefthook_LocalOverlay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Main config wires 4 hooks, missing pre-push.
	main := strings.Replace(lefthookAllHooksYAML,
		"pre-push:\n  commands:\n    entire:\n      run: entire hooks git pre-push {1}\n", "", 1)
	writeLefthookConfig(t, dir, "lefthook.yml", main)
	// Local overlay adds pre-push.
	writeLefthookConfig(t, dir, "lefthook-local.yml",
		"pre-push:\n  commands:\n    entire:\n      run: entire hooks git pre-push {1}\n")

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected wired=true when lefthook-local fills in the missing hook")
	}
}

func TestIsEntireWiredIntoManager_Lefthook_NoConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ok, err := isEntireWiredIntoManager(dir, "lefthook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected wired=false when no lefthook config exists")
	}
}

// wiredManagerHooks reports which hooks are wired so doctor can show the
// missing set. Verify it returns the accurate subset.
func TestLefthookWiredHooks_PartialReporting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	partial := strings.Replace(lefthookAllHooksYAML,
		"pre-push:\n  commands:\n    entire:\n      run: entire hooks git pre-push {1}\n", "", 1)
	writeLefthookConfig(t, dir, "lefthook.yml", partial)

	wired, err := lefthookWiredHooks(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wired["pre-push"] {
		t.Error("pre-push should not be reported as wired")
	}
	if !wired["commit-msg"] {
		t.Error("commit-msg should be reported as wired")
	}
}
