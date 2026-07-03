package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// importScanCache memoizes the import rung's dry-run scan, which reads and
// parses every discovered transcript file. Invalidation is exact rather than
// TTL-based: the cache key fingerprints the discovered files' identity
// (path, mtime, size) plus the local metadata-branch tip, so an appended
// transcript or a new checkpoint/import (ref moves) recomputes immediately,
// while an unchanged repo pays only the cheap discovery glob on hot paths
// like `entire status`. Persistence via the shared jsonFileCache shell.
type importScanCache struct {
	path string
}

func defaultImportScanCache() importScanCache {
	return importScanCache{path: filepath.Join(userdirs.Cache(), "onboarding_imports.json")}
}

// importScanInput is one discovered transcript file's cache-relevant identity.
type importScanInput struct {
	Path    string
	ModTime time.Time
	Size    int64
}

// importScanFingerprint hashes the scan inputs order-independently.
func importScanFingerprint(files []importScanInput, metadataTip string) string {
	lines := make([]string, 0, len(files)+1)
	for _, f := range files {
		lines = append(lines, fmt.Sprintf("%s|%d|%d", f.Path, f.ModTime.UnixNano(), f.Size))
	}
	sort.Strings(lines)
	lines = append(lines, "tip|"+metadataTip)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// importScanEntry doubles as the on-disk schema: agentImportStatus's json
// tags are part of the cache format.
type importScanEntry struct {
	Fingerprint string              `json:"fingerprint"`
	Statuses    []agentImportStatus `json:"statuses"`
}

func (c importScanCache) shell() jsonFileCache[importScanEntry] {
	return jsonFileCache[importScanEntry](c)
}

func (c importScanCache) get(repoRoot, fingerprint string) ([]agentImportStatus, bool) {
	entry, found := c.shell().load()[repoRoot]
	if !found || entry.Fingerprint != fingerprint {
		return nil, false
	}
	return entry.Statuses, true
}

func (c importScanCache) put(repoRoot, fingerprint string, statuses []agentImportStatus) {
	shell := c.shell()
	entries := shell.load()
	entries[repoRoot] = importScanEntry{Fingerprint: fingerprint, Statuses: statuses}
	shell.store(entries)
}
