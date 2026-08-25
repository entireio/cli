package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// writeTelemetrySettings puts a repo with a known telemetry opt-in state on
// disk and points settings resolution at it.
func writeTelemetrySettings(t *testing.T, telemetry string) {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	// InitializeSession resolves HEAD, so the repo needs a commit.
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, paths.EntireDir), 0o755))
	body := `{"enabled": true`
	if telemetry != "" {
		body += `, "telemetry": ` + telemetry
	}
	body += `}`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, paths.EntireDir, "settings.json"), []byte(body), 0o644))
	t.Chdir(tmpDir)
}

func telemetrySkillEvent(id string) agent.SkillEvent {
	return agent.SkillEvent{
		ID:        id,
		EventType: agent.SkillEventTypeToolInvocation,
		Skill:     agent.SkillEventSkill{Name: "entire:search"},
		Source: agent.SkillEventSource{
			Agent:  "claude-code",
			Signal: agent.SkillSignalClaudeSkillToolUse,
		},
	}
}

// captureSkillEmissions swaps the emitter seam and returns what each call
// forwarded, so tests can assert the gate and the call ordering without a
// PostHog client.
func captureSkillEmissions(t *testing.T) *[][]agent.SkillEvent {
	t.Helper()
	calls := [][]agent.SkillEvent{}
	prev := emitSkillTelemetry
	emitSkillTelemetry = func(events []agent.SkillEvent, _ bool, _ string) {
		calls = append(calls, events)
	}
	t.Cleanup(func() { emitSkillTelemetry = prev })
	return &calls
}

func TestEmitSkillInvocationTelemetry_RequiresOptIn(t *testing.T) {
	tests := []struct {
		name      string
		telemetry string
		wantCalls int
	}{
		{"opted in", "true", 1},
		{"opted out", "false", 0},
		// Telemetry is opt-in: an absent key must not send.
		{"unset", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeTelemetrySettings(t, tt.telemetry)
			calls := captureSkillEmissions(t)

			EmitSkillInvocationTelemetry(t.Context(), []agent.SkillEvent{telemetrySkillEvent("a")})

			if len(*calls) != tt.wantCalls {
				t.Errorf("emitted %d times, want %d", len(*calls), tt.wantCalls)
			}
		})
	}
}

func TestEmitSkillInvocationTelemetry_NoEventsNoLoad(t *testing.T) {
	writeTelemetrySettings(t, "true")
	calls := captureSkillEmissions(t)

	EmitSkillInvocationTelemetry(t.Context(), nil)

	if len(*calls) != 0 {
		t.Errorf("emitted %d times for zero events, want 0", len(*calls))
	}
}

// The emitter must run after MutateSessionState releases the session gate:
// settings I/O and a process spawn under the lock block concurrent hooks.
//
// The discriminator is durability, not visibility. From inside the emitter we
// mutate the session and immediately re-read it from disk. Called after the
// gate is released, that mutation is an OUTER one and saves, so the re-read
// sees it. Called from inside the closure it would be a NESTED one, which by
// contract defers its write to the outer frame's save — so the re-read would
// still show the old value. (Comparing in-memory state would prove nothing:
// the nested call shares the outer's state pointer.)
func TestEmitSkillInvocationTelemetry_RunsOutsideTheSessionGate(t *testing.T) {
	writeTelemetrySettings(t, "true")

	sessionID := "sess-gate-scope"
	require.NoError(t, (&ManualCommitStrategy{}).InitializeSession(
		t.Context(), sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))

	const emitterMark = "written-by-emitter"
	var emitterWritePersisted bool
	prev := emitSkillTelemetry
	emitSkillTelemetry = func(_ []agent.SkillEvent, _ bool, _ string) {
		mutErr := MutateSessionState(t.Context(), sessionID, func(state *SessionState) error {
			state.LastPrompt = emitterMark
			return nil
		})
		if mutErr != nil {
			t.Errorf("emitter mutation failed: %v", mutErr)
			return
		}
		reloaded, err := LoadSessionState(t.Context(), sessionID)
		emitterWritePersisted = err == nil && reloaded != nil && reloaded.LastPrompt == emitterMark
	}
	t.Cleanup(func() { emitSkillTelemetry = prev })

	var appended []agent.SkillEvent
	saved, err := MutateSessionStateSaved(t.Context(), sessionID, func(state *SessionState) error {
		appended = AppendNewSkillEvents(state, []agent.SkillEvent{telemetrySkillEvent("a")})
		return nil
	})
	require.NoError(t, err)
	require.True(t, saved)
	require.Len(t, appended, 1)

	EmitSkillInvocationTelemetry(t.Context(), appended)

	require.True(t, emitterWritePersisted,
		"the emitter's mutation did not persist, so it ran nested inside the session gate")
}

// The "emit only after a durable save" contract: a skipped mutation reports no
// save, so callers never announce events the next extraction pass will re-derive.
func TestMutateSessionStateSaved_ReportsSkipAsUnsaved(t *testing.T) {
	writeTelemetrySettings(t, "true")

	sessionID := "sess-skip-report"
	require.NoError(t, (&ManualCommitStrategy{}).InitializeSession(
		t.Context(), sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))

	saved, err := MutateSessionStateSaved(t.Context(), sessionID, func(state *SessionState) error {
		state.LastPrompt = "not-persisted"
		return ErrMutationSkip
	})
	require.NoError(t, err, "ErrMutationSkip is not an error")
	require.False(t, saved, "a skipped mutation must not report itself as saved")

	reloaded, err := LoadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotEqual(t, "not-persisted", reloaded.LastPrompt, "skip must not have written")

	saved, err = MutateSessionStateSaved(t.Context(), sessionID, func(state *SessionState) error {
		state.LastPrompt = "persisted"
		return nil
	})
	require.NoError(t, err)
	require.True(t, saved)
}
