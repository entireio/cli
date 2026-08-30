package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// Transcripts are append-only JSONL, but every checkpoint used to re-redact them
// from byte zero: a 70MB Codex rollout cost ~67s per Stop hook, and a session
// with N checkpoints re-redacted O(N^2) bytes overall. Redaction is ~99.7% of
// that write path (git object writing is milliseconds), and it is by far the
// dominant cost of a Stop hook on a large session.
//
// So keep the redacted output for the prefix already processed and redact only
// what was appended. Correctness rests on two properties:
//
//   - Redaction is per-line and stateless (see redact.redactJSONLLines), so
//     redacting a prefix and a suffix separately and concatenating them yields
//     exactly what redacting the whole file would. redact's own tests pin this
//     as redact(A+B) == redact(A)+redact(B) for newline-terminated A.
//   - A cached prefix always ends immediately after a "\n". Because of that the
//     redacted prefix also ends with "\n", and plain byte concatenation
//     reproduces the full result with no boundary fixups.
//
// The prefix is only reused when the source bytes it covered still hash the
// same and the redaction rules have not changed. Anything else -- a rewritten
// transcript, a compaction, changed custom rules, a CLI upgrade -- falls back to
// redacting everything.
//
// Scope: all three whole-transcript paths reuse prefixes -- the shadow-branch
// metadata write (via createRedactedBlobFromFile, which walks files), and
// post-commit condensation and the Stop finalize rewrite (via
// RedactTranscriptCached, which hold the transcript in memory).
// Single-JSON-value transcripts (OpenCode export) get only the sharding in
// redact.JSONLContent, since they have no line structure to split on.
//
// Those paths do NOT all redact the same bytes: the metadata walk stores a
// sanitized transcript, while condensation and finalize store a sanitized *and*
// image-externalized one. They stay separate because their cache keys are
// different strings -- the walk uses its real tree path, the in-memory callers a
// synthetic one (see transcriptCacheKey) -- and a key is hashed into its own
// file. Sharing a key would be safe, since the prefix hash check rejects a
// mismatch, but it would miss on every checkpoint.

const (
	// RedactCacheDirName sits in the git common dir, NOT under .entire/, because
	// anything inside the worktree metadata directory would be walked into the
	// checkpoint tree and committed.
	//
	// Exported so `entire clean` can reclaim it: every entry is derived data that
	// is rebuilt on the next checkpoint, so the whole directory is safe to delete
	// at any time.
	RedactCacheDirName = "entire-redact-cache"
)

// redactCacheMinBytes is the file size below which incremental reuse is not
// worth its bookkeeping; a small file redacts in milliseconds.
//
// A var rather than a const so tests can lower it: every test of this cache
// otherwise has to build and redact a megabyte of realistic content, and
// redaction is the expensive part -- the suite cost minutes under -race and
// timed out CI. Production never assigns to it.
var redactCacheMinBytes = 1 << 20 // 1MiB

// redactPrefixEntry is the persisted record of one file's already-redacted
// prefix. Written atomically; a missing, unreadable, or stale entry simply means
// a full redaction.
type redactPrefixEntry struct {
	// Fingerprint identifies the redaction rules that produced RedactedBlob.
	Fingerprint string `json:"fingerprint"`
	// SourceBytes is the length of the source prefix covered, always ending
	// immediately after a newline.
	SourceBytes int `json:"source_bytes"`
	// SourceHash is the SHA-256 of source[:SourceBytes], proving the prefix has
	// not been rewritten underneath us.
	SourceHash string `json:"source_hash"`
	// RedactedBlob is the git blob holding the redacted prefix. Set by the
	// metadata walk, which had to write that blob for the checkpoint tree anyway.
	RedactedBlob string `json:"redacted_blob,omitempty"`
	// RedactedFile names a file beside this entry holding the redacted prefix.
	// Used by callers that hold a transcript in memory, for whom a git blob is
	// pure overhead: go-git deflates the whole payload before discovering the
	// object already exists (dotgit dedups the rename, not the compression), so a
	// 65MB transcript cost ~1-2s of zlib per checkpoint. Worse, the store chunks
	// transcripts at agent.MaxChunkSize (50MB), so above that the whole-transcript
	// blob matches no chunk, is never deduped, and leaves an unreachable object
	// per checkpoint that `git gc` later prunes -- silently reverting the cache to
	// full redaction. A plain file has none of that, and `entire clean` already
	// reclaims this directory.
	RedactedFile string `json:"redacted_file,omitempty"`
}

