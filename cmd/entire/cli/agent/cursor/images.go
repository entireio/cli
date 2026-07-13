package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Compile-time interface assertion.
var _ agent.SidecarImageProvider = (*CursorAgent)(nil)

// cursorChatsDirEnv overrides the base directory that holds Cursor's per-session
// SQLite blob stores. Used by tests and mock environments.
const cursorChatsDirEnv = "ENTIRE_TEST_CURSOR_CHATS_DIR"

const (
	// maxStoreDBBytes bounds the work: a store.db larger than this is skipped
	// (best-effort no-op). sqlite3's hex() output is ~2x the blob size and is
	// buffered in memory, so this caps peak memory. A normal Cursor store holding
	// screenshots is well under this.
	maxStoreDBBytes = 64 << 20 // 64MB

	// sqlite3Timeout bounds the sidecar read so a locked, huge, or malformed
	// store.db can never hang the git commit / stop hook it runs inside.
	sqlite3Timeout = 30 * time.Second
)

// storeDBBlobQuery selects the hex encoding of every blob whose leading bytes
// match a known image magic number (JPEG, PNG, GIF, or RIFF/WEBP). sqlite3's
// hex() returns uppercase, so the literals are uppercase.
const storeDBBlobQuery = "SELECT hex(data) FROM blobs WHERE " +
	"substr(hex(data),1,6)='FFD8FF' OR " + // JPEG
	"substr(hex(data),1,8)='89504E47' OR " + // PNG
	"substr(hex(data),1,8)='47494638' OR " + // GIF
	"(substr(hex(data),1,8)='52494646' AND substr(hex(data),17,8)='57454250');" // RIFF....WEBP

// SidecarImages captures images that Cursor stores outside the JSONL transcript.
// Cursor keeps pasted/generated images in a per-session SQLite blob store
// (~/.cursor/chats/<workspace>/<session>/store.db), not the transcript Entire
// condenses, so they would otherwise be lost from the checkpoint. This locates
// that store for the session, shells out to the sqlite3 binary to read the image
// blobs, and returns them as checkpoint assets.
//
// It is best-effort: when the store, the sqlite3 binary, or the expected schema
// is absent, or the store is too large, it returns no images and no error.
// sessionRef is the transcript path.
func (c *CursorAgent) SidecarImages(ctx context.Context, sessionRef string) ([]agent.CompactedTranscriptAsset, error) {
	logCtx := logging.WithComponent(ctx, "agent.cursor")

	sessionID := sessionIDFromTranscriptPath(sessionRef)
	if sessionID == "" {
		return nil, nil
	}

	dbPaths, err := findStoreDBs(sessionID)
	if err != nil {
		return nil, fmt.Errorf("locate cursor store.db: %w", err)
	}
	if len(dbPaths) == 0 {
		return nil, nil // no sidecar store for this session
	}

	if !sqlite3Available() {
		logging.Debug(logCtx, "sqlite3 not found; skipping cursor sidecar image capture")
		return nil, nil
	}

	assets := make([]agent.CompactedTranscriptAsset, 0)
	seen := make(map[string]struct{})
	for _, dbPath := range dbPaths {
		hexBlobs, err := readImageBlobs(logCtx, dbPath)
		if err != nil {
			return nil, fmt.Errorf("read cursor store.db blobs: %w", err)
		}
		for _, h := range hexBlobs {
			data, err := hex.DecodeString(h)
			if err != nil {
				logging.Debug(logCtx, "skipping undecodable cursor blob", slog.String("error", err.Error()))
				continue
			}
			if len(data) > agent.MaxChunkSize {
				// A blob this large would become an unpushable git object; drop it.
				logging.Debug(logCtx, "skipping oversized cursor image", slog.Int("bytes", len(data)))
				continue
			}
			mediaType, ext := detectImageType(data)
			if mediaType == "" {
				continue // not an image after all
			}
			sum := sha256.Sum256(data)
			name := fmt.Sprintf("img-%s.%s", hex.EncodeToString(sum[:16]), ext)
			if _, dup := seen[name]; dup {
				continue // identical image already captured
			}
			seen[name] = struct{}{}
			assets = append(assets, agent.CompactedTranscriptAsset{
				Name:      name,
				MediaType: mediaType,
				Data:      data,
			})
		}
	}

	if len(assets) > 0 {
		logging.Debug(logCtx, "captured cursor sidecar images",
			slog.Int("count", len(assets)), slog.String("session", sessionID))
	}
	return assets, nil
}

// sessionIDFromTranscriptPath extracts the Cursor session id from a transcript
// path. Both the nested (<id>/<id>.jsonl) and flat (<id>.jsonl) layouts name the
// file after the session id, so the base name without extension is the id.
// Returns "" for a path whose base resolves to "." or ".." (never a real id).
func sessionIDFromTranscriptPath(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	base := filepath.Base(transcriptPath)
	id := strings.TrimSuffix(base, filepath.Ext(base))
	if id == "." || id == ".." {
		return ""
	}
	return id
}

