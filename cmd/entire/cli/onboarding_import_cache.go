package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
// like `entire status`.
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

type importScanEntry struct {
	Fingerprint string              `json:"fingerprint"`
	Statuses    []agentImportStatus `json:"statuses"`
}

type importScanFile struct {
	Entries map[string]importScanEntry `json:"entries"`
}

func (c importScanCache) load() importScanFile {
	var f importScanFile
	data, err := os.ReadFile(c.path)
	if err != nil || json.Unmarshal(data, &f) != nil || f.Entries == nil {
		return importScanFile{Entries: map[string]importScanEntry{}}
	}
	return f
}

func (c importScanCache) get(repoRoot, fingerprint string) ([]agentImportStatus, bool) {
	entry, found := c.load().Entries[repoRoot]
	if !found || entry.Fingerprint != fingerprint {
		return nil, false
	}
	return entry.Statuses, true
}

func (c importScanCache) put(repoRoot, fingerprint string, statuses []agentImportStatus) {
	f := c.load()
	f.Entries[repoRoot] = importScanEntry{Fingerprint: fingerprint, Statuses: statuses}
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	//nolint:errcheck,gosec // best-effort cache write; a miss next time is fine
	os.WriteFile(c.path, data, 0o600)
}
