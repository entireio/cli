package codex

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

// In a linked worktree Codex shares trust with the main checkout, so both
// are marked untrusted; profile MCP servers go through -c.
func TestPrepareCodexReviewConfigLinkedWorktree(t *testing.T) {
	main := t.TempDir()
	testutil.InitRepo(t, main)
	testutil.WriteFile(t, main, "README.md", "x")
	testutil.GitAdd(t, main, "README.md")
	testutil.GitCommit(t, main, "init")
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, main, "worktree", "add", "-q", "-b", "review", wt)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Chdir(wt)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	cfg := reviewtypes.RunConfig{AgentConfig: &reviewtypes.AgentConfig{
		MCPServers: map[string]json.RawMessage{"docs": json.RawMessage(`{"command":"/opt/mcp/docs","args":["--stdio"]}`)},
	}}
	got, _, err := prepareCodexReviewConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(got.ExtraArgs, "\n")
	mainResolved, _ := filepath.EvalSymlinks(main) //nolint:errcheck // a temp dir always resolves
	wtResolved, _ := filepath.EvalSymlinks(wt)     //nolint:errcheck // a temp dir always resolves
	if want := untrustedProjectOverride([]string{wtResolved, mainResolved}); strings.Count(args, "projects=") != 1 || !slices.Contains(got.ExtraArgs, want) {
		t.Errorf("ExtraArgs want one override %s:\n%s", want, args)
	}
	for _, want := range []string{`mcp_servers.docs.command="/opt/mcp/docs"`, `mcp_servers.docs.args=["--stdio"]`} {
		if !slices.Contains(got.ExtraArgs, want) {
			t.Errorf("ExtraArgs missing %s:\n%s", want, args)
		}
	}
	if got.WorkDir != wtResolved {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, wtResolved)
	}
}

// codexReviewRepo chdirs into a fresh repo with an isolated, empty Codex home,
// and returns that home.
func codexReviewRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "x")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	return home
}

func TestPrepareCodexReviewConfigRefusesUnsupported(t *testing.T) {
	codexReviewRepo(t)

	for name, cfg := range map[string]reviewtypes.AgentConfig{
		"hooks via settings": {Settings: json.RawMessage(`{"hooks":{}}`)},
		"env values":         {MCPServers: map[string]json.RawMessage{"docs": json.RawMessage(`{"command":"/opt/x","env":{"TOKEN":"s"}}`)}},
	} {
		c := cfg
		if _, _, err := prepareCodexReviewConfig(t.Context(), reviewtypes.RunConfig{AgentConfig: &c}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Codex merges -c flags into the user's config table by table, so a profile
// MCP server named like one in ~/.codex/config.toml would run as a mix of the
// two. That name is refused; other names, and a profile without MCP servers,
// are not affected.
func TestPrepareCodexReviewConfigRefusesUserMCPServerName(t *testing.T) {
	docs := map[string]json.RawMessage{"docs": json.RawMessage(`{"command":"/opt/mcp/docs"}`)}
	other := map[string]json.RawMessage{"other": json.RawMessage(`{"command":"/opt/mcp/other"}`)}
	userConfig := "[mcp_servers.docs]\nurl = \"https://mcp.example\"\nbearer_token_env_var = \"DOCS_TOKEN\"\n"
	for _, tc := range []struct {
		name    string
		config  string // "" writes no config.toml
		link    bool   // config.toml is a symlink to the file
		servers map[string]json.RawMessage
		wantErr string
	}{
		{name: "same name", config: userConfig, servers: docs, wantErr: `MCP server "docs" is also defined in`},
		{name: "same name through a symlinked config", config: userConfig, link: true, servers: docs, wantErr: `MCP server "docs" is also defined in`},
		{name: "other name", config: userConfig, servers: other},
		{name: "no user config", servers: docs},
		{name: "unparseable config", config: "[mcp_servers.docs\n", servers: docs, wantErr: "parse"},
		{name: "oversized config", config: "# " + strings.Repeat("x", maxHooksFileBytes) + "\n", servers: docs, wantErr: "too large"},
		{name: "no profile servers never reads the config", config: "[mcp_servers.docs\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := codexReviewRepo(t)
			if tc.config != "" {
				target := filepath.Join(home, "config.toml")
				if tc.link {
					target = filepath.Join(t.TempDir(), "dotfiles.toml")
					if err := os.Symlink(target, filepath.Join(home, "config.toml")); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(target, []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := prepareCodexReviewConfig(t.Context(), reviewtypes.RunConfig{AgentConfig: &reviewtypes.AgentConfig{MCPServers: tc.servers}})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// Entire's hooks are passed as session flags, since the untrusted checkout's
// hooks.json isn't loaded, and run without persisted trust.
func TestEntireHookOverrides(t *testing.T) {
	t.Parallel()
	args := strings.Join(entireHookOverrides(false), "\n")
	for _, want := range []string{`hooks.SessionStart=[{hooks=[{type="command",command=`, "entire hooks codex session-start", "hooks.Stop="} {
		if !strings.Contains(args, want) {
			t.Errorf("overrides missing %q:\n%s", want, args)
		}
	}
}

func TestCodexMCPOverridesPassApprovalMode(t *testing.T) {
	t.Parallel()
	args, err := codexMCPOverrides(map[string]json.RawMessage{"docs": json.RawMessage(`{"command":"/opt/docs","default_tools_approval_mode":"approve"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, `mcp_servers.docs.default_tools_approval_mode="approve"`) {
		t.Fatalf("args = %v", args)
	}
}
