package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
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

func TestRecordForeignEvidence_ReplicatesStateWithoutRetiringSource(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	rootA := newBindingRepo(t)
	rootB := newBindingRepo(t)
	for _, root := range []string{rootA, rootB} {
		testutil.WriteFile(t, root, "tracked.txt", "base\n")
		testutil.GitAdd(t, root, "tracked.txt")
		testutil.GitCommit(t, root, "initial")
	}
	enableEntireAt(t, rootB)
	testutil.WriteFile(t, rootB, "agent.go", "package agent\n")
	t.Chdir(rootA)

	started := time.Now().Add(-time.Hour)
	lastInteraction := started.Add(30 * time.Minute)
	source := &session.State{
		SessionID:                 "sess-1",
		BaseCommit:                testutil.GetHeadHash(t, rootA),
		AttributionBaseCommit:     testutil.GetHeadHash(t, rootA),
		WorktreePath:              rootA,
		StartedAt:                 started,
		LastInteractionTime:       &lastInteraction,
		Phase:                     session.PhaseActive,
		StepCount:                 4,
		CheckpointTranscriptStart: 99,
		LastCheckpointID:          id.CheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		AgentType:                 types.AgentType("Claude Code"),
		TranscriptPath:            "/tmp/shared.jsonl",
		FilesTouched:              []string{"source-only.go"},
	}
	sourceStore := session.NewStateStoreWithDir(filepath.Join(rootA, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(ctx, source); err != nil {
		t.Fatal(err)
	}

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(rootB, "agent.go")})

	targetStore := session.NewStateStoreWithDir(filepath.Join(rootB, ".git", session.SessionStateDirName))
	target, err := targetStore.Load(ctx, "sess-1")
	if err != nil || target == nil {
		t.Fatalf("load replicated target: state=%+v err=%v", target, err)
	}
	if target.SessionID != source.SessionID || target.TranscriptPath != source.TranscriptPath || !target.StartedAt.Equal(source.StartedAt) {
		t.Errorf("shared session identity was not preserved: %+v", target)
	}
	if target.WorktreePath != rootB || target.BaseCommit != testutil.GetHeadHash(t, rootB) || target.AttributionBaseCommit != target.BaseCommit {
		t.Errorf("target-local repository identity is wrong: %+v", target)
	}
	if target.StepCount != 0 || target.CheckpointTranscriptStart != 0 || target.LastCheckpointID != id.EmptyCheckpointID {
		t.Errorf("target-local checkpoint bookkeeping was not reset: %+v", target)
	}
	if len(target.FilesTouched) != 1 || target.FilesTouched[0] != "agent.go" {
		t.Errorf("target files touched = %v, want [agent.go]", target.FilesTouched)
	}
	if target.Phase != session.PhaseActive || target.AdoptedIntoWorktreePath != "" {
		t.Errorf("replicated state must be active and additive, got phase=%s tombstone=%q", target.Phase, target.AdoptedIntoWorktreePath)
	}

	stillSource, err := sourceStore.Load(ctx, "sess-1")
	if err != nil || stillSource == nil {
		t.Fatalf("source must remain live: state=%+v err=%v", stillSource, err)
	}
	if stillSource.AdoptedIntoWorktreePath != "" || stillSource.StepCount != source.StepCount {
		t.Errorf("source was retired or rewritten: %+v", stillSource)
	}
	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil || rec == nil || rec.BoundRepos[0].AdoptedAt == nil {
		t.Fatalf("durable adopted marker missing: rec=%+v err=%v", rec, err)
	}

	// The marker is only a fast-path hint. Cleanup or stale-state collection can
	// remove the target file while the machine record remains; later evidence
	// must reconstruct the missing replica instead of trusting the old marker.
	targetStatePath := filepath.Join(rootB, ".git", session.SessionStateDirName, "sess-1.json")
	if err := os.Remove(targetStatePath); err != nil {
		t.Fatal(err)
	}
	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(rootA), rootA,
		[]string{filepath.Join(rootB, "agent.go")})
	recreated, err := targetStore.Load(ctx, "sess-1")
	if err != nil || recreated == nil {
		t.Fatalf("marker must not suppress missing-state repair: state=%+v err=%v", recreated, err)
	}
}

