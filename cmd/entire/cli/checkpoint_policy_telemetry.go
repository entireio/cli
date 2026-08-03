package cli

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// emitCheckpointPolicyBlocked reports a checkpoint_policy_blocked telemetry
// event when telemetry is opted in (settings.Telemetry == true). Best-effort
// and non-blocking; failures to load settings simply suppress the event.
func emitCheckpointPolicyBlocked(ctx context.Context, event telemetry.CheckpointPolicyBlockedEvent) {
	s, err := LoadEntireSettings(ctx)
	if err != nil || s.Telemetry == nil || !*s.Telemetry {
		return
	}
	telemetry.TrackCheckpointPolicyBlocked(event, versioninfo.Version)
}
