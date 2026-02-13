package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhaseFromString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  Phase
	}{
		{name: "active", input: "active", want: PhaseActive},
		{name: "active_committed_maps_to_active", input: "active_committed", want: PhaseActive},
		{name: "idle", input: "idle", want: PhaseIdle},
		{name: "ended", input: "ended", want: PhaseEnded},
		{name: "empty_string_defaults_to_idle", input: "", want: PhaseIdle},
		{name: "unknown_string_defaults_to_idle", input: "bogus", want: PhaseIdle},
		{name: "uppercase_treated_as_unknown", input: "ACTIVE", want: PhaseIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := PhaseFromString(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPhase_IsActive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		phase Phase
		want  bool
	}{
		{name: "active_is_active", phase: PhaseActive, want: true},
		{name: "idle_is_not_active", phase: PhaseIdle, want: false},
		{name: "ended_is_not_active", phase: PhaseEnded, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.phase.IsActive())
		})
	}
}

func TestEvent_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		event Event
		want  string
	}{
		{EventTurnStart, "TurnStart"},
		{EventTurnEnd, "TurnEnd"},
		{EventGitCommit, "GitCommit"},
		{EventSessionStart, "SessionStart"},
		{EventSessionStop, "SessionStop"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.event.String())
		})
	}
}

func TestAction_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		action Action
		want   string
	}{
		{ActionCondense, "Condense"},
		{ActionCondenseIfFilesTouched, "CondenseIfFilesTouched"},
		{ActionDiscardIfNoFiles, "DiscardIfNoFiles"},
		{ActionMigrateShadowBranch, "MigrateShadowBranch"},
		{ActionWarnStaleSession, "WarnStaleSession"},
		{ActionClearEndedAt, "ClearEndedAt"},
		{ActionUpdateLastInteraction, "UpdateLastInteraction"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.action.String())
		})
	}
}

// transitionCase is a single row in the transition table test.
type transitionCase struct {
	name        string
	current     Phase
	event       Event
	ctx         TransitionContext
	wantPhase   Phase
	wantActions []Action
}

// runTransitionTests runs a slice of transition cases as parallel subtests.
func runTransitionTests(t *testing.T, tests []transitionCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := Transition(tt.current, tt.event, tt.ctx)
			assert.Equal(t, tt.wantPhase, result.NewPhase, "unexpected NewPhase")
			assert.Equal(t, tt.wantActions, result.Actions, "unexpected Actions")
		})
	}
}

func TestTransitionFromIdle(t *testing.T) {
	t.Parallel()
	runTransitionTests(t, []transitionCase{
		{
			name:        "TurnStart_transitions_to_ACTIVE",
			current:     PhaseIdle,
			event:       EventTurnStart,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "GitCommit_triggers_condense",
			current:     PhaseIdle,
			event:       EventGitCommit,
			wantPhase:   PhaseIdle,
			wantActions: []Action{ActionCondense, ActionUpdateLastInteraction},
		},
		{
			name:        "GitCommit_rebase_skips_everything",
			current:     PhaseIdle,
			event:       EventGitCommit,
			ctx:         TransitionContext{IsRebaseInProgress: true},
			wantPhase:   PhaseIdle,
			wantActions: nil,
		},
		{
			name:        "SessionStop_transitions_to_ENDED",
			current:     PhaseIdle,
			event:       EventSessionStop,
			wantPhase:   PhaseEnded,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "SessionStart_is_noop",
			current:     PhaseIdle,
			event:       EventSessionStart,
			wantPhase:   PhaseIdle,
			wantActions: nil,
		},
		{
			name:        "TurnEnd_is_noop",
			current:     PhaseIdle,
			event:       EventTurnEnd,
			wantPhase:   PhaseIdle,
			wantActions: nil,
		},
	})
}