// redactCache reads and writes redactPrefixEntry records under a directory in
// the git common dir. A nil *redactCache disables incremental reuse, which is
// what every caller that cannot resolve the git dir gets.
type redactCache struct {
	// dir is the absolute cache directory, kept for messages. All I/O is a name
	// inside root, the shared *os.Root over the git common dir: entry names are
	// hashes of tree paths, which are safe by construction, but going through
	// the root means that stays true without anyone re-deriving the argument.
	dir  string
	root *os.Root
	name string
}

// newRedactCache returns a cache rooted at gitCommonDir, or nil when the
// directory is empty or cannot be created. Callers treat nil as "no caching".
func newRedactCache(gitCommonDir string) *redactCache {
	if gitCommonDir == "" {
		return nil
	}
	root, err := gitdir.OpenAt(gitCommonDir)
	if err != nil {
		return nil
	}
	if err := osroot.MkdirAllNoSymlink(root, RedactCacheDirName, 0o700); err != nil {
		return nil
	}
	return &redactCache{
		dir:  filepath.Join(gitCommonDir, RedactCacheDirName),
		root: root,
		name: RedactCacheDirName,
	}
}

// repoRedactCache resolves the prefix cache for repo, or nil when the git common
// directory is unavailable (a bare repository, for instance). Nil disables
// incremental reuse without failing the write.
//
// resolveGitCommonDir memoizes per worktree and the sibling shadow-branch and
// push-queue writes already warm it, so this is cheap to call per checkpoint.
func repoRedactCache(ctx context.Context, repo *git.Repository) *redactCache {
	dir, err := resolveGitCommonDir(ctx, repo)
	if err != nil {
		return nil
	}
	return newRedactCache(dir)
}

// redactionFingerprint combines the redaction config with the CLI build. The
// build is a deliberately conservative proxy for the vendored betterleaks
// ruleset and the pipeline itself, neither of which is introspectable: an
// upgrade invalidates every cached prefix and costs one full redaction.
func redactionFingerprint() string {
	return versioninfo.Version + ":" + versioninfo.Commit + ":" + redact.ConfigFingerprint()
}

// entryName renders a tree path's cache entry as a name relative to c.root.
func (c *redactCache) entryName(treePath string) string {
	sum := sha256.Sum256([]byte(treePath))
	return c.name + "/" + hex.EncodeToString(sum[:]) + ".json"
}

func (c *redactCache) load(treePath string) *redactPrefixEntry {
	data, err := osroot.ReadFileNoFollow(c.root, c.entryName(treePath))
	if err != nil {
		return nil
	}
	var entry redactPrefixEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	if entry.SourceBytes <= 0 || entry.SourceHash == "" {
		return nil
	}
	if entry.RedactedBlob == "" && entry.RedactedFile == "" {
		return nil
	}
	return &entry
}

// storePrefix records the prefix just written so the next checkpoint can reuse
// it. sourceHash must be the digest of the whole of the source content, and blob
// the object holding its redacted form.
//
// Failures are silent: losing a cache entry only costs a full redaction next
// time.
func (c *redactCache) storePrefix(ctx context.Context, treePath, sourceHash string, sourceBytes int, blob plumbing.Hash) {
	c.writeEntry(ctx, treePath, redactPrefixEntry{
		Fingerprint:  redactionFingerprint(),
		SourceBytes:  sourceBytes,
		SourceHash:   sourceHash,
		RedactedBlob: blob.String(),
	})
}

// storePrefixBytes is storePrefix for callers with no blob to point at: it writes
// the redacted prefix as a file beside the entry. See redactPrefixEntry.
func (c *redactCache) storePrefixBytes(ctx context.Context, treePath, sourceHash string, sourceBytes int, redacted []byte) {
	if c == nil {
		return
	}
	name := prefixFileName(treePath)
	if err := jsonutil.WriteFileAtomicIn(c.root, c.name+"/"+name, redacted, 0o600); err != nil {
		logging.Debug(logging.WithComponent(ctx, "redaction"),
			"failed to store redaction prefix bytes", slog.String("error", err.Error()))
		return
	}
	c.writeEntry(ctx, treePath, redactPrefixEntry{
		Fingerprint:  redactionFingerprint(),
		SourceBytes:  sourceBytes,
		SourceHash:   sourceHash,
		RedactedFile: name,
	})
}

