package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func newReviewConfigRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	names := []string{}
	for name, content := range files {
		testutil.WriteFile(t, dir, name, content)
		names = append(names, name)
	}
	testutil.WriteFile(t, dir, "README.md", "x")
	testutil.GitAdd(t, dir, append(names, "README.md")...)
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	return dir
}

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// A profile config replaces the checkout's settings and MCP servers, keeps
// Entire's tracking hooks, and loads the checkout's skills as a plugin.
func TestPrepareReviewAgentConfig(t *testing.T) {
	newReviewConfigRepo(t, map[string]string{
		".claude/skills/review/SKILL.md": "---\nname: review\ndescription: d\n---\nReview.",
		".claude/commands/check.md":      "Check.",
	})
	cfg := reviewtypes.RunConfig{
		Skills: []string{"/review", "/other"},
		AgentConfig: &reviewtypes.AgentConfig{
			Settings:   json.RawMessage(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/bin/true"}]}]}}`),
			MCPServers: map[string]json.RawMessage{"docs": json.RawMessage(`{"url":"https://mcp.example"}`)},
		},
	}
	got, cleanup, err := prepareReviewAgentConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--strict-mcp-config", flagSettingSources} {
		if !slices.Contains(got.ExtraArgs, flag) {
			t.Errorf("ExtraArgs %v missing %s", got.ExtraArgs, flag)
		}
	}
	if argAfter(got.ExtraArgs, flagSettingSources) != "user" {
		t.Errorf("--setting-sources = %q, want user", argAfter(got.ExtraArgs, flagSettingSources))
	}
	settingsData, err := os.ReadFile(argAfter(got.ExtraArgs, "--settings"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/usr/bin/true", "entire hooks claude-code stop"} {
		if !strings.Contains(string(settingsData), want) {
			t.Errorf("settings missing %q: %s", want, settingsData)
		}
	}
	mcpData, err := os.ReadFile(argAfter(got.ExtraArgs, "--mcp-config"))
	if err != nil || !strings.Contains(string(mcpData), "mcp.example") {
		t.Errorf("mcp config = %s (%v)", mcpData, err)
	}
	pluginDir := argAfter(got.ExtraArgs, "--plugin-dir")
	for _, rel := range []string{".claude-plugin/plugin.json", "skills/review/SKILL.md", "commands/check.md"} {
		if _, err := os.Stat(filepath.Join(pluginDir, rel)); err != nil {
			t.Errorf("plugin missing %s: %v", rel, err)
		}
	}
	if !slices.Equal(got.Skills, []string{"/project:review", "/other"}) {
		t.Errorf("Skills = %v, want the checkout's skill namespaced", got.Skills)
	}
	cleanup()
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind", pluginDir)
	}
}

// Without a profile config nothing changes.
func TestPrepareReviewAgentConfigNoConfig(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{Skills: []string{"/review"}}
	got, cleanup, err := prepareReviewAgentConfig(t.Context(), cfg)
	if err != nil || cleanup != nil || len(got.ExtraArgs) != 0 {
		t.Fatalf("got %+v, cleanup %v, err %v", got, cleanup != nil, err)
	}
}

// A symlinked skill is refused rather than followed.
func TestPrepareReviewAgentConfigRefusesSymlinkedSkill(t *testing.T) {
	dir := newReviewConfigRepo(t, map[string]string{"elsewhere.md": "x"})
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../elsewhere.md", filepath.Join(dir, ".claude", "skills", "link.md")); err != nil {
		t.Fatal(err)
	}
	testutil.GitAdd(t, dir, ".claude/skills/link.md")
	testutil.GitCommit(t, dir, "symlink")

	_, _, err := prepareReviewAgentConfig(t.Context(), reviewtypes.RunConfig{AgentConfig: &reviewtypes.AgentConfig{}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
}

// A hook that runs code from the checkout is refused at run time.
func TestPrepareReviewAgentConfigRefusesCheckoutCommand(t *testing.T) {
	newReviewConfigRepo(t, nil)
	cfg := reviewtypes.RunConfig{AgentConfig: &reviewtypes.AgentConfig{
		Settings: json.RawMessage(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"./hook.sh"}]}]}}`),
	}}
	if _, _, err := prepareReviewAgentConfig(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Fatalf("err = %v, want a relative-path refusal", err)
	}
}
