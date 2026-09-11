package strategy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	require.NoError(t, MutateSessionStateOnSaved(t.Context(), sessionID,
		func(state *SessionState) error {
			appended = AppendNewSkillEvents(state, []agent.SkillEvent{telemetrySkillEvent("a")})
			return nil
		},
		func() { EmitSkillInvocationTelemetry(t.Context(), appended) },
	))
	require.Len(t, appended, 1)

	require.True(t, emitterWritePersisted,
		"the emitter's mutation did not persist, so it ran nested inside the session gate")
}

// The "emit only after a durable save" contract: a skipped mutation runs no
// post-save effect, so callers never announce events the next extraction pass
// will re-derive.
// nestedPrompt is the marker a nested frame writes, so a test can tell whether
// the outer frame flushed the nested mutation to disk.
const nestedPrompt = "written-by-nested"

func TestMutateSessionStateOnSaved_SkipRunsNoEffect(t *testing.T) {
	writeTelemetrySettings(t, "true")

	sessionID := "sess-skip-report"
	require.NoError(t, (&ManualCommitStrategy{}).InitializeSession(
		t.Context(), sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))

	effects := 0
	err := MutateSessionStateOnSaved(t.Context(), sessionID, func(state *SessionState) error {
		state.LastPrompt = "not-persisted"
		return ErrMutationSkip
	}, func() { effects++ })
	require.NoError(t, err, "ErrMutationSkip is not an error")
	require.Zero(t, effects, "a skipped mutation must not run its post-save effect")

	reloaded, err := LoadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotEqual(t, "not-persisted", reloaded.LastPrompt, "skip must not have written")

	require.NoError(t, MutateSessionStateOnSaved(t.Context(), sessionID, func(state *SessionState) error {
		state.LastPrompt = "persisted"
		return nil
	}, func() { effects++ }))
	require.Equal(t, 1, effects, "a saved mutation must run its post-save effect exactly once")
}

// A nested frame does not save, so it cannot know whether its mutation lands.
// When the outer frame skips, the nested frame's effect must not run: for skill
// telemetry the ledger entry never landed, so the next extraction pass
// re-derives the event — and an announcement made here would duplicate it.
func TestMutateSessionStateOnSaved_NestedEffectHeldForTheOuterFrame(t *testing.T) {
	writeTelemetrySettings(t, "true")

	tests := []struct {
		name        string
		outerResult error
		wantEffects []string
	}{
		{"outer saves", nil, []string{"nested", "outer"}},
		{"outer skips", ErrMutationSkip, nil},
		{"outer fails", errors.New("boom"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := "sess-nested-" + strings.ReplaceAll(tt.name, " ", "-")
			require.NoError(t, (&ManualCommitStrategy{}).InitializeSession(
				t.Context(), sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))

			var effects []string
			err := MutateSessionStateOnSaved(t.Context(), sessionID, func(_ *SessionState) error {
				nestedErr := MutateSessionStateOnSaved(t.Context(), sessionID, func(nested *SessionState) error {
					nested.LastPrompt = nestedPrompt
					return nil
				}, func() { effects = append(effects, "nested") })
				require.NoError(t, nestedErr)
				require.Empty(t, effects, "the nested effect must not run while the outer frame is still open")
				return tt.outerResult
			}, func() { effects = append(effects, "outer") })

			if tt.outerResult == nil || errors.Is(tt.outerResult, ErrMutationSkip) {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.outerResult)
			}
			require.Equal(t, tt.wantEffects, effects)

			reloaded, loadErr := LoadSessionState(t.Context(), sessionID)
			require.NoError(t, loadErr)
			if tt.outerResult == nil {
				require.Equal(t, nestedPrompt, reloaded.LastPrompt,
					"the outer save must flush the nested mutation")
			} else {
				require.NotEqual(t, nestedPrompt, reloaded.LastPrompt,
					"no save happened, so the nested mutation must not be on disk")
			}
		})
	}
}

