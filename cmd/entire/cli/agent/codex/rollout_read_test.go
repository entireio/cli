package codex

import (
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolloutReadRejectsOutsideRootAndSymlinkedParent(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	path := writeRollout(t, outside, "child.jsonl", "child", nil)
	ag := &CodexAgent{RolloutRoots: []string{root}}
	_, ok := ag.loadDirectRollout(t.Context(), agent.SubagentReference{AgentID: "child", DeclaredTranscriptPath: path})
	require.False(t, ok)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	_, ok = ag.loadDirectRollout(t.Context(), agent.SubagentReference{AgentID: "child", DeclaredTranscriptPath: filepath.Join(root, "escape", "child.jsonl")})
	require.False(t, ok)
}

func TestRolloutClassificationBoundsFirstRecord(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "huge.jsonl")
	data := `{"type":"session_meta","payload":{"thread_source":"user","padding":"` + strings.Repeat("x", int(rolloutMetadataByteLimit)) + `"}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	result := classifyRolloutDetailed(path, []string{root})
	require.Equal(t, rolloutUnknown, result.Classification)
	require.Equal(t, rolloutIssueUnreadable, result.Issue)
}
