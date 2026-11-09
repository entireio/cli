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

// A symlinked skills dir is skipped, not followed: the trust gate lists it as
// not inspected, so the review runs without it instead of failing.
func TestPrepareReviewAgentConfigSkipsSymlinkedSkillsDir(t *testing.T) {
	dir := newReviewConfigRepo(t, map[string]string{".agents/skills/review/SKILL.md": "---\nname: review\ndescription: d\n---\nReview."})
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.agents/skills", filepath.Join(dir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	testutil.GitAdd(t, dir, ".claude/skills")
	testutil.GitCommit(t, dir, "symlink")

	got, cleanup, err := prepareReviewAgentConfig(t.Context(), reviewtypes.RunConfig{Skills: []string{"/review"}, AgentConfig: &reviewtypes.AgentConfig{}})
	if err != nil {
		t.Fatalf("a symlinked skills dir must not fail the review: %v", err)
	}
	t.Cleanup(cleanup)
	if slices.Contains(got.ExtraArgs, "--plugin-dir") {
		t.Errorf("nothing was copied, yet a plugin is loaded: %v", got.ExtraArgs)
	}
	if !slices.Equal(got.Skills, []string{"/review"}) {
		t.Errorf("Skills = %v, want the skipped skill left as is", got.Skills)
	}
}

// A symlinked skill or a submodule beside regular skills is skipped; the
// regular ones load, and only they are namespaced.
func TestPrepareReviewAgentConfigSkipsSymlinkAndSubmoduleEntries(t *testing.T) {
	dir := newReviewConfigRepo(t, map[string]string{
		"elsewhere/SKILL.md":           "x",
		".claude/skills/kept/SKILL.md": "---\nname: kept\ndescription: d\n---\nKept.",
	})
	if err := os.Symlink("../../elsewhere", filepath.Join(dir, ".claude", "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	testutil.GitAdd(t, dir, ".claude/skills/linked")
	head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	testutil.RunGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",.claude/skills/sub")
	testutil.GitCommit(t, dir, "symlink and submodule")

	got, cleanup, err := prepareReviewAgentConfig(t.Context(), reviewtypes.RunConfig{
		Skills:      []string{"/kept", "/linked", "/sub"},
		AgentConfig: &reviewtypes.AgentConfig{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	pluginDir := argAfter(got.ExtraArgs, "--plugin-dir")
	if _, err := os.Stat(filepath.Join(pluginDir, "skills", "kept", "SKILL.md")); err != nil {
		t.Errorf("regular skill not copied: %v", err)
	}
	for _, name := range []string{"linked", "sub"} {
		if _, err := os.Lstat(filepath.Join(pluginDir, "skills", name)); !os.IsNotExist(err) {
			t.Errorf("skipped entry %s is in the plugin (%v)", name, err)
		}
	}
	if !slices.Equal(got.Skills, []string{"/project:kept", "/linked", "/sub"}) {
		t.Errorf("Skills = %v, want only the copied skill namespaced", got.Skills)
	}
}

// A null settings or hooks value still gets Entire's hooks rather than
// crashing the review.
func TestReviewProfileSettingsNull(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{`null`, `{"hooks":null}`} {
		out, err := reviewProfileSettings(json.RawMessage(profile))
		if err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
		if !strings.Contains(string(out), "entire hooks claude-code") {
			t.Errorf("%s: Entire's hooks missing: %s", profile, out)
		}
	}
}

func TestReviewProfileSettingsRefusesMalformedHookEvent(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{`{"hooks":{"Stop":{}}}`, `{"hooks":{"Stop":null}}`, `{"hooks":{"Stop":"x"}}`} {
		if _, err := reviewProfileSettings(json.RawMessage(profile)); err == nil {
			t.Errorf("%s: accepted", profile)
		}
	}
}
