package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// maxCheckpointBlobBytes caps a single git-blob read from a checkpoint tree.
// It matches agent.MaxChunkSize, the cap the checkpoint store already writes
// transcripts under, so any blob this store wrote is readable here; a larger
// blob means the tree was tampered with, mis-chunked, or written by an
// incompatible producer, and reading it should fail loudly rather than buffer
// an unbounded, potentially attacker-controlled amount of memory.
//
// Checkpoint trees are not always locally authored: they can be fetched from
// a shared or pushed remote (entire/checkpoints/v1, or a per-checkpoint ref),
// which any collaborator with checkpoint-push access — or a compromised or
// malicious remote — controls the bytes of. This mirrors the cap the HTTP
// checkpoint reader already enforces (checkpoint_api_reader.go's
// maxAPITranscriptBytes) for the equivalent read over the network.
const maxCheckpointBlobBytes = agent.MaxChunkSize

// ErrCheckpointBlobTooLarge is returned by readBlobContents when a blob
// exceeds maxCheckpointBlobBytes. Callers that would otherwise treat a
// content-read failure as "field absent, keep going" must check for this
// specific error and fail loudly instead — an oversized blob is a signal a
// tree was tampered with or corrupted, not a normal missing-file case, and
// must never be swallowed into an empty/partial result that looks like
// nothing went wrong.
var ErrCheckpointBlobTooLarge = errors.New("checkpoint blob exceeds read limit")

// readBlobContents reads file's content as a string, capped at
// maxCheckpointBlobBytes. It is the checkpoint-tree equivalent of
// checkpoint_api_reader.go's capped HTTP transcript read: LimitReader+1 so a
// blob exactly at the cap is not mistaken for an oversized one, and a blob
// over the cap is rejected with ErrCheckpointBlobTooLarge rather than
// silently truncated to the first maxCheckpointBlobBytes bytes and returned
// as if that were the whole file.
func readBlobContents(file *object.File) (string, error) {
	r, err := file.Reader()
	if err != nil {
		return "", fmt.Errorf("open blob %s: %w", file.Name, err)
	}
	defer r.Close()

	data, err := io.ReadAll(io.LimitReader(r, maxCheckpointBlobBytes+1))
	if err != nil {
		return "", fmt.Errorf("read blob %s: %w", file.Name, err)
	}
	if len(data) > maxCheckpointBlobBytes {
		return "", fmt.Errorf("%w: %s is larger than %d MB", ErrCheckpointBlobTooLarge, file.Name, maxCheckpointBlobBytes>>20)
	}
	return string(data), nil
}

// BlobFetchFunc fetches missing blob objects by hash from a remote.
type BlobFetchFunc func(ctx context.Context, hashes []plumbing.Hash) error

// RefFetchFunc fetches a single checkpoint ref from the remote into the local
// ref of the same name. The git-refs store uses it to resolve a checkpoint ref
// that is not present locally (e.g. written on another machine). The checkpoint
// package cannot resolve the remote target itself, so the CLI layer injects it.
type RefFetchFunc func(ctx context.Context, ref plumbing.ReferenceName) error

// MetadataBranchFetchFunc fetches the v1 metadata branch from the configured
// checkpoint remote into the local ref. It is the git-branch store's counterpart
// to RefFetchFunc: the git-refs store resolves one missing per-checkpoint ref,
// while the git-branch store's whole record lives on a single branch, so a
// missing local branch is resolved by fetching that branch.
//
// Without it, a clone whose checkpoints live on a dedicated checkpoint_remote
// cannot read any committed checkpoint until something else happens to populate
// the branch: the store falls back to origin's remote-tracking ref, which a
// checkpoint_remote setup does not have. The checkpoint package cannot resolve
// the remote target itself, so the CLI layer injects it.
//
// Only wire it on foreground, user-initiated read paths. It fires only when the
// branch is missing both locally and on origin — i.e. when the read would
// otherwise fail outright — but it is still a network call, and hook paths must
// stay network-free.
type MetadataBranchFetchFunc func(ctx context.Context) error