func TestRecordForeignEvidence_NoRepoSourceSynthesizesState(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	targetRoot := newBindingRepo(t)
	testutil.WriteFile(t, targetRoot, "tracked.txt", "base\n")
	testutil.GitAdd(t, targetRoot, "tracked.txt")
	testutil.GitCommit(t, targetRoot, "initial")
	enableEntireAt(t, targetRoot)
	testutil.WriteFile(t, targetRoot, "from-parent.go", "package parent\n")

	meta := binding.SessionMeta{
		AgentType:      "Claude Code",
		TranscriptPath: "/tmp/parent-launch.jsonl",
	}
	recordForeignEvidence(ctx, "sess-parent", meta, "",
		[]string{filepath.Join(targetRoot, "from-parent.go")})

	store := session.NewStateStoreWithDir(filepath.Join(targetRoot, ".git", session.SessionStateDirName))
	state, err := store.Load(ctx, "sess-parent")
	if err != nil || state == nil {
		t.Fatalf("load synthesized state: state=%+v err=%v", state, err)
	}
	if state.SessionID != "sess-parent" || state.AgentType != types.AgentType("Claude Code") || state.TranscriptPath != meta.TranscriptPath {
		t.Errorf("machine session identity not copied: %+v", state)
	}
	if state.Phase != session.PhaseActive || state.BaseCommit != testutil.GetHeadHash(t, targetRoot) {
		t.Errorf("synthesized state is not active at target HEAD: %+v", state)
	}
	if state.StartedAt.IsZero() || state.TurnID == "" {
		t.Errorf("synthesized state lacks lifecycle identity: %+v", state)
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

// newNestedBindingRepo initializes a git repo at outer/rel and returns its
// canonical absolute path.
func newNestedBindingRepo(t *testing.T, outer, rel string) string {
	t.Helper()
	dir := filepath.Join(outer, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.InitRepo(t, dir)
	return dir
}

// Regression: on a machine where the session repo encloses other repos
// path-wise ($HOME as a dotfiles repo is the loudest case), a kept path
// inside an unregistered nested repo never became foreign, so no evidence was
// ever recorded — and on a home-as-repo machine the in-repo evidence path
// could never fire at all.
func TestRecordForeignEvidence_UnregisteredNestedRepoRecorded(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	outer := newBindingRepo(t)
	nested := newNestedBindingRepo(t, outer, "tmp-global-test")

	kept := []string{"tmp-global-test/notes.md", "README.md"}
	keptBefore := append([]string(nil), kept...)

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(outer), outer, nil, kept)

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected exactly the nested repo bound, got %+v", rec)
	}
	// The record must carry the INNERMOST (nested) root — same identity the
	// no-repo branch records for this path — never the session repo's.
	if got := rec.BoundRepos[0].WorktreeRoot; got != nested {
		t.Errorf("bound root = %q, want nested root %q", got, nested)
	}
	// Kept-paths contract: the tap reads the capture input, never mutates it.
	for i := range kept {
		if kept[i] != keptBefore[i] {
			t.Fatalf("kept list mutated: %v, want %v", kept, keptBefore)
		}
	}
}

// Regression: a nested repo registered in the session repo's .gitmodules is
// part of the parent project — binding the session to it is noise.
func TestRecordForeignEvidence_RegisteredSubmoduleNotRecorded(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	outer := newBindingRepo(t)
	newNestedBindingRepo(t, outer, "vendor/sub")

	gitmodules := "[submodule \"vendor/sub\"]\n\tpath = vendor/sub\n\turl = https://example.com/sub.git\n"
	if err := os.WriteFile(filepath.Join(outer, ".gitmodules"), []byte(gitmodules), 0o600); err != nil {
		t.Fatal(err)
	}

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(outer), outer, nil,
		[]string{"vendor/sub/lib.go"})

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil && len(rec.BoundRepos) > 0 {
		t.Fatalf("registered submodule must not bind, got %+v", rec)
	}
}

// Regression: the common case (kept paths, no nested repos) must stay
// fork-free — detection is a stat-only ancestor walk and the resolver is
// never invoked.
func TestRecordForeignEvidence_NoNestedReposNoResolution(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	outer := newBindingRepo(t)
	if err := os.MkdirAll(filepath.Join(outer, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}

	binding.ClearResolveCache()
	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(outer), outer, nil,
		[]string{"pkg/a.go", "pkg/deep/b.go", "c.go"})

	if n := binding.ResolveCacheSizeForTesting(); n != 0 {
		t.Errorf("no-nested-repos turn resolved %d dirs, want 0 (zero git forks)", n)
	}
	if _, err := os.Stat(filepath.Join(userdirs.Config(), "sessions")); !os.IsNotExist(err) {
		t.Fatalf("no evidence must create nothing: %v", err)
	}
}

