package paths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures follow a real Claude Code 2.1.291 Workflow run: per agent an
// agent-<id>.jsonl transcript and an agent-<id>.meta.json, plus one
// journal.jsonl per run (#2685).
func writeWorkflowFixture(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
}

func TestWorkflowAgentTranscripts(t *testing.T) {
	t.Parallel()

	subagentsDir := SubagentsDir(t.TempDir(), "sess")
	run1 := filepath.Join(subagentsDir, SubagentWorkflowsDirName, "wf_e5264e60-494")
	run2 := filepath.Join(subagentsDir, SubagentWorkflowsDirName, "wf_2")
	a1 := filepath.Join(run1, "agent-ae3d7b8f2930c8787.jsonl")
	a2 := filepath.Join(run2, "agent-ac82c55f48a03882b.jsonl")
	writeWorkflowFixture(t, a1)
	writeWorkflowFixture(t, a2)
	writeWorkflowFixture(t, filepath.Join(run1, "agent-ae3d7b8f2930c8787.meta.json"))
	writeWorkflowFixture(t, filepath.Join(run1, "journal.jsonl"))
	writeWorkflowFixture(t, filepath.Join(run1, "agent-.jsonl"))
	writeWorkflowFixture(t, filepath.Join(run1, "agent-a b.jsonl"))
	writeWorkflowFixture(t, filepath.Join(run1, "nested", "agent-deep.jsonl"))
	writeWorkflowFixture(t, filepath.Join(subagentsDir, SubagentWorkflowsDirName, "agent-norun.jsonl"))
	require.NoError(t, os.MkdirAll(filepath.Join(run1, "agent-dir.jsonl"), 0o750))

	assert.Equal(t, map[string]string{
		"ae3d7b8f2930c8787": a1,
		"ac82c55f48a03882b": a2,
	}, WorkflowAgentTranscripts(subagentsDir))
}

func TestWorkflowAgentTranscripts_NoWorkflows(t *testing.T) {
	t.Parallel()

	assert.Empty(t, WorkflowAgentTranscripts(filepath.Join(t.TempDir(), "missing")))
	assert.Empty(t, WorkflowAgentTranscripts(""))
}

// Only regular files in real run directories count: a link cannot pull an
// unrelated file into a checkpoint as a workflow agent's transcript.
func TestWorkflowAgentTranscripts_SkipsSymlinks(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	subagentsDir := SubagentsDir(base, "sess")
	run := filepath.Join(subagentsDir, SubagentWorkflowsDirName, "wf_1")
	require.NoError(t, os.MkdirAll(run, 0o750))
	outsideFile := filepath.Join(base, "outside.jsonl")
	writeWorkflowFixture(t, outsideFile)
	if err := os.Symlink(outsideFile, filepath.Join(run, "agent-linked.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	outsideRun := filepath.Join(base, "outside-run")
	writeWorkflowFixture(t, filepath.Join(outsideRun, "agent-inlinkedrun.jsonl"))
	require.NoError(t, os.Symlink(outsideRun, filepath.Join(subagentsDir, SubagentWorkflowsDirName, "wf_linked")))

	assert.Empty(t, WorkflowAgentTranscripts(subagentsDir))
}

func TestResolveSubagentTranscriptPath(t *testing.T) {
	t.Parallel()

	const sessionID = "sess"
	const agentID = "a0123456789abcdef"
	name := AgentTranscriptFileName(agentID)

	t.Run("nested wins over legacy and workflow", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		nested := filepath.Join(SubagentsDir(dir, sessionID), name)
		writeWorkflowFixture(t, nested)
		writeWorkflowFixture(t, filepath.Join(dir, name))
		writeWorkflowFixture(t, filepath.Join(SubagentsDir(dir, sessionID), SubagentWorkflowsDirName, "wf_1", name))
		assert.Equal(t, nested, ResolveSubagentTranscriptPath(dir, sessionID, agentID))
	})

	t.Run("legacy wins over workflow", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		legacy := filepath.Join(dir, name)
		writeWorkflowFixture(t, legacy)
		writeWorkflowFixture(t, filepath.Join(SubagentsDir(dir, sessionID), SubagentWorkflowsDirName, "wf_1", name))
		assert.Equal(t, legacy, ResolveSubagentTranscriptPath(dir, sessionID, agentID))
	})

	t.Run("workflow run", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		want := filepath.Join(SubagentsDir(dir, sessionID), SubagentWorkflowsDirName, "wf_1", name)
		writeWorkflowFixture(t, want)
		assert.Equal(t, want, ResolveSubagentTranscriptPath(dir, sessionID, agentID))
	})

	t.Run("unsafe or empty agent ID never resolves", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeWorkflowFixture(t, filepath.Join(dir, "agent-.jsonl"))
		writeWorkflowFixture(t, filepath.Join(SubagentsDir(dir, sessionID), SubagentWorkflowsDirName, "wf_1", "agent-*.jsonl"))
		assert.Empty(t, ResolveSubagentTranscriptPath(dir, sessionID, ""))
		assert.Empty(t, ResolveSubagentTranscriptPath(dir, sessionID, "*"))
		assert.Empty(t, ResolveSubagentTranscriptPath(dir, sessionID, "../x"))
	})
}

func TestWorkflowRunAgentTranscripts(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	subagentsDir := SubagentsDir(base, "sess")
	workflows := filepath.Join(subagentsDir, SubagentWorkflowsDirName)
	a1 := filepath.Join(workflows, "wf_1", "agent-a1.jsonl")
	writeWorkflowFixture(t, a1)
	writeWorkflowFixture(t, filepath.Join(workflows, "wf_1", "journal.jsonl"))
	writeWorkflowFixture(t, filepath.Join(workflows, "wf_2", "agent-a2.jsonl"))

	assert.Equal(t, map[string]string{"a1": a1}, WorkflowRunAgentTranscripts(subagentsDir, "wf_1"), "only the named run")
	assert.Empty(t, WorkflowRunAgentTranscripts(subagentsDir, "wf_missing"))
	for _, unsafe := range []string{"", "..", "../wf_1", "wf_1/..", "*"} {
		assert.Empty(t, WorkflowRunAgentTranscripts(subagentsDir, unsafe), "run ID %q", unsafe)
	}

	outsideRun := filepath.Join(base, "outside-run")
	writeWorkflowFixture(t, filepath.Join(outsideRun, "agent-out.jsonl"))
	if err := os.Symlink(outsideRun, filepath.Join(workflows, "wf_linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	assert.Empty(t, WorkflowRunAgentTranscripts(subagentsDir, "wf_linked"), "a linked run directory is not read")
}