func TestTransitionFromActive(t *testing.T) {
	t.Parallel()
	runTransitionTests(t, []transitionCase{
		{
			name:        "TurnStart_stays_ACTIVE",
			current:     PhaseActive,
			event:       EventTurnStart,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "TurnEnd_transitions_to_IDLE",
			current:     PhaseActive,
			event:       EventTurnEnd,
			wantPhase:   PhaseIdle,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "GitCommit_condenses_immediately",
			current:     PhaseActive,
			event:       EventGitCommit,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionCondense, ActionMigrateShadowBranch, ActionUpdateLastInteraction},
		},
		{
			name:        "GitCommit_rebase_skips_everything",
			current:     PhaseActive,
			event:       EventGitCommit,
			ctx:         TransitionContext{IsRebaseInProgress: true},
			wantPhase:   PhaseActive,
			wantActions: nil,
		},
		{
			name:        "SessionStop_transitions_to_ENDED",
			current:     PhaseActive,
			event:       EventSessionStop,
			wantPhase:   PhaseEnded,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "SessionStart_warns_stale_session",
			current:     PhaseActive,
			event:       EventSessionStart,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionWarnStaleSession},
		},
	})
}

func TestPhaseFromString_ActiveCommittedMigration(t *testing.T) {
	t.Parallel()
	got := PhaseFromString("active_committed")
	if got != PhaseActive {
		t.Errorf("PhaseFromString(\"active_committed\") = %q, want %q", got, PhaseActive)
	}
}

func TestTransition_ActiveCommittedBackwardCompat(t *testing.T) {
	t.Parallel()

	// State files may still contain "active_committed" from before this phase
	// was removed. Transition normalizes it to PhaseActive via PhaseFromString.
	runTransitionTests(t, []transitionCase{
		{
			name:        "active_committed_TurnEnd_treated_as_ACTIVE",
			current:     Phase("active_committed"),
			event:       EventTurnEnd,
			wantPhase:   PhaseIdle,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "active_committed_GitCommit_treated_as_ACTIVE",
			current:     Phase("active_committed"),
			event:       EventGitCommit,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionCondense, ActionMigrateShadowBranch, ActionUpdateLastInteraction},
		},
	})
}

func TestTransitionFromEnded(t *testing.T) {
	t.Parallel()
	runTransitionTests(t, []transitionCase{
		{
			name:        "TurnStart_transitions_to_ACTIVE",
			current:     PhaseEnded,
			event:       EventTurnStart,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionClearEndedAt, ActionUpdateLastInteraction},
		},
		{
			name:        "GitCommit_with_files_condenses",
			current:     PhaseEnded,
			event:       EventGitCommit,
			ctx:         TransitionContext{HasFilesTouched: true},
			wantPhase:   PhaseEnded,
			wantActions: []Action{ActionCondenseIfFilesTouched, ActionUpdateLastInteraction},
		},
		{
			name:        "GitCommit_without_files_discards",
			current:     PhaseEnded,
			event:       EventGitCommit,
			wantPhase:   PhaseEnded,
			wantActions: []Action{ActionDiscardIfNoFiles, ActionUpdateLastInteraction},
		},
		{
			name:        "GitCommit_rebase_skips_everything",
			current:     PhaseEnded,
			event:       EventGitCommit,
			ctx:         TransitionContext{IsRebaseInProgress: true},
			wantPhase:   PhaseEnded,
			wantActions: nil,
		},
		{
			name:        "SessionStart_transitions_to_IDLE",
			current:     PhaseEnded,
			event:       EventSessionStart,
			wantPhase:   PhaseIdle,
			wantActions: []Action{ActionClearEndedAt},
		},
		{
			name:        "TurnEnd_is_noop",
			current:     PhaseEnded,
			event:       EventTurnEnd,
			wantPhase:   PhaseEnded,
			wantActions: nil,
		},
		{
			name:        "SessionStop_is_noop",
			current:     PhaseEnded,
			event:       EventSessionStop,
			wantPhase:   PhaseEnded,
			wantActions: nil,
		},
	})
}