// writeEntry persists the record itself. Written after any prefix payload, so a
// crash between the two leaves an entry-less payload (ignored, reclaimed by
// `entire clean`) rather than an entry pointing at nothing.
func (c *redactCache) writeEntry(ctx context.Context, treePath string, entry redactPrefixEntry) {
	if c == nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := jsonutil.WriteFileAtomicIn(c.root, c.entryName(treePath), data, 0o600); err != nil {
		logging.Debug(logging.WithComponent(ctx, "redaction"),
			"failed to store redaction prefix cache", slog.String("error", err.Error()))
	}
}

// prefixFileName is the payload file for treePath, alongside its .json entry.
func prefixFileName(treePath string) string {
	sum := sha256.Sum256([]byte(treePath))
	return hex.EncodeToString(sum[:]) + ".prefix"
}

// readPrefix loads the redacted prefix an entry points at, from wherever it
// lives. sizeHint pre-sizes the buffer so the join can append in place instead of
// copying the whole prefix a second time.
//
// Each branch checks the field it is about to use rather than trusting load() to
// have rejected a record with neither set. load() does reject that, so this is
// not a reachable path today -- but the guarantee would otherwise live 80 lines
// away from the code depending on it, and plumbing.NewHash("") does not fail
// loudly: it yields the zero hash and surfaces as a puzzling missing-object
// error instead of a bad cache entry.
func (c *redactCache) readPrefix(repo *git.Repository, entry *redactPrefixEntry, sizeHint int) ([]byte, error) {
	if entry.RedactedFile != "" {
		return readFileBytes(c.root, c.name+"/"+entry.RedactedFile, sizeHint)
	}
	if entry.RedactedBlob == "" {
		return nil, errors.New("cache entry names neither a prefix file nor a blob")
	}
	return readBlobBytes(repo, plumbing.NewHash(entry.RedactedBlob), sizeHint)
}

// redactResult is the outcome of one incremental redaction attempt.
type redactResult struct {
	// Redacted is the content to store. Nil means the caller must redact the
	// whole content itself.
	Redacted []byte
	// SourceHash is the digest of the whole source, computed as a side effect of
	// prefix validation so the caller never hashes the content a second time.
	SourceHash string
	// StorePrefix reports whether the caller should record this result for the
	// next checkpoint to reuse.
	StorePrefix bool
}

// incrementalRedactionCandidate reports whether a stored file is worth prefix
// caching: a large, append-only, line-delimited session transcript.
//
// Both checks below are load-bearing for different reasons, so neither is
// redundant:
//
//   - The filename establishes append-only. Only full.jsonl is appended to;
//     transcript.jsonl (the compact transcript) is regenerated in full each
//     checkpoint and agent.ChunkFileName yields "full.jsonl.001" for oversized
//     transcripts, so neither should qualify.
//   - redact.IsLineDelimited establishes that splicing is sound. The filename
//     alone is not a safe proxy: OpenCode writes a single JSON object
//     ({"info":...,"messages":[...]}) to this very path, and redacting a fragment
//     of a single JSON value drops out of the field-aware pass into raw entropy
//     detection over partial JSON.
func incrementalRedactionCandidate(content []byte, treePath string) bool {
	return len(content) >= redactCacheMinBytes &&
		filepath.Base(filepath.ToSlash(treePath)) == paths.TranscriptFileName &&
		redact.IsLineDelimited(content)
}

// transcriptRedactor redacts a whole byte range. The same function redacts the
// appended suffix and, on a cache miss, the whole content -- passing one redactor
// for both is what makes it impossible to splice a prefix produced by one
// pipeline onto a suffix produced by another.
type transcriptRedactor func(ctx context.Context, content []byte) ([]byte, error)

