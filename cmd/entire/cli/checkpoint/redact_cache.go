package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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
// Scope: this covers the shadow-branch metadata write only. Condensation
// (strategy/manual_commit_condensation.go) and the Stop finalize rewrite
// (strategy/manual_commit_hooks.go) still redact whole transcripts; they inherit
// the sharding in redact.JSONLContent but not the prefix reuse. Single-JSON-value
// transcripts (OpenCode export) get neither, since they have no line structure to
// split on.

const (
	// RedactCacheDirName sits in the git common dir, NOT under .entire/, because
	// anything inside the worktree metadata directory would be walked into the
	// checkpoint tree and committed.
	//
	// Exported so `entire clean` can reclaim it: every entry is derived data that
	// is rebuilt on the next checkpoint, so the whole directory is safe to delete
	// at any time.
	RedactCacheDirName = "entire-redact-cache"

	// redactCacheMinBytes is the file size below which incremental reuse is not
	// worth its bookkeeping; a small file redacts in milliseconds.
	redactCacheMinBytes = 1 << 20 // 1MiB
)

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
	// RedactedBlob is the git blob holding the redacted prefix.
	RedactedBlob string `json:"redacted_blob"`
}

// redactCache reads and writes redactPrefixEntry records under a directory in
// the git common dir. A nil *redactCache disables incremental reuse, which is
// what every caller that cannot resolve the git dir gets.
type redactCache struct {
	dir string
}

// newRedactCache returns a cache rooted at gitCommonDir, or nil when the
// directory is empty or cannot be created. Callers treat nil as "no caching".
func newRedactCache(gitCommonDir string) *redactCache {
	if gitCommonDir == "" {
		return nil
	}
	dir := filepath.Join(gitCommonDir, RedactCacheDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	return &redactCache{dir: dir}
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

func (c *redactCache) path(treePath string) string {
	sum := sha256.Sum256([]byte(treePath))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".json")
}

func (c *redactCache) load(treePath string) *redactPrefixEntry {
	data, err := os.ReadFile(c.path(treePath))
	if err != nil {
		return nil
	}
	var entry redactPrefixEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	if entry.SourceBytes <= 0 || entry.SourceHash == "" || entry.RedactedBlob == "" {
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
	if c == nil {
		return
	}
	data, err := json.Marshal(redactPrefixEntry{
		Fingerprint:  redactionFingerprint(),
		SourceBytes:  sourceBytes,
		SourceHash:   sourceHash,
		RedactedBlob: blob.String(),
	})
	if err != nil {
		return
	}
	if err := jsonutil.WriteFileAtomic(c.path(treePath), data, 0o600); err != nil {
		logging.Debug(logging.WithComponent(ctx, "redaction"),
			"failed to store redaction prefix cache", slog.String("error", err.Error()))
	}
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

// redactIncrementally redacts content, reusing a previously redacted prefix when
// one is available and still valid.
func redactIncrementally(
	ctx context.Context,
	repo *git.Repository,
	cache *redactCache,
	content []byte,
	treePath string,
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

	prefix, readErr := readBlobBytes(repo, plumbing.NewHash(entry.RedactedBlob))
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

	if entry.SourceBytes == len(content) {
		// Nothing appended since the last checkpoint: the stored entry already
		// describes exactly this content, so there is nothing to re-record.
		return redactResult{Redacted: prefix}, nil
	}

	suffix := content[entry.SourceBytes:]
	// A degraded scanner must not be spliced onto the reused prefix: fail the
	// write instead of persisting an under-scanned suffix.
	redactedSuffix, err := RedactBlobBytes(ctx, suffix, treePath, false)
	if err != nil {
		return redactResult{}, err
	}
	out := make([]byte, 0, len(prefix)+len(redactedSuffix))
	out = append(out, prefix...)
	out = append(out, redactedSuffix...)

	logging.Debug(logCtx, "redacted transcript incrementally",
		slog.String("path", treePath),
		slog.Int("reused_bytes", entry.SourceBytes),
		slog.Int("redacted_bytes", len(suffix)))

	return redactResult{Redacted: out, SourceHash: fullHash, StorePrefix: true}, nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readBlobBytes loads a blob's full contents.
func readBlobBytes(repo *git.Repository, hash plumbing.Hash) ([]byte, error) {
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
	out := make([]byte, blob.Size)
	if _, err := io.ReadFull(reader, out); err != nil {
		return nil, fmt.Errorf("failed to read blob %s: %w", hash, err)
	}
	return out, nil
}
