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
	stateStore := NewStateStoreWithDir(t.TempDir())
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	const runID = "abcdef012345"
	// FindingsDoc is display-only now (see manifest.go) — RunFix resolves
	// findings by RunID through the state store, never by following this
	// path. Point it at an arbitrary path to prove that.
	writeFixManifest(t, store, runID, "Why is checkout flaky?", now, "/nonexistent/decoy-path.md")
	if err := stateStore.WriteFindings(runID, []byte(findings)); err != nil {
		t.Fatalf("WriteFindings: %v", err)
	}

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}},
		FixDeps{
			ManifestStore: store,
			StateStore:    stateStore,
			FixAgent:      "test-agent",
			Launch:        stubLaunch(&rec),
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

// TestRunFix_IgnoresFindingsDoc_ArbitraryFileRead is a genuine reproduction
// of the vulnerability this fix closes: manifest.FindingsDoc is untrusted
// data decoded from a JSON file on disk (see manifest.go's doc comment) --
// whoever writes the manifest could previously point it at any absolute
// path on the filesystem (a private SSH key, another repo's secrets) and
// have its raw bytes read into the fix agent's launch prompt. RunFix must
// resolve findings by RunID through the state store only, never by
// following FindingsDoc directly.
func TestRunFix_IgnoresFindingsDoc_ArbitraryFileRead(t *testing.T) {
	t.Parallel()

	// A file outside anything RunFix should ever touch, containing a
	// marker that must never reach the launched agent's prompt.
	decoyDir := t.TempDir()
	decoyPath := decoyDir + "/not-a-findings-doc-secret.txt"
	if err := os.WriteFile(decoyPath, []byte("SHOULD-NOT-BE-READ-INTO-PROMPT"), 0o600); err != nil {
		t.Fatalf("write decoy file: %v", err)
	}

	store := NewLocalManifestStoreWithDir(t.TempDir())
	stateStore := NewStateStoreWithDir(t.TempDir())
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	const runID = "abcdef012345"
	// The manifest names the decoy file as its findings doc. No content is
	// written into the state store for this run — the vulnerable pre-fix
	// path would fall back to reading FindingsDoc directly.
	writeFixManifest(t, store, runID, "topic", now, decoyPath)

	var rec fixLaunchRecord
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}},
		FixDeps{
			ManifestStore: store,
			StateStore:    stateStore,
			Launch:        stubLaunch(&rec),
		},
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !rec.called {
		t.Fatal("Launch was not called")
	}
	if strings.Contains(rec.prompt, "SHOULD-NOT-BE-READ-INTO-PROMPT") {
		t.Fatalf("decoy file content leaked into the fix agent's prompt via FindingsDoc: %q", rec.prompt)
	}
	if !strings.Contains(rec.prompt, "(no findings recorded)") {
		t.Errorf("prompt should note absent findings (no state-store entry for this run), got: %q", rec.prompt)
	}
}

func TestRunFix_TolerateMissingDocs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewLocalManifestStoreWithDir(dir)
	stateStore := NewStateStoreWithDir(t.TempDir())
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	// No findings written to the state store for this run.
	writeFixManifest(t, store, "abcdef012345", "topic", now, "")

	var rec fixLaunchRecord
	var errBuf bytes.Buffer
	err := RunFix(context.Background(),
		FixInput{Out: &bytes.Buffer{}, ErrOut: &errBuf},
		FixDeps{
			ManifestStore: store,
			StateStore:    stateStore,
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
