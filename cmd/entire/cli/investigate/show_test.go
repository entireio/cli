package investigate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeShowManifest persists a LocalManifest with the supplied identity
// to store. Mirrors writeFixManifest but accepts the additional
// findingsContent / stancesByAgent fields the show tests care about.
func writeShowManifest(
	t *testing.T,
	store *LocalManifestStore,
	runID, topic string,
	started time.Time,
	outcome string,
	findingsDoc string,
	findingsContent string,
	stances map[string]string,
) {
	t.Helper()
	m := LocalManifest{
		RunID:           runID,
		Topic:           topic,
		Slug:            SlugifyTopic(topic),
		StartingSHA:     "deadbeefcafe",
		FindingsDoc:     findingsDoc,
		FindingsContent: findingsContent,
		Agents:          []string{"claude-code", "codex"},
		Outcome:         outcome,
		StancesByAgent:  stances,
		StartedAt:       started,
		EndedAt:         started.Add(10 * time.Minute),
	}
	if err := store.Write(context.Background(), m); err != nil {
		t.Fatalf("Write %s: %v", runID, err)
	}
}

func TestRunShow_NoManifestsPrintsEmpty(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())

	var out bytes.Buffer
	err := RunShow(context.Background(), ShowInput{Out: &out}, ShowDeps{ManifestStore: store})
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	if !strings.Contains(out.String(), "No local investigations found.") {
		t.Errorf("expected empty-store notice, got: %q", out.String())
	}
}

func TestRunShow_SingleManifestDefaults(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "abcdef012345", "only topic", t1, "quorum", "",
		"## Findings\n\nThe answer is 42.\n",
		map[string]string{"claude-code": "agree", "codex": "agree"},
	)

	var out bytes.Buffer
	err := RunShow(context.Background(), ShowInput{Out: &out}, ShowDeps{ManifestStore: store})
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "Investigation abcdef012345") {
		t.Errorf("output missing header: %q", s)
	}
	if !strings.Contains(s, "Prompt:   only topic") {
		t.Errorf("output missing prompt: %q", s)
	}
	if !strings.Contains(s, "Outcome:  quorum") {
		t.Errorf("output missing outcome: %q", s)
	}
	if !strings.Contains(s, "The answer is 42.") {
		t.Errorf("output missing findings body: %q", s)
	}
	if !strings.Contains(s, "claude-code: agree") {
		t.Errorf("output missing stance entries: %q", s)
	}
}

func TestRunShow_MultipleManifestsRequiresID(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "aaaaaaaaaaaa", "first topic", t1, "quorum", "", "first body\n", nil)
	writeShowManifest(t, store, "bbbbbbbbbbbb", "second topic", t2, "stalled", "", "second body\n", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(), ShowInput{Out: &out}, ShowDeps{ManifestStore: store})
	if err == nil {
		t.Fatal("expected error when multiple manifests and no run id, got nil")
	}
	if !strings.Contains(err.Error(), "multiple investigations available") {
		t.Errorf("expected guidance about multiple investigations, got: %v", err)
	}
	if !strings.Contains(err.Error(), "aaaaaaaaaaaa") || !strings.Contains(err.Error(), "bbbbbbbbbbbb") {
		t.Errorf("expected both run ids in the listing, got: %v", err)
	}
}

func TestRunShow_ExactRunIDMatch(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "aaaaaaaaaaaa", "first topic", t1, "quorum", "", "first body\n", nil)
	writeShowManifest(t, store, "bbbbbbbbbbbb", "second topic", t2, "quorum", "", "second body\n", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(),
		ShowInput{RunID: "aaaaaaaaaaaa", Out: &out},
		ShowDeps{ManifestStore: store},
	)
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "Investigation aaaaaaaaaaaa") {
		t.Errorf("expected the requested run, got: %q", s)
	}
	if !strings.Contains(s, "first body") {
		t.Errorf("expected the requested findings body, got: %q", s)
	}
	if strings.Contains(s, "second body") {
		t.Errorf("output should not include other run's findings: %q", s)
	}
}

func TestRunShow_PrefixMatchUnique(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "aabbccddeeff", "alpha", t1, "quorum", "", "alpha body\n", nil)
	writeShowManifest(t, store, "112233445566", "beta", t2, "quorum", "", "beta body\n", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(),
		ShowInput{RunID: "aabb", Out: &out},
		ShowDeps{ManifestStore: store},
	)
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	if !strings.Contains(out.String(), "Investigation aabbccddeeff") {
		t.Errorf("prefix match should resolve to aabbccddeeff, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "alpha body") {
		t.Errorf("expected alpha findings, got: %q", out.String())
	}
}

func TestRunShow_PrefixMatchAmbiguous(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "aabbccddeeff", "alpha", t1, "quorum", "", "alpha body\n", nil)
	writeShowManifest(t, store, "aabb11223344", "beta", t2, "quorum", "", "beta body\n", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(),
		ShowInput{RunID: "aabb", Out: &out},
		ShowDeps{ManifestStore: store},
	)
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous run id prefix") {
		t.Errorf("expected ambiguity message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "aabbccddeeff") || !strings.Contains(err.Error(), "aabb11223344") {
		t.Errorf("expected both candidate ids in error, got: %v", err)
	}
}

func TestRunShow_NoSuchRunID(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "aabbccddeeff", "alpha", t1, "quorum", "", "alpha body\n", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(),
		ShowInput{RunID: "ffff", Out: &out},
		ShowDeps{ManifestStore: store},
	)
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "no investigation found") {
		t.Errorf("expected not-found message, got: %v", err)
	}
}

func TestRunShow_PrintsFindingsContentWhenEmbedded(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	body := "## Hypothesis\n\nThe build is slow because of npm.\n"
	// FindingsDoc points at a non-existent path on purpose — embedded content must win.
	writeShowManifest(t, store, "abcdef012345", "build perf", t1, "quorum",
		"/tmp/does-not-exist.md", body, nil)

	var out bytes.Buffer
	err := RunShow(context.Background(), ShowInput{Out: &out}, ShowDeps{ManifestStore: store})
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	if !strings.Contains(out.String(), body) {
		t.Errorf("expected embedded findings content verbatim, got: %q", out.String())
	}
}

func TestRunShow_FallsBackToFindingsDocOnDisk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)

	findingsPath := filepath.Join(dir, "findings.md")
	body := "## Resumable run\n\nPartial progress only.\n"
	if err := os.WriteFile(findingsPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write findings: %v", err)
	}

	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	// FindingsContent is empty (paused/cancelled run) — disk read must succeed.
	writeShowManifest(t, store, "abcdef012345", "paused topic", t1, "paused",
		findingsPath, "", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(), ShowInput{Out: &out}, ShowDeps{ManifestStore: store})
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	if !strings.Contains(out.String(), "Partial progress only.") {
		t.Errorf("expected on-disk findings body, got: %q", out.String())
	}
}

func TestRunShow_NoContentAvailable(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	// Both empty content and missing on-disk doc — soft notice should fire.
	writeShowManifest(t, store, "abcdef012345", "lost run", t1, "cancelled",
		"/tmp/this/path/does/not/exist.md", "", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(), ShowInput{Out: &out}, ShowDeps{ManifestStore: store})
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	if !strings.Contains(out.String(), "No findings content available for run abcdef012345.") {
		t.Errorf("expected soft no-content notice, got: %q", out.String())
	}
}
