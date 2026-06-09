package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// treeRefName returns the ref that points at a checkpoint's metadata subtree:
// refs/entire/checkpoints/<id[:2]>/<id[2:]>/tree.
func treeRefName(cpID id.CheckpointID) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/entire/checkpoints/" + cpID.Path() + "/tree")
}

// checkpointEntry pairs a checkpoint ID with its metadata-subtree hash on v1.
type checkpointEntry struct {
	ID   id.CheckpointID
	Tree plumbing.Hash
}

// buildCheckpointList walks the entire/checkpoints/v1 branch tree and returns
// one entry per checkpoint. A missing v1 branch yields an empty list (no error).
func buildCheckpointList(ctx context.Context, repo *git.Repository) ([]checkpointEntry, error) {
	branch := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(branch, true)
	if err != nil {
		// No v1 branch -> nothing to migrate.
		return nil, nil //nolint:nilerr // absent branch is an empty list, not an error
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("resolve v1 commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("resolve v1 tree: %w", err)
	}

	var entries []checkpointEntry
	walkErr := checkpoint.WalkCheckpointShards(ctx, repo, tree, func(cpID id.CheckpointID, cpTreeHash plumbing.Hash) error {
		entries = append(entries, checkpointEntry{ID: cpID, Tree: cpTreeHash})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk checkpoint shards: %w", walkErr)
	}
	return entries, nil
}

// writeCacheFile writes one "<id>\t<treeHash>" line per entry, creating parent
// directories as needed.
func writeCacheFile(path string, entries []checkpointEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s\t%s\n", e.ID, e.Tree.String())
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil { //nolint:gosec // cache file under git common dir
		return fmt.Errorf("write cache file: %w", err)
	}
	return nil
}

// readCacheFile parses the TSV cache file. Blank lines are skipped; malformed
// lines return an error naming the offending content.
func readCacheFile(path string) ([]checkpointEntry, error) {
	f, err := os.Open(path) //nolint:gosec // cache file under git common dir
	if err != nil {
		return nil, fmt.Errorf("open cache file: %w", err)
	}
	defer f.Close()

	var entries []checkpointEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		idStr, hashStr, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("malformed cache line: %q", line)
		}
		cpID, err := id.NewCheckpointID(idStr)
		if err != nil {
			return nil, fmt.Errorf("malformed cache line %q: %w", line, err)
		}
		entries = append(entries, checkpointEntry{ID: cpID, Tree: plumbing.NewHash(hashStr)})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read cache file: %w", err)
	}
	return entries, nil
}

const (
	treeRefPrefix = "refs/entire/checkpoints/"
	treeRefSuffix = "/tree"
)

// snapshotExistingTreeRefs reads every refs/entire/checkpoints/.../tree ref once
// into a map keyed by full ref name. Reading up front keeps the later existence
// check a concurrency-safe map read (go-git ref reads are not parallelized).
func snapshotExistingTreeRefs(repo *git.Repository) (map[string]plumbing.Hash, error) {
	iter, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("list references: %w", err)
	}
	defer iter.Close()

	out := make(map[string]plumbing.Hash)
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if strings.HasPrefix(name, treeRefPrefix) && strings.HasSuffix(name, treeRefSuffix) {
			out[name] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate references: %w", err)
	}
	return out, nil
}

// refUpdate is a single ref that must be created or repointed.
type refUpdate struct {
	Ref  string
	Hash plumbing.Hash
}

// migrateRefsResult summarizes a run.
type migrateRefsResult struct {
	Created int
	Skipped int
	Total   int
}

// processEntries fans the entries across a worker pool. Each worker compares the
// desired ref against the pre-snapshotted existing refs: it skips when present
// and already correct, otherwise emits a refUpdate. Progress (processed/total)
// is written to the progress writer, throttled. Writes are NOT performed here.
func processEntries(ctx context.Context, entries []checkpointEntry, existing map[string]plumbing.Hash, workers int, progress io.Writer) ([]refUpdate, migrateRefsResult) {
	total := len(entries)
	if workers < 1 {
		workers = 1
	}

	in := make(chan checkpointEntry)
	out := make(chan refUpdate)
	var processed int64

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for e := range in {
				if ctx.Err() != nil {
					return
				}
				ref := treeRefName(e.ID).String()
				if cur, ok := existing[ref]; !ok || cur != e.Tree {
					out <- refUpdate{Ref: ref, Hash: e.Tree}
				}
				n := atomic.AddInt64(&processed, 1)
				reportProgress(progress, n, int64(total))
			}
		})
	}

	go func() {
		defer close(in)
		for _, e := range entries {
			if ctx.Err() != nil {
				return
			}
			in <- e
		}
	}()

	go func() {
		wg.Wait()
		close(out)
	}()

	var updates []refUpdate
	for u := range out {
		updates = append(updates, u)
	}

	flushProgress(progress)
	return updates, migrateRefsResult{
		Created: len(updates),
		Skipped: total - len(updates),
		Total:   total,
	}
}