// redactIncrementally redacts content, reusing a previously redacted prefix when
// one is available and still valid.
func redactIncrementally(
	ctx context.Context,
	repo *git.Repository,
	cache *redactCache,
	content []byte,
	treePath string,
	redactor transcriptRedactor,
) (redactResult, error) {
	res, err := reusePrefix(ctx, repo, cache, content, treePath, redactor)
	if err != nil || res.Redacted != nil {
		return res, err
	}
	// Nothing reusable: redact the whole content with the same redactor that
	// would have handled the suffix. Doing this here rather than at each call
	// site is what makes "one pipeline for prefix and suffix" structural instead
	// of a convention two callers happen to follow.
	res.Redacted, err = redactor(ctx, content)
	if err != nil {
		return redactResult{}, err
	}
	return res, nil
}

// reusePrefix attempts the incremental path, returning a nil Redacted when the
// content is not eligible or no valid prefix is available.
func reusePrefix(
	ctx context.Context,
	repo *git.Repository,
	cache *redactCache,
	content []byte,
	treePath string,
	redactor transcriptRedactor,
) (redactResult, error) {
	if cache == nil || !incrementalRedactionCandidate(content, treePath) {
		return redactResult{}, nil
	}

	// Only a file ending on a line boundary can be cached, because the reuse
	// contract requires the stored prefix to end just after a "\n".
	if content[len(content)-1] != '\n' {
		return redactResult{}, nil
	}

	logCtx := logging.WithComponent(ctx, "redaction")

	entry := cache.load(treePath)
	if entry == nil || entry.Fingerprint != redactionFingerprint() || entry.SourceBytes > len(content) {
		return redactResult{SourceHash: hashBytes(content), StorePrefix: true}, nil
	}

	// Hash the prefix and the remainder in one pass: Sum snapshots the digest
	// without resetting it, so validating the prefix also yields the whole-content
	// hash the caller stores, instead of a second pass over ~70MB.
	digest := sha256.New()
	digest.Write(content[:entry.SourceBytes])
	prefixHash := hex.EncodeToString(digest.Sum(nil))
	digest.Write(content[entry.SourceBytes:])
	fullHash := hex.EncodeToString(digest.Sum(nil))

	// The prefix must still be byte-identical; a rewritten or compacted
	// transcript invalidates it.
	if prefixHash != entry.SourceHash {
		logging.Debug(logCtx, "redaction prefix changed, redacting in full",
			slog.String("path", treePath), slog.Int("prefix_bytes", entry.SourceBytes))
		return redactResult{SourceHash: fullHash, StorePrefix: true}, nil
	}

	suffix := content[entry.SourceBytes:]
	// Redact the suffix before reading the prefix so the prefix can be read into
	// a buffer sized for both and joined in place, instead of copying the whole
	// (up to tens of MB) prefix a second time.
	//
	// A degraded scanner must not be spliced onto the reused prefix: fail the
	// write instead of persisting an under-scanned suffix.
	var redactedSuffix []byte
	if len(suffix) > 0 {
		var err error
		if redactedSuffix, err = redactor(ctx, suffix); err != nil {
			return redactResult{}, err
		}
	}

	prefix, readErr := cache.readPrefix(repo, entry, len(redactedSuffix))
	if readErr != nil {
		logging.Debug(logCtx, "cached redacted prefix unreadable, redacting in full",
			slog.String("path", treePath), slog.String("error", readErr.Error()))
		return redactResult{SourceHash: fullHash, StorePrefix: true}, nil
	}
	// A prefix that does not end on a newline would corrupt the join, so refuse
	// it rather than emit spliced output.
	if len(prefix) > 0 && prefix[len(prefix)-1] != '\n' {
		logging.Debug(logCtx, "cached redacted prefix does not end at a line boundary, redacting in full",
			slog.String("path", treePath))
		return redactResult{SourceHash: fullHash, StorePrefix: true}, nil
	}

	if len(suffix) == 0 {
		// Nothing appended since the last checkpoint: the stored entry already
		// describes exactly this content, so there is nothing to re-record.
		return redactResult{Redacted: prefix}, nil
	}

	logging.Debug(logCtx, "redacted transcript incrementally",
		slog.String("path", treePath),
		slog.Int("reused_bytes", entry.SourceBytes),
		slog.Int("redacted_bytes", len(suffix)))

	return redactResult{Redacted: append(prefix, redactedSuffix...), SourceHash: fullHash, StorePrefix: true}, nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readBlobBytes loads a blob's full contents into a buffer with room for
// sizeHint extra bytes, so the caller can append the redacted suffix without
// copying the whole prefix again.
func readBlobBytes(repo *git.Repository, hash plumbing.Hash, sizeHint int) ([]byte, error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob %s: %w", hash, err)
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to open blob %s: %w", hash, err)
	}
	defer func() { _ = reader.Close() }()

	// blob.Size is known, so read into an exact buffer: io.ReadAll grows by
	// doubling and allocates roughly 2.3x the payload for a large transcript.
	out := make([]byte, blob.Size, int64(sizeHint)+blob.Size)
	if _, err := io.ReadFull(reader, out); err != nil {
		return nil, fmt.Errorf("failed to read blob %s: %w", hash, err)
	}
	return out, nil
}

