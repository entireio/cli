package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComputeFilesHash_Deterministic(t *testing.T) {
	t.Parallel()

	files := []string{"b.go", "a.go", "c.go"}
	hash1 := computeFilesHash(files)
	hash2 := computeFilesHash(files)

	if hash1 != hash2 {
		t.Errorf("expected deterministic hash, got %s and %s", hash1, hash2)
	}

	// Order shouldn't matter
	files2 := []string{"c.go", "a.go", "b.go"}
	hash3 := computeFilesHash(files2)

	if hash1 != hash3 {
		t.Errorf("expected order-independent hash, got %s and %s", hash1, hash3)
	}
}

func TestComputeFilesHash_DifferentFiles(t *testing.T) {
	t.Parallel()

	hash1 := computeFilesHash([]string{"a.go", "b.go"})
	hash2 := computeFilesHash([]string{"a.go", "c.go"})

	if hash1 == hash2 {
		t.Error("expected different hashes for different file lists")
	}
}

func TestComputeFilesHash_Empty(t *testing.T) {
	t.Parallel()

	hash := computeFilesHash(nil)
	if hash == "" {
		t.Error("expected non-empty hash for empty file list")
	}
}

func TestWingmanPayload_RoundTrip(t *testing.T) {
	t.Parallel()

	payload := WingmanPayload{
		SessionID:     "test-session-123",
		RepoRoot:      "/tmp/repo",
		BaseCommit:    "abc123def456",
		ModifiedFiles: []string{"main.go", "util.go"},
		NewFiles:      []string{"new.go"},
		DeletedFiles:  []string{"old.go"},
		Prompts:       []string{"Fix the bug", "Add tests"},
		CommitMessage: "fix: resolve issue",
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded WingmanPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.SessionID != payload.SessionID {
		t.Errorf("session_id: got %q, want %q", decoded.SessionID, payload.SessionID)
	}
	if decoded.RepoRoot != payload.RepoRoot {
		t.Errorf("repo_root: got %q, want %q", decoded.RepoRoot, payload.RepoRoot)
	}
	if decoded.BaseCommit != payload.BaseCommit {
		t.Errorf("base_commit: got %q, want %q", decoded.BaseCommit, payload.BaseCommit)
	}
	if len(decoded.ModifiedFiles) != len(payload.ModifiedFiles) {
		t.Errorf("modified_files: got %d, want %d", len(decoded.ModifiedFiles), len(payload.ModifiedFiles))
	}
	if len(decoded.NewFiles) != len(payload.NewFiles) {
		t.Errorf("new_files: got %d, want %d", len(decoded.NewFiles), len(payload.NewFiles))
	}
	if len(decoded.DeletedFiles) != len(payload.DeletedFiles) {
		t.Errorf("deleted_files: got %d, want %d", len(decoded.DeletedFiles), len(payload.DeletedFiles))
	}
	if len(decoded.Prompts) != len(payload.Prompts) {
		t.Errorf("prompts: got %d, want %d", len(decoded.Prompts), len(payload.Prompts))
	}
	if decoded.CommitMessage != payload.CommitMessage {
		t.Errorf("commit_message: got %q, want %q", decoded.CommitMessage, payload.CommitMessage)
	}
}

func TestBuildReviewPrompt_IncludesAllSections(t *testing.T) {
	t.Parallel()

	prompts := []string{"Fix the authentication bug"}
	fileList := "auth.go (modified), auth_test.go (new)"
	diff := `diff --git a/auth.go b/auth.go
--- a/auth.go
+++ b/auth.go
@@ -10,6 +10,8 @@ func Login(user, pass string) error {
+    if user == "" {
+        return errors.New("empty user")
+    }
`

	commitMsg := "Fix empty user login crash"
	sessionCtx := "## Summary\nFixed authentication bug for empty usernames"

	result := buildReviewPrompt(prompts, commitMsg, sessionCtx, "test-session-456", fileList, diff)

	if !strings.Contains(result, "Fix the authentication bug") {
		t.Error("prompt should contain user prompt")
	}
	if !strings.Contains(result, "auth.go (modified)") {
		t.Error("prompt should contain file list")
	}
	if !strings.Contains(result, "diff --git") {
		t.Error("prompt should contain diff")
	}
	if !strings.Contains(result, "intent-aware review") {
		t.Error("prompt should contain reviewer instruction")
	}
	if !strings.Contains(result, "Fix empty user login crash") {
		t.Error("prompt should contain commit message")
	}
	if !strings.Contains(result, "Fixed authentication bug") {
		t.Error("prompt should contain session context")
	}
	if !strings.Contains(result, ".entire/metadata/test-session-456/full.jsonl") {
		t.Error("prompt should contain checkpoint transcript path")
	}
	if !strings.Contains(result, ".entire/metadata/test-session-456/prompt.txt") {
		t.Error("prompt should contain checkpoint prompt path")
	}
	if !strings.Contains(result, ".entire/metadata/test-session-456/context.md") {
		t.Error("prompt should contain checkpoint context path")
	}
}

func TestBuildReviewPrompt_EmptyPrompts(t *testing.T) {
	t.Parallel()

	result := buildReviewPrompt(nil, "", "", "", "file.go (modified)", "some diff")

	if !strings.Contains(result, "(no prompts captured)") {
		t.Error("should show no-prompts placeholder for empty prompts")
	}
	if !strings.Contains(result, "(no commit message)") {
		t.Error("should show placeholder for empty commit message")
	}
	if !strings.Contains(result, "(no session context available)") {
		t.Error("should show placeholder for empty session context")
	}
}

func TestBuildReviewPrompt_TruncatesLargeDiff(t *testing.T) {
	t.Parallel()

	// Create a diff larger than maxDiffSize
	largeDiff := strings.Repeat("x", maxDiffSize+1000)

	result := buildReviewPrompt([]string{"test"}, "", "", "test-session", "file.go", largeDiff)

	if !strings.Contains(result, "diff truncated at 100KB") {
		t.Error("should truncate large diffs")
	}
	// The prompt should not contain the full diff
	if strings.Contains(result, strings.Repeat("x", maxDiffSize+1000)) {
		t.Error("should not contain the full oversized diff")
	}
}

func TestWingmanState_SaveLoad(t *testing.T) {
	// Uses t.Chdir so cannot be parallel
	tmpDir := t.TempDir()

	// Initialize a real git repo (paths.AbsPath needs git rev-parse)
	cmd := exec.CommandContext(context.Background(), "git", "init", tmpDir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git init: %v", err)
	}

	// Create .entire directory
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	t.Chdir(tmpDir)

	state := &WingmanState{
		SessionID:     "test-session",
		FilesHash:     "abc123",
		ReviewApplied: false,
	}

	if err := saveWingmanState(state); err != nil {
		t.Fatalf("failed to save state: %v", err)
	}

	loaded, err := loadWingmanState()
	if err != nil {
		t.Fatalf("failed to load state: %v", err)
	}

	if loaded.SessionID != state.SessionID {
		t.Errorf("session_id: got %q, want %q", loaded.SessionID, state.SessionID)
	}
	if loaded.FilesHash != state.FilesHash {
		t.Errorf("files_hash: got %q, want %q", loaded.FilesHash, state.FilesHash)
	}
	if loaded.ReviewApplied != state.ReviewApplied {
		t.Errorf("review_applied: got %v, want %v", loaded.ReviewApplied, state.ReviewApplied)
	}
}

func TestWingmanStripGitEnv(t *testing.T) {
	t.Parallel()

	env := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"GIT_DIR=/repo/.git",
		"GIT_WORK_TREE=/repo",
		"EDITOR=vim",
	}

	filtered := wingmanStripGitEnv(env)

	for _, e := range filtered {
		if strings.HasPrefix(e, "GIT_") {
			t.Errorf("GIT_ variable should be stripped: %s", e)
		}
	}

	if len(filtered) != 3 {
		t.Errorf("expected 3 non-GIT vars, got %d", len(filtered))
	}
}

