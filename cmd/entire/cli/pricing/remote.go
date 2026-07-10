package pricing

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// RemoteCache is the on-disk shape of the cached remote pricing table. It wraps
// the same fileSchema the embedded models/*.json files use — so validateRate and
// the schema_version contract are shared, not reimplemented — with the fetch
// bookkeeping the background refresh needs: FetchedAt drives the 24h refresh
// backoff, ETag drives conditional (If-None-Match) requests, SourceURL records
// where the table came from for diagnostics.
type RemoteCache struct {
	FetchedAt time.Time   `json:"fetched_at"`
	ETag      string      `json:"etag,omitempty"`
	SourceURL string      `json:"source_url,omitempty"`
	Doc       *fileSchema `json:"doc,omitempty"`
}

// remoteCacheFileName is the cache file basename. It lives beside the discovery
// caches under the per-user cache dir (userdirs.Cache()).
const remoteCacheFileName = "pricing_remote.json"

// remoteCachePath returns the absolute path of the remote pricing cache file.
// Path resolution goes through userdirs.Cache — the single implementation every
// cache consumer shares — so the remote pricing cache sits under ~/.cache/entire
// (or $XDG_CACHE_HOME/entire) with nodes.json and the other discovery caches.
func remoteCachePath() string {
	return filepath.Join(userdirs.Cache(), remoteCacheFileName)
}

// loadRemoteCache reads and parses the cache file. A missing file, a corrupt
// file, or any read error all return nil: the remote layer is purely additive,
// so "no usable cache" degrades to "embedded defaults only" and a damaged cache
// self-heals on the next successful refresh (which overwrites it atomically)
// rather than being rewritten here on the read path.
func loadRemoteCache() *RemoteCache {
	data, err := os.ReadFile(remoteCachePath()) // #nosec G304 -- path derived from userdirs, not user input
	if err != nil {
		return nil
	}
	var rc RemoteCache
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil
	}
	return &rc
}

// LoadRemoteEntries returns the valid model rates from the cached remote pricing
// table, or nil when there is nothing usable. It is read-only and performs NO
// network I/O — the daily refresh (RefreshRemoteCache) is what populates the
// cache. It never errors: a missing or corrupt cache, an absent Doc, an
// unsupported schema_version, and individually invalid entries all degrade to
// "fewer (or zero) remote entries", so LoadPricingTable can layer whatever is
// valid on top of the embedded defaults without a failure path.
func LoadRemoteEntries(ctx context.Context) []ModelRate {
	rc := loadRemoteCache()
	if rc == nil || rc.Doc == nil {
		return nil
	}
	if rc.Doc.SchemaVersion != 1 {
		logging.Debug(ctx, "pricing: ignoring remote cache with unsupported schema_version",
			slog.Int("schema_version", rc.Doc.SchemaVersion))
		return nil
	}
	valid := make([]ModelRate, 0, len(rc.Doc.Models))
	for i := range rc.Doc.Models {
		m := rc.Doc.Models[i]
		if err := validateRate(m); err != nil {
			// Drop the single bad entry, keep the rest: one malformed row from
			// the remote table must not sink the whole merge.
			logging.Debug(ctx, "pricing: dropping invalid remote entry",
				slog.String("id", m.ID), slog.String("error", err.Error()))
			continue
		}
		valid = append(valid, m)
	}
	if len(valid) == 0 {
		return nil
	}
	return valid
}

// RemoteFetchedAt returns the time the remote pricing cache was last written, or
// the zero time when there is no cache. It exists for staleness diagnostics
// (the LoadPricingTable debug log and the manual refresh command).
func RemoteFetchedAt() time.Time {
	rc := loadRemoteCache()
	if rc == nil {
		return time.Time{}
	}
	return rc.FetchedAt
}
