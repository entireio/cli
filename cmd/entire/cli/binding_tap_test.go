package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// No test in this file may call t.Parallel: the recordForeignEvidence tests
// set ENTIRE_CONFIG_DIR via t.Setenv for per-test record isolation (the
// in-process testdirs fallback is shared per process), and the
// FilterAndNormalizePathsCollectingForeign test uses t.Chdir.

func newBindingRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	resolved, err := filepath.EvalSymlinks(dir) // macOS /var → /private/var
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func bindingTestMeta(root string) binding.SessionMeta {
	return binding.SessionMeta{
		AgentType:      testAgentName,
		TranscriptPath: "/tmp/transcript.jsonl",
		LaunchRoot:     root,
	}
}

func enableEntireAt(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".entire", "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecordForeignEvidence_EnabledForeignRepoRecorded(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)
	rootB := newBindingRepo(t)
	enableEntireAt(t, rootB)

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(rootB, "pkg", "f.go")})

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected one bound repo, got %+v", rec)
	}
	br := rec.BoundRepos[0]
	if br.WorktreeRoot != rootB {
		t.Errorf("bound root = %q, want %q", br.WorktreeRoot, rootB)
	}
	if !br.Enabled {
		t.Error("repo with .entire/settings.json must record Enabled=true")
	}
	if br.EvidenceCount != 1 {
		t.Errorf("evidence count = %d, want 1", br.EvidenceCount)
	}
	if rec.AgentType != testAgentName || rec.LaunchRoot != rootA {
		t.Errorf("session meta not stored: %+v", rec)
	}
}

func TestRecordForeignEvidence_DisabledForeignRepoRecorded(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)
	rootC := newBindingRepo(t) // no .entire setup

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(rootC, "f.go")})

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected one bound repo, got %+v", rec)
	}
	if rec.BoundRepos[0].Enabled {
		t.Error("repo without .entire must record Enabled=false")
	}
}

func TestRecordForeignEvidence_NonRepoPathIgnored(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(t.TempDir(), "f.txt")})

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil && len(rec.BoundRepos) > 0 {
		t.Fatalf("non-repo path must not bind anything, got %+v", rec)
	}
}

func TestRecordForeignEvidence_SameWorktreeSkipped(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(rootA, "inside.go")})

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil && len(rec.BoundRepos) > 0 {
		t.Fatalf("same-worktree path must be skipped, got %+v", rec)
	}
}

func TestRecordForeignEvidence_EmptyForeignIsFree(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA, nil)

	// Perf-invariant proxy: nothing was resolved, locked, or written — the
	// sessions dir must not even exist.
	if _, err := os.Stat(filepath.Join(userdirs.Config(), "sessions")); !os.IsNotExist(err) {
		t.Fatalf("empty foreign slice must create nothing: %v", err)
	}
}

func TestRecordForeignEvidence_UnknownSessionIgnored(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)
	rootB := newBindingRepo(t)

	// unknownSessionID passes ValidateSessionID and would otherwise create a
	// sessions/unknown.json aggregating unrelated sessions.
	recordForeignEvidence(ctx, unknownSessionID, bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(rootB, "f.go")})
	recordForeignEvidence(ctx, "", bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(rootB, "f.go")})

	if _, err := os.Stat(filepath.Join(userdirs.Config(), "sessions")); !os.IsNotExist(err) {
		t.Fatalf("unknown/empty session must create nothing: %v", err)
	}
}

func TestRecordForeignEvidence_CapsForeignReposPerTurn(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)

	foreign := make([]string, 0, maxForeignReposPerTurn+1)
	for i := range maxForeignReposPerTurn + 1 {
		foreign = append(foreign, filepath.Join(newBindingRepo(t), fmt.Sprintf("f%d.go", i)))
	}

	binding.ClearResolveCache()
	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA, foreign)

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != maxForeignReposPerTurn {
		t.Fatalf("cap: bound repos = %d, want %d", len(rec.BoundRepos), maxForeignReposPerTurn)
	}
	// The cap must bound RESOLUTION (git forks), not just recording: once the
	// cap is hit, paths in not-yet-seen directories are never resolved.
	if n := binding.ResolveCacheSizeForTesting(); n > maxForeignReposPerTurn {
		t.Errorf("resolved %d distinct dirs, cap is %d — cap must apply during resolution", n, maxForeignReposPerTurn)
	}
}