func TestTransitionBackwardCompat(t *testing.T) {
	t.Parallel()
	runTransitionTests(t, []transitionCase{
		{
			name:        "empty_phase_TurnStart_treated_as_IDLE",
			current:     Phase(""),
			event:       EventTurnStart,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "empty_phase_GitCommit_treated_as_IDLE",
			current:     Phase(""),
			event:       EventGitCommit,
			wantPhase:   PhaseIdle,
			wantActions: []Action{ActionCondense, ActionUpdateLastInteraction},
		},
		{
			name:        "empty_phase_SessionStop_treated_as_IDLE",
			current:     Phase(""),
			event:       EventSessionStop,
			wantPhase:   PhaseEnded,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
		{
			name:        "empty_phase_SessionStart_treated_as_IDLE",
			current:     Phase(""),
			event:       EventSessionStart,
			wantPhase:   PhaseIdle,
			wantActions: nil,
		},
		{
			name:        "empty_phase_TurnEnd_treated_as_IDLE",
			current:     Phase(""),
			event:       EventTurnEnd,
			wantPhase:   PhaseIdle,
			wantActions: nil,
		},
		{
			name:        "unknown_phase_TurnStart_treated_as_IDLE",
			current:     Phase("bogus"),
			event:       EventTurnStart,
			wantPhase:   PhaseActive,
			wantActions: []Action{ActionUpdateLastInteraction},
		},
	})
}

func TestTransition_rebase_GitCommit_is_noop_for_all_phases(t *testing.T) {
	t.Parallel()

	rebaseCtx := TransitionContext{IsRebaseInProgress: true}

	for _, phase := range allPhases {
		result := Transition(phase, EventGitCommit, rebaseCtx)
		assert.Empty(t, result.Actions,
			"rebase should produce empty actions for phase %s", phase)
		assert.Equal(t, phase, result.NewPhase,
			"rebase should not change phase for %s", phase)
	}
}

func TestTransition_all_phase_event_combinations_are_defined(t *testing.T) {
	t.Parallel()

	// Verify that calling Transition for every (phase, event) combination
	// does not panic and returns a valid phase.
	for _, phase := range allPhases {
		for _, event := range allEvents {
			result := Transition(phase, event, TransitionContext{})
			assert.NotEmpty(t, string(result.NewPhase),
				"Transition(%s, %s) returned empty phase", phase, event)

			// Verify the resulting phase is a known phase.
			normalized := PhaseFromString(string(result.NewPhase))
			assert.Equal(t, result.NewPhase, normalized,
				"Transition(%s, %s) returned non-canonical phase %q",
				phase, event, result.NewPhase)
		}
	}
}

func TestApplyCommonActions_SetsPhase(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseIdle}
	result := TransitionResult{
		NewPhase: PhaseActive,
		Actions:  []Action{ActionUpdateLastInteraction},
	}

	remaining := ApplyCommonActions(state, result)

	assert.Equal(t, PhaseActive, state.Phase)
	assert.Empty(t, remaining, "UpdateLastInteraction should be consumed")
}

func TestApplyCommonActions_UpdatesLastInteractionTime(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseIdle}
	before := time.Now()

	result := TransitionResult{
		NewPhase: PhaseActive,
		Actions:  []Action{ActionUpdateLastInteraction},
	}

	_ = ApplyCommonActions(state, result)

	require.NotNil(t, state.LastInteractionTime)
	assert.False(t, state.LastInteractionTime.Before(before),
		"LastInteractionTime should be >= test start time")
}

func TestApplyCommonActions_ClearsEndedAt(t *testing.T) {
	t.Parallel()

	endedAt := time.Now().Add(-time.Hour)
	state := &State{
		Phase:   PhaseEnded,
		EndedAt: &endedAt,
	}

	result := TransitionResult{
		NewPhase: PhaseIdle,
		Actions:  []Action{ActionClearEndedAt},
	}

	remaining := ApplyCommonActions(state, result)

	assert.Nil(t, state.EndedAt, "EndedAt should be cleared")
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Empty(t, remaining, "ClearEndedAt should be consumed")
}

