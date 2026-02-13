//go:generate go run gen_state_diagram.go

package session

import (
	"fmt"
	"strings"
	"time"
)

// Phase represents the lifecycle stage of a session.
type Phase string

const (
	PhaseActive Phase = "active"
	PhaseIdle   Phase = "idle"
	PhaseEnded  Phase = "ended"
)

// PhaseActiveCommitted is kept as a variable (not a valid phase constant) so that
// external packages referencing it continue to compile during the migration.
// It maps to PhaseActive at runtime via PhaseFromString.
//
// Deprecated: ACTIVE_COMMITTED has been removed from the state machine.
// All references should be migrated to PhaseActive.
var PhaseActiveCommitted Phase = "active_committed"

// allPhases is the canonical list of phases for enumeration (e.g., diagram generation).
var allPhases = []Phase{PhaseIdle, PhaseActive, PhaseEnded}

// PhaseFromString normalizes a phase string, treating empty or unknown values
// as PhaseIdle for backward compatibility with pre-state-machine session files.
func PhaseFromString(s string) Phase {
	switch Phase(s) {
	case PhaseActive:
		return PhaseActive
	case "active_committed": // backward compat: old state files
		return PhaseActive
	case PhaseIdle:
		return PhaseIdle
	case PhaseEnded:
		return PhaseEnded
	default:
		return PhaseIdle
	}
}

// IsActive reports whether the phase represents an active agent turn.
func (p Phase) IsActive() bool {
	return p == PhaseActive
}

// Event represents something that happened to a session.
type Event int

const (
	EventTurnStart    Event = iota // Agent begins working on a prompt
	EventTurnEnd                   // Agent finishes its turn
	EventGitCommit                 // A git commit was made (PostCommit hook)
	EventSessionStart              // Session process started (SessionStart hook)
	EventSessionStop               // Session process ended (SessionStop hook)
)

// allEvents is the canonical list of events for enumeration.
var allEvents = []Event{EventTurnStart, EventTurnEnd, EventGitCommit, EventSessionStart, EventSessionStop}

// String returns a human-readable name for the event.
func (e Event) String() string {
	switch e {
	case EventTurnStart:
		return "TurnStart"
	case EventTurnEnd:
		return "TurnEnd"
	case EventGitCommit:
		return "GitCommit"
	case EventSessionStart:
		return "SessionStart"
	case EventSessionStop:
		return "SessionStop"
	default:
		return fmt.Sprintf("Event(%d)", int(e))
	}
}

// Action represents a side effect that should be performed after a transition.
// The caller is responsible for executing these -- the state machine only declares them.
type Action int

const (
	ActionCondense               Action = iota // Condense session data to permanent storage
	ActionCondenseIfFilesTouched               // Condense only if FilesTouched is non-empty
	ActionDiscardIfNoFiles                     // Discard session if FilesTouched is empty
	ActionMigrateShadowBranch                  // Migrate shadow branch to new HEAD
	ActionWarnStaleSession                     // Warn user about stale session(s)
	ActionClearEndedAt                         // Clear EndedAt timestamp (session re-entering)
	ActionUpdateLastInteraction                // Update LastInteractionTime
)

