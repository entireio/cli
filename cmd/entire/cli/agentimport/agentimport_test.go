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
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
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
		"claude-code", "cursor", "pi", "factoryai-droid", "codex", "copilot-cli",
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

// Timestamps of the u1 and u2 user turns in writeFixtureSession and the inline
// Claude fixtures below.
var (
	fixtureU1At = time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	fixtureU2At = fixtureU1At.Add(time.Minute)
)

// importedCheckpointID returns the ID Run stores turn under in stores' backend.
// Run and cp.Open resolve that backend from the same settings, so the ID's
// format follows whichever primary they select.
func importedCheckpointID(t *testing.T, stores *cp.Stores, sessionID string, turn Turn) id.CheckpointID {
	t.Helper()
	cid, _, err := turnCheckpointID(sessionID, turn, stores.PrimaryIsRefs(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cid
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

	opts := Options{LinkCommitSHA: repoHeadSHA(t, repo), RepoRoot: repoDir, OverridePath: claudeDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}
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

	// Regression: the session state must land in the repo being imported
	// into (opts.RepoRoot), not wherever the process runs — a CWD-resolved
	// store once leaked fixture sessions into the developer's real
	// .git/entire-sessions, where commit linking picked one up and stamped a
	// dangling trailer.
	store, err := session.NewStateStoreForWorktree(context.Background(), repoDir)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := store.Load(context.Background(), "sess1")
	if err != nil || imported == nil {
		t.Fatalf("imported session state should exist in the target repo's store: %v", err)
	}
	if !imported.Kind.IsImported() {
		t.Fatalf("imported session state must be Kind=imported, got %q", imported.Kind)
	}
}

// TestRun_StampsLinkCommitSHA proves a validated uppercase input is persisted
// canonically in each checkpoint's root and session metadata.
//
// The anchor is deliberately the PARENT, not HEAD. Every fixture in this file
// anchors to the repo's only commit, which makes "persisted what the caller
// gave us" and "read HEAD itself" indistinguishable — an implementation that
// ignored opts.LinkCommitSHA entirely would satisfy all of them. A second
// commit is what separates the two, so this test fails if Run ever starts
// resolving the anchor on its own. Uppercase input covers canonicalization,
// which is a different property and does not imply this one.
func TestRun_StampsLinkCommitSHA(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	commitSHA := repoHeadSHA(t, repo)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	writeAndCommit(t, wt, repoDir, "y", "second")
	if tip := repoHeadSHA(t, repo); tip == commitSHA {
		t.Fatal("fixture needs HEAD to differ from the anchor")
	}

	claudeDirWithSHA := t.TempDir()
	writeFixtureSession(t, claudeDirWithSHA, "sess-with-sha.jsonl")
	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDirWithSHA,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		LinkCommitSHA: strings.ToUpper(commitSHA),
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
	cid := importedCheckpointID(t, stores, "sess-with-sha", Turn{UUID: "u1", CreatedAt: fixtureU1At})
	md, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHA != commitSHA {
		t.Fatalf("expected commit_sha %q, got %q", commitSHA, md.CommitSHA)
	}

	root, err := stores.Persistent.Read(context.Background(), cid)
	if err != nil {
		t.Fatal(err)
	}
	if root.CommitSHA != commitSHA {
		t.Fatalf("root commit_sha = %q, want %q", root.CommitSHA, commitSHA)
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
	cid1 := importedCheckpointID(t, stores, "sess-anchor", Turn{UUID: "u1", CreatedAt: fixtureU1At})
	md1, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md1.CommitSHA != firstSHA {
		t.Fatalf("turn1 CommitSHA = %q, want recorded commit %q", md1.CommitSHA, firstSHA)
	}

	cid2 := importedCheckpointID(t, stores, "sess-anchor", Turn{UUID: "u2", CreatedAt: fixtureU2At})
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
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir, OverridePath: claudeDir,
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
	cid := importedCheckpointID(t, stores, "sess1", Turn{UUID: "u1", CreatedAt: fixtureU1At})
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

	opts := Options{LinkCommitSHA: repoHeadSHA(t, repo), RepoRoot: repoDir, OverridePath: cursorDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}
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
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir, OverridePath: claudeDir,
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
	cid := importedCheckpointID(t, stores, "sess-author", Turn{UUID: "u1", CreatedAt: fixtureU1At})
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
	// this repo can't see any real identity. The package TestMain already
	// installs an empty ConfigLoader, so GlobalScope carries no identity; this
	// keeps the env-level isolation as well, because it is what makes the
	// assertion hold under go-git's Auto loader too — that one reads all of
	// git's global sources (~/.gitconfig, XDG, GIT_CONFIG_GLOBAL, system
	// /etc/gitconfig), so a test moved onto it stays correct rather than
	// silently picking up the developer's identity. Mirrors the checkpoint
	// package's pointHomeAt helper.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitenv.UnsetGlobalConfig(t)

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
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir, OverridePath: claudeDir,
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
	cid := importedCheckpointID(t, stores, "sess-noauthor", Turn{UUID: "u1", CreatedAt: fixtureU1At})
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
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir, OverridePath: claudeDir, DryRun: true,
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
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir, OverridePath: codexDir,
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
	cid := importedCheckpointID(t, stores, "codex-import-1", turns[0])
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

// TestRun_StopsOnContextCancellation proves Ctrl-C mid-import halts Run's own
// loops, independently of whether the configured checkpoint store rejects a
// canceled write. DryRun writes nothing, so the loop checks are the only thing
// that can stop this run.
func TestRun_StopsOnContextCancellation(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel as soon as the first turn is processed, standing in for a Ctrl-C
	// during the import. DryRun reports every turn through TurnSkipped.
	opts := Options{
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir,
		OverridePath:  claudeDir,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		DryRun:        true,
		Progress:      &Progress{TurnSkipped: func(int, int, int) { cancel() }},
	}

	res, err := Run(ctx, repo, claudeImporter{}, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	// 4 turns across 2 sessions; the cancel lands after the first.
	if res.TurnsImported != 1 {
		t.Fatalf("run continued past cancellation: processed %d turns, want 1 (%+v)",
			res.TurnsImported, res)
	}
	if res.SessionsScanned != 1 {
		t.Fatalf("run walked into session %d after cancellation, want to stop at 1",
			res.SessionsScanned)
	}
}

// TestRun_CancellationStopsRefsBackedImport is the end-to-end regression for
// the reported bug, in the configuration it was reported on: `entire enable`
// defaults a first-time repo to the git-refs checkpoint backend and then
// offers to import agent history. See gitRefsStore.writeSession for why no
// layer below this one stopped the run.
func TestRun_CancellationStopsRefsBackedImport(t *testing.T) {
	// Not parallel: sets the checkpoint backend via the environment.
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")

	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := Options{
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir,
		OverridePath:  claudeDir,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		Progress:      &Progress{TurnWritten: func(int, int, int) { cancel() }},
	}

	res, err := Run(ctx, repo, claudeImporter{}, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if res.TurnsImported != 1 {
		t.Fatalf("import continued past cancellation: TurnsImported = %d, want 1 (%+v)",
			res.TurnsImported, res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("wrote %d checkpoints after cancellation, want 1 (the in-flight turn)", len(infos))
	}
}

// TestRun_GitRefsPrimaryDerivesULIDs: under a git-refs primary, imports get
// ULID IDs like every other git-refs checkpoint, stamped with the turn's time,
// and stay idempotent across re-runs.
func TestRun_GitRefsPrimaryDerivesULIDs(t *testing.T) {
	// Not parallel: sets the checkpoint backend via the environment.
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")

	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess-ulid.jsonl")
	opts := Options{
		LinkCommitSHA: repoHeadSHA(t, repo),
		RepoRoot:      repoDir,
		OverridePath:  claudeDir,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	}

	res, err := Run(context.Background(), repo, claudeImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res)
	}

	want := map[id.CheckpointID]time.Time{}
	for uuid, at := range map[string]time.Time{"u1": fixtureU1At, "u2": fixtureU2At} {
		cid, err := DeriveULIDCheckpointID("sess-ulid", uuid, at)
		if err != nil {
			t.Fatal(err)
		}
		want[cid] = at
	}
	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != len(want) {
		t.Fatalf("got %d checkpoints, want %d: %+v", len(infos), len(want), infos)
	}
	for _, info := range infos {
		at, ok := want[info.CheckpointID]
		if !ok {
			t.Fatalf("unexpected checkpoint %s (kind %v), want the derived ULIDs", info.CheckpointID, info.CheckpointID.Kind())
		}
		if got, _ := info.CheckpointID.Time(); !got.Equal(at) {
			t.Errorf("%s: ULID time = %v, want the turn's %v", info.CheckpointID, got, at)
		}
		if !info.Imported {
			t.Errorf("%s: not flagged imported", info.CheckpointID)
		}
	}

	res2, err := Run(context.Background(), repo, claudeImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TurnsImported != 0 || res2.TurnsSkipped != 2 {
		t.Fatalf("re-run not idempotent: %+v", res2)
	}

	lastCID, err := DeriveULIDCheckpointID("sess-ulid", "u2", fixtureU2At)
	if err != nil {
		t.Fatal(err)
	}
	if st := loadStateAt(t, repoDir, "sess-ulid"); st == nil || st.LastCheckpointID != lastCID {
		t.Fatalf("session state LastCheckpointID = %+v, want %s", st, lastCID)
	}
}

// TestRun_SkipsTurnsImportedUnderOtherPrimary: a turn already imported in one
// ID format is a skip when re-imported after the primary switched to the
// other backend, not a duplicate in the new format. Covers hex imports (from
// git-branch, or from before imports followed the backend format) re-run under
// git-refs, and ULID imports re-run after reverting to git-branch.
func TestRun_SkipsTurnsImportedUnderOtherPrimary(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		wantKind            id.Kind
	}{
		{name: "hex then git-refs", first: "git-branch", second: "git-refs", wantKind: id.KindLegacy},
		{name: "ULID then git-branch", first: "git-refs", second: "git-branch", wantKind: id.KindULID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: sets the checkpoint backend via the environment.
			t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", tc.first)

			repo, repoDir := initRepoWithCommit(t)
			claudeDir := t.TempDir()
			writeFixtureSession(t, claudeDir, "sess-switch.jsonl")
			opts := Options{
				LinkCommitSHA: repoHeadSHA(t, repo),
				RepoRoot:      repoDir,
				OverridePath:  claudeDir,
				Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
			}

			if res, err := Run(context.Background(), repo, claudeImporter{}, opts); err != nil || res.TurnsImported != 2 {
				t.Fatalf("%s import: %+v, %v", tc.first, res, err)
			}

			t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", tc.second)
			res, err := Run(context.Background(), repo, claudeImporter{}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if res.TurnsImported != 0 || res.TurnsSkipped != 2 {
				t.Fatalf("turns imported under %s were not skipped under %s: %+v", tc.first, tc.second, res)
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
				t.Fatalf("got %d checkpoints, want the 2 original imports: %+v", len(infos), infos)
			}
			for _, info := range infos {
				if info.CheckpointID.Kind() != tc.wantKind {
					t.Errorf("checkpoint %s: kind %v, want the original import's %v", info.CheckpointID, info.CheckpointID.Kind(), tc.wantKind)
				}
			}

			last, _, err := turnCheckpointID("sess-switch", Turn{UUID: "u2", CreatedAt: fixtureU2At}, tc.first == "git-refs", nil)
			if err != nil {
				t.Fatal(err)
			}
			if st := loadStateAt(t, repoDir, "sess-switch"); st == nil || st.LastCheckpointID != last {
				t.Fatalf("session state LastCheckpointID = %+v, want the original import %s", st, last)
			}
		})
	}
}

// TestRun_GitRefsModTimeTurnsKeepIDsAsTranscriptGrows: Cursor and Factory
// turns carry the transcript file's modtime, which moves whenever the session
// grows. Under git-refs that time must not feed the ULID, or re-importing a
// grown transcript would write every earlier turn again under a new ID.
func TestRun_GitRefsModTimeTurnsKeepIDsAsTranscriptGrows(t *testing.T) {
	// Not parallel: sets the checkpoint backend via the environment.
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")

	repo, repoDir := initRepoWithCommit(t)
	cursorDir := t.TempDir()
	path := filepath.Join(cursorDir, "sessGrow.jsonl")
	lines := []string{
		`{"role":"user","message":{"role":"user","content":"first"}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{LinkCommitSHA: repoHeadSHA(t, repo), RepoRoot: repoDir, OverridePath: cursorDir, Now: time.Now()}
	if res, err := Run(context.Background(), repo, cursorImporter{}, opts); err != nil || res.TurnsImported != 1 {
		t.Fatalf("first import: %+v, %v", res, err)
	}

	lines = append(lines, `{"role":"user","message":{"role":"user","content":"second"}}`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), repo, cursorImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsSkipped != 1 || res.TurnsImported != 1 {
		t.Fatalf("grown transcript: want the first turn skipped and the second imported, got %+v", res)
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
		t.Fatalf("got %d checkpoints, want 2 (no duplicate of the first turn): %+v", len(infos), infos)
	}
}

// TestRun_SkipsTurnsImportedOnlyOnRemote: from a fresh clone, turns imported
// elsewhere exist only as remote refs. The idempotency listing must see them
// by name, or a git-refs re-import writes hex-imported turns again as ULIDs.
func TestRun_SkipsTurnsImportedOnlyOnRemote(t *testing.T) {
	// Not parallel: sets the checkpoint backend via the environment.
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")

	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess-remote.jsonl")
	var remote []plumbing.ReferenceName
	for _, uuid := range []string{"u1", "u2"} {
		ref, err := cp.RefName(DeriveCheckpointID("sess-remote", uuid))
		if err != nil {
			t.Fatal(err)
		}
		remote = append(remote, ref)
	}
	opts := Options{
		LinkCommitSHA:   repoHeadSHA(t, repo),
		RepoRoot:        repoDir,
		OverridePath:    claudeDir,
		Now:             time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		RemoteRefLister: func(context.Context) ([]plumbing.ReferenceName, error) { return remote, nil },
	}

	res, err := Run(context.Background(), repo, claudeImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 0 || res.TurnsSkipped != 2 {
		t.Fatalf("remote-only imports were not skipped: %+v", res)
	}
}

// loadStateAt reads a session state by id from repoDir's store.
func loadStateAt(t *testing.T, repoDir, sid string) *session.State {
	t.Helper()
	store, err := session.NewStateStoreForWorktree(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("NewStateStoreForWorktree: %v", err)
	}
	st, err := store.Load(context.Background(), sid)
	if err != nil {
		t.Fatalf("Load %s: %v", sid, err)
	}
	return st
}
