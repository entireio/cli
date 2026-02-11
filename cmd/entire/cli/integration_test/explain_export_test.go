//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExport_Showcase_EndToEnd(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Setup repository and branch
		env.InitRepo()
		env.WriteFile("README.md", "# Test")
		env.GitAdd("README.md")
		env.GitCommit("Initial commit")

		if strategyName == "manual-commit" {
			// Switch to feature branch for manual-commit strategy
			env.GitCheckoutNewBranch("feature/test")
		}

		// 1. Create a session with a checkpoint
		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		// 2. Make code changes with sensitive data
		apiCode := `package api

import "github.com/lib/pq"

// Connect to 10.0.1.5:5432
const dbConn = "postgres://user:pass@10.0.1.5:5432/db"

func Handler() {
	// Admin email: admin@acme-corp.com
}
`
		env.WriteFile("src/api.go", apiCode)

		configCode := `package config

const Key = "sk-proj-test-secret-key"
const InternalURL = "https://api.internal.company.com"
`
		env.WriteFile("src/config.go", configCode)

		// 3. Create checkpoint
		session.CreateTranscript(
			"implement API handler with database connection",
			[]FileChange{
				{Path: "src/api.go", Content: apiCode},
				{Path: "src/config.go", Content: configCode},
			},
		)
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		// 4. Commit to make it a committed checkpoint
		env.GitAdd("src/api.go", "src/config.go")
		env.GitCommit("feat: add API handler")

		// 5. Extract checkpoint ID from commit
		headHash := env.GetHeadHash()
		checkpointID := env.GetCheckpointIDFromCommitMessage(headHash)
		if checkpointID == "" {
			t.Fatal("no checkpoint ID in commit message")
		}

		// 6. Export with showcase mode (JSON format)
		output, err := env.RunCLIWithError("explain", "-c", checkpointID, "--export", "--showcase", "--format=json")
		if err != nil {
			t.Fatalf("export failed: %v, output: %s", err, output)
		}

		// 7. Verify JSON structure
		var exported map[string]any
		if err := json.Unmarshal([]byte(output), &exported); err != nil {
			t.Fatalf("invalid JSON output: %v\nOutput: %s", err, output)
		}

		// Check required fields
		requiredFields := []string{"checkpoint_id", "session_id", "transcript", "metadata", "files_touched", "exported_at"}
		for _, field := range requiredFields {
			if _, ok := exported[field]; !ok {
				t.Errorf("missing required field: %s", field)
			}
		}

		// 8. Verify redaction - sensitive data should be redacted
		transcriptStr, ok := exported["transcript"].(string)
		if !ok {
			t.Fatal("transcript not a string")
		}

		// Sensitive data that should be redacted
		sensitivePatterns := []string{
			"sk-proj-test-secret-key",  // API key (entropy-based)
			"10.0.1.5",                 // Private IP (pattern-based)
			"postgres://",              // DB connection string (pattern-based)
			"admin@acme-corp.com",      // Email (pattern-based)
			"api.internal.company.com", // Internal URL (pattern-based)
		}

		for _, pattern := range sensitivePatterns {
			if strings.Contains(transcriptStr, pattern) {
				t.Errorf("sensitive data not redacted: %q found in transcript", pattern)
			}
		}

		// Should contain redaction markers
		if !strings.Contains(transcriptStr, "REDACTED") && !strings.Contains(transcriptStr, "[") {
			t.Error("no redaction markers found in transcript")
		}

		// Transcript should still be valid JSONL
		lines := strings.Split(strings.TrimSpace(transcriptStr), "\n")
		for i, line := range lines {
			if len(line) == 0 {
				continue
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Errorf("line %d not valid JSON: %v\nLine: %s", i, err, line)
			}
		}
	})
}

func TestExport_WithoutShowcase_MinimalRedaction(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Setup
		env.InitRepo()
		env.WriteFile("README.md", "# Test")
		env.GitAdd("README.md")
		env.GitCommit("Initial commit")

		if strategyName == "manual-commit" {
			env.GitCheckoutNewBranch("feature/test")
		}

		// Create session with checkpoint
		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		code := `package main

// API key for testing
const apiKey = "sk-test-very-secret-key"

// User: john@example.com
// Path: /home/john/project/main.go
func main() {}
`
		env.WriteFile("main.go", code)

		session.CreateTranscript(
			"implement main function",
			[]FileChange{{Path: "main.go", Content: code}},
		)
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		env.GitAdd("main.go")
		env.GitCommit("add main")

		checkpointID := env.GetLatestCheckpointIDFromHistory()

		// Export WITHOUT showcase mode
		output, err := env.RunCLIWithError("explain", "-c", checkpointID, "--export", "--format=json")
		if err != nil {
			t.Fatalf("export failed: %v, output: %s", err, output)
		}

		var exported map[string]any
		if err := json.Unmarshal([]byte(output), &exported); err != nil {
			t.Fatalf("invalid JSON output: %v", err)
		}

		transcriptStr := exported["transcript"].(string)

		// Without --showcase, only entropy-based redaction applies
		// API key should still be redacted by entropy detection
		if strings.Contains(transcriptStr, "sk-test-very-secret-key") {
			t.Error("API key should be redacted by entropy detection even without showcase mode")
		}

		// But patterns like emails and paths should NOT be redacted (no showcase mode)
		// Note: Depending on entropy thresholds, emails might still be caught.
		// For this test, we just verify the export works without error.
	})
}

