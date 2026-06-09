package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// These benchmarks compare two ways to resolve a single checkpoint's metadata
// subtree on a real, large checkpoints repo:
//
//   - "V1Walk" (previous approach): resolve the entire/checkpoints/v1 branch ->
//     commit -> root tree, then navigate to the <id[:2]>/<id[2:]> subtree. This
//     loads the commit, the 256-entry root tree, the bucket tree, and finally
//     the checkpoint subtree.
//   - "TreeRef" (new approach): read refs/entire/checkpoints/<id[:2]>/<id[2:]>/tree
//     directly and load that one tree object.
//
// Each is offered in two flavors:
//   - warm: a single shared *git.Repository handle (go-git's object cache warms
//     up after the first iteration). This isolates the marginal per-lookup cost
//     for repeated lookups inside one long-lived process.
//   - cold: a fresh gitrepo.OpenPath per op, modelling a one-shot CLI invocation
//     (e.g. `entire checkpoint explain <id>`) where nothing is cached yet.
//
// The benchmarks point at a real repo and SKIP when it is absent, so they never
// run in CI. Override the path with ENTIRE_BENCH_CHECKPOINTS_REPO; the default
// is ~/git/entireio/cli-checkpoints.
//
// Run with, e.g.:
//
//	go test ./cmd/entire/cli/ -run '^$' -bench 'BenchmarkResolveSubtree' -benchmem

func benchRepoPath(b *testing.B) string {
	b.Helper()
	if p := os.Getenv("ENTIRE_BENCH_CHECKPOINTS_REPO"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		b.Skipf("cannot resolve home dir: %v", err)
	}
	return filepath.Join(home, "git", "entireio", "cli-checkpoints")
}

// benchCheckpointIDs collects the checkpoint IDs that already have tree refs.
// Using the tree refs (rather than walking v1) to enumerate keeps setup cheap
// and guarantees both resolvers can find every sampled ID.
func benchCheckpointIDs(b *testing.B, repo *git.Repository) []id.CheckpointID {
	b.Helper()
	iter, err := repo.References()
	if err != nil {
		b.Fatalf("list references: %v", err)
	}
	defer iter.Close()

	var ids []id.CheckpointID
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if !strings.HasPrefix(name, treeRefPrefix) || !strings.HasSuffix(name, treeRefSuffix) {
			return nil
		}
		middle := strings.TrimSuffix(strings.TrimPrefix(name, treeRefPrefix), treeRefSuffix)
		cpID, err := id.NewCheckpointID(strings.ReplaceAll(middle, "/", ""))
		if err != nil {
			return nil //nolint:nilerr // skip refs whose path is not a valid checkpoint id
		}
		ids = append(ids, cpID)
		return nil
	})
	if err != nil {
		b.Fatalf("iterate references: %v", err)
	}
	if len(ids) == 0 {
		b.Skip("no checkpoint tree refs found; run `entire checkpoint migrate-refs` against the repo first")
	}
	return ids
}

func openBenchRepo(b *testing.B) *git.Repository {
	b.Helper()
	dir := benchRepoPath(b)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		b.Skipf("benchmark repo not found at %s (set ENTIRE_BENCH_CHECKPOINTS_REPO)", dir)
	}
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		b.Fatalf("open repo %s: %v", dir, err)
	}
	return repo
}

// resolveSubtreeViaV1Walk is the previous approach: navigate the v1 branch tree.
func resolveSubtreeViaV1Walk(repo *git.Repository, cpID id.CheckpointID) (*object.Tree, error) {
	branch := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(branch, true)
	if err != nil {
		return nil, err
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, err
	}
	root, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	sub, err := root.Tree(cpID.Path())
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// resolveSubtreeViaTreeRef is the new approach: read the per-checkpoint tree ref.
func resolveSubtreeViaTreeRef(repo *git.Repository, cpID id.CheckpointID) (*object.Tree, error) {
	ref, err := repo.Reference(treeRefName(cpID), false)
	if err != nil {
		return nil, err
	}
	tree, err := repo.TreeObject(ref.Hash())
	if err != nil {
		return nil, err
	}
	return tree, nil
}

func BenchmarkResolveSubtree_V1Walk_Warm(b *testing.B) {
	repo := openBenchRepo(b)
	ids := benchCheckpointIDs(b, repo)

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		tree, err := resolveSubtreeViaV1Walk(repo, ids[i%len(ids)])
		if err != nil {
			b.Fatalf("v1 walk: %v", err)
		}
		if tree.Hash.IsZero() {
			b.Fatal("zero tree hash")
		}
		i++
	}
}

func BenchmarkResolveSubtree_TreeRef_Warm(b *testing.B) {
	repo := openBenchRepo(b)
	ids := benchCheckpointIDs(b, repo)

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		tree, err := resolveSubtreeViaTreeRef(repo, ids[i%len(ids)])
		if err != nil {
			b.Fatalf("tree ref: %v", err)
		}
		if tree.Hash.IsZero() {
			b.Fatal("zero tree hash")
		}
		i++
	}
}

func BenchmarkResolveSubtree_V1Walk_Cold(b *testing.B) {
	dir := benchRepoPath(b)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		b.Skipf("benchmark repo not found at %s (set ENTIRE_BENCH_CHECKPOINTS_REPO)", dir)
	}
	// Enumerate IDs once with a throwaway handle; the timed loop opens fresh.
	seedRepo := openBenchRepo(b)
	ids := benchCheckpointIDs(b, seedRepo)

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		repo, err := gitrepo.OpenPath(dir)
		if err != nil {
			b.Fatalf("open repo: %v", err)
		}
		tree, err := resolveSubtreeViaV1Walk(repo, ids[i%len(ids)])
		if err != nil {
			b.Fatalf("v1 walk: %v", err)
		}
		if tree.Hash.IsZero() {
			b.Fatal("zero tree hash")
		}
		i++
	}
}

func BenchmarkResolveSubtree_TreeRef_Cold(b *testing.B) {
	dir := benchRepoPath(b)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		b.Skipf("benchmark repo not found at %s (set ENTIRE_BENCH_CHECKPOINTS_REPO)", dir)
	}
	seedRepo := openBenchRepo(b)
	ids := benchCheckpointIDs(b, seedRepo)

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		repo, err := gitrepo.OpenPath(dir)
		if err != nil {
			b.Fatalf("open repo: %v", err)
		}
		tree, err := resolveSubtreeViaTreeRef(repo, ids[i%len(ids)])
		if err != nil {
			b.Fatalf("tree ref: %v", err)
		}
		if tree.Hash.IsZero() {
			b.Fatal("zero tree hash")
		}
		i++
	}
}
