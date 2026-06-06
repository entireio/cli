package investigate

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixLaunchRecord captures the (agentName, prompt) pair Launch was called
// with so the test can assert what RunFix forwarded to the launcher.
type fixLaunchRecord struct {
	called    bool
	agentName string
	prompt    string
}

// stubLaunch returns a Launch function that records its arguments into
// rec. The returned function always reports success; tests that need a
// failing launch can substitute their own closure.
func stubLaunch(rec *fixLaunchRecord) func(context.Context, string, string) error {
	return func(_ context.Context, agentName, prompt string) error {
		rec.called = true
		rec.agentName = agentName
		rec.prompt = prompt
		return nil
	}
}

// writeFixManifest is a shorthand for tests: build a manifest with the
// supplied identity and persist it to store. RunID/Topic/StartedAt are
// the discriminators tests care about; the rest is filled with sensible
// defaults so the manifest passes Write validation.
func writeFixManifest(t *testing.T, store *LocalManifestStore, runID, topic string, started time.Time, findingsDoc string) {
	t.Helper()
	m := LocalManifest{
		RunID:       runID,
		Topic:       topic,
		Slug:        SlugifyTopic(topic),
		StartingSHA: "deadbeefcafe",
		FindingsDoc: findingsDoc,
		Agents:      []string{"claude-code", "codex"},
		Outcome:     "quorum",
		StartedAt:   started,
		EndedAt:     started.Add(10 * time.Minute),
	}
	if err := store.Write(context.Background(), m); err != nil {
		t.Fatalf("Write %s: %v", runID, err)
	}
}

func TestRunFix_PicksMostRecent(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	writeFixManifest(t, store, "aaaaaaaaaaaa", "older topic", t1, "")
	writeFixManifest(t, store, "bbbbbbbbbbbb", "newest topic", t2, "")

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}},
		FixDeps{
			ManifestStore: store,
			Launch:        stubLaunch(&rec),
		},
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !rec.called {
		t.Fatal("Launch was not called")
	}
	if !strings.Contains(rec.prompt, "newest topic") {
		t.Errorf("prompt did not reference newest topic: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, `<untrusted source="investigation-prompt">`) {
		t.Errorf("prompt should wrap the investigation prompt in an untrusted block: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, "Run ID: bbbbbbbbbbbb") {
		t.Errorf("prompt did not reference newest run ID: %q", rec.prompt)
	}
}

func TestRunFix_ResolvesByRunID(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	writeFixManifest(t, store, "aaaaaaaaaaaa", "older topic", t1, "")
	writeFixManifest(t, store, "bbbbbbbbbbbb", "newest topic", t2, "")

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{RunID: "aaaaaaaaaaaa", Out: &bytes.Buffer{}},
		FixDeps{
			ManifestStore: store,
			Launch:        stubLaunch(&rec),
		},
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !strings.Contains(rec.prompt, "older topic") {
		t.Errorf("prompt should target the requested run, got: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, "Run ID: aaaaaaaaaaaa") {
		t.Errorf("prompt should reference the requested run id, got: %q", rec.prompt)
	}
}

func TestRunFix_RunIDNotFound(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	writeFixManifest(t, store, "aaaaaaaaaaaa", "topic", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC), "")

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{RunID: "ffffffffffff"},
		FixDeps{
			ManifestStore: store,
			Launch:        stubLaunch(&rec),
		},
	)
	if err == nil {
		t.Fatal("expected error for missing run id, got nil")
	}
	if !strings.Contains(err.Error(), "ffffffffffff") {
		t.Errorf("error should mention the run id, got: %v", err)
	}
	if rec.called {
		t.Error("Launch must not be called when manifest resolution fails")
	}
}

func TestRunFix_NoManifests(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{},
		FixDeps{
			ManifestStore: store,
			Launch:        stubLaunch(&rec),
		},
	)
	if err == nil {
		t.Fatal("expected error for empty store, got nil")
	}
	if !strings.Contains(err.Error(), "no local investigations found") {
		t.Errorf("unexpected error message: %v", err)
	}
	if rec.called {
		t.Error("Launch must not be called when no manifests exist")
	}
}

