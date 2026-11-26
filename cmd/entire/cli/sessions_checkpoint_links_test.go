package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestBuildSessionInfoJSON_EntireSessionIDAndCheckpointCount(t *testing.T) {
	t.Parallel()
	state := &strategy.SessionState{SessionID: "s1", AgentType: "Unregistered", CheckpointLinks: []session.CheckpointLink{{CheckpointID: "000000000001"}, {CheckpointID: "000000000002"}}}
	info := buildSessionInfoJSON(state, "idle")
	if info.EntireSessionID != "Unregistered:s1" || info.CheckpointCount != 2 {
		t.Errorf("got %q, %d; want Unregistered:s1, 2", info.EntireSessionID, info.CheckpointCount)
	}
}
