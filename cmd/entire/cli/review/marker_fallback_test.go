package review_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// setupMarkerTestRepo initialises a temp git repo and chdirs into it.
func setupMarkerTestRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	return tmp
}

func TestReviewMarker_RoundTrip(t *testing.T) {
	tmp := setupMarkerTestRepo(t)

	m := review.PendingReviewMarker{
		AgentName:   "claude-code",
		Skills:      []string{"/pr-review-toolkit:review-pr"},
		Prompt:      "Please run these review skills in order:\n  1. /pr-review-toolkit:review-pr\n",
		StartingSHA: "deadbeef",
		StartedAt:   time.Now().UTC(),
	}
	ctx := context.Background()
	if err := review.WritePendingReviewMarker(ctx, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok, err := review.ReadPendingReviewMarker(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok {
		t.Fatal("expected marker present")
	}
	if got.AgentName != m.AgentName || got.StartingSHA != m.StartingSHA {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.Prompt != m.Prompt {
		t.Errorf("Prompt roundtrip mismatch: got %q want %q", got.Prompt, m.Prompt)
	}

	// Marker file must live under .git/entire-sessions/, not the worktree.
	markerGlob := filepath.Join(tmp, ".git", "entire-sessions", "*")
	entries, err := filepath.Glob(markerGlob)
	if err != nil {
		t.Fatalf("glob sessions dir: %v", err)
	}
	if len(entries) == 0 {
		t.Errorf("no marker file found under %s — path resolution may have regressed", markerGlob)
	}

	if err := review.ClearPendingReviewMarker(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	_, ok, err = review.ReadPendingReviewMarker(ctx)
	if err != nil {
		t.Fatalf("read-after-clear: %v", err)
	}
	if ok {
		t.Error("expected marker absent after clear")
	}
}

func TestRunMarkerFallback_WritesMarkerAndPrintsGuidance(t *testing.T) {
	setupMarkerTestRepo(t)

	ctx := context.Background()
	cfg := reviewtypes.RunConfig{
		Skills:      []string{"/review-pr", "/test-auditor"},
		StartingSHA: "abc123",
	}
	var buf bytes.Buffer
	if err := review.RunMarkerFallback(ctx, "cursor", cfg, "/worktrees/myrepo", &buf); err != nil {
		t.Fatalf("RunMarkerFallback: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Marker written") {
		t.Errorf("expected 'Marker written' in output, got: %s", out)
	}

	m, ok, err := review.ReadPendingReviewMarker(ctx)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !ok {
		t.Fatal("marker should have been written")
	}
	if m.AgentName != "cursor" {
		t.Errorf("AgentName = %q, want cursor", m.AgentName)
	}
	if m.StartingSHA != "abc123" {
		t.Errorf("StartingSHA = %q, want abc123", m.StartingSHA)
	}
	if m.WorktreePath != "/worktrees/myrepo" {
		t.Errorf("WorktreePath = %q, want /worktrees/myrepo", m.WorktreePath)
	}
}

func TestRunMarkerFallback_DoesNotCallRun(t *testing.T) {
	// This test pins that RunMarkerFallback never invokes review.Run:
	// the marker-based path is for non-launchable agents only. We verify
	// by ensuring the marker exists after the call (Run would have cleared
	// it via the cleanup defer in the launchable path).
	setupMarkerTestRepo(t)

	ctx := context.Background()
	cfg := reviewtypes.RunConfig{
		Skills:      []string{"/pr-review-toolkit:review-pr"},
		StartingSHA: "deadbeef",
	}
	var buf bytes.Buffer
	if err := review.RunMarkerFallback(ctx, "opencode", cfg, "", &buf); err != nil {
		t.Fatalf("RunMarkerFallback: %v", err)
	}

	_, ok, err := review.ReadPendingReviewMarker(ctx)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !ok {
		t.Error("marker was cleared — RunMarkerFallback must NOT call Run() or install a cleanup defer")
	}
}