// Starvation regression: the resolution budget must count distinct
// directories, not paths — a turn with many paths in one foreign repo dir
// must not exhaust the budget before a later repo's single path is reached.
func TestRecordForeignEvidence_ManyPathsInOneRepoDoNotStarveOthers(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)
	rootB := newBindingRepo(t)
	rootC := newBindingRepo(t)

	foreign := make([]string, 0, 21)
	for i := range 20 {
		foreign = append(foreign, filepath.Join(rootB, "pkg", fmt.Sprintf("f%d.go", i)))
	}
	foreign = append(foreign, filepath.Join(rootC, "g.go"))

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA, foreign)

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 2 {
		t.Fatalf("expected both repos recorded, got %+v", rec)
	}
	roots := map[string]bool{}
	for _, br := range rec.BoundRepos {
		roots[br.WorktreeRoot] = true
	}
	if !roots[rootB] || !roots[rootC] {
		t.Errorf("recorded roots %v, want both %q and %q", roots, rootB, rootC)
	}
}

func TestRecordForeignEvidence_BoundsResolutionForMisses(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)

	// 30 paths in 30 distinct NON-repo directories: every one is a resolver
	// miss, so without the attempts bound each would cost a git fork.
	base := t.TempDir()
	const missDirs = 30
	foreign := make([]string, 0, missDirs)
	for i := range missDirs {
		dir := filepath.Join(base, fmt.Sprintf("dir%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		foreign = append(foreign, filepath.Join(dir, "f.go"))
	}

	binding.ClearResolveCache()
	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA, foreign)

	forks := binding.ResolveCacheSizeForTesting()
	t.Logf("git resolutions for %d distinct non-repo dirs: %d", missDirs, forks)
	if forks > maxForeignResolutionsPerTurn {
		t.Errorf("resolved %d distinct dirs, attempts bound is %d — misses must be bounded too",
			forks, maxForeignResolutionsPerTurn)
	}

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil && len(rec.BoundRepos) > 0 {
		t.Fatalf("non-repo paths must record nothing, got %+v", rec)
	}
}

func TestFilterAndNormalizePathsCollectingForeign(t *testing.T) {
	// t.Chdir per the git-in-tests rules: path helpers must never resolve
	// against the real repo CWD.
	repo := newBindingRepo(t)
	t.Chdir(repo)
	outside := filepath.Join(t.TempDir(), "elsewhere", "f.go")

	files := []string{
		filepath.Join(repo, "sub", "kept.go"), // in-repo absolute → kept
		"relative/kept.go",                    // relative → kept unchanged
		"../traversal.go",                     // relative junk → kept (clamp behavior today), NOT foreign
		outside,                               // absolute out-of-repo → foreign
	}

	kept, foreign := FilterAndNormalizePathsCollectingForeign(files, repo)

	wantKept := []string{"sub/kept.go", "relative/kept.go", "../traversal.go"}
	if len(kept) != len(wantKept) {
		t.Fatalf("kept = %v, want %v", kept, wantKept)
	}
	for i, w := range wantKept {
		if kept[i] != w {
			t.Errorf("kept[%d] = %q, want %q", i, kept[i], w)
		}
	}
	if len(foreign) != 1 || foreign[0] != outside {
		t.Errorf("foreign = %v, want [%s]", foreign, outside)
	}

	// Behavior-preservation pin: the original returns exactly the kept slice.
	orig := FilterAndNormalizePaths(files, repo)
	if len(orig) != len(kept) {
		t.Fatalf("original kept %v diverges from sibling %v", orig, kept)
	}
	for i := range orig {
		if orig[i] != kept[i] {
			t.Errorf("original[%d] = %q, sibling = %q", i, orig[i], kept[i])
		}
	}
}
