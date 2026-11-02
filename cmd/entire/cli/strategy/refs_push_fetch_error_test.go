package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFetchAndRebaseRefCommon_CancelledContextNamesCause is the end-to-end half
// of remote.TestErrWithGitOutput_KeepsCauseWhenGitIsSilent: an interrupted flush
// must not log one contentless "fetch failed: " per queued ref.
func TestFetchAndRebaseRefCommon_CancelledContextNamesCause(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir) // CWD-based repository resolution; cannot run in parallel.

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := fetchAndRebaseRefCommon(ctx, bareDir, refs[0])
	require.Error(t, err)
	assert.NotEqual(t, "fetch failed: ", err.Error(), "a cancelled fetch must still name its cause")
	assert.NotEmpty(t, strings.TrimSpace(strings.TrimPrefix(err.Error(), "fetch failed:")))
}
