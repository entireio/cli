package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// The tests below change CODEX_HOME and ENTIRE_CONFIG_DIR, so none of them
// runs in parallel.

// isolateCodexHomes points the active CODEX_HOME at a fresh directory and the
// per-user registry at an empty one, and returns the active home.
func isolateCodexHomes(t *testing.T) string {
	t.Helper()

	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")
	active := t.TempDir()
	t.Setenv("CODEX_HOME", active)
	return active
}

// linkTo returns a link to dir, skipping the test where links are unsupported.
func linkTo(t *testing.T, dir string) string {
	t.Helper()

	link := filepath.Join(t.TempDir(), "codex-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	return link
}

// resolveChild runs the inventory for one declared child under home and
// returns the path it resolved, or "".
func resolveChild(t *testing.T, declared, home string) string {
	t.Helper()

	return resolveChildWith(t, &CodexAgent{}, declared, home)
}

// resolveChildWith is resolveChild for a given agent.
func resolveChildWith(t *testing.T, ag *CodexAgent, declared, home string) string {
	t.Helper()

	refs := []agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: declared}}
	extraction, ok := agent.ExtractWithSubagentInventory(t.Context(), ag, nil, 0, refs, home)
	require.True(t, ok)
	require.Len(t, extraction.Children, 1)
	return extraction.Children[0].ResolvedPath
}

func TestExtractWithSubagentInventory_LooksUnderATrustedRecordedHome(t *testing.T) {
	isolateCodexHomes(t)
	recorded, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	child := writeRollout(t, recorded, "archived_sessions/rollout-2026-10-01T10-00-00-child.jsonl", "child", nil)

	require.Empty(t, resolveChild(t, child, recorded), "a home that is neither active nor recorded must not be searched")

	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, recorded))
	require.Equal(t, child, resolveChild(t, child, recorded))
}

func TestExtractWithSubagentInventory_RecordedHomeSpelledThroughALink(t *testing.T) {
	isolateCodexHomes(t)
	recorded, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, recorded))
	link := linkTo(t, recorded)
	// The session's transcript paths spell the home through the link.
	child := writeRollout(t, link, "sessions/2026/10/01/rollout-2026-10-01T10-00-00-child.jsonl", "child", nil)

	require.Equal(t, child, resolveChild(t, child, link))
}

func TestExtractWithSubagentInventory_ActiveHomeKeepsTheAgentsOwnResolution(t *testing.T) {
	active := isolateCodexHomes(t)
	// The agent resolves rollouts from its own roots, as with the
	// ENTIRE_TEST_CODEX_SESSION_DIR override; scoping to the home would miss
	// them.
	custom := t.TempDir()
	child := writeRollout(t, custom, "2026/10/01/rollout-2026-10-01T10-00-00-child.jsonl", "child", nil)
	ag := &CodexAgent{RolloutRoots: []string{custom}}

	require.Equal(t, child, resolveChildWith(t, ag, child, active))
}

func TestExtractWithSubagentInventory_ChecksTheSpellingItScopesTo(t *testing.T) {
	isolateCodexHomes(t)
	recorded, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, recorded))
	require.NoError(t, os.Mkdir(filepath.Join(recorded, "sessions"), 0o700))

	// A home spelled <dir>/link/.. resolves through the link to the recorded
	// home but cleans to <dir>, which is not trusted and must not be searched.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	if err := os.Symlink(filepath.Join(recorded, "sessions"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	planted := writeRollout(t, dir, "sessions/2026/10/01/rollout-2026-10-01T10-00-00-child.jsonl", "child", nil)

	require.Empty(t, resolveChild(t, planted, dir+string(filepath.Separator)+"link"+string(filepath.Separator)+".."))
}