// RemoteRefListFunc enumerates the per-checkpoint refs present on the configured
// checkpoint remote (names only, via `ls-remote refs/entire/checkpoints/*` — no
// object transfer), returning their full ref names. The git-refs store uses it
// in List to discover checkpoints written on another machine that have no local
// ref yet; each discovered checkpoint is then hydrated lazily on read via
// RefFetchFunc. The checkpoint package cannot resolve the remote target itself,
// so the CLI layer injects it.
//
// Scope is stricter than the on-demand read fetch: with no checkpoint_remote
// configured the lister returns (nil, nil) and List stays local-only. The
// on-demand fetch (FetchURL) falls back to origin in that case. When a
// checkpoint_remote is configured the lister queries the resolved checkpoint
// URL (which can still fall through to origin in FetchURL edge cases).
type RemoteRefListFunc func(ctx context.Context) ([]plumbing.ReferenceName, error)

// FetchingTree wraps a git tree to automatically fetch missing blobs on demand.
// After a treeless fetch (--filter=blob:none), tree objects are available locally
// but blob objects are not. Each File() call checks whether the target blob
// exists locally and fetches it from the remote if missing, using FindEntry
// to locate the blob hash without resolving the blob itself.
//
// Because go-git's ObjectStorage caches the packfile index and never refreshes
// it, blobs fetched by external git commands (e.g. git fetch-pack) may not be
// visible to go-git's storer. As a fallback, File() reads the blob via
// "git cat-file" which always sees the current on-disk object store.
//
// For best performance, call PreFetch before reading files. PreFetch walks
// the tree, identifies locally-missing blobs, and batch-fetches them in a
// single network round-trip instead of one fetch per File() miss.
type FetchingTree struct {
	inner  *object.Tree
	ctx    context.Context
	storer storer.EncodedObjectStorer
	fetch  BlobFetchFunc
}

// NewFetchingTree wraps a git tree with on-demand blob fetching.
// The storer is used to check if blobs exist locally, and fetch is called
// to download any that are missing. If fetch is nil, File() behaves
// identically to the underlying tree.
func NewFetchingTree(ctx context.Context, tree *object.Tree, s storer.EncodedObjectStorer, fetch BlobFetchFunc) *FetchingTree {
	return &FetchingTree{
		inner:  tree,
		ctx:    ctx,
		storer: s,
		fetch:  fetch,
	}
}

// File returns the file at the given path. Resolution order:
//  1. go-git's storer (fast path, in-memory).
//  2. `git cat-file -p` against the on-disk object store (handles
//     partial-clone-filtered blobs that go-git can't see, plus packfiles
//     created by external git commands after this process opened the repo).
//  3. Remote fetch via the configured fetcher, then cat-file again.
//
// Trying cat-file BEFORE the remote fetch is critical: in partial-clone
// repos, blobs are commonly on disk but invisible to go-git's storer
// (filtered out, or in a packfile not in go-git's index cache). Without
// this short-circuit, every File() would burn a multi-second network
// round-trip even though the blob is already local.
func (t *FetchingTree) File(path string) (*object.File, error) {
	if file, err := t.inner.File(path); err == nil {
		return file, nil
	}

	entry, findErr := t.inner.FindEntry(path)
	if findErr != nil {
		logging.Debug(t.ctx, "FetchingTree.File: entry not found",
			slog.String("path", path),
			slog.String("error", findErr.Error()),
		)
		return nil, findErr //nolint:wrapcheck // return original error
	}

	if file, gitErr := t.readFileViaGit(path, entry); gitErr == nil {
		return file, nil
	}

	if t.fetch == nil {
		return nil, fmt.Errorf("blob %s not available locally and no fetcher configured", entry.Hash.String()[:12])
	}

	logging.Debug(t.ctx, "FetchingTree.File: blob missing locally, fetching from remote",
		slog.String("path", path),
		slog.String("hash", entry.Hash.String()[:12]),
	)
	if fetchErr := t.fetch(t.ctx, []plumbing.Hash{entry.Hash}); fetchErr != nil {
		logging.Warn(t.ctx, "FetchingTree.File: blob fetch failed",
			slog.String("path", path),
			slog.String("hash", entry.Hash.String()[:12]),
			slog.String("error", fetchErr.Error()),
		)
		return nil, fetchErr
	}

	if file, err := t.inner.File(path); err == nil {
		return file, nil
	}

	logging.Debug(t.ctx, "FetchingTree.File: storer cache stale, reading via git cat-file",
		slog.String("path", path),
		slog.String("hash", entry.Hash.String()[:12]),
	)
	return t.readFileViaGit(path, entry)
}