// findStoreDBs locates every SQLite blob store for a session. Cursor lays these
// out as <chats>/<workspace-hash>/<session-id>/store.db; the workspace hash is
// not derivable from the session id, so we enumerate workspaces and check each.
//
// Only the workspace level is globbed; the session id is joined as a LITERAL
// path component (checked with os.Stat), so glob metacharacters in the id can't
// widen the match to a different session's store. Returns all matches (a session
// id is a UUID, so normally exactly one) — callers union + dedup the images,
// which avoids silently dropping images when a session resolves under more than
// one workspace directory.
func findStoreDBs(sessionID string) ([]string, error) {
	base := os.Getenv(cursorChatsDirEnv)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home directory: %w", err)
		}
		base = filepath.Join(home, ".cursor", "chats")
	}

	workspaces, err := filepath.Glob(filepath.Join(base, "*"))
	if err != nil {
		return nil, fmt.Errorf("glob cursor workspaces: %w", err)
	}
	var dbs []string
	for _, ws := range workspaces {
		p := filepath.Join(ws, sessionID, "store.db")
		if fileExists(p) {
			dbs = append(dbs, p)
		}
	}
	return dbs, nil
}

// readImageBlobs copies the store to a temp location (so a live Cursor session
// cannot lock or mutate it mid-read, and any WAL is applied) and shells out to
// sqlite3 to select image blobs as hex. Returns one hex string per image blob.
//
// Best-effort: a store larger than maxStoreDBBytes, or one whose schema is not
// the expected blobs(data) shape, returns (nil, nil) — an expected miss, not an
// error, so it never spams a warning on every checkpoint.
func readImageBlobs(ctx context.Context, dbPath string) ([]string, error) {
	if info, err := os.Stat(dbPath); err == nil && info.Size() > maxStoreDBBytes {
		logging.Debug(ctx, "cursor store.db too large; skipping sidecar capture",
			slog.Int64("bytes", info.Size()))
		return nil, nil
	}

	tmpDir, err := os.MkdirTemp("", "entire-cursor-store-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpDB := filepath.Join(tmpDir, "store.db")
	if err := copyFile(dbPath, tmpDB); err != nil {
		return nil, fmt.Errorf("copy store.db: %w", err)
	}
	// Copy the WAL/SHM sidecars if present so committed-but-not-checkpointed
	// pages are applied when sqlite3 opens the copy. Best-effort: a missing or
	// uncopyable sidecar just means we read the main db as-is.
	for _, suffix := range []string{"-wal", "-shm"} {
		src := dbPath + suffix
		if !fileExists(src) {
			continue
		}
		if err := copyFile(src, tmpDB+suffix); err != nil {
			logging.Debug(ctx, "skipping cursor store.db sidecar copy",
				slog.String("file", src), slog.String("error", err.Error()))
		}
	}

	cctx, cancel := context.WithTimeout(ctx, sqlite3Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sqlite3", tmpDB, storeDBBlobQuery)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if isSchemaMismatch(stderr) {
				logging.Debug(ctx, "cursor store.db schema not recognized; skipping",
					slog.String("detail", stderr))
				return nil, nil
			}
			return nil, fmt.Errorf("sqlite3 query failed: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("sqlite3 query failed: %w", err)
	}

	var blobs []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			blobs = append(blobs, line)
		}
	}
	return blobs, nil
}

// isSchemaMismatch reports whether a sqlite3 error is a benign schema-shape
// mismatch (a store version whose blob table/columns differ from what the query
// assumes) rather than a genuine failure. Such stores are treated as an expected
// no-op, not an error.
func isSchemaMismatch(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such table") || strings.Contains(s, "no such column")
}

// detectImageType returns the media type and file extension for known image
// magic bytes, or ("", "") when the bytes are not a recognized image.
func detectImageType(data []byte) (mediaType, ext string) {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", "png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg", "jpg"
	case len(data) >= 6 && string(data[:6]) == "GIF89a", len(data) >= 6 && string(data[:6]) == "GIF87a":
		return "image/gif", "gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp", "webp"
	default:
		return "", ""
	}
}

func sqlite3Available() bool {
	_, err := exec.LookPath("sqlite3")
	return err == nil
}

func fileExists(path string) bool {
	// path is built from a workspace glob result plus a filepath.Base-sanitized
	// session id (separators stripped, "."/".." rejected), so no traversal.
	info, err := os.Stat(path) //nolint:gosec // G703 false positive: path is sanitized (see above)
	return err == nil && !info.IsDir()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // path is an internal, non-user-controlled store location
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst) //nolint:gosec // dst is a temp file we created
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy contents: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close destination: %w", err)
	}
	return nil
}
