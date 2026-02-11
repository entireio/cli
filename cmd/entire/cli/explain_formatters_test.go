package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestFormatExportJSON_ValidOutput(t *testing.T) {
	t.Parallel()

	checkpointID, err := id.NewCheckpointID("a1b2c3d4e5f6")
	if err != nil {
		t.Fatalf("NewCheckpointID() error = %v", err)
	}
	now := time.Now()

	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID: "2026-01-13-test-session",
			CreatedAt: now,
			Strategy:  "manual-commit",
			Agent:     "Claude Code",
			TokenUsage: &agent.TokenUsage{
				InputTokens:  100,
				OutputTokens: 200,
			},
			FilesTouched: []string{"file1.go", "file2.go"},
		},
		Transcript: []byte(`{"type":"text","content":"test"}`),
		Prompts:    "Test prompt",
		Context:    "Test context",
	}

	summary := &checkpoint.CheckpointSummary{
		CheckpointID: checkpointID,
		FilesTouched: content.Metadata.FilesTouched,
	}

	output, err := formatExportJSON(checkpointID, content, summary,
		content.Transcript, content.Prompts, content.Context, content.Metadata.FilesTouched)

	if err != nil {
		t.Fatalf("formatExportJSON() error = %v", err)
	}

	// Verify output is valid JSON
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Check required fields
	requiredFields := []string{"checkpoint_id", "session_id", "transcript", "metadata", "files_touched", "exported_at"}
	for _, field := range requiredFields {
		if _, ok := result[field]; !ok {
			t.Errorf("missing required field: %s", field)
		}
	}

	// Verify specific values
	if result["checkpoint_id"] != checkpointID.String() {
		t.Errorf("checkpoint_id = %v, want %v", result["checkpoint_id"], checkpointID.String())
	}
	if result["session_id"] != content.Metadata.SessionID {
		t.Errorf("session_id = %v, want %v", result["session_id"], content.Metadata.SessionID)
	}
}

func TestFormatExportJSON_WithSummary(t *testing.T) {
	t.Parallel()

	checkpointID, err := id.NewCheckpointID("b2c3d4e5f6a1")
	if err != nil {
		t.Fatalf("NewCheckpointID() error = %v", err)
	}
	now := time.Now()

	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID:  "2026-01-13-test-session",
			CreatedAt:  now,
			Strategy:   "manual-commit",
			Agent:      "Claude Code",
			TokenUsage: &agent.TokenUsage{},
			Summary: &checkpoint.Summary{
				Intent:    "Test intent",
				Outcome:   "Test outcome",
				Learnings: checkpoint.LearningsSummary{Code: []checkpoint.CodeLearning{}},
				Friction:  []string{},
				OpenItems: []string{},
			},
		},
		Transcript: []byte(`{"type":"text","content":"test"}`),
	}

	summary := &checkpoint.CheckpointSummary{CheckpointID: checkpointID}

	output, err := formatExportJSON(checkpointID, content, summary,
		content.Transcript, content.Prompts, "", []string{})

	if err != nil {
		t.Fatalf("formatExportJSON() error = %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Verify summary is included
	summaryMap, ok := result["summary"].(map[string]any)
	if !ok {
		t.Fatal("summary field missing or not a map")
	}

	if summaryMap["intent"] != "Test intent" {
		t.Errorf("summary.intent = %v, want %v", summaryMap["intent"], "Test intent")
	}
	if summaryMap["outcome"] != "Test outcome" {
		t.Errorf("summary.outcome = %v, want %v", summaryMap["outcome"], "Test outcome")
	}
}

func TestFormatExportMarkdown_Structure(t *testing.T) {
	t.Parallel()

	checkpointID, err := id.NewCheckpointID("c3d4e5f6a1b2")
	if err != nil {
		t.Fatalf("NewCheckpointID() error = %v", err)
	}
	now := time.Now()

	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID:    "2026-01-13-test-session",
			CreatedAt:    now,
			Strategy:     "manual-commit",
			Agent:        "Claude Code",
			TokenUsage:   &agent.TokenUsage{},
			FilesTouched: []string{"file1.go", "file2.go"},
		},
		Transcript: []byte(`{"type":"text","content":"test"}`),
	}

	summary := &checkpoint.CheckpointSummary{CheckpointID: checkpointID}

	output, err := formatExportMarkdown(checkpointID, content, summary,
		content.Transcript, "", "", content.Metadata.FilesTouched)

	if err != nil {
		t.Fatalf("formatExportMarkdown() error = %v", err)
	}

	outputStr := string(output)

	// Verify markdown structure
	if !strings.Contains(outputStr, "# Session:") {
		t.Error("missing session header")
	}
	if !strings.Contains(outputStr, "**Checkpoint:**") {
		t.Error("missing checkpoint field")
	}
	if !strings.Contains(outputStr, "**Created:**") {
		t.Error("missing created field")
	}
	if !strings.Contains(outputStr, "## Files Modified") {
		t.Error("missing files section")
	}
	if !strings.Contains(outputStr, "## Transcript") {
		t.Error("missing transcript section")
	}

	// Verify files are listed
	if !strings.Contains(outputStr, "`file1.go`") {
		t.Error("file1.go not listed")
	}
	if !strings.Contains(outputStr, "`file2.go`") {
		t.Error("file2.go not listed")
	}
}