// Regression: detection that only checks the kept path's immediate parent for
// .git misses repos whose files sit in subdirectories — the walk must cover
// every ancestor strictly between the file's dir and the session root.
func TestRecordForeignEvidence_DeeplyNestedRepoDetected(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	outer := newBindingRepo(t)
	nested := newNestedBindingRepo(t, outer, "a/b/nested")

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(outer), outer, nil,
		[]string{"a/b/nested/x/y.md"})

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 1 || rec.BoundRepos[0].WorktreeRoot != nested {
		t.Fatalf("deeply nested repo not bound, got %+v", rec)
	}
}

// Regression 1: nested-repo evidence bypassing the shared per-turn resolution
// budget would let one pathological transcript fork unbounded git processes.
// Regression 2 (per-root dedupe): emitting one candidate per kept DIRECTORY
// instead of per detected nested ROOT spent one fork per directory of a busy
// nested repo — exhausting the budget on redundant resolutions of the same
// repo and starving a second nested repo later in the same turn.
func TestRecordForeignEvidence_NestedEvidenceSharesResolutionBudget(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	outer := newBindingRepo(t)
	busy := newNestedBindingRepo(t, outer, "busy")
	second := newNestedBindingRepo(t, outer, "second")

	// More distinct dirs in the busy repo than the whole resolution budget,
	// then one path in the second repo LAST — per-directory emission would
	// burn all 16 resolutions on the busy repo before reaching it.
	kept := make([]string, 0, maxForeignResolutionsPerTurn+3)
	for i := range maxForeignResolutionsPerTurn + 2 {
		dir := filepath.Join(outer, "busy", fmt.Sprintf("d%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, fmt.Sprintf("busy/d%02d/f.go", i))
	}
	kept = append(kept, "second/g.go")

	binding.ClearResolveCache()
	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(outer), outer, nil, kept)

	// One representative path per nested root → one resolution per repo.
	if n := binding.ResolveCacheSizeForTesting(); n != 2 {
		t.Errorf("resolved %d distinct dirs, want 2 (one per detected nested root)", n)
	}
	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 2 {
		t.Fatalf("expected both nested repos bound, got %+v", rec)
	}
	roots := map[string]bool{}
	for _, br := range rec.BoundRepos {
		roots[br.WorktreeRoot] = true
	}
	if !roots[busy] || !roots[second] {
		t.Errorf("recorded roots %v, want both %q and %q", roots, busy, second)
	}
}

// Regression: a repo nested UNDER a registered submodule path (typically the
// submodule's own submodule — only vendor/sub is in the session root's
// .gitmodules) was recorded as foreign, against the "part of the parent
// project" rationale. Containment must respect the "/" boundary: the sibling
// vendor/subextra shares the string prefix but is NOT under vendor/sub and
// must still be recorded.
func TestRecordForeignEvidence_TransitiveSubmoduleNotRecorded(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	outer := newBindingRepo(t)
	newNestedBindingRepo(t, outer, "vendor/sub")
	newNestedBindingRepo(t, outer, "vendor/sub/inner")
	subextra := newNestedBindingRepo(t, outer, "vendor/subextra")

	gitmodules := "[submodule \"vendor/sub\"]\n\tpath = vendor/sub\n\turl = https://example.com/sub.git\n"
	if err := os.WriteFile(filepath.Join(outer, ".gitmodules"), []byte(gitmodules), 0o600); err != nil {
		t.Fatal(err)
	}

	recordForeignEvidence(ctx, "sess-1", bindingTestMeta(outer), outer, nil,
		[]string{"vendor/sub/inner/f.go", "vendor/subextra/g.go"})

	rec, err := binding.LoadRecord(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected only the unregistered sibling bound, got %+v", rec)
	}
	if got := rec.BoundRepos[0].WorktreeRoot; got != subextra {
		t.Errorf("bound root = %q, want %q (vendor/sub/inner is transitively registered)", got, subextra)
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
