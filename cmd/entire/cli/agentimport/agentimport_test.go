package agentimport

import (
	"context"
	"errors"
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
		// When must be a real timestamp: the anchor resolver's bounded walk
		// stops at commits older than its date cutoff, and a zero-value When
		// (year 1) would halt the walk at the first commit.
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
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

// TestRun_StampsLinkCommitSHA proves Options.LinkCommitSHA is copied verbatim
// into each imported checkpoint's commit_sha metadata field, and that leaving
// it unset leaves commit_sha empty. Run resolves nothing itself.
func TestRun_StampsLinkCommitSHA(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	const commitSHA = "b01b59663fd4860fd15a9939499be44a14dbf168"

	claudeDirWithSHA := t.TempDir()
	writeFixtureSession(t, claudeDirWithSHA, "sess-with-sha.jsonl")
	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDirWithSHA,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		LinkCommitSHA: commitSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cid := DeriveCheckpointID("sess-with-sha", "u1")
	md, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHA != commitSHA {
		t.Fatalf("expected commit_sha %q, got %q", commitSHA, md.CommitSHA)
	}

	// A separate session fixture (own sessionID/turn UUIDs) run with
	// LinkCommitSHA unset must persist an empty commit_sha. Reusing the same
	// session would be idempotently skipped, so this needs its own fixture.
	claudeDirNoSHA := t.TempDir()
	writeFixtureSession(t, claudeDirNoSHA, "sess-no-sha.jsonl")
	res2, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDirNoSHA,
		Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res2)
	}

	cid2 := DeriveCheckpointID("sess-no-sha", "u1")
	md2, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md2.CommitSHA != "" {
		t.Fatalf("expected empty commit_sha, got %q", md2.CommitSHA)
	}
}

