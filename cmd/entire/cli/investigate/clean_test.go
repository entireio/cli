package investigate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cleanTestEnv bundles a manifest store + a per-run dir root rooted at
// t.TempDir(), plus injectable CleanDeps that point at them. Tests that
// need a real filesystem layout use this to avoid touching the host repo.
type cleanTestEnv struct {
	store      *LocalManifestStore
	runDirRoot string
}

func newCleanTestEnv(t *testing.T) *cleanTestEnv {
	t.Helper()
	manifestDir := t.TempDir()
	runDirRoot := t.TempDir()
	return &cleanTestEnv{
		store:      NewLocalManifestStoreWithDir(manifestDir),
		runDirRoot: runDirRoot,
	}
}

// runDir returns the per-run dir for runID. Mirrors StateStore.RunDir.
func (e *cleanTestEnv) runDir(runID string) string {
	return filepath.Join(e.runDirRoot, runID)
}

// deps builds a CleanDeps targeted at this env, with the supplied confirm
// behavior. Pass nil confirm to force a "yes" answer.
func (e *cleanTestEnv) deps(confirm func(ctx context.Context, message string) (bool, error)) CleanDeps {
	if confirm == nil {
		confirm = func(_ context.Context, _ string) (bool, error) { return true, nil }
	}
	return CleanDeps{
		ManifestStore: e.store,
		RunDir:        e.runDir,
		ManifestPath:  e.store.PathFor,
		Confirm:       confirm,
	}
}

// seed creates a manifest + populated per-run dir for runID. The dir
// holds a findings.md so tests can assert removal.
func (e *cleanTestEnv) seed(t *testing.T, runID, topic string, started time.Time) {
	t.Helper()
	m := LocalManifest{
		RunID:       runID,
		Topic:       topic,
		Slug:        SlugifyTopic(topic),
		StartingSHA: "deadbeefcafe",
		Agents:      []string{"claude-code"},
		Outcome:     "quorum",
		StartedAt:   started,
		EndedAt:     started.Add(5 * time.Minute),
	}
	if err := e.store.Write(context.Background(), m); err != nil {
		t.Fatalf("seed manifest %s: %v", runID, err)
	}
	dir := e.runDir(runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir per-run dir %s: %v", runID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "findings.md"), []byte("body\n"), 0o600); err != nil {
		t.Fatalf("write findings.md %s: %v", runID, err)
	}
}

func (e *cleanTestEnv) manifestExists(t *testing.T, m LocalManifest) bool {
	t.Helper()
	_, err := os.Stat(e.store.PathFor(m))
	return err == nil
}

func (e *cleanTestEnv) runDirExists(t *testing.T, runID string) bool {
	t.Helper()
	_, err := os.Stat(e.runDir(runID))
	return err == nil
}

func TestRunClean_RequiresArgOrAll(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{Out: &out, ErrOut: &errOut},
		env.deps(nil),
	)
	if err == nil {
		t.Fatal("expected error when neither RunID nor All is set")
	}
	if !strings.Contains(err.Error(), "pass a run id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunClean_NoManifestsReportsEmpty(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{All: true, Out: &out, ErrOut: &errOut},
		env.deps(nil),
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if !strings.Contains(out.String(), "No local investigations found.") {
		t.Errorf("expected empty-store notice, got: %q", out.String())
	}
}

func TestRunClean_SingleByRunIDDeletes(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	env.seed(t, "aaaaaaaaaaaa", "first", t1)
	env.seed(t, "bbbbbbbbbbbb", "second", t2)

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{RunID: "aaaaaaaaaaaa", Force: true, Out: &out, ErrOut: &errOut},
		env.deps(nil),
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}

	mA := LocalManifest{RunID: "aaaaaaaaaaaa", StartedAt: t1}
	mB := LocalManifest{RunID: "bbbbbbbbbbbb", StartedAt: t2}
	if env.manifestExists(t, mA) {
		t.Error("manifest A should have been deleted")
	}
	if env.runDirExists(t, "aaaaaaaaaaaa") {
		t.Error("run dir A should have been deleted")
	}
	if !env.manifestExists(t, mB) {
		t.Error("manifest B should still exist")
	}
	if !env.runDirExists(t, "bbbbbbbbbbbb") {
		t.Error("run dir B should still exist")
	}
	if !strings.Contains(out.String(), "Deleted 1 investigation(s)") {
		t.Errorf("expected deletion summary, got: %q", out.String())
	}
}

func TestRunClean_PrefixMatchUnique(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	env.seed(t, "aaaaaaaaaaaa", "first", t1)
	env.seed(t, "bbbbbbbbbbbb", "second", t2)

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{RunID: "aaaa", Force: true, Out: &out, ErrOut: &errOut},
		env.deps(nil),
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if env.runDirExists(t, "aaaaaaaaaaaa") {
		t.Error("run dir A should have been deleted by prefix match")
	}
	if !env.runDirExists(t, "bbbbbbbbbbbb") {
		t.Error("run dir B should still exist")
	}
}