// reportProgress prints "n/total". On a terminal it rewrites a single line with
// \r; otherwise it prints periodically (every 100 items) to avoid log spam.
func reportProgress(w io.Writer, n, total int64) {
	if interactive.IsTerminalWriter(w) {
		fmt.Fprintf(w, "\r%d/%d", n, total)
		return
	}
	if n == total || n%100 == 0 {
		fmt.Fprintf(w, "%d/%d\n", n, total)
	}
}

// flushProgress ends the single-line terminal progress with a newline.
func flushProgress(w io.Writer) {
	if interactive.IsTerminalWriter(w) {
		fmt.Fprintln(w)
	}
}

// applyRefUpdates creates/repoints refs in batches via `git update-ref --stdin`.
// Each batch is one atomic git transaction. The "update <ref> <newvalue>" line
// form (old value omitted) creates-or-updates regardless of current value; ref
// names are controlled hex + "/tree", so the non-NUL line format is safe.
func applyRefUpdates(ctx context.Context, repoRoot string, updates []refUpdate, batchSize int) error {
	if len(updates) == 0 {
		return nil
	}
	if batchSize < 1 {
		batchSize = 1000
	}
	for start := 0; start < len(updates); start += batchSize {
		end := min(start+batchSize, len(updates))
		if err := applyRefUpdateBatch(ctx, repoRoot, updates[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func applyRefUpdateBatch(ctx context.Context, repoRoot string, batch []refUpdate) error {
	var stdin bytes.Buffer
	for _, u := range batch {
		fmt.Fprintf(&stdin, "update %s %s\n", u.Ref, u.Hash.String())
	}
	cmd := exec.CommandContext(ctx, "git", "update-ref", "--stdin")
	cmd.Dir = repoRoot
	cmd.Stdin = &stdin
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git update-ref --stdin: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

const refUpdateBatchSize = 1000

// migrateRefsOptions configures a migrate-refs run. repoRoot, out, and progress
// are required; the cobra layer fills them from CWD and the command's streams.
type migrateRefsOptions struct {
	repoRoot  string
	cacheFile string
	workers   int
	refresh   bool
	dryRun    bool
	out       io.Writer
	progress  io.Writer
}

// runMigrateTreeRefs runs both phases and returns the summary. In dry-run mode
// the returned Created count reflects the refs that WOULD be written.
func runMigrateTreeRefs(ctx context.Context, opts migrateRefsOptions) (migrateRefsResult, error) {
	if opts.workers < 1 {
		opts.workers = runtime.NumCPU()
	}

	repo, err := gitrepo.OpenPath(opts.repoRoot)
	if err != nil {
		return migrateRefsResult{}, fmt.Errorf("open repository: %w", err)
	}

	// Phase 1: build (or reuse) the checkpoint list.
	entries, err := loadOrBuildList(ctx, repo, opts)
	if err != nil {
		return migrateRefsResult{}, err
	}
	if len(entries) == 0 {
		fmt.Fprintln(opts.out, "No checkpoints found on entire/checkpoints/v1; nothing to do.")
		return migrateRefsResult{}, nil
	}

	// Phase 2: snapshot existing refs, decide updates, apply.
	existing, err := snapshotExistingTreeRefs(repo)
	if err != nil {
		return migrateRefsResult{}, err
	}
	updates, result := processEntries(ctx, entries, existing, opts.workers, opts.progress)

	if opts.dryRun {
		fmt.Fprintf(opts.out, "[dry-run] would create %d, skip %d (total %d)\n", result.Created, result.Skipped, result.Total)
		return result, nil
	}

	if err := applyRefUpdates(ctx, opts.repoRoot, updates, refUpdateBatchSize); err != nil {
		return migrateRefsResult{}, err
	}
	fmt.Fprintf(opts.out, "created=%d skipped=%d total=%d\n", result.Created, result.Skipped, result.Total)
	return result, nil
}

// loadOrBuildList returns the cached entry list when a cache file exists and
// --refresh is not set; otherwise it walks v1 and writes the cache file.
func loadOrBuildList(ctx context.Context, repo *git.Repository, opts migrateRefsOptions) ([]checkpointEntry, error) {
	if !opts.refresh && opts.cacheFile != "" {
		if _, statErr := os.Stat(opts.cacheFile); statErr == nil {
			entries, err := readCacheFile(opts.cacheFile)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(opts.out, "Reusing cached list: %d checkpoints (%s)\n", len(entries), opts.cacheFile)
			return entries, nil
		}
	}

	entries, err := buildCheckpointList(ctx, repo)
	if err != nil {
		return nil, err
	}
	if opts.cacheFile != "" {
		if err := writeCacheFile(opts.cacheFile, entries); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(opts.out, "Found %d checkpoints on entire/checkpoints/v1\n", len(entries))
	return entries, nil
}
