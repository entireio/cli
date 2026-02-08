//go:build integration

package integration

import (
	"testing"
	"time"
)

// TestLastInteractionAt_SetOnFirstPrompt verifies that LastInteractionAt is set
// when a session is first initialized via UserPromptSubmit.
func TestLastInteractionAt_SetOnFirstPrompt(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, _ string) {
		session := env.NewSession()

		beforePrompt := time.Now()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		state, err := env.GetSessionState(session.ID)
		if err != nil {
			t.Fatalf("GetSessionState failed: %v", err)
		}
		if state == nil {
			t.Fatal("session state should exist after UserPromptSubmit")
		}

		if state.LastInteractionAt == nil {
			t.Fatal("LastInteractionAt should be set after first prompt")
		}
		if state.LastInteractionAt.Before(beforePrompt) {
			t.Errorf("LastInteractionAt %v should be after test start %v",
				*state.LastInteractionAt, beforePrompt)
		}
	})
}

// TestLastInteractionAt_UpdatedOnSubsequentPrompts verifies that LastInteractionAt
// is updated on each subsequent UserPromptSubmit call.
func TestLastInteractionAt_UpdatedOnSubsequentPrompts(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, _ string) {
		session := env.NewSession()

		// First prompt
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("first SimulateUserPromptSubmit failed: %v", err)
		}

		state1, err := env.GetSessionState(session.ID)
		if err != nil {
			t.Fatalf("GetSessionState after first prompt failed: %v", err)
		}
		if state1.LastInteractionAt == nil {
			t.Fatal("LastInteractionAt should be set after first prompt")
		}
		firstInteraction := *state1.LastInteractionAt

		// Small delay to ensure timestamps differ
		time.Sleep(10 * time.Millisecond)

		// Second prompt
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("second SimulateUserPromptSubmit failed: %v", err)
		}

		state2, err := env.GetSessionState(session.ID)
		if err != nil {
			t.Fatalf("GetSessionState after second prompt failed: %v", err)
		}
		if state2.LastInteractionAt == nil {
			t.Fatal("LastInteractionAt should be set after second prompt")
		}

		if !state2.LastInteractionAt.After(firstInteraction) {
			t.Errorf("LastInteractionAt should be updated: first=%v, second=%v",
				firstInteraction, *state2.LastInteractionAt)
		}
	})
}

// TestLastInteractionAt_PreservedAcrossCheckpoints verifies that LastInteractionAt
// survives a full checkpoint cycle (prompt → stop → prompt).
func TestLastInteractionAt_PreservedAcrossCheckpoints(t *testing.T) {
	t.Parallel()
	RunForAllStrategiesWithRepoEnv(t, func(t *testing.T, env *TestEnv, _ string) {
		session := env.NewSession()

		// First prompt + checkpoint
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		env.WriteFile("file1.txt", "content1")
		session.CreateTranscript("Create file1", []FileChange{
			{Path: "file1.txt", Content: "content1"},
		})
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		time.Sleep(10 * time.Millisecond)

		// Second prompt
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("second SimulateUserPromptSubmit failed: %v", err)
		}

		state, err := env.GetSessionState(session.ID)
		if err != nil {
			t.Fatalf("GetSessionState failed: %v", err)
		}
		if state.LastInteractionAt == nil {
			t.Fatal("LastInteractionAt should be set after second prompt")
		}

		// LastInteractionAt should be after StartedAt (second prompt is later)
		if !state.LastInteractionAt.After(state.StartedAt) {
			t.Errorf("LastInteractionAt %v should be after StartedAt %v",
				*state.LastInteractionAt, state.StartedAt)
		}
	})
}
