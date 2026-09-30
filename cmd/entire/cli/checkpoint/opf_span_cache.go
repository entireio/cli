package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
)

// OPFSpanCacheDirName holds OPF scan results between the background scan that
// produces them and the rewrite that applies them. Like RedactCacheDirName it
// sits in the git common dir, where nothing is walked into a checkpoint tree,
// and every entry is derived data: deleting the directory only costs a rescan.
const OPFSpanCacheDirName = "entire-opf-cache"

// OPFSpanCacheMaxAge bounds how long an entry is kept after it was written.
// A checkpoint is normally rewritten and pushed within minutes of its scan, so
// an entry this old belongs to content that has shipped or been abandoned.
const OPFSpanCacheMaxAge = 30 * 24 * time.Hour

// opfSpanCache is the git-common-dir implementation of redact.OPFSpanCache.
// Keys are hex SHA-256 digests produced by the redact package, so every entry
// name is safe by construction; I/O still goes through the shared root.
type opfSpanCache struct {
	root *os.Root
}

// OPFSpanCacheForRepo opens the cache in repo's git common dir. It resolves the
// directory from the repository itself, never from the process's working
// directory, so a caller holding one repo can never read or write another's
// scan results.
func OPFSpanCacheForRepo(repo *git.Repository) (redact.OPFSpanCache, error) {
	_, commonDir, err := repositoryDirs(repo)
	if err != nil {
		return nil, err
	}
	return OPFSpanCacheAt(commonDir)
}

// OPFSpanCacheAt opens the cache under gitCommonDir, creating its directory.
func OPFSpanCacheAt(gitCommonDir string) (redact.OPFSpanCache, error) {
	root, err := gitdir.OpenAt(gitCommonDir)
	if err != nil {
		return nil, fmt.Errorf("open git common dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, OPFSpanCacheDirName, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", OPFSpanCacheDirName, err)
	}
	return &opfSpanCache{root: root}, nil
}

// opfSpanRecord is the stored form of one redact.Span. It is a separate,
// tagged type so the on-disk format does not follow redact's field names.
type opfSpanRecord struct {
	Start int    `json:"s"`
	End   int    `json:"e"`
	Label string `json:"l"`
}

func opfSpanEntryName(key string) (string, error) {
	if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
		return "", fmt.Errorf("invalid OPF span cache key %q", key)
	}
	return OPFSpanCacheDirName + "/" + key + ".json", nil
}

func (c *opfSpanCache) LoadOPFSpans(key string) (map[string][]redact.Span, bool) {
	name, err := opfSpanEntryName(key)
	if err != nil {
		return nil, false
	}
	data, err := osroot.ReadFileNoFollow(c.root, name)
	if err != nil {
		return nil, false
	}
	var records map[string][]opfSpanRecord
	if err := json.Unmarshal(data, &records); err != nil || records == nil {
		return nil, false
	}
	spans := make(map[string][]redact.Span, len(records))
	for leaf, recs := range records {
		out := make([]redact.Span, 0, len(recs))
		for _, r := range recs {
			out = append(out, redact.Span{Start: r.Start, End: r.End, Label: r.Label})
		}
		spans[leaf] = out
	}
	return spans, true
}

func (c *opfSpanCache) StoreOPFSpans(key string, spans map[string][]redact.Span) error {
	name, err := opfSpanEntryName(key)
	if err != nil {
		return err
	}
	records := make(map[string][]opfSpanRecord, len(spans))
	for leaf, ss := range spans {
		recs := make([]opfSpanRecord, 0, len(ss))
		for _, sp := range ss {
			recs = append(recs, opfSpanRecord{Start: sp.Start, End: sp.End, Label: sp.Label})
		}
		records[leaf] = recs
	}
	data, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("encode OPF span cache entry: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(c.root, name, data, 0o600); err != nil {
		return fmt.Errorf("write OPF span cache entry: %w", err)
	}
	return nil
}

// PruneOPFSpanCache removes entries written before now-maxAge and returns how
// many it removed. Best-effort: entries it cannot inspect are left in place.
func PruneOPFSpanCache(gitCommonDir string, now time.Time, maxAge time.Duration) (int, error) {
	root, err := gitdir.OpenAt(gitCommonDir)
	if err != nil {
		return 0, fmt.Errorf("open git common dir: %w", err)
	}
	entries, err := osroot.ReadDirNoSymlinks(root, OPFSpanCacheDirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list %s: %w", OPFSpanCacheDirName, err)
	}
	removed := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, infoErr := e.Info()
		if infoErr != nil || now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if osroot.RemoveNoSymlinks(root, OPFSpanCacheDirName+"/"+e.Name()) == nil {
			removed++
		}
	}
	return removed, nil
}
