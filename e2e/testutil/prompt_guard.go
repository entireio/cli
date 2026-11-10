package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// failIfPrompted fails t when any hook in the repo showed the commit-linking
// prompt, the first-push checkpoint push policy question, or the
// global-enable push consent question. No E2E commit or
// push has a person behind it, so a prompt means the hook misjudged an agent's
// shell as interactive, which is the bug users hit as a hung commit (#2725).
// Since the prompts give up after a timeout, the git command still succeeds
// and the test would otherwise pass slowly.
func failIfPrompted(t *testing.T, repoDir string) {
	t.Helper()
	logDir := filepath.Join(repoDir, ".entire", "logs")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return // no logs: nothing ran a hook
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(logDir, e.Name()))
		if err != nil {
			continue
		}
		for line := range strings.Lines(string(data)) {
			if strings.Contains(line, strategy.CommitLinkingPromptLogMsg) || strings.Contains(line, strategy.CheckpointPushQuestionLogMsg) ||
				strings.Contains(line, strategy.GlobalEnablePushConsentLogMsg) {
				t.Errorf("a git hook prompted an agent (%s): %s", e.Name(), strings.TrimSpace(line))
				return
			}
		}
	}
}
