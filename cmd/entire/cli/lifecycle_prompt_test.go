package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestAppendPrompt_RejectsSymlinks(t *testing.T) {
	for _, target := range []string{"session directory", "prompt file"} {
		t.Run(target, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			t.Chdir(repoDir)

			const sessionID = "symlinked-prompt"
			sessionDir := filepath.Join(repoDir, paths.SessionMetadataDirFromSessionID(sessionID))
			outside := t.TempDir()
			outsidePrompt := filepath.Join(outside, paths.PromptFileName)
			require.NoError(t, os.WriteFile(outsidePrompt, []byte("original"), 0o600))

			link, destination := sessionDir, outside
			if target == "prompt file" {
				link = filepath.Join(sessionDir, paths.PromptFileName)
				destination = outsidePrompt
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
			if err := os.Symlink(destination, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			require.Error(t, appendPromptToFile(context.Background(), sessionID, "new prompt"))
			appended, err := appendPromptToFileIfLastDiffers(context.Background(), sessionID, "late prompt")
			require.Error(t, err)
			require.False(t, appended)
			content, err := os.ReadFile(outsidePrompt)
			require.NoError(t, err)
			require.Equal(t, "original", string(content))
		})
	}
}
