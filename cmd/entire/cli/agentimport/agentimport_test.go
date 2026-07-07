package agentimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestDeriveCheckpointID_StableAndDistinct(t *testing.T) {
	t.Parallel()
	a := DeriveCheckpointID("sess", "turn-1")
	b := DeriveCheckpointID("sess", "turn-1")
	c := DeriveCheckpointID("sess", "turn-2")
	if a != b {
		t.Errorf("not deterministic: %s != %s", a, b)
	}
	if a == c {
		t.Errorf("collision across turns: %s == %s", a, c)
	}
	if a.IsEmpty() {
		t.Error("derived id is empty")
	}
}

func TestRegistry_HasClaude(t *testing.T) {
	t.Parallel()

	for _, imp := range All() {
		if imp.Name() == "claude-code" {
			return
		}
	}
	t.Fatal("claude-code importer not registered")
}

// TestRegistry_AllSupportedAgents asserts every supported importer is
// registered with a distinct name and a non-empty agent type.
func TestRegistry_AllSupportedAgents(t *testing.T) {
	t.Parallel()
	want := []string{
		"claude-code", "cursor", "pi", "factoryai-droid", "codex", "copilot-cli", "gemini",
	}
	registered := make(map[string]Importer)
	for _, imp := range All() {
		if _, dup := registered[imp.Name()]; dup {
			t.Errorf("duplicate importer name %q", imp.Name())
		}
		registered[imp.Name()] = imp
	}

	for _, name := range want {
		imp, ok := registered[name]
		if !ok {
			t.Errorf("%s importer not registered", name)
			continue
		}
		if imp.AgentType() == "" {
			t.Errorf("%s importer has empty AgentType", name)
		}
	}
	if len(All()) != len(want) {
		t.Errorf("registered %d importers, want %d (%v)", len(All()), len(want), want)
	}
}

func initRepoWithCommit(t *testing.T) (*git.Repository, string) {
	t.Helper()
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatal(err)
	}
	return repo, repoDir
}

func writeFixtureSession(t *testing.T, dir, name string) {
	t.Helper()
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRun_ImportsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")

	opts := Options{RepoRoot: repoDir, OverridePath: claudeDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}
	imp := claudeImporter{}

	res, err := Run(context.Background(), repo, imp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res)
	}

	res2, err := Run(context.Background(), repo, imp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TurnsImported != 0 || res2.TurnsSkipped != 2 {
		t.Fatalf("re-run not idempotent: %+v", res2)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 imported checkpoints on v1, got %+v", infos)
	}
	for _, in := range infos {
		if !in.Imported {
			t.Fatalf("checkpoint %s missing Imported flag: %+v", in.CheckpointID, in)
		}
	}
}

// TestRun_AppliesConfiguredCustomRedaction proves imported transcripts honor
// repo/user-configured custom_redactions (loaded at the command via
// strategy.EnsureRedactionConfigured), not just always-on secret scanning.
// It mutates process-global redaction config, so it cannot run in parallel.
func TestRun_AppliesConfiguredCustomRedaction(t *testing.T) {
	// A benign marker word that always-on secret scanning would never flag, so
	// redacting it can only be the configured custom rule's doing.
	const secret = "bananaphone-marker-word"
	redact.ConfigureCustomRules(redact.CustomRulesConfig{
		Inline: map[string]string{"acme-token": secret},
	})
	t.Cleanup(func() { redact.ConfigureCustomRules(redact.CustomRulesConfig{}) })

	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"use ` + secret + ` please"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(claudeDir, "sess1.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 1 {
		t.Fatalf("want 1 imported, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cid := DeriveCheckpointID("sess1", "u1")
	sc, err := stores.Persistent.ReadSessionContent(context.Background(), cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sc.Transcript), secret) {
		t.Fatalf("custom-configured secret was not redacted from imported transcript")
	}
	if !strings.Contains(string(sc.Transcript), redact.RedactedPlaceholder) {
		t.Fatalf("expected %q in redacted transcript, got: %s", redact.RedactedPlaceholder, sc.Transcript)
	}
}

// TestRun_CursorImporterEndToEnd exercises the generic Run pipeline through a
// non-Claude importer whose turns carry nil tokens and an empty model, proving
// the checkpoint write tolerates those (the riskiest divergence from Claude).
func TestRun_CursorImporterEndToEnd(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	cursorDir := t.TempDir()
	content := strings.Join([]string{
		`{"role":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"role":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"hi"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(cursorDir, "sessC.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{RepoRoot: repoDir, OverridePath: cursorDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}
	res, err := Run(context.Background(), repo, cursorImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 1 {
		t.Fatalf("want 1 imported, got %+v", res)
	}

	// Re-run is idempotent.
	res2, err := Run(context.Background(), repo, cursorImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TurnsImported != 0 || res2.TurnsSkipped != 1 {
		t.Fatalf("re-run not idempotent: %+v", res2)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || !infos[0].Imported {
		t.Fatalf("expected 1 imported cursor checkpoint, got %+v", infos)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")

	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDir, DryRun: true,
		Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("dry-run should count 2 turns, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("dry-run must not write, got %+v", infos)
	}
}