func TestRunClean_PrefixMatchAmbiguous(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	env.seed(t, "abc111111111", "first", t1)
	env.seed(t, "abc222222222", "second", t2)

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{RunID: "abc", Force: true, Out: &out, ErrOut: &errOut},
		env.deps(nil),
	)
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("unexpected error: %v", err)
	}
	// Nothing should have been deleted.
	if !env.runDirExists(t, "abc111111111") {
		t.Error("run dir abc111... should still exist")
	}
	if !env.runDirExists(t, "abc222222222") {
		t.Error("run dir abc222... should still exist")
	}
}

func TestRunClean_AllDeletesEverything(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	env.seed(t, "aaaaaaaaaaaa", "first", t1)
	env.seed(t, "bbbbbbbbbbbb", "second", t2)

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{All: true, Force: true, Out: &out, ErrOut: &errOut},
		env.deps(nil),
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if env.runDirExists(t, "aaaaaaaaaaaa") {
		t.Error("run dir A should have been deleted")
	}
	if env.runDirExists(t, "bbbbbbbbbbbb") {
		t.Error("run dir B should have been deleted")
	}
	if !strings.Contains(out.String(), "Deleted 2 investigation(s)") {
		t.Errorf("expected deletion summary, got: %q", out.String())
	}
}

func TestRunClean_ConfirmDeclinedAborts(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	env.seed(t, "aaaaaaaaaaaa", "first", t1)

	confirm := func(_ context.Context, _ string) (bool, error) { return false, nil }

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{All: true, Out: &out, ErrOut: &errOut},
		env.deps(confirm),
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if !env.runDirExists(t, "aaaaaaaaaaaa") {
		t.Error("run dir should still exist after declined confirmation")
	}
	if !strings.Contains(out.String(), "Aborted.") {
		t.Errorf("expected 'Aborted.' notice, got: %q", out.String())
	}
}

func TestRunClean_ForceSkipsConfirm(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	env.seed(t, "aaaaaaaaaaaa", "first", t1)

	confirm := func(_ context.Context, _ string) (bool, error) {
		return false, errors.New("confirm should not be called when --force is set")
	}

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{All: true, Force: true, Out: &out, ErrOut: &errOut},
		env.deps(confirm),
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if env.runDirExists(t, "aaaaaaaaaaaa") {
		t.Error("run dir should have been deleted with --force")
	}
}

func TestRunClean_MissingRunDirOK(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	env.seed(t, "aaaaaaaaaaaa", "first", t1)
	// Simulate the terminal-outcome case: per-run dir already cleaned up.
	if err := os.RemoveAll(env.runDir("aaaaaaaaaaaa")); err != nil {
		t.Fatalf("remove per-run dir: %v", err)
	}

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{RunID: "aaaaaaaaaaaa", Force: true, Out: &out, ErrOut: &errOut},
		env.deps(nil),
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	mA := LocalManifest{RunID: "aaaaaaaaaaaa", StartedAt: t1}
	if env.manifestExists(t, mA) {
		t.Error("manifest should have been deleted")
	}
	if !strings.Contains(out.String(), "Deleted 1 investigation(s)") {
		t.Errorf("expected deletion summary, got: %q", out.String())
	}
	if strings.Contains(out.String(), "failed") {
		t.Errorf("missing run dir should not be reported as failure, got: %q", out.String())
	}
}

func TestRunClean_AggregatesFailures(t *testing.T) {
	t.Parallel()

	env := newCleanTestEnv(t)
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	env.seed(t, "aaaaaaaaaaaa", "first", t1)
	env.seed(t, "bbbbbbbbbbbb", "second", t2)
	env.seed(t, "cccccccccccc", "third", t3)

	// Inject a failing ManifestPath for runID "bbbbbbbbbbbb" — point at a
	// directory we can't os.Remove (because it has children). The real
	// path remains untouched.
	badDir := filepath.Join(t.TempDir(), "not-removable")
	if err := os.MkdirAll(filepath.Join(badDir, "child"), 0o750); err != nil {
		t.Fatalf("setup bad dir: %v", err)
	}

	deps := env.deps(nil)
	deps.ManifestPath = func(m LocalManifest) string {
		if m.RunID == "bbbbbbbbbbbb" {
			return badDir
		}
		return env.store.PathFor(m)
	}

	var out, errOut bytes.Buffer
	err := RunClean(context.Background(),
		CleanInput{All: true, Force: true, Out: &out, ErrOut: &errOut},
		deps,
	)
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if !strings.Contains(out.String(), "Deleted 2 investigation(s) (1 failed).") {
		t.Errorf("expected aggregated failure summary, got: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "bbbbbbbbbbbb") {
		t.Errorf("expected per-run failure warning on errOut, got: %q", errOut.String())
	}
	if env.runDirExists(t, "aaaaaaaaaaaa") {
		t.Error("run dir A should have been deleted")
	}
	if env.runDirExists(t, "cccccccccccc") {
		t.Error("run dir C should have been deleted")
	}
}