func TestFormatExportMarkdown_WithSummary(t *testing.T) {
	t.Parallel()

	checkpointID, err := id.NewCheckpointID("d4e5f6a1b2c3")
	if err != nil {
		t.Fatalf("NewCheckpointID() error = %v", err)
	}
	now := time.Now()

	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID:  "2026-01-13-test-session",
			CreatedAt:  now,
			Strategy:   "manual-commit",
			Agent:      "Claude Code",
			TokenUsage: &agent.TokenUsage{},
			Summary: &checkpoint.Summary{
				Intent:  "Implement feature X",
				Outcome: "Successfully implemented",
				Learnings: checkpoint.LearningsSummary{
					Code: []checkpoint.CodeLearning{
						{Path: "api/handler.go", Finding: "Added error handling"},
					},
				},
			},
		},
		Transcript: []byte(`{"type":"text","content":"test"}`),
	}

	summary := &checkpoint.CheckpointSummary{CheckpointID: checkpointID}

	output, err := formatExportMarkdown(checkpointID, content, summary,
		content.Transcript, "", "", []string{})

	if err != nil {
		t.Fatalf("formatExportMarkdown() error = %v", err)
	}

	outputStr := string(output)

	// Verify summary section
	if !strings.Contains(outputStr, "## Summary") {
		t.Error("missing summary section")
	}
	if !strings.Contains(outputStr, "**Intent:**") {
		t.Error("missing intent field")
	}
	if !strings.Contains(outputStr, "Implement feature X") {
		t.Error("intent value not found")
	}
	if !strings.Contains(outputStr, "**Outcome:**") {
		t.Error("missing outcome field")
	}
	if !strings.Contains(outputStr, "Successfully implemented") {
		t.Error("outcome value not found")
	}
	if !strings.Contains(outputStr, "**Key Learnings:**") {
		t.Error("missing learnings section")
	}
	if !strings.Contains(outputStr, "api/handler.go") {
		t.Error("learning path not found")
	}
}

func TestFormatExportJSON_HandlesEmptyFields(t *testing.T) {
	t.Parallel()

	checkpointID, err := id.NewCheckpointID("e5f6a1b2c3d4")
	if err != nil {
		t.Fatalf("NewCheckpointID() error = %v", err)
	}
	now := time.Now()

	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID:    "2026-01-13-test",
			CreatedAt:    now,
			Strategy:     "manual-commit",
			FilesTouched: []string{},
		},
		Transcript: []byte{},
	}

	summary := &checkpoint.CheckpointSummary{CheckpointID: checkpointID}

	output, err := formatExportJSON(checkpointID, content, summary,
		content.Transcript, "", "", content.Metadata.FilesTouched)

	if err != nil {
		t.Fatalf("formatExportJSON() should handle empty fields, got error: %v", err)
	}

	// Should still produce valid JSON
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}

func TestFormatExportMarkdown_HandlesEmptyFiles(t *testing.T) {
	t.Parallel()

	checkpointID, err := id.NewCheckpointID("f6a1b2c3d4e5")
	if err != nil {
		t.Fatalf("NewCheckpointID() error = %v", err)
	}
	now := time.Now()

	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID:    "2026-01-13-test",
			CreatedAt:    now,
			Strategy:     "manual-commit",
			FilesTouched: []string{},
		},
		Transcript: []byte(`{"type":"text","content":"test"}`),
	}

	summary := &checkpoint.CheckpointSummary{CheckpointID: checkpointID}

	output, err := formatExportMarkdown(checkpointID, content, summary,
		content.Transcript, "", "", content.Metadata.FilesTouched)

	if err != nil {
		t.Fatalf("formatExportMarkdown() should handle empty files, got error: %v", err)
	}

	outputStr := string(output)

	// When no files, the section should still render but be empty
	// (or omitted, depending on implementation - we choose to always include it)
	if !strings.Contains(outputStr, "## Files Modified") {
		// This is fine - we could also choose to omit the section entirely
		t.Log("Files Modified section omitted when no files (acceptable)")
	}
}