// TestRun_AnchorsTurnToRecordedCommit proves a turn whose transcript records a
// resolvable, default-branch-reachable commit anchors to that real commit
// instead of the LinkCommitSHA fallback, while a turn with no recorded commit
// still falls back exactly as before.
func TestRun_AnchorsTurnToRecordedCommit(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	firstCommit, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	firstSHA := firstCommit.Hash().String()

	// Second commit on the default branch, so tip != first commit.
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	writeAndCommit(t, wt, repoDir, "y", "second")
	tipHead, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	tipSHA := tipHead.Hash().String()

	claudeDir := t.TempDir()
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
		`{"type":"user","uuid":"tr1","toolUseResult":{"gitOperation":{"commit":{"sha":"` + firstSHA[:7] + `","kind":"committed"}}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(claudeDir, "sess-anchor.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		LinkCommitSHA: tipSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cid1 := DeriveCheckpointID("sess-anchor", "u1")
	md1, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md1.CommitSHA != firstSHA {
		t.Fatalf("turn1 CommitSHA = %q, want recorded commit %q", md1.CommitSHA, firstSHA)
	}

	cid2 := DeriveCheckpointID("sess-anchor", "u2")
	md2, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md2.CommitSHA != tipSHA {
		t.Fatalf("turn2 CommitSHA = %q, want fallback %q", md2.CommitSHA, tipSHA)
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

// TestRun_StampsImporterGitAuthorOnCheckpointCommit proves an imported
// checkpoint's underlying git commit on entire/checkpoints/v1 carries the
// importer's configured git identity (resolved once per Run via
// checkpoint.GetGitAuthorFromRepo), not an empty signature.
//
// Motivation: on the GitHub->mirror ingestion path, the data plane has no
// pusher identity for imported sessions and falls back to the checkpoint
// commit's git author. An empty author meant imported sessions couldn't be
// attributed to the importer.
func TestRun_StampsImporterGitAuthorOnCheckpointCommit(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess-author.jsonl")

	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ar, ok := stores.Persistent.(cp.AuthorReader)
	if !ok {
		t.Fatalf("persistent store %T does not implement AuthorReader", stores.Persistent)
	}
	cid := DeriveCheckpointID("sess-author", "u1")
	author, err := ar.GetCheckpointAuthor(context.Background(), cid)
	if err != nil {
		t.Fatal(err)
	}
	// initRepoWithCommit uses testutil.InitRepo, which configures this
	// repo-local git identity.
	const wantName, wantEmail = "Test User", "test@example.com"
	if author.Name != wantName || author.Email != wantEmail {
		t.Fatalf("checkpoint commit author = %+v, want Name=%q Email=%q (the repo's configured git identity)",
			author, wantName, wantEmail)
	}
}

// TestRun_UnconfiguredGitIdentityFallsBackToDefaults proves that when the
// importer's repo has no configured git user (no local or global user.name /
// user.email), the imported checkpoint commit still gets a signature — the
// same "Unknown"/"unknown@local" default checkpoint.GetGitAuthorFromRepo
// already applies elsewhere, rather than an empty one.
func TestRun_UnconfiguredGitIdentityFallsBackToDefaults(t *testing.T) {
	// Cannot use t.Parallel(): isolates git config resolution via t.Setenv so
	// this repo can't see any real identity. GetGitAuthorFromRepo resolves
	// GlobalScope through go-git's Auto loader, which reads all of git's global
	// sources; neutralize every one or the fallback assertion is flaky wherever
	// an identity is configured (~/.gitconfig, XDG, GIT_CONFIG_GLOBAL, or system
	// /etc/gitconfig). Mirrors the checkpoint package's pointHomeAt helper.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// t.Setenv registers restoration of the original value; unset it for the
	// test since an empty GIT_CONFIG_GLOBAL disables global config entirely.
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	if err := os.Unsetenv("GIT_CONFIG_GLOBAL"); err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Seed one commit with a real timestamp (the anchor resolver's bounded
	// walk stops at commits older than its date cutoff; a zero-value When
	// would halt it immediately). The commit's own author signature is
	// independent of GetGitAuthorFromRepo's config-based resolution under
	// test here.
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "Seed", Email: "seed@test.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess-noauthor.jsonl")

	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ar, ok := stores.Persistent.(cp.AuthorReader)
	if !ok {
		t.Fatalf("persistent store %T does not implement AuthorReader", stores.Persistent)
	}
	cid := DeriveCheckpointID("sess-noauthor", "u1")
	author, err := ar.GetCheckpointAuthor(context.Background(), cid)
	if err != nil {
		t.Fatal(err)
	}
	if author.Name != "Unknown" || author.Email != "unknown@local" {
		t.Fatalf("checkpoint commit author = %+v, want the GetGitAuthorFromRepo defaults", author)
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

// TestRun_CodexImportSanitizesAndKeepsOffsetsAligned covers `entire import` for
// Codex, which reads raw third-party rollouts. It guards two properties, neither of
// which had any import-side test before:
//
//  1. The stored transcript is sanitized — no encrypted payloads reach storage.
//  2. Turn offsets still line up. The Codex importer derives
//     CheckpointTranscriptStart from raw line indices (splitLineTurns), so
//     sanitization must not change the line count; a dropped line would silently
//     mis-scope every imported turn after it. This is the property that made import
//     a fourth casualty of the old drop-the-compaction-line behavior.
//
// It does NOT pin the sanitize-before-redact ORDER in Run(): the store sanitizes as a
// last-resort safety net, so the stored content is identical either way. Getting the
// order right in Run() is a wasted-work fix (redaction scanning ciphertext the store
// would discard), and it is not observable from the stored result.
func TestRun_CodexImportSanitizesAndKeepsOffsetsAligned(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	codexDir := t.TempDir()

	const ciphertext = "Y2lwaGVydGV4dC1wYXlsb2FkLXNob3VsZC1uZXZlci1iZS1zdG9yZWQ="
	rollout := strings.Join([]string{
		`{"timestamp":"2026-06-20T00:00:00Z","type":"session_meta","payload":{"id":"codex-import-1","cwd":"` + repoDir + `"}}`,
		`{"timestamp":"2026-06-20T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt"}]}}`,
		`{"timestamp":"2026-06-20T00:00:02Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"` + ciphertext + `"}}`,
		`{"timestamp":"2026-06-20T00:00:03Z","type":"response_item","payload":{"type":"compaction","encrypted_content":"` + ciphertext + `"}}`,
		`{"timestamp":"2026-06-20T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}}`,
	}, "\n") + "\n"
	rawLines := len(strings.Split(strings.TrimRight(rollout, "\n"), "\n"))

	if err := os.WriteFile(filepath.Join(codexDir, "codex-import-1.jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), repo, codexImporter{}, Options{
		RepoRoot: repoDir, OverridePath: codexDir,
		Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported == 0 {
		t.Fatalf("expected at least one imported turn, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Derive the turn UUID the same way the importer does rather than guessing it.
	turns, splitErr := codexImporter{}.SplitTurns(SessionFile{SessionID: "codex-import-1"}, []byte(rollout))
	if splitErr != nil {
		t.Fatalf("SplitTurns: %v", splitErr)
	}
	if len(turns) == 0 {
		t.Fatal("codex importer produced no turns")
	}
	cid := DeriveCheckpointID("codex-import-1", turns[0].UUID)
	sc, err := stores.Persistent.ReadSessionContent(context.Background(), cid, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(%s): %v", cid, err)
	}

	stored := string(sc.Transcript)
	if strings.Contains(stored, ciphertext) {
		t.Error("imported transcript still carries encrypted_content ciphertext")
	}
	if strings.Contains(stored, "encrypted_content") {
		t.Error("imported transcript still has an encrypted_content key")
	}
	if !strings.Contains(stored, "first prompt") || !strings.Contains(stored, "first answer") {
		t.Errorf("imported transcript lost conversation content:\n%s", stored)
	}
	if got := len(strings.Split(strings.TrimRight(stored, "\n"), "\n")); got != rawLines {
		t.Errorf("stored transcript has %d lines, rollout had %d — imported turn offsets "+
			"(CheckpointTranscriptStart from raw line indices) would drift", got, rawLines)
	}
}

// TestRun_CancelledContextStopsBeforeScanning verifies the session loop honors
// context cancellation: a cancelled context stops the import cleanly (surfacing
// context.Canceled) before scanning any session or writing any checkpoint, so a
// large import aborts on Ctrl-C instead of grinding through every remaining
// session.
func TestRun_CancelledContextStopsBeforeScanning(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := Options{RepoRoot: repoDir, OverridePath: claudeDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}
	res, err := Run(ctx, repo, claudeImporter{}, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if res.SessionsScanned != 0 {
		t.Fatalf("cancelled import must not scan any session, got %+v", res)
	}
	if res.TurnsImported != 0 {
		t.Fatalf("cancelled import must import no turns, got %+v", res)
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
		t.Fatalf("cancelled import must write no checkpoints, got %d", len(infos))
	}
}
