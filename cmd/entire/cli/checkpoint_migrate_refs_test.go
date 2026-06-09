package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestTreeRefName(t *testing.T) {
	t.Parallel()

	got := treeRefName(id.MustCheckpointID("ab3c4d5e6f70"))
	want := "refs/entire/checkpoints/ab/3c4d5e6f70/tree"
	if got.String() != want {
		t.Fatalf("treeRefName = %q, want %q", got.String(), want)
	}
}

// seedV1Checkpoints inits a repo, makes an initial commit, and writes the given
// checkpoint IDs onto entire/checkpoints/v1. Returns the repo dir.
func seedV1Checkpoints(t *testing.T, ids ...string) string {
	t.Helper()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# t"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t.com"},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
	for i, s := range ids {
		err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
			CheckpointID:     id.MustCheckpointID(s),
			SessionID:        "session-" + s,
			Strategy:         "manual-commit",
			Transcript:       redact.AlreadyRedacted([]byte("line\n")),
			Prompts:          []string{"prompt"},
			AuthorName:       "T",
			AuthorEmail:      "t@t.com",
			CheckpointsCount: i + 1,
		})
		if err != nil {
			t.Fatalf("WriteCommitted(%s): %v", s, err)
		}
	}
	return dir
}

func TestBuildCheckpointList(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70", "cd1122334455")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}

	entries, err := buildCheckpointList(context.Background(), repo)
	if err != nil {
		t.Fatalf("buildCheckpointList: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	got := []string{entries[0].ID.String(), entries[1].ID.String()}
	sort.Strings(got)
	want := []string{"ab3c4d5e6f70", "cd1122334455"}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for _, e := range entries {
		if e.Tree.IsZero() {
			t.Fatalf("entry %s has zero tree hash", e.ID)
		}
		if _, err := repo.TreeObject(e.Tree); err != nil {
			t.Fatalf("tree %s not a tree object: %v", e.Tree, err)
		}
	}
}

func TestBuildCheckpointList_NoBranch(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t) // no checkpoints -> no v1 branch
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	entries, err := buildCheckpointList(context.Background(), repo)
	if err != nil {
		t.Fatalf("buildCheckpointList: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestCacheFileRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "checkpoints.tsv")
	in := []checkpointEntry{
		{ID: id.MustCheckpointID("ab3c4d5e6f70"), Tree: plumbing.NewHash("1111111111111111111111111111111111111111")},
		{ID: id.MustCheckpointID("cd1122334455"), Tree: plumbing.NewHash("2222222222222222222222222222222222222222")},
	}
	if err := writeCacheFile(path, in); err != nil {
		t.Fatalf("writeCacheFile: %v", err)
	}
	out, err := readCacheFile(path)
	if err != nil {
		t.Fatalf("readCacheFile: %v", err)
	}
	if len(out) != 2 || out[0].ID != in[0].ID || out[0].Tree != in[0].Tree {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestReadCacheFile_Malformed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.tsv")
	if err := os.WriteFile(path, []byte("not-a-valid-line\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readCacheFile(path); err == nil {
		t.Fatalf("expected error for malformed line, got nil")
	}
}

func TestSnapshotExistingTreeRefs(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}

	// Pre-create one tree ref and an unrelated ref to confirm filtering.
	treeHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	if err := repo.Storer.SetReference(plumbing.NewHashReference(
		"refs/entire/checkpoints/ab/3c4d5e6f70/tree", treeHash)); err != nil {
		t.Fatalf("set tree ref: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(
		"refs/entire/checkpoints/v1.1", treeHash)); err != nil {
		t.Fatalf("set v1.1 ref: %v", err)
	}

	got, err := snapshotExistingTreeRefs(repo)
	if err != nil {
		t.Fatalf("snapshotExistingTreeRefs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d refs, want 1: %v", len(got), got)
	}
	if h, ok := got["refs/entire/checkpoints/ab/3c4d5e6f70/tree"]; !ok || h != treeHash {
		t.Fatalf("missing or wrong tree ref: %v", got)
	}
}

func TestProcessEntries_SkipsCorrectAndCollectsMissing(t *testing.T) {
	t.Parallel()

	good := plumbing.NewHash("1111111111111111111111111111111111111111")
	stale := plumbing.NewHash("2222222222222222222222222222222222222222")
	fresh := plumbing.NewHash("3333333333333333333333333333333333333333")

	entries := []checkpointEntry{
		{ID: id.MustCheckpointID("aa0000000001"), Tree: good},  // already correct -> skip
		{ID: id.MustCheckpointID("bb0000000002"), Tree: fresh}, // stale ref -> update
		{ID: id.MustCheckpointID("cc0000000003"), Tree: fresh}, // missing -> create
	}
	existing := map[string]plumbing.Hash{
		"refs/entire/checkpoints/aa/0000000001/tree": good,
		"refs/entire/checkpoints/bb/0000000002/tree": stale,
	}

	var progress bytes.Buffer
	updates, result, err := processEntries(context.Background(), entries, existing, 4, &progress)
	if err != nil {
		t.Fatalf("processEntries: %v", err)
	}

	if result.Total != 3 || result.Skipped != 1 || result.Created != 2 {
		t.Fatalf("result = %+v, want total=3 skipped=1 created=2", result)
	}
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
	gotRefs := map[string]plumbing.Hash{}
	for _, u := range updates {
		gotRefs[u.Ref] = u.Hash
	}
	if gotRefs["refs/entire/checkpoints/bb/0000000002/tree"] != fresh {
		t.Fatalf("bb not updated to fresh: %v", gotRefs)
	}
	if gotRefs["refs/entire/checkpoints/cc/0000000003/tree"] != fresh {
		t.Fatalf("cc not created with fresh: %v", gotRefs)
	}
	if progress.Len() == 0 {
		t.Fatalf("expected progress output, got none")
	}
}

func TestProcessEntries_ContextCancelled(t *testing.T) {
	t.Parallel()

	entries := []checkpointEntry{
		{ID: id.MustCheckpointID("aa0000000001"), Tree: plumbing.NewHash("1111111111111111111111111111111111111111")},
		{ID: id.MustCheckpointID("bb0000000002"), Tree: plumbing.NewHash("2222222222222222222222222222222222222222")},
		{ID: id.MustCheckpointID("cc0000000003"), Tree: plumbing.NewHash("3333333333333333333333333333333333333333")},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := processEntries(ctx, entries, map[string]plumbing.Hash{}, 4, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestApplyRefUpdates_MultipleBatches(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70", "cd1122334455")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	entries, err := buildCheckpointList(context.Background(), repo)
	if err != nil || len(entries) != 2 {
		t.Fatalf("buildCheckpointList: %v len=%d", err, len(entries))
	}

	updates := make([]refUpdate, 0, len(entries))
	for _, e := range entries {
		updates = append(updates, refUpdate{Ref: treeRefName(e.ID).String(), Hash: e.Tree})
	}

	// batchSize=1 forces one batch per update (two batches here).
	if err := applyRefUpdates(context.Background(), dir, updates, 1); err != nil {
		t.Fatalf("applyRefUpdates: %v", err)
	}

	for _, e := range entries {
		ref, err := repo.Reference(treeRefName(e.ID), false)
		if err != nil {
			t.Fatalf("ref for %s missing: %v", e.ID, err)
		}
		if ref.Hash() != e.Tree {
			t.Fatalf("ref %s -> %s, want %s", e.ID, ref.Hash(), e.Tree)
		}
	}
}

func TestApplyRefUpdates(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	// Use the real subtree hash so update-ref's object existence check passes.
	entries, err := buildCheckpointList(context.Background(), repo)
	if err != nil || len(entries) != 1 {
		t.Fatalf("buildCheckpointList: %v len=%d", err, len(entries))
	}
	want := entries[0].Tree

	updates := []refUpdate{
		{Ref: "refs/entire/checkpoints/ab/3c4d5e6f70/tree", Hash: want},
	}
	if err := applyRefUpdates(context.Background(), dir, updates, 1000); err != nil {
		t.Fatalf("applyRefUpdates: %v", err)
	}

	ref, err := repo.Reference("refs/entire/checkpoints/ab/3c4d5e6f70/tree", false)
	if err != nil {
		t.Fatalf("reference not created: %v", err)
	}
	if ref.Hash() != want {
		t.Fatalf("ref points at %s, want %s", ref.Hash(), want)
	}
}

func TestApplyRefUpdates_Empty(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70")
	if err := applyRefUpdates(context.Background(), dir, nil, 1000); err != nil {
		t.Fatalf("applyRefUpdates(nil): %v", err)
	}
}

func TestRunMigrateTreeRefs_EndToEnd(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70", "cd1122334455")
	cache := filepath.Join(t.TempDir(), "checkpoints.tsv")

	var stdout, progress bytes.Buffer
	opts := migrateRefsOptions{
		repoRoot:  dir,
		cacheFile: cache,
		workers:   4,
		out:       &stdout,
		progress:  &progress,
	}

	res, err := runMigrateTreeRefs(context.Background(), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Total != 2 || res.Created != 2 || res.Skipped != 0 {
		t.Fatalf("first run result = %+v", res)
	}

	// Cache file exists after phase 1.
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	expected, err := buildCheckpointList(context.Background(), repo)
	if err != nil {
		t.Fatalf("buildCheckpointList: %v", err)
	}
	for _, e := range expected {
		ref, err := repo.Reference(treeRefName(e.ID), false)
		if err != nil {
			t.Fatalf("ref for %s missing: %v", e.ID, err)
		}
		if ref.Hash() != e.Tree {
			t.Fatalf("ref %s -> %s, want %s", e.ID, ref.Hash(), e.Tree)
		}
	}

	// Second run is idempotent: everything skipped.
	res2, err := runMigrateTreeRefs(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res2.Created != 0 || res2.Skipped != 2 {
		t.Fatalf("second run result = %+v, want created=0 skipped=2", res2)
	}
}

func TestRunMigrateTreeRefs_DryRun(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70")
	cache := filepath.Join(t.TempDir(), "checkpoints.tsv")
	var stdout, progress bytes.Buffer
	opts := migrateRefsOptions{
		repoRoot: dir, cacheFile: cache, workers: 2, dryRun: true,
		out: &stdout, progress: &progress,
	}
	res, err := runMigrateTreeRefs(context.Background(), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Created != 1 {
		t.Fatalf("dry-run result = %+v, want created=1 (would-create)", res)
	}
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	if _, err := repo.Reference("refs/entire/checkpoints/ab/3c4d5e6f70/tree", false); err == nil {
		t.Fatalf("dry-run must not create refs")
	}
}

func TestRunMigrateTreeRefs_ResumeReusesCache(t *testing.T) {
	t.Parallel()

	dir := seedV1Checkpoints(t, "ab3c4d5e6f70")
	cache := filepath.Join(t.TempDir(), "checkpoints.tsv")
	// Pre-seed a cache file with a DIFFERENT id; without --refresh it is reused.
	// The tree hash must be a real object: git update-ref rejects refs that point
	// at nonexistent objects, so reuse the seeded checkpoint's actual subtree.
	repoForTree, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	seeded, err := buildCheckpointList(context.Background(), repoForTree)
	if err != nil || len(seeded) != 1 {
		t.Fatalf("buildCheckpointList: %v len=%d", err, len(seeded))
	}
	preTree := seeded[0].Tree
	if err := writeCacheFile(cache, []checkpointEntry{
		{ID: id.MustCheckpointID("ff0000000099"), Tree: preTree},
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	var stdout, progress bytes.Buffer
	opts := migrateRefsOptions{repoRoot: dir, cacheFile: cache, workers: 1, out: &stdout, progress: &progress}
	res, err := runMigrateTreeRefs(context.Background(), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("resume should process the cached entry only, got total=%d", res.Total)
	}
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	if _, err := repo.Reference("refs/entire/checkpoints/ff/0000000099/tree", false); err != nil {
		t.Fatalf("cached entry ref not created: %v", err)
	}
}

func TestNewCheckpointMigrateRefsCmd_Hidden(t *testing.T) {
	t.Parallel()

	cmd := newCheckpointMigrateRefsCmd()
	if cmd.Use != "migrate-refs" {
		t.Fatalf("Use = %q, want migrate-refs", cmd.Use)
	}
	if !cmd.Hidden {
		t.Fatalf("command must be hidden")
	}
	for _, name := range []string{"workers", "cache-file", "refresh", "dry-run"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s flag", name)
		}
	}
}

func TestCheckpointGroup_RegistersMigrateRefs(t *testing.T) {
	t.Parallel()

	group := newCheckpointGroupCmd()
	var found bool
	for _, c := range group.Commands() {
		if c.Name() == "migrate-refs" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("checkpoint group does not register migrate-refs")
	}
}
