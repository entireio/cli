package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openRepoHeadTree opens the repo at dir and returns the HEAD commit tree.
func openRepoHeadTree(t *testing.T, dir string) *object.Tree {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	return tree
}

func TestReadAgentTypeFromTree_OnlyClaude(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	// git doesn't track empty dirs — add a file inside .claude/
	testutil.WriteFile(t, dir, ".claude/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".claude/settings.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeClaudeCode, result)
}

func TestReadAgentTypeFromTree_OnlyGemini(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, ".gemini/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".gemini/settings.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeGemini, result)
}

func TestReadAgentTypeFromTree_OnlyCodex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, ".codex/config.json", `{}`)
	testutil.GitAdd(t, dir, ".codex/config.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeCodex, result)
}

func TestReadAgentTypeFromTree_OnlyCursor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, ".cursor/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".cursor/settings.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeCursor, result)
}

func TestReadAgentTypeFromTree_OnlyFactory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, ".factory/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".factory/settings.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeFactoryAIDroid, result)
}

func TestReadAgentTypeFromTree_ClaudeAndCodex_ReturnsUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, ".claude/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".claude/settings.json")
	testutil.WriteFile(t, dir, ".codex/config.json", `{}`)
	testutil.GitAdd(t, dir, ".codex/config.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeUnknown, result)
}

func TestReadAgentTypeFromTree_ClaudeAndGemini_ReturnsUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, ".claude/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".claude/settings.json")
	testutil.WriteFile(t, dir, ".gemini/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".gemini/settings.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeUnknown, result)
}

func TestReadAgentTypeFromTree_NoAgentDirs_ReturnsUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	result := ReadAgentTypeFromTree(tree, "nonexistent-path")
	assert.Equal(t, agent.AgentTypeUnknown, result)
}

func TestReadAgentTypeFromTree_MetadataJSON_OverridesDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	// .claude/ dir present — without metadata.json this would return ClaudeCode
	testutil.WriteFile(t, dir, ".claude/settings.json", `{}`)
	testutil.GitAdd(t, dir, ".claude/settings.json")
	// metadata.json at checkpoint path declares a different agent
	testutil.WriteFile(t, dir, "cp/metadata.json", `{"agent":"Cursor"}`)
	testutil.GitAdd(t, dir, "cp/metadata.json")
	testutil.GitCommit(t, dir, "init")

	tree := openRepoHeadTree(t, dir)
	// Pass "cp" as checkpointPath — ReadAgentTypeFromTree looks for "cp/metadata.json"
	result := ReadAgentTypeFromTree(tree, "cp")
	assert.Equal(t, agent.AgentTypeCursor, result)
}
