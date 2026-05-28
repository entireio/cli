package investigate_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
)

// TestRunInvestigateFindings_NoManifests checks that an empty store
// produces an actionable empty-state line and returns nil.
func TestRunInvestigateFindings_NoManifests(t *testing.T) {
	setupInvestigateRepo(t)

	deps := newTestDeps(t, []types.AgentName{"a"}, []string{"a"})
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--findings"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "No local investigations found") {
		t.Errorf("expected empty-state message, got: %s", out.String())
	}
}

// TestRunInvestigateFindings_PrintsListNonTTY writes 2 manifests and
// verifies that --findings (non-TTY because cmd.SetOut isn't a terminal)
// lists both run-ids with fix hints.
func TestRunInvestigateFindings_PrintsListNonTTY(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	store := investigate.NewLocalManifestStoreWithDir(tmp + "/manifests")
	now := time.Now().UTC()
	if err := store.Write(context.Background(), investigate.LocalManifest{
		RunID:     "aaaaaaaaaaaa",
		Topic:     "first topic",
		Slug:      "first-topic",
		Agents:    []string{"agent-1"},
		Outcome:   "quorum",
		StartedAt: now.Add(-2 * time.Hour),
		EndedAt:   now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(context.Background(), investigate.LocalManifest{
		RunID:     "bbbbbbbbbbbb",
		Topic:     "second topic",
		Slug:      "second-topic",
		Agents:    []string{"agent-2"},
		Outcome:   "stalled",
		StartedAt: now,
		EndedAt:   now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	// Use printInvestigateFindingsList indirectly via a stub manifest store
	// — the cmd-driven path uses NewLocalManifestStore (git common dir),
	// not the per-test dir, so we exercise the list helper through its
	// public consumer. List() returns newest-first, then printer renders.
	out := &bytes.Buffer{}
	manifests, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 2 {
		t.Fatalf("List returned %d manifests, want 2", len(manifests))
	}
	investigate.PrintInvestigateFindingsListForTest(out, manifests)

	got := out.String()
	for _, want := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "first topic", "second topic", "entire investigate fix"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestRunInvestigateFindings_PrintsCapturedMarker verifies that
// manifests whose findings have been embedded into FindingsContent
// (terminal outcomes) advertise the capture rather than a now-stale
// file path. Manifests with only a FindingsDoc fall back to printing
// the path.
func TestRunInvestigateFindings_PrintsCapturedMarker(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	manifests := []investigate.LocalManifest{
		{
			RunID:           "aaaaaaaaaaaa",
			Topic:           "captured run",
			Slug:            "captured-run",
			Agents:          []string{"a"},
			Outcome:         "quorum",
			FindingsDoc:     "/stale/path/findings.md",
			FindingsContent: "# Findings\n\nbody\n",
			StartedAt:       now,
			EndedAt:         now,
		},
		{
			RunID:       "bbbbbbbbbbbb",
			Topic:       "paused run",
			Slug:        "paused-run",
			Agents:      []string{"b"},
			Outcome:     "paused",
			FindingsDoc: "/live/path/findings.md",
			StartedAt:   now,
			EndedAt:     now,
		},
	}

	out := &bytes.Buffer{}
	investigate.PrintInvestigateFindingsListForTest(out, manifests)
	got := out.String()

	// Both rows must surface a `view:` hint pointing at the show
	// subcommand — that's the actionable next step regardless of where
	// the findings live.
	if !strings.Contains(got, "  view:    entire investigate show aaaaaaaaaaaa") {
		t.Errorf("expected view hint for terminal run, got:\n%s", got)
	}
	if !strings.Contains(got, "  view:    entire investigate show bbbbbbbbbbbb") {
		t.Errorf("expected view hint for paused run, got:\n%s", got)
	}
	// Terminal outcome → `fix:` hint; paused → `resume:` hint instead.
	if !strings.Contains(got, "  fix:     entire investigate fix aaaaaaaaaaaa") {
		t.Errorf("expected fix hint for terminal run, got:\n%s", got)
	}
	if !strings.Contains(got, "  resume:  entire investigate --continue bbbbbbbbbbbb") {
		t.Errorf("expected resume hint for paused run, got:\n%s", got)
	}
	if strings.Contains(got, "entire investigate fix bbbbbbbbbbbb") {
		t.Errorf("paused run must not advertise `fix` (no terminal findings), got:\n%s", got)
	}
	// Paused run still has its findings.md on disk — surface the path
	// for direct inspection. Terminal run's path is stale (per-run dir
	// auto-cleaned) so it must not be printed.
	if !strings.Contains(got, "  path:    /live/path/findings.md") {
		t.Errorf("expected file path for paused run, got:\n%s", got)
	}
	if strings.Contains(got, "/stale/path/findings.md") {
		t.Errorf("should NOT print stale path when findings are captured, got:\n%s", got)
	}
	if strings.Contains(got, "<captured in manifest>") {
		t.Errorf("legacy `<captured in manifest>` placeholder should be gone, got:\n%s", got)
	}
}