// A panicking frame never saved, so it must announce nothing — and must leave
// the gate usable, since the release happens before the (discarded) effects.
func TestMutateSessionStateOnSaved_PanicDiscardsQueuedEffects(t *testing.T) {
	writeTelemetrySettings(t, "true")

	sessionID := "sess-panic-discard"
	require.NoError(t, (&ManualCommitStrategy{}).InitializeSession(
		t.Context(), sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))

	effects := 0
	func() {
		defer func() {
			require.NotNil(t, recover(), "the panic must propagate out of the mutation")
		}()
		//nolint:errcheck // the closure panics, so this call never returns a value.
		MutateSessionStateOnSaved(t.Context(), sessionID, func(_ *SessionState) error {
			require.NoError(t, MutateSessionStateOnSaved(t.Context(), sessionID,
				func(nested *SessionState) error {
					nested.LastPrompt = nestedPrompt
					return nil
				},
				func() { effects++ },
			))
			panic("boom")
		}, func() { effects++ })
	}()
	require.Zero(t, effects, "a panicking frame must not announce anything")

	reloaded, err := LoadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotEqual(t, nestedPrompt, reloaded.LastPrompt,
		"the panic aborted the save, so nothing may be on disk")

	// The gate must be free (release runs before the effects would have) and
	// the panicking frame's queue must not survive into this one.
	require.NoError(t, MutateSessionStateOnSaved(t.Context(), sessionID, func(state *SessionState) error {
		state.StepCount++
		return nil
	}, nil))
	require.Zero(t, effects, "a later frame ran an effect the panicking frame queued")
}

// The queue is per-frame: effects owed to one outer frame must not fire again
// for the next one on the same session.
func TestMutateSessionStateOnSaved_NestedEffectDoesNotLeakToTheNextFrame(t *testing.T) {
	writeTelemetrySettings(t, "true")

	sessionID := "sess-nested-no-leak"
	require.NoError(t, (&ManualCommitStrategy{}).InitializeSession(
		t.Context(), sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))

	nestedRuns := 0
	require.NoError(t, MutateSessionStateOnSaved(t.Context(), sessionID, func(_ *SessionState) error {
		return MutateSessionStateOnSaved(t.Context(), sessionID, func(nested *SessionState) error {
			nested.StepCount++
			return nil
		}, func() { nestedRuns++ })
	}, nil))
	require.Equal(t, 1, nestedRuns)

	require.NoError(t, MutateSessionStateOnSaved(t.Context(), sessionID, func(state *SessionState) error {
		state.StepCount++
		return nil
	}, nil))
	require.Equal(t, 1, nestedRuns, "a later frame re-ran an effect it did not own")
}

// A panicking effect must not take the effects queued behind it down with it,
// and must not escape into callers that do not recover — PostCommit would die
// mid-commit over best-effort telemetry. The panic in the MUTATION function is
// already covered above; this is the same discipline applied to the effects,
// which is where it was missing.
func TestMutateSessionStateOnSaved_PanickingEffectDoesNotSkipTheRest(t *testing.T) {
	writeTelemetrySettings(t, "true")

	sessionID := "sess-effect-panic"
	require.NoError(t, (&ManualCommitStrategy{}).InitializeSession(
		t.Context(), sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))

	var effects []string
	err := MutateSessionStateOnSaved(t.Context(), sessionID, func(_ *SessionState) error {
		nestedErr := MutateSessionStateOnSaved(t.Context(), sessionID, func(nested *SessionState) error {
			nested.LastPrompt = nestedPrompt
			return nil
		}, func() {
			effects = append(effects, "nested")
			panic("effect boom")
		})
		require.NoError(t, nestedErr)
		return nil
	}, func() { effects = append(effects, "outer") })

	require.NoError(t, err, "an effect's panic must not surface as the mutation's error")
	require.Equal(t, []string{"nested", "outer"}, effects,
		"the outer effect must still run after the nested one panicked")

	// The save itself happened before any effect ran, so the panic must not
	// have cost the mutation.
	reloaded, loadErr := LoadSessionState(t.Context(), sessionID)
	require.NoError(t, loadErr)
	require.Equal(t, nestedPrompt, reloaded.LastPrompt)

	// The gate must still be usable: release() runs before the effects.
	require.NoError(t, MutateSessionStateOnSaved(t.Context(), sessionID, func(state *SessionState) error {
		state.LastPrompt = "after-panic"
		return nil
	}, func() {}))
}