func TestApplyCommonActions_PassesThroughStrategyActions(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseActive}
	result := TransitionResult{
		NewPhase: PhaseIdle,
		Actions:  []Action{ActionCondense, ActionUpdateLastInteraction},
	}

	remaining := ApplyCommonActions(state, result)

	assert.Equal(t, []Action{ActionCondense}, remaining,
		"ActionCondense should be passed through to caller")
	assert.Equal(t, PhaseIdle, state.Phase)
	require.NotNil(t, state.LastInteractionTime)
}

func TestApplyCommonActions_MultipleStrategyActions(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseActive}
	result := TransitionResult{
		NewPhase: PhaseActive,
		Actions:  []Action{ActionCondense, ActionMigrateShadowBranch, ActionUpdateLastInteraction},
	}

	remaining := ApplyCommonActions(state, result)

	assert.Equal(t, []Action{ActionCondense, ActionMigrateShadowBranch}, remaining)
	assert.Equal(t, PhaseActive, state.Phase)
}

func TestApplyCommonActions_WarnStaleSessionPassedThrough(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseActive}
	result := TransitionResult{
		NewPhase: PhaseActive,
		Actions:  []Action{ActionWarnStaleSession},
	}

	remaining := ApplyCommonActions(state, result)

	assert.Equal(t, []Action{ActionWarnStaleSession}, remaining)
}

func TestApplyCommonActions_NoActions(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseIdle}
	result := TransitionResult{
		NewPhase: PhaseIdle,
		Actions:  nil,
	}

	remaining := ApplyCommonActions(state, result)

	assert.Nil(t, remaining)
	assert.Equal(t, PhaseIdle, state.Phase)
}

func TestApplyCommonActions_EndedToActiveTransition(t *testing.T) {
	t.Parallel()

	endedAt := time.Now().Add(-time.Hour)
	state := &State{
		Phase:   PhaseEnded,
		EndedAt: &endedAt,
	}

	// Simulate ENDED → ACTIVE transition (EventTurnStart)
	result := Transition(PhaseEnded, EventTurnStart, TransitionContext{})
	remaining := ApplyCommonActions(state, result)

	assert.Equal(t, PhaseActive, state.Phase)
	assert.Nil(t, state.EndedAt, "EndedAt should be cleared on re-entry")
	require.NotNil(t, state.LastInteractionTime)
	assert.Empty(t, remaining, "all actions should be consumed")
}

func TestApplyCommonActions_EndedToIdleOnSessionStart(t *testing.T) {
	t.Parallel()

	endedAt := time.Now().Add(-time.Hour)
	state := &State{
		Phase:   PhaseEnded,
		EndedAt: &endedAt,
	}

	// Simulate ENDED → IDLE transition (EventSessionStart re-entry)
	result := Transition(PhaseEnded, EventSessionStart, TransitionContext{})
	remaining := ApplyCommonActions(state, result)

	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Nil(t, state.EndedAt, "EndedAt should be cleared on session re-entry")
	assert.Empty(t, remaining, "only ClearEndedAt, which is consumed")
}

func TestMermaidDiagram(t *testing.T) {
	t.Parallel()

	diagram := MermaidDiagram()

	// Verify the diagram contains expected state declarations.
	assert.Contains(t, diagram, "stateDiagram-v2")
	assert.Contains(t, diagram, "IDLE")
	assert.Contains(t, diagram, "ACTIVE")
	assert.Contains(t, diagram, "ENDED")
	assert.NotContains(t, diagram, "ACTIVE_COMMITTED")

	// Verify key transitions are present.
	assert.Contains(t, diagram, "idle --> active")
	assert.Contains(t, diagram, "active --> active")   // GitCommit stays ACTIVE now
	assert.Contains(t, diagram, "active --> idle")      // TurnEnd
	assert.Contains(t, diagram, "ended --> idle")
	assert.Contains(t, diagram, "ended --> active")

	// Verify actions appear in labels.
	assert.Contains(t, diagram, "Condense")
	assert.Contains(t, diagram, "MigrateShadowBranch")
	assert.Contains(t, diagram, "ClearEndedAt")
	assert.Contains(t, diagram, "WarnStaleSession")
}
