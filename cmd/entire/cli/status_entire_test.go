package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Under the Entire tier every push delivers checkpoints, so the counter must
// not tell the user to push the Entire remote specifically.
func TestFormatUnpushedCheckpointsLine_EntireTier(t *testing.T) {
	t.Parallel()

	got := formatUnpushedCheckpointsLine(checkpointSyncInfo{Remote: "entire", Source: string(strategy.SyncRemoteSourceEntire), Unpushed: 2})
	assert.Equal(t, "2 checkpoints not yet on entire — they sync with your next git push, to any remote", got)
	assert.NotContains(t, got, "'git push entire'")

	got = formatUnpushedCheckpointsLine(checkpointSyncInfo{Remote: "origin", Source: string(strategy.SyncRemoteSourceDefault), Unpushed: 1})
	assert.Equal(t, "1 checkpoint not yet on origin — it syncs with your next 'git push origin'", got)
}
