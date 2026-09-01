package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/stretchr/testify/require"
)

// A missing .entire must stay classifiable with os.IsNotExist, not just
// errors.Is. opencode's PrepareTranscript and pi's GetTranscriptPosition both
// branch on os.IsNotExist, and it does not unwrap a %w — so wrapping the
// absent-directory error told them a fresh clone was a hard failure.
func TestTranscriptFile_MissingEntireDirStaysNotExist(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	entiredir.Reset()
	t.Cleanup(entiredir.Reset)

	under := filepath.Join(tmp, ".entire", "tmp", "opencode", "sess.jsonl")
	require.NoFileExists(t, filepath.Join(tmp, ".entire"))

	_, readErr := agent.ReadTranscriptFile(under)
	require.Error(t, readErr)
	require.True(t, os.IsNotExist(readErr),
		"ReadTranscriptFile must stay os.IsNotExist-classifiable, got %v", readErr)

	_, statErr := agent.StatTranscriptFile(under)
	require.Error(t, statErr)
	require.True(t, os.IsNotExist(statErr),
		"StatTranscriptFile must stay os.IsNotExist-classifiable, got %v", statErr)
}