// String returns a human-readable name for the action.
func (a Action) String() string {
	switch a {
	case ActionCondense:
		return "Condense"
	case ActionCondenseIfFilesTouched:
		return "CondenseIfFilesTouched"
	case ActionDiscardIfNoFiles:
		return "DiscardIfNoFiles"
	case ActionMigrateShadowBranch:
		return "MigrateShadowBranch"
	case ActionWarnStaleSession:
		return "WarnStaleSession"
	case ActionClearEndedAt:
		return "ClearEndedAt"
	case ActionUpdateLastInteraction:
		return "UpdateLastInteraction"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// TransitionContext provides read-only context for transitions that need
// to inspect session state without mutating it.
type TransitionContext struct {
	HasFilesTouched    bool // len(FilesTouched) > 0
	IsRebaseInProgress bool // .git/rebase-merge/ or .git/rebase-apply/ exists
}

// TransitionResult holds the outcome of a state machine transition.
type TransitionResult struct {
	NewPhase Phase
	Actions  []Action
}

// Transition computes the next phase and required actions given the current
// phase and an event. This is a pure function with no side effects.
//
// Empty or unknown phase values are normalized to PhaseIdle for backward
// compatibility with session state files created before phase tracking.
func Transition(current Phase, event Event, ctx TransitionContext) TransitionResult {
	// Normalize empty/unknown phase to idle for backward compatibility.
	current = PhaseFromString(string(current))

	switch current {
	case PhaseIdle:
		return transitionFromIdle(event, ctx)
	case PhaseActive:
		return transitionFromActive(event, ctx)
	case PhaseEnded:
		return transitionFromEnded(event, ctx)
	default:
		// PhaseFromString guarantees we only get known phases, but the
		// exhaustive linter requires handling the default case.
		return TransitionResult{NewPhase: PhaseIdle}
	}
}

func transitionFromIdle(event Event, ctx TransitionContext) TransitionResult {
	switch event {
	case EventTurnStart:
		return TransitionResult{
			NewPhase: PhaseActive,
			Actions:  []Action{ActionUpdateLastInteraction},
		}
	case EventTurnEnd:
		// Turn end while idle is a no-op (no active turn to end).
		return TransitionResult{NewPhase: PhaseIdle}
	case EventGitCommit:
		if ctx.IsRebaseInProgress {
			return TransitionResult{NewPhase: PhaseIdle}
		}
		return TransitionResult{
			NewPhase: PhaseIdle,
			Actions:  []Action{ActionCondense, ActionUpdateLastInteraction},
		}
	case EventSessionStart:
		// Already condensable, no-op.
		return TransitionResult{NewPhase: PhaseIdle}
	case EventSessionStop:
		return TransitionResult{
			NewPhase: PhaseEnded,
			Actions:  []Action{ActionUpdateLastInteraction},
		}
	default:
		return TransitionResult{NewPhase: PhaseIdle}
	}
}

func transitionFromActive(event Event, ctx TransitionContext) TransitionResult {
	switch event {
	case EventTurnStart:
		// Ctrl-C recovery: the previous turn's TurnEnd was skipped (agent crashed or
		// user interrupted). Just continue as active.
		return TransitionResult{
			NewPhase: PhaseActive,
			Actions:  []Action{ActionUpdateLastInteraction},
		}
	case EventTurnEnd:
		return TransitionResult{
			NewPhase: PhaseIdle,
			Actions:  []Action{ActionUpdateLastInteraction},
		}
	case EventGitCommit:
		if ctx.IsRebaseInProgress {
			return TransitionResult{NewPhase: PhaseActive}
		}
		return TransitionResult{
			NewPhase: PhaseActive,
			Actions:  []Action{ActionCondense, ActionMigrateShadowBranch, ActionUpdateLastInteraction},
		}
	case EventSessionStart:
		return TransitionResult{
			NewPhase: PhaseActive,
			Actions:  []Action{ActionWarnStaleSession},
		}
	case EventSessionStop:
		return TransitionResult{
			NewPhase: PhaseEnded,
			Actions:  []Action{ActionUpdateLastInteraction},
		}
	default:
		return TransitionResult{NewPhase: PhaseActive}
	}
}

func transitionFromEnded(event Event, ctx TransitionContext) TransitionResult {
	switch event {
	case EventTurnStart:
		return TransitionResult{
			NewPhase: PhaseActive,
			Actions:  []Action{ActionClearEndedAt, ActionUpdateLastInteraction},
		}
	case EventTurnEnd:
		// Turn end while ended is a no-op.
		return TransitionResult{NewPhase: PhaseEnded}
	case EventGitCommit:
		if ctx.IsRebaseInProgress {
			return TransitionResult{NewPhase: PhaseEnded}
		}
		if ctx.HasFilesTouched {
			return TransitionResult{
				NewPhase: PhaseEnded,
				Actions:  []Action{ActionCondenseIfFilesTouched, ActionUpdateLastInteraction},
			}
		}
		return TransitionResult{
			NewPhase: PhaseEnded,
			Actions:  []Action{ActionDiscardIfNoFiles, ActionUpdateLastInteraction},
		}
	case EventSessionStart:
		return TransitionResult{
			NewPhase: PhaseIdle,
			Actions:  []Action{ActionClearEndedAt},
		}
	case EventSessionStop:
		// Already ended, no-op.
		return TransitionResult{NewPhase: PhaseEnded}
	default:
		return TransitionResult{NewPhase: PhaseEnded}
	}
}

// ApplyCommonActions applies the common (non-strategy-specific) actions from a
// TransitionResult to the given State. It updates Phase, LastInteractionTime,
// and EndedAt as indicated by the transition.
//
// Returns the subset of actions that require strategy-specific handling
// (e.g., ActionCondense, ActionMigrateShadowBranch, ActionWarnStaleSession).
// The caller is responsible for dispatching those.
func ApplyCommonActions(state *State, result TransitionResult) []Action {
	state.Phase = result.NewPhase

	var remaining []Action
	for _, action := range result.Actions {
		switch action {
		case ActionUpdateLastInteraction:
			now := time.Now()
			state.LastInteractionTime = &now
		case ActionClearEndedAt:
			state.EndedAt = nil
		case ActionCondense, ActionCondenseIfFilesTouched, ActionDiscardIfNoFiles,
			ActionMigrateShadowBranch, ActionWarnStaleSession:
			// Strategy-specific actions — pass through to caller.
			remaining = append(remaining, action)
		}
	}
	return remaining
}

// MermaidDiagram generates a Mermaid state diagram from the transition table.
// The diagram is derived by calling Transition() for representative context
// variants per phase/event, so it stays in sync with the implementation.
func MermaidDiagram() string {
	var b strings.Builder
	b.WriteString("stateDiagram-v2\n")

	// State declarations with descriptions.
	b.WriteString("    state \"IDLE\" as idle\n")
	b.WriteString("    state \"ACTIVE\" as active\n")
	b.WriteString("    state \"ENDED\" as ended\n")
	b.WriteString("\n")

	// Context variants for GitCommit: rebase, files/no-files.
	type contextVariant struct {
		label string
		ctx   TransitionContext
	}

	// For each phase/event, generate edges.
	for _, phase := range allPhases {
		for _, event := range allEvents {
			var variants []contextVariant
			switch {
			case event == EventGitCommit && phase == PhaseEnded:
				// HasFilesTouched only affects PhaseEnded transitions;
				// other phases produce identical edges for both values.
				variants = []contextVariant{
					{"[files]", TransitionContext{HasFilesTouched: true}},
					{"[no files]", TransitionContext{HasFilesTouched: false}},
					{"[rebase]", TransitionContext{IsRebaseInProgress: true}},
				}
			case event == EventGitCommit:
				variants = []contextVariant{
					{"", TransitionContext{}},
					{"[rebase]", TransitionContext{IsRebaseInProgress: true}},
				}
			default:
				variants = []contextVariant{
					{"", TransitionContext{}},
				}
			}

			for _, v := range variants {
				result := Transition(phase, event, v.ctx)

				// Build edge label.
				label := event.String()
				if v.label != "" {
					label += " " + v.label
				}
				if len(result.Actions) > 0 {
					actionNames := make([]string, 0, len(result.Actions))
					for _, a := range result.Actions {
						actionNames = append(actionNames, a.String())
					}
					label += " / " + strings.Join(actionNames, ", ")
				}

				from := string(phase)
				to := string(result.NewPhase)

				fmt.Fprintf(&b, "    %s --> %s : %s\n", from, to, label)
			}
		}
	}

	return b.String()
}
