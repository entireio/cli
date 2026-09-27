//go:build !windows

package codex

import (
	"github.com/stretchr/testify/require"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRolloutClassificationRejectsFIFO(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "pipe.jsonl")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	require.Equal(t, rolloutUnknown, classifyRolloutDetailed(path, []string{root}).Classification)
}
