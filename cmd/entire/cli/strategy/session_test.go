package strategy

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestSessionStruct(t *testing.T) {
	now := time.Now()
	checkpoints := []Checkpoint{
		{
			CheckpointID:     id.MustCheckpointID("abc123def456"),
			Message:          "First checkpoint",
			Timestamp:        now.Add(-time.Hour),
			IsTaskCheckpoint: false,
			ToolUseID:        "",
		},
		{
			CheckpointID:     id.MustCheckpointID("def456abc789"),
			Message:          "Task checkpoint",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			ToolUseID:        "toolu_123",
		},
	}

	session := Session{
		ID:          "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e",
		Description: "Fix lint errors",
		Strategy:    StrategyNameManualCommit,
		StartTime:   now.Add(-2 * time.Hour),
		Checkpoints: checkpoints,
	}

	if session.ID != "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e" {
		t.Errorf("expected session ID to match, got %s", session.ID)
	}
	if session.Description != "Fix lint errors" {
		t.Errorf("expected description to match, got %s", session.Description)
	}
	if session.Strategy != StrategyNameManualCommit {
		t.Errorf("expected strategy to be manual-commit, got %s", session.Strategy)
	}
	if len(session.Checkpoints) != 2 {
		t.Errorf("expected 2 checkpoints, got %d", len(session.Checkpoints))
	}
	if session.StartTime.IsZero() {
		t.Error("expected StartTime to be set")
	}
}

func TestCheckpointStruct(t *testing.T) {
	now := time.Now()

	// Test session checkpoint (not task)
	sessionCheckpoint := Checkpoint{
		CheckpointID:     "abc1234567890",
		Message:          "Session save",
		Timestamp:        now,
		IsTaskCheckpoint: false,
		ToolUseID:        "",
	}

	if sessionCheckpoint.CheckpointID != "abc1234567890" {
		t.Errorf("expected CheckpointID to match, got %s", sessionCheckpoint.CheckpointID)
	}
	if sessionCheckpoint.Message != "Session save" {
		t.Errorf("expected Message to match, got %s", sessionCheckpoint.Message)
	}
	if sessionCheckpoint.Timestamp != now {
		t.Error("expected Timestamp to match")
	}
	if sessionCheckpoint.IsTaskCheckpoint {
		t.Error("expected session checkpoint to not be a task checkpoint")
	}
	if sessionCheckpoint.ToolUseID != "" {
		t.Error("expected session checkpoint to have empty ToolUseID")
	}

	// Test task checkpoint
	taskCheckpoint := Checkpoint{
		CheckpointID:     "def0987654321",
		Message:          "Task: implement feature",
		Timestamp:        now,
		IsTaskCheckpoint: true,
		ToolUseID:        "toolu_abc123",
	}

	if taskCheckpoint.CheckpointID != "def0987654321" {
		t.Errorf("expected CheckpointID to match, got %s", taskCheckpoint.CheckpointID)
	}
	if taskCheckpoint.Message != "Task: implement feature" {
		t.Errorf("expected Message to match, got %s", taskCheckpoint.Message)
	}
	if taskCheckpoint.Timestamp != now {
		t.Error("expected Timestamp to match")
	}
	if !taskCheckpoint.IsTaskCheckpoint {
		t.Error("expected task checkpoint to be a task checkpoint")
	}
	if taskCheckpoint.ToolUseID != "toolu_abc123" {
		t.Errorf("expected ToolUseID to match, got %s", taskCheckpoint.ToolUseID)
	}
}