// PreFetch walks the tree recursively, identifies blob entries that are missing
// from the local object store, and batch-fetches them in a single call to
// t.fetch. This avoids per-blob network round-trips during subsequent File()
// calls. It is safe to call even when all blobs are already local (no-op).
// Returns the number of blobs fetched.
func (t *FetchingTree) PreFetch() (int, error) {
	if t.fetch == nil || t.storer == nil {
		return 0, nil
	}

	missing := t.collectMissingBlobs(t.inner)
	if len(missing) == 0 {
		return 0, nil
	}

	logging.Debug(t.ctx, "FetchingTree.PreFetch: batch-fetching missing blobs",
		slog.Int("count", len(missing)),
	)

	if err := t.fetch(t.ctx, missing); err != nil {
		return 0, fmt.Errorf("prefetch %d blobs: %w", len(missing), err)
	}

	return len(missing), nil
}

// CollectMissingBlobs returns the hashes of every blob entry in this tree
// (recursively) that isn't present in the local object store. Useful for
// callers that want to decide whether network work is needed before
// running PreFetch (e.g., to avoid showing a spinner in fast no-op cases).
func (t *FetchingTree) CollectMissingBlobs() []plumbing.Hash {
	return t.collectMissingBlobs(t.inner)
}

// collectMissingBlobs recursively walks a tree and returns hashes of blob
// entries that are not present in the local object store. The walk asks
// go-git first and settles every storer miss in a single `git cat-file
// --batch-check`, so the cost is one subprocess per tree rather than one per
// candidate blob.
func (t *FetchingTree) collectMissingBlobs(tree *object.Tree) []plumbing.Hash {
	candidates := t.collectStorerMisses(tree)
	if len(candidates) == 0 {
		return nil
	}
	return t.rejectBlobsOnDisk(candidates)
}

// collectStorerMisses walks a tree recursively and returns the hash of every
// blob entry go-git's storer cannot see. That is only half an answer: in a
// partial-clone repo the storer also misses blobs that ARE on disk (filtered
// out of its index, or in a packfile written after this process opened the
// repo), which is what rejectBlobsOnDisk settles.
func (t *FetchingTree) collectStorerMisses(tree *object.Tree) []plumbing.Hash {
	var candidates []plumbing.Hash
	for _, entry := range tree.Entries {
		if entry.Mode.IsFile() {
			if t.storer.HasEncodedObject(entry.Hash) != nil {
				candidates = append(candidates, entry.Hash)
			}
			continue
		}
		// Recurse into subtrees (tree objects are local after treeless fetch).
		subtree, err := tree.Tree(entry.Name)
		if err == nil {
			candidates = append(candidates, t.collectStorerMisses(subtree)...)
		}
	}
	return candidates
}

// rejectBlobsOnDisk returns the candidates that git cannot find in the local
// object store, preserving order. A failed probe is treated as "nothing is on
// disk" — the same direction the per-hash check failed in, and the safe one: a
// candidate wrongly reported missing costs one batched fetch, while one wrongly
// reported present makes File() fall back to a per-blob fetch.
func (t *FetchingTree) rejectBlobsOnDisk(candidates []plumbing.Hash) []plumbing.Hash {
	onDisk, err := t.blobsOnDisk(candidates)
	if err != nil {
		logging.Debug(t.ctx, "FetchingTree.rejectBlobsOnDisk: batch probe failed, treating every candidate as missing",
			slog.Int("candidates", len(candidates)),
			slog.String("error", err.Error()),
		)
		return candidates
	}

	missing := make([]plumbing.Hash, 0, len(candidates))
	for _, hash := range candidates {
		if !onDisk[hash.String()] {
			missing = append(missing, hash)
		}
	}
	return missing
}