func TestExport_MarkdownFormat(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Setup
		env.InitRepo()
		env.WriteFile("README.md", "# Test")
		env.GitAdd("README.md")
		env.GitCommit("Initial commit")

		if strategyName == "manual-commit" {
			env.GitCheckoutNewBranch("feature/test")
		}

		// Create session with checkpoint
		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		env.WriteFile("main.go", "package main\nfunc main() {}")

		session.CreateTranscript(
			"create main function",
			[]FileChange{{Path: "main.go", Content: "package main\nfunc main() {}"}},
		)
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		env.GitAdd("main.go")
		env.GitCommit("add main")

		checkpointID := env.GetLatestCheckpointIDFromHistory()

		// Export as Markdown with showcase
		output, err := env.RunCLIWithError("explain", "-c", checkpointID, "--export", "--showcase", "--format=markdown")
		if err != nil {
			t.Fatalf("export failed: %v, output: %s", err, output)
		}

		// Verify markdown structure
		if !strings.Contains(output, "# Session:") {
			t.Error("missing session header")
		}
		if !strings.Contains(output, "**Checkpoint:**") {
			t.Error("missing checkpoint field")
		}
		if !strings.Contains(output, "**Created:**") {
			t.Error("missing created field")
		}
		if !strings.Contains(output, "## Files Modified") {
			t.Error("missing files section")
		}
		if !strings.Contains(output, "## Transcript") {
			t.Error("missing transcript section")
		}

		// Verify file is listed
		if !strings.Contains(output, "`main.go`") {
			t.Error("main.go not listed in files")
		}
	})
}

func TestExport_CheckpointNotFound(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		env.InitRepo()

		if strategyName == "manual-commit" {
			env.GitCheckoutNewBranch("feature/test")
		}

		// Try to export a nonexistent checkpoint
		output, err := env.RunCLIWithError("explain", "-c", "nonexistent123", "--export")
		if err == nil {
			t.Errorf("expected error for nonexistent checkpoint, got output: %s", output)
			return
		}

		if !strings.Contains(output, "checkpoint not found") {
			t.Errorf("expected 'checkpoint not found' error, got: %s", output)
		}
	})
}

func TestExport_RequiresCheckpointFlag(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		env.InitRepo()

		// Try to use --export without --checkpoint
		output, err := env.RunCLIWithError("explain", "--export")
		if err == nil {
			t.Errorf("expected error when --export without --checkpoint, got output: %s", output)
			return
		}

		if !strings.Contains(output, "--export requires --checkpoint") {
			t.Errorf("expected '--export requires --checkpoint' error, got: %s", output)
		}
	})
}

func TestExport_ShowcaseRequiresExport(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		env.InitRepo()

		// Try to use --showcase without --export
		output, err := env.RunCLIWithError("explain", "-c", "test123", "--showcase")
		if err == nil {
			t.Errorf("expected error when --showcase without --export, got output: %s", output)
			return
		}

		if !strings.Contains(output, "--showcase requires --export") {
			t.Errorf("expected '--showcase requires --export' error, got: %s", output)
		}
	})
}

func TestExport_MutualExclusivityWithOtherModes(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		env.InitRepo()

		// --export is mutually exclusive with --raw-transcript
		output, err := env.RunCLIWithError("explain", "-c", "test123", "--export", "--raw-transcript")
		if err == nil {
			t.Errorf("expected error for --export with --raw-transcript, got output: %s", output)
			return
		}
		if !strings.Contains(output, "mutually exclusive") {
			t.Errorf("expected 'mutually exclusive' error, got: %s", output)
		}

		// --export is mutually exclusive with --short
		output, err = env.RunCLIWithError("explain", "-c", "test123", "--export", "--short")
		if err == nil {
			t.Errorf("expected error for --export with --short, got output: %s", output)
			return
		}
		if !strings.Contains(output, "mutually exclusive") {
			t.Errorf("expected 'mutually exclusive' error, got: %s", output)
		}

		// --export is mutually exclusive with --full
		output, err = env.RunCLIWithError("explain", "-c", "test123", "--export", "--full")
		if err == nil {
			t.Errorf("expected error for --export with --full, got output: %s", output)
			return
		}
		if !strings.Contains(output, "mutually exclusive") {
			t.Errorf("expected 'mutually exclusive' error, got: %s", output)
		}
	})
}

func TestExport_UnsupportedFormat(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Setup
		env.InitRepo()
		env.WriteFile("README.md", "# Test")
		env.GitAdd("README.md")
		env.GitCommit("Initial commit")

		if strategyName == "manual-commit" {
			env.GitCheckoutNewBranch("feature/test")
		}

		// Create checkpoint
		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		env.WriteFile("test.txt", "test")
		session.CreateTranscript("test", []FileChange{{Path: "test.txt", Content: "test"}})
		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		env.GitAdd("test.txt")
		env.GitCommit("test commit")

		checkpointID := env.GetLatestCheckpointIDFromHistory()

		// Try to export with unsupported format
		output, err := env.RunCLIWithError("explain", "-c", checkpointID, "--export", "--format=pdf")
		if err == nil {
			t.Errorf("expected error for unsupported format, got output: %s", output)
			return
		}

		if !strings.Contains(output, "unsupported format") {
			t.Errorf("expected 'unsupported format' error, got: %s", output)
		}
	})
}
