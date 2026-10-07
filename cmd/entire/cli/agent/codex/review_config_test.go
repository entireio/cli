package codex

import (
	"encoding/json"
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
	for _, root := range []string{wtResolved, mainResolved} {
		if !strings.Contains(args, untrustedProjectOverride(root)) {
			t.Errorf("ExtraArgs missing untrusted override for %s:\n%s", root, args)
		}
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

func TestPrepareCodexReviewConfigRefusesUnsupported(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "x")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

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