// blobsOnDisk reports which of the given hashes the local object store holds,
// keyed by hex string, asking `git cat-file --batch-check` once for the whole
// set — git prints "<oid> missing" for an object it cannot find and
// "<oid> <type> <size>" for one it can.
//
// GIT_NO_LAZY_FETCH is what makes the answer an answer: in a promisor repo
// (any partial clone, including the URL-keyed remote section our own filtered
// fetches leave behind) an unguarded probe fetches the very blob it is asking
// about — one `git fetch --stdin` per missing object — and then reports it
// present. --batch-check batches the lookups, not the promisor fetches.
func (t *FetchingTree) blobsOnDisk(hashes []plumbing.Hash) (map[string]bool, error) {
	lines := make([]string, len(hashes))
	for i, hash := range hashes {
		lines[i] = hash.String()
	}

	cmd := exec.CommandContext(t.ctx, "git", "cat-file", "--batch-check")
	cmd.Env = append(cmd.Environ(), "GIT_NO_LAZY_FETCH=1")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cat-file --batch-check over %d hashes: %w", len(hashes), err)
	}

	onDisk := make(map[string]bool, len(hashes))
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		// "<oid> missing" carries two fields as well, so test the verdict
		// rather than the field count.
		if len(fields) < 2 || fields[1] == "missing" {
			continue
		}
		onDisk[fields[0]] = true
	}
	return onDisk, nil
}

// readFileViaGit reads a blob via "git cat-file -p <hash>" and returns an
// in-memory *object.File. This bypasses go-git's storer which may have a
// stale packfile index after external git commands fetched new objects.
func (t *FetchingTree) readFileViaGit(path string, entry *object.TreeEntry) (*object.File, error) {
	cmd := exec.CommandContext(t.ctx, "git", "cat-file", "-p", entry.Hash.String())
	cmd.Env = append(cmd.Environ(), "GIT_NO_LAZY_FETCH=1")
	content, cmdErr := cmd.Output()
	if cmdErr != nil {
		logging.Warn(t.ctx, "FetchingTree.readFileViaGit: cat-file failed",
			slog.String("path", path),
			slog.String("hash", entry.Hash.String()[:12]),
			slog.String("error", cmdErr.Error()),
		)
		return nil, fmt.Errorf("blob %s not readable after fetch: %w", entry.Hash.String()[:12], cmdErr)
	}

	// Create an in-memory encoded object to construct the File.
	memObj := &plumbing.MemoryObject{}
	memObj.SetType(plumbing.BlobObject)
	memObj.SetSize(int64(len(content)))
	w, wErr := memObj.Writer()
	if wErr != nil {
		return nil, fmt.Errorf("memory object writer: %w", wErr)
	}
	if _, wErr = w.Write(content); wErr != nil {
		return nil, fmt.Errorf("memory object write: %w", wErr)
	}
	if wErr = w.Close(); wErr != nil {
		return nil, fmt.Errorf("memory object close: %w", wErr)
	}

	blob := &object.Blob{}
	if dErr := blob.Decode(memObj); dErr != nil {
		return nil, fmt.Errorf("blob decode: %w", dErr)
	}

	logging.Debug(t.ctx, "FetchingTree.readFileViaGit: blob read successfully",
		slog.String("path", path),
		slog.String("hash", entry.Hash.String()[:12]),
		slog.Int64("size", int64(len(content))),
	)

	return object.NewFile(path, entry.Mode, blob), nil
}

// Tree returns the subtree at the given path, wrapped with the same fetching
// behavior.
func (t *FetchingTree) Tree(path string) (*FetchingTree, error) {
	subtree, err := t.inner.Tree(path)
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", path, err)
	}
	return &FetchingTree{
		inner:  subtree,
		ctx:    t.ctx,
		storer: t.storer,
		fetch:  t.fetch,
	}, nil
}

// RawEntries returns the direct tree entries (no blob reads needed).
func (t *FetchingTree) RawEntries() []object.TreeEntry {
	return t.inner.Entries
}

// FileReader provides read access to files within a git tree.
// Both *object.Tree and *FetchingTree implement this interface.
type FileReader interface {
	File(path string) (*object.File, error)
}

// FileOpener provides access to a file's content reader.
// *object.File implements this interface.
type FileOpener interface {
	Reader() (io.ReadCloser, error)
}