// readFileBytes is readBlobBytes for a file-backed prefix, named inside root.
func readFileBytes(root *os.Root, path string, sizeHint int) ([]byte, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open prefix %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat prefix %s: %w", path, err)
	}
	out := make([]byte, info.Size(), int64(sizeHint)+info.Size())
	if _, err := io.ReadFull(f, out); err != nil {
		return nil, fmt.Errorf("failed to read prefix %s: %w", path, err)
	}
	return out, nil
}

// transcriptCacheKey is the cache key for one session's in-memory transcript.
// It ends in paths.TranscriptFileName because incrementalRedactionCandidate gates
// on that basename, and carries the session ID so concurrent sessions in one
// worktree never share an entry.
//
// It cannot collide with the metadata walk's entries even though both describe
// "a session transcript": the walk keys on its real tree path
// (.entire/metadata/<session>/full.jsonl), a different string and so a different
// cache file. That difference matters because the two do not redact the same
// bytes -- the walk stores a sanitized transcript, these callers a sanitized
// *and* image-externalized one.
func transcriptCacheKey(sessionID string) string {
	return "committed/" + sessionID + "/" + paths.TranscriptFileName
}

// RedactTranscriptCached redacts a whole session transcript, reusing the prefix
// redacted for the previous checkpoint when it is still valid and recording this
// result for the next one. Output is byte-identical to redacting content in one
// pass; see the file header for why splicing is sound.
//
// Named for the guarantee rather than the mechanism: reuse is best-effort, so a
// caller gets a correct redaction either way and should not read the name as an
// O(appended) promise. An unresolvable git dir, an unreadable prefix, a rewritten
// prefix, or a CLI upgrade all fall back to redacting everything; only a redactor
// error propagates.
//
// This is the entry point for callers holding a transcript in memory rather than
// walking a file (condensation and the Stop finalize rewrite). Those re-redacted
// the full transcript on every checkpoint, which is what made a Stop hook on a
// 65MB Codex rollout exceed Codex's 30s hook timeout outright rather than merely
// run slow.
//
// A nil repo or empty sessionID declines caching and redacts the whole content,
// which is what the per-subagent caller wants: a task transcript is written once
// per task, not appended across checkpoints.
func RedactTranscriptCached(
	ctx context.Context,
	repo *git.Repository,
	sessionID string,
	content []byte,
	redactor func(ctx context.Context, content []byte) (redact.RedactedBytes, error),
) (redact.RedactedBytes, error) {
	if repo == nil || sessionID == "" {
		return redactor(ctx, content)
	}

	treePath := transcriptCacheKey(sessionID)

	// Resolve the cache only for content that could actually use it: a small or
	// non-line-delimited payload would otherwise pay the git-common-dir lookup
	// (a subprocess on first call in the process) for nothing.
	if !incrementalRedactionCandidate(content, treePath) {
		return redactor(ctx, content)
	}

	cache := repoRedactCache(ctx, repo)
	result, err := redactIncrementally(ctx, repo, cache, content, treePath,
		func(ctx context.Context, b []byte) ([]byte, error) {
			out, redErr := redactor(ctx, b)
			if redErr != nil {
				return nil, redErr
			}
			return out.Bytes(), nil
		})
	if err != nil {
		return redact.RedactedBytes{}, err
	}

	if result.StorePrefix {
		// Stored as bytes rather than a git blob: see redactPrefixEntry.
		cache.storePrefixBytes(ctx, treePath, result.SourceHash, len(content), result.Redacted)
	}

	return redact.AlreadyRedacted(result.Redacted), nil
}
