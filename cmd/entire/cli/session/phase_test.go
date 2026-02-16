package session

import (
	"errors"
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
		{name: "active_committed", input: "active_committed", want: PhaseActive},
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
			wantActions: []Action{ActionCondense, ActionUpdateLastInteraction},
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
	assert.Contains(t, diagram, "active --> active") // ACTIVE+GitCommit stays ACTIVE
	assert.Contains(t, diagram, "ended --> idle")
	assert.Contains(t, diagram, "ended --> active")

	// Verify actions appear in labels.
	assert.Contains(t, diagram, "Condense")
	assert.Contains(t, diagram, "ClearEndedAt")
	assert.Contains(t, diagram, "WarnStaleSession")
	assert.NotContains(t, diagram, "MigrateShadowBranch")
}

// mockActionHandler records which handler methods were called.
type mockActionHandler struct {
	condenseCalled               bool
	condenseIfFilesTouchedCalled bool
	discardIfNoFilesCalled       bool
	warnStaleSessionCalled       bool
	returnErr                    error
}

func (m *mockActionHandler) HandleCondense(_ *State) error {
	m.condenseCalled = true
	return m.returnErr
}

func (m *mockActionHandler) HandleCondenseIfFilesTouched(_ *State) error {
	m.condenseIfFilesTouchedCalled = true
	return m.returnErr
}

func (m *mockActionHandler) HandleDiscardIfNoFiles(_ *State) error {
	m.discardIfNoFilesCalled = true
	return m.returnErr
}

func (m *mockActionHandler) HandleWarnStaleSession(_ *State) error {
	m.warnStaleSessionCalled = true
	return m.returnErr
}

func TestApplyTransition_SetsPhaseAndHandlesCommonActions(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseIdle}
	handler := &mockActionHandler{}
	result := TransitionResult{
		NewPhase: PhaseActive,
		Actions:  []Action{ActionUpdateLastInteraction},
	}

	err := ApplyTransition(state, result, handler)

	require.NoError(t, err)
	assert.Equal(t, PhaseActive, state.Phase)
	require.NotNil(t, state.LastInteractionTime)
	assert.False(t, handler.condenseCalled)
}

func TestApplyTransition_CallsHandlerForCondense(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseActive}
	handler := &mockActionHandler{}
	result := TransitionResult{
		NewPhase: PhaseIdle,
		Actions:  []Action{ActionCondense, ActionUpdateLastInteraction},
	}

	err := ApplyTransition(state, result, handler)

	require.NoError(t, err)
	assert.True(t, handler.condenseCalled)
	assert.Equal(t, PhaseIdle, state.Phase)
	require.NotNil(t, state.LastInteractionTime)
}

func TestApplyTransition_CallsHandlerForCondenseIfFilesTouched(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseEnded}
	handler := &mockActionHandler{}
	result := TransitionResult{
		NewPhase: PhaseEnded,
		Actions:  []Action{ActionCondenseIfFilesTouched, ActionUpdateLastInteraction},
	}

	err := ApplyTransition(state, result, handler)

	require.NoError(t, err)
	assert.True(t, handler.condenseIfFilesTouchedCalled)
}

func TestApplyTransition_CallsHandlerForDiscardIfNoFiles(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseEnded}
	handler := &mockActionHandler{}
	result := TransitionResult{
		NewPhase: PhaseEnded,
		Actions:  []Action{ActionDiscardIfNoFiles, ActionUpdateLastInteraction},
	}

	err := ApplyTransition(state, result, handler)

	require.NoError(t, err)
	assert.True(t, handler.discardIfNoFilesCalled)
}

func TestApplyTransition_CallsHandlerForWarnStaleSession(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseActive}
	handler := &mockActionHandler{}
	result := TransitionResult{
		NewPhase: PhaseActive,
		Actions:  []Action{ActionWarnStaleSession},
	}

	err := ApplyTransition(state, result, handler)

	require.NoError(t, err)
	assert.True(t, handler.warnStaleSessionCalled)
}

func TestApplyTransition_ClearsEndedAt(t *testing.T) {
	t.Parallel()

	endedAt := time.Now().Add(-time.Hour)
	state := &State{Phase: PhaseEnded, EndedAt: &endedAt}
	handler := &mockActionHandler{}
	result := TransitionResult{
		NewPhase: PhaseIdle,
		Actions:  []Action{ActionClearEndedAt},
	}

	err := ApplyTransition(state, result, handler)

	require.NoError(t, err)
	assert.Nil(t, state.EndedAt)
}

func TestApplyTransition_ReturnsHandlerError_ButRunsCommonActions(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseActive}
	handler := &mockActionHandler{returnErr: errors.New("condense failed")}
	// Synthetic transition with [Condense, UpdateLastInteraction] actions.
	result := TransitionResult{
		NewPhase: PhaseIdle,
		Actions:  []Action{ActionCondense, ActionUpdateLastInteraction},
	}

	err := ApplyTransition(state, result, handler)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "condense failed")
	assert.Equal(t, PhaseIdle, state.Phase)
	// Common action must still run even though handler failed.
	require.NotNil(t, state.LastInteractionTime,
		"UpdateLastInteraction must run despite earlier handler error")
}

func TestApplyTransition_StopsOnFirstHandlerError(t *testing.T) {
	t.Parallel()

	state := &State{Phase: PhaseActive}
	handler := &mockActionHandler{returnErr: errors.New("condense failed")}
	result := TransitionResult{
		NewPhase: PhaseIdle,
		Actions:  []Action{ActionCondense, ActionWarnStaleSession},
	}

	err := ApplyTransition(state, result, handler)

	require.Error(t, err)
	assert.True(t, handler.condenseCalled)
	assert.False(t, handler.warnStaleSessionCalled, "should stop on first error")
}

func TestApplyTransition_ClearEndedAtRunsDespiteHandlerError(t *testing.T) {
	t.Parallel()

	endedAt := time.Now().Add(-time.Hour)
	state := &State{Phase: PhaseEnded, EndedAt: &endedAt}
	handler := &mockActionHandler{returnErr: errors.New("condense failed")}
	// Synthetic action list to test the mechanism: no real transition produces
	// this exact ordering, but it verifies that ClearEndedAt (common) always
	// runs even when a preceding handler action fails.
	result := TransitionResult{
		NewPhase: PhaseEnded,
		Actions:  []Action{ActionCondenseIfFilesTouched, ActionClearEndedAt},
	}

	err := ApplyTransition(state, result, handler)

	require.Error(t, err)
	assert.Nil(t, state.EndedAt, "ClearEndedAt must run despite earlier handler error")
}