func TestIsWingmanEnabled_Settings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		options  map[string]any
		expected bool
	}{
		{
			name:     "nil options",
			options:  nil,
			expected: false,
		},
		{
			name:     "empty options",
			options:  map[string]any{},
			expected: false,
		},
		{
			name:     "wingman not present",
			options:  map[string]any{"summarize": map[string]any{"enabled": true}},
			expected: false,
		},
		{
			name:     "wingman enabled",
			options:  map[string]any{"wingman": map[string]any{"enabled": true}},
			expected: true,
		},
		{
			name:     "wingman disabled",
			options:  map[string]any{"wingman": map[string]any{"enabled": false}},
			expected: false,
		},
		{
			name:     "wingman wrong type",
			options:  map[string]any{"wingman": "invalid"},
			expected: false,
		},
		{
			name:     "wingman enabled wrong type",
			options:  map[string]any{"wingman": map[string]any{"enabled": "yes"}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &EntireSettings{
				StrategyOptions: tt.options,
			}
			if got := s.IsWingmanEnabled(); got != tt.expected {
				t.Errorf("IsWingmanEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestWingmanState_ApplyAttemptedAt_RoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Now().Truncate(time.Second)
	state := &WingmanState{
		SessionID:        "sess-1",
		FilesHash:        "hash1",
		ReviewedAt:       now,
		ReviewApplied:    false,
		ApplyAttemptedAt: &now,
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded WingmanState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.ApplyAttemptedAt == nil {
		t.Fatal("ApplyAttemptedAt should not be nil after round-trip")
	}
	if !decoded.ApplyAttemptedAt.Truncate(time.Second).Equal(now) {
		t.Errorf("ApplyAttemptedAt: got %v, want %v", decoded.ApplyAttemptedAt, now)
	}
}

func TestWingmanState_ApplyAttemptedAt_OmitEmpty(t *testing.T) {
	t.Parallel()

	state := &WingmanState{
		SessionID: "sess-1",
		FilesHash: "hash1",
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	if strings.Contains(string(data), "apply_attempted_at") {
		t.Error("ApplyAttemptedAt should be omitted when nil")
	}
}

func TestLoadWingmanStateDirect(t *testing.T) {
	t.Parallel()

	t.Run("missing file returns nil", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		got := loadWingmanStateDirect(tmpDir)
		if got != nil {
			t.Errorf("expected nil for missing file, got %+v", got)
		}
	})

	t.Run("valid file returns state", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		entireDir := filepath.Join(tmpDir, ".entire")
		if err := os.MkdirAll(entireDir, 0o755); err != nil {
			t.Fatal(err)
		}

		now := time.Now()
		stateJSON := `{"session_id":"sess-1","files_hash":"hash1","reviewed_at":"` + now.Format(time.RFC3339Nano) + `","review_applied":false}`
		if err := os.WriteFile(filepath.Join(entireDir, "wingman-state.json"), []byte(stateJSON), 0o644); err != nil {
			t.Fatal(err)
		}

		got := loadWingmanStateDirect(tmpDir)
		if got == nil {
			t.Fatal("expected non-nil state")
		}
		if got.SessionID != "sess-1" {
			t.Errorf("SessionID: got %q, want %q", got.SessionID, "sess-1")
		}
	})
}

func TestShouldSkipPendingReview_NoReviewFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}

	if shouldSkipPendingReview(tmpDir, "sess-1") {
		t.Error("should not skip when no REVIEW.md exists")
	}
}

func TestShouldSkipPendingReview_SameSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write REVIEW.md
	if err := os.WriteFile(filepath.Join(entireDir, "REVIEW.md"), []byte("review"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write state with same session
	saveWingmanStateDirect(tmpDir, &WingmanState{
		SessionID:  "sess-1",
		FilesHash:  "hash1",
		ReviewedAt: time.Now(),
	})

	if !shouldSkipPendingReview(tmpDir, "sess-1") {
		t.Error("should skip when same session has fresh review")
	}
}

func TestShouldSkipPendingReview_DifferentSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	reviewPath := filepath.Join(entireDir, "REVIEW.md")
	if err := os.WriteFile(reviewPath, []byte("stale review"), 0o644); err != nil {
		t.Fatal(err)
	}

	saveWingmanStateDirect(tmpDir, &WingmanState{
		SessionID:  "old-session",
		FilesHash:  "hash1",
		ReviewedAt: time.Now(),
	})

	if shouldSkipPendingReview(tmpDir, "new-session") {
		t.Error("should not skip when review is from different session")
	}

	// REVIEW.md should have been cleaned up
	if _, err := os.Stat(reviewPath); err == nil {
		t.Error("stale REVIEW.md should have been deleted")
	}
}

func TestShouldSkipPendingReview_StaleTTL(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	reviewPath := filepath.Join(entireDir, "REVIEW.md")
	if err := os.WriteFile(reviewPath, []byte("old review"), 0o644); err != nil {
		t.Fatal(err)
	}

	// State with same session but old ReviewedAt
	saveWingmanStateDirect(tmpDir, &WingmanState{
		SessionID:  "sess-1",
		FilesHash:  "hash1",
		ReviewedAt: time.Now().Add(-2 * time.Hour), // 2 hours old
	})

	if shouldSkipPendingReview(tmpDir, "sess-1") {
		t.Error("should not skip when review is older than TTL")
	}

	if _, err := os.Stat(reviewPath); err == nil {
		t.Error("stale REVIEW.md should have been deleted")
	}
}

func TestHasAnyLiveSession_NoSessionDir(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// No .git at all
	if hasAnyLiveSession(tmpDir) {
		t.Error("should return false with no .git directory")
	}
}

func TestHasAnyLiveSession_EmptySessionDir(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "entire-sessions"), 0o755); err != nil {
		t.Fatal(err)
	}

	if hasAnyLiveSession(tmpDir) {
		t.Error("should return false with empty session dir")
	}
}

