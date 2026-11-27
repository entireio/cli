package cli

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestBuildSessionInfoJSON_EntireSessionIDAndCheckpointCount(t *testing.T) {
	t.Parallel()
	state := &strategy.SessionState{SessionID: "s1", AgentType: "Unregistered", CheckpointLinks: []session.CheckpointLink{{CheckpointID: "000000000001"}, {CheckpointID: "000000000002"}}}
	info := buildSessionInfoJSON(state, "idle")
	if info.EntireSessionID != "Unregistered:s1" || info.SessionCheckpointCount != 2 {
		t.Errorf("got %q, %d; want Unregistered:s1, 2", info.EntireSessionID, info.SessionCheckpointCount)
	}
}

// The session's recorded checkpoints and its pending turns (and their
// deprecated "checkpoints" alias) are distinct keys with distinct values.
func TestBuildSessionInfoJSON_CheckpointKeysAreUnambiguous(t *testing.T) {
	t.Parallel()
	state := &strategy.SessionState{SessionID: "s1", AgentType: "Unregistered", StepCount: 0, CheckpointLinks: []session.CheckpointLink{
		{CheckpointID: "000000000001"}, {CheckpointID: "000000000002"}, {CheckpointID: "000000000003"},
	}}
	out, err := json.Marshal(buildSessionInfoJSON(state, "idle"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["session_checkpoint_count"] != float64(3) {
		t.Errorf("session_checkpoint_count = %v, want 3", raw["session_checkpoint_count"])
	}
	if raw["checkpoints"] != float64(0) || raw["pending_turns"] != float64(0) {
		t.Errorf("checkpoints/pending_turns = %v/%v, want 0/0", raw["checkpoints"], raw["pending_turns"])
	}
	if _, ok := raw["checkpoint_count"]; ok {
		t.Errorf("ambiguous checkpoint_count key present: %s", out)
	}
}
