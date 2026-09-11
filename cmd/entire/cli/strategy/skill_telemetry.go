package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// EmitSkillInvocationTelemetry forwards newly recorded skill events to
// telemetry. Gated on the same opt-in telemetry setting as command tracking;
// skill names are additionally allowlisted inside the telemetry package, so
// custom names never leave the machine verbatim. Skill names and signal kinds
// only, never prompt text or transcript content.
//
// Hand this to MutateSessionStateOnSaved as the post-save effect rather than
// calling it inside the mutation closure. That helper runs the effect only once
// the state was durably written, and only after the session gate is released:
// the settings load (disk I/O) and detached-process spawn must not extend the
// gate hold time and block concurrent hooks, and emitting only after a save
// keeps reporting exactly-once — events whose append was never persisted are
// re-derived, and re-announced, by the next extraction pass.
func EmitSkillInvocationTelemetry(ctx context.Context, events []agent.SkillEvent) {
	if len(events) == 0 {
		return
	}
	s, err := settings.Load(ctx)
	if err != nil || !s.IsTelemetryEnabled() {
		return
	}
	emitSkillTelemetry(events, s.Enabled, versioninfo.Version)
}

// emitSkillTelemetry is the send step, separated from the gating above so tests
// can assert what the gate lets through — and that emission happens outside the
// session gate — without a PostHog client.
//
//nolint:gochecknoglobals // test seam, set and restored by in-package tests.
var emitSkillTelemetry = trackSkillInvocations

func trackSkillInvocations(events []agent.SkillEvent, isEntireEnabled bool, version string) {
	invocations := make([]telemetry.SkillInvocation, 0, len(events))
	for _, ev := range events {
		invocations = append(invocations, telemetry.SkillInvocation{
			Skill:     ev.Skill.Name,
			Agent:     ev.Source.Agent,
			Signal:    ev.Source.Signal,
			EventType: ev.EventType,
		})
	}
	telemetry.TrackSkillInvocationsDetached(invocations, isEntireEnabled, version)
}