func TestHasAnyLiveSession_IdleSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create an IDLE session
	if err := os.WriteFile(filepath.Join(sessDir, "sess-idle.json"), []byte(`{"phase":"idle"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !hasAnyLiveSession(tmpDir) {
		t.Error("should return true when IDLE session exists")
	}
}

func TestHasAnyLiveSession_ActiveSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(sessDir, "sess-active.json"), []byte(`{"phase":"active"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !hasAnyLiveSession(tmpDir) {
		t.Error("should return true when ACTIVE session exists")
	}
}

func TestHasAnyLiveSession_AllEnded(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(sessDir, "sess-1.json"), []byte(`{"phase":"ended"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess-2.json"), []byte(`{"phase":"ended"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if hasAnyLiveSession(tmpDir) {
		t.Error("should return false when all sessions are ended")
	}
}

func TestHasAnyLiveSession_MixedPhases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(sessDir, "sess-ended.json"), []byte(`{"phase":"ended"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess-idle.json"), []byte(`{"phase":"idle"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !hasAnyLiveSession(tmpDir) {
		t.Error("should return true when at least one non-ended session exists")
	}
}

func TestLoadSettingsTarget_Local(t *testing.T) {
	// Uses t.Chdir so cannot be parallel
	tmpDir := t.TempDir()

	cmd := exec.CommandContext(context.Background(), "git", "init", tmpDir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git init: %v", err)
	}

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write project settings with wingman disabled
	projectSettings := `{"strategy": "manual-commit", "enabled": true, "strategy_options": {"wingman": {"enabled": false}}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(projectSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write local settings with different strategy
	localSettings := `{"strategy": "` + strategyDisplayAutoCommit + `"}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(tmpDir)

	t.Run("local=false returns merged settings", func(t *testing.T) {
		s, err := loadSettingsTarget(false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Merged: local overrides project strategy
		if s.Strategy != strategyDisplayAutoCommit {
			t.Errorf("Strategy = %q, want %q", s.Strategy, strategyDisplayAutoCommit)
		}
	})

	t.Run("local=true returns only local settings", func(t *testing.T) {
		s, err := loadSettingsTarget(true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.Strategy != strategyDisplayAutoCommit {
			t.Errorf("Strategy = %q, want %q", s.Strategy, strategyDisplayAutoCommit)
		}
		// Local file has no wingman settings
		if s.IsWingmanEnabled() {
			t.Error("local settings should not have wingman enabled")
		}
	})
}

func TestSaveSettingsTarget_Local(t *testing.T) {
	// Uses t.Chdir so cannot be parallel
	tmpDir := t.TempDir()

	cmd := exec.CommandContext(context.Background(), "git", "init", tmpDir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git init: %v", err)
	}

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(tmpDir)

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"wingman": map[string]any{"enabled": true},
		},
	}

	t.Run("local=true saves to local file", func(t *testing.T) {
		if err := saveSettingsTarget(s, true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		localPath := filepath.Join(entireDir, "settings.local.json")
		data, err := os.ReadFile(localPath)
		if err != nil {
			t.Fatalf("local settings file should exist: %v", err)
		}
		if !strings.Contains(string(data), `"wingman"`) {
			t.Error("local settings should contain wingman config")
		}

		// Project file should not exist
		projectPath := filepath.Join(entireDir, "settings.json")
		if _, err := os.Stat(projectPath); err == nil {
			t.Error("project settings file should not have been created")
		}
	})

	t.Run("local=false saves to project file", func(t *testing.T) {
		if err := saveSettingsTarget(s, false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		projectPath := filepath.Join(entireDir, "settings.json")
		data, err := os.ReadFile(projectPath)
		if err != nil {
			t.Fatalf("project settings file should exist: %v", err)
		}
		if !strings.Contains(string(data), `"wingman"`) {
			t.Error("project settings should contain wingman config")
		}
	})
}

func TestShouldSkipPendingReview_OrphanNoState(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	reviewPath := filepath.Join(entireDir, "REVIEW.md")
	if err := os.WriteFile(reviewPath, []byte("orphan review"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No state file
	if shouldSkipPendingReview(tmpDir, "sess-1") {
		t.Error("should not skip when no state file exists (orphan)")
	}

	if _, err := os.Stat(reviewPath); err == nil {
		t.Error("orphan REVIEW.md should have been deleted")
	}
}

func TestHasAnyLiveSession_StaleSessionSkipped(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create an ACTIVE session file with a very old modification time
	sessFile := filepath.Join(sessDir, "stale-active.json")
	if err := os.WriteFile(sessFile, []byte(`{"phase":"active"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Set modification time to 3 hours ago (beyond staleSessionThreshold)
	staleTime := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(sessFile, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	if hasAnyLiveSession(tmpDir) {
		t.Error("should return false when only session is stale (3h old)")
	}
}

func TestHasAnyLiveSession_FreshSessionNotSkipped(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a stale ACTIVE session (should be skipped)
	staleFile := filepath.Join(sessDir, "stale.json")
	if err := os.WriteFile(staleFile, []byte(`{"phase":"active_committed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(staleFile, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	// Create a fresh IDLE session (should not be skipped)
	freshFile := filepath.Join(sessDir, "fresh.json")
	if err := os.WriteFile(freshFile, []byte(`{"phase":"idle"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !hasAnyLiveSession(tmpDir) {
		t.Error("should return true when a fresh live session exists alongside stale ones")
	}
}

func TestRunWingmanApply_EndedPhaseProceeds(t *testing.T) {
	t.Parallel()

	// This test verifies that runWingmanApply does NOT abort when the session
	// phase is ENDED (the bug fix). We can't test the full auto-apply (it
	// spawns claude CLI), but we can verify it passes the phase check.
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create REVIEW.md
	reviewPath := filepath.Join(entireDir, "REVIEW.md")
	if err := os.WriteFile(reviewPath, []byte("review content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create wingman state
	saveWingmanStateDirect(tmpDir, &WingmanState{
		SessionID:  "sess-ended",
		FilesHash:  "hash1",
		ReviewedAt: time.Now(),
	})

	// Create session state dir with ENDED phase
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess-ended.json"), []byte(`{"phase":"ended"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// runWingmanApply will pass the phase check but fail at triggerAutoApply
	// (no claude CLI). The important thing is it doesn't return nil early
	// with "session became active" — it should get past the phase check.
	err := runWingmanApply(tmpDir)
	if err == nil {
		t.Log("runWingmanApply returned nil (auto-apply succeeded or no claude CLI)")
	} else {
		// Expected: fails at triggerAutoApply because claude CLI isn't available
		if strings.Contains(err.Error(), "session became active") {
			t.Error("should not abort with 'session became active' for ENDED phase")
		}
		t.Logf("runWingmanApply failed as expected (no claude CLI): %v", err)
	}

	// Verify apply was attempted (state should be updated)
	state := loadWingmanStateDirect(tmpDir)
	if state == nil {
		t.Fatal("expected wingman state to exist")
	}
	if state.ApplyAttemptedAt == nil {
		t.Error("ApplyAttemptedAt should be set after passing phase check")
	}
}

func TestRunWingmanApply_ActivePhaseAborts(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create REVIEW.md
	reviewPath := filepath.Join(entireDir, "REVIEW.md")
	if err := os.WriteFile(reviewPath, []byte("review content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create wingman state
	saveWingmanStateDirect(tmpDir, &WingmanState{
		SessionID:  "sess-active",
		FilesHash:  "hash1",
		ReviewedAt: time.Now(),
	})

	// Create session state dir with ACTIVE phase
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess-active.json"), []byte(`{"phase":"active"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// runWingmanApply should return nil (aborted) without attempting apply
	err := runWingmanApply(tmpDir)
	if err != nil {
		t.Errorf("expected nil error (clean abort), got: %v", err)
	}

	// Verify apply was NOT attempted
	state := loadWingmanStateDirect(tmpDir)
	if state != nil && state.ApplyAttemptedAt != nil {
		t.Error("ApplyAttemptedAt should not be set when phase is ACTIVE")
	}
}

func TestRunWingmanApply_ActiveCommittedPhaseAborts(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create REVIEW.md
	if err := os.WriteFile(filepath.Join(entireDir, "REVIEW.md"), []byte("review"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create wingman state
	saveWingmanStateDirect(tmpDir, &WingmanState{
		SessionID:  "sess-ac",
		FilesHash:  "hash1",
		ReviewedAt: time.Now(),
	})

	// Create session state dir with ACTIVE_COMMITTED phase
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess-ac.json"), []byte(`{"phase":"active_committed"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runWingmanApply(tmpDir)
	if err != nil {
		t.Errorf("expected nil error (clean abort), got: %v", err)
	}

	// Verify apply was NOT attempted
	state := loadWingmanStateDirect(tmpDir)
	if state != nil && state.ApplyAttemptedAt != nil {
		t.Error("ApplyAttemptedAt should not be set when phase is ACTIVE_COMMITTED")
	}
}

func TestRunWingmanApply_IdlePhaseProceeds(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create REVIEW.md
	if err := os.WriteFile(filepath.Join(entireDir, "REVIEW.md"), []byte("review"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create wingman state
	saveWingmanStateDirect(tmpDir, &WingmanState{
		SessionID:  "sess-idle",
		FilesHash:  "hash1",
		ReviewedAt: time.Now(),
	})

	// Create session state dir with IDLE phase
	sessDir := filepath.Join(tmpDir, ".git", "entire-sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess-idle.json"), []byte(`{"phase":"idle"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should pass phase check (IDLE is safe) then fail at triggerAutoApply
	err := runWingmanApply(tmpDir)

	// Verify apply was attempted (passes the phase check)
	state := loadWingmanStateDirect(tmpDir)
	if state == nil {
		t.Fatal("expected wingman state to exist")
	}
	if state.ApplyAttemptedAt == nil {
		t.Error("ApplyAttemptedAt should be set after passing phase check")
	}
	// We expect an error from triggerAutoApply (no claude CLI), that's fine
	_ = err
}