func TestRunFix_ComposesPromptBody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	findings := "## Finding 1\n\nThe checkout button times out after 30s.\n"
	store := NewLocalManifestStoreWithDir(dir)
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	// Absolute sentinel — readDocOrWarn rejects relative paths.
	findingsPath := filepath.Join(dir, "findings-sentinel.md")
	writeFixManifest(t, store, "abcdef012345", "Why is checkout flaky?", now,
		findingsPath,
	)

	read := func(name string) ([]byte, error) {
		if name == findingsPath {
			return []byte(findings), nil
		}
		t.Fatalf("unexpected ReadFile path: %q", name)
		return nil, errors.New("unreachable")
	}

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}},
		FixDeps{
			ManifestStore: store,
			FixAgent:      "test-agent",
			Launch:        stubLaunch(&rec),
			ReadFile:      read,
		},
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if rec.agentName != "test-agent" {
		t.Errorf("agentName = %q, want test-agent", rec.agentName)
	}
	if !strings.Contains(rec.prompt, "Do not re-investigate the same") {
		t.Errorf("prompt missing the 'do not re-investigate' preamble: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, "## Investigation findings") {
		t.Errorf("prompt missing findings section heading: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, strings.TrimSpace(findings)) {
		t.Errorf("prompt missing findings body verbatim: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, "Why is checkout flaky?") {
		t.Errorf("prompt missing investigation prompt: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, `<untrusted source="prior-findings">`) {
		t.Errorf("prompt should wrap findings in an untrusted block: %q", rec.prompt)
	}
}

func TestRunFix_TolerateMissingDocs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	// Manifest references a findings file that does not exist in dir.
	writeFixManifest(t, store, "abcdef012345", "topic", now,
		filepath.Join(dir, "missing-findings.md"),
	)

	var rec fixLaunchRecord
	var errBuf bytes.Buffer
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}, ErrOut: &errBuf},
		FixDeps{
			ManifestStore: store,
			Launch:        stubLaunch(&rec),
		},
	)
	if err != nil {
		t.Fatalf("RunFix should tolerate missing docs, got: %v", err)
	}
	if !rec.called {
		t.Fatal("Launch was not called despite tolerable missing docs")
	}
	if !strings.Contains(rec.prompt, "(no findings recorded)") {
		t.Errorf("prompt should note absent findings: %q", rec.prompt)
	}
	if !strings.Contains(errBuf.String(), "warning: could not read") {
		t.Errorf("expected warnings about missing docs, got: %q", errBuf.String())
	}
}

// TestRunFix_PrefersFindingsContentOverDoc verifies that when the
// manifest has FindingsContent embedded (terminal outcomes have the
// per-run dir auto-cleaned by R3, so FindingsDoc points at a deleted
// path), RunFix uses the embedded content instead of warning about the
// missing file.
func TestRunFix_PrefersFindingsContentOverDoc(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "topic",
		Slug:            SlugifyTopic("topic"),
		StartingSHA:     "deadbeefcafe",
		FindingsDoc:     filepath.Join(dir, "deleted-findings.md"),
		FindingsContent: "# Investigation: topic\n\nembedded findings body\n",
		Agents:          []string{"claude-code"},
		Outcome:         "quorum",
		StartedAt:       now,
		EndedAt:         now.Add(10 * time.Minute),
	}
	if err := store.Write(context.Background(), m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var rec fixLaunchRecord
	var errBuf bytes.Buffer
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}, ErrOut: &errBuf},
		FixDeps{ManifestStore: store, Launch: stubLaunch(&rec)},
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !strings.Contains(rec.prompt, "embedded findings body") {
		t.Errorf("prompt should embed manifest.FindingsContent, got: %q", rec.prompt)
	}
	if strings.Contains(errBuf.String(), "could not read") {
		t.Errorf("expected no missing-doc warning when FindingsContent is set, got: %q", errBuf.String())
	}
}

func TestRunFix_FallsBackToDefaultFixAgent(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(t.TempDir())
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writeFixManifest(t, store, "abcdef012345", "topic", now, "")

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}},
		FixDeps{
			ManifestStore: store,
			Launch:        stubLaunch(&rec),
		},
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if rec.agentName != defaultFixAgent {
		t.Errorf("agentName = %q, want default %q", rec.agentName, defaultFixAgent)
	}
}
