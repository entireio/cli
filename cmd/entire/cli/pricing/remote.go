package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
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

// RemoteURL is the default source for the remote pricing table. It is a var so
// tests can point it at an httptest server; the ENTIRE_PRICING_URL environment
// variable overrides it at fetch time for self-hosted setups.
var RemoteURL = "https://entire.io/pricing/v1/models.json"

const (
	// remoteRefreshInterval is the minimum cache age before a refresh is
	// attempted. Every fetch outcome — success, 304, or failure — resets
	// FetchedAt, so a broken endpoint is retried at most once per interval.
	remoteRefreshInterval = 24 * time.Hour
	// remoteMaxBytes caps the response body read (1 MiB), mirroring versioncheck.
	remoteMaxBytes = 1 << 20
)

// remoteFetchTimeout bounds the whole refresh request (dial + read), matching
// the version check's 2s budget so a slow endpoint never stalls a background
// worker. It is a var so tests can shrink it to exercise the timeout path fast.
var remoteFetchTimeout = 2 * time.Second

// Refresh outcomes reported by RefreshResult.Outcome.
const (
	refreshOutcomeUpdated     = "updated"      // 200 with a usable doc: Doc+ETag replaced.
	refreshOutcomeNotModified = "not-modified" // 304: prior Doc kept, FetchedAt bumped.
	refreshOutcomeUnchanged   = "unchanged"    // error/404/garbage: prior Doc kept, FetchedAt bumped.
	refreshOutcomeSkipped     = "skipped"      // throttled: fresh cache, no request made.
)

// RefreshResult summarizes what a refresh attempt did, for the manual
// `entire tokens pricing-refresh` report.
type RefreshResult struct {
	SourceURL string
	Outcome   string
	Entries   int
	FetchedAt time.Time
	ETag      string
}

// Staleness returns how old the cache is relative to now, or 0 when FetchedAt
// is unset.
func (r RefreshResult) Staleness() time.Duration {
	if r.FetchedAt.IsZero() {
		return 0
	}
	return time.Since(r.FetchedAt)
}

// remoteURL returns the effective source URL, honoring the ENTIRE_PRICING_URL
// override read at fetch time.
func remoteURL() string {
	if u := os.Getenv("ENTIRE_PRICING_URL"); u != "" {
		return u
	}
	return RemoteURL
}

// ShouldRefresh reports whether the remote pricing cache is stale (older than
// the 24h interval) or absent. Trigger sites call it to gate a background
// refresh spawn so a fresh cache skips the work — and the spawn — entirely.
func ShouldRefresh() bool {
	rc := loadRemoteCache()
	if rc == nil {
		return true
	}
	return time.Since(rc.FetchedAt) >= remoteRefreshInterval
}

// RefreshRemoteCache fetches the remote pricing table and updates the local
// cache, honoring the 24h throttle. It is the entry point the detached
// __refresh_pricing worker calls. See refreshRemoteCache for the full contract.
func RefreshRemoteCache(ctx context.Context) error {
	_, err := refreshRemoteCache(ctx, false)
	return err
}

// RefreshRemoteCacheForce is RefreshRemoteCache without the freshness throttle:
// it always performs the network fetch and returns a RefreshResult describing
// the outcome. The manual `entire tokens pricing-refresh` command uses it.
func RefreshRemoteCacheForce(ctx context.Context) (RefreshResult, error) {
	return refreshRemoteCache(ctx, true)
}

// refreshRemoteCache is the shared implementation. It takes a cross-process
// flock for herd control, so several concurrently-spawned workers collapse to a
// single network request: the first takes the lock and refreshes, the rest take
// the lock, observe the freshly-written FetchedAt, and skip (unless force).
//
// Failure is absorbed, not surfaced: a network error, timeout, non-200 status,
// oversized/garbage body, or a doc that fails the schema/sanity check all keep
// the previous Doc+ETag and only bump FetchedAt — so a broken endpoint degrades
// to "keep serving the last good table" and is retried at most once per 24h. A
// 304 keeps the Doc and bumps FetchedAt. It returns an error only for a
// genuinely local failure (cache-dir create, lock, or write).
func refreshRemoteCache(ctx context.Context, force bool) (RefreshResult, error) {
	res := RefreshResult{SourceURL: remoteURL(), Outcome: refreshOutcomeSkipped}

	// Cheap pre-lock throttle: skip the lock entirely when the cache is fresh
	// and we are not forcing. The under-lock re-check below is the authoritative
	// herd-control guard; this just avoids lock contention on the common path.
	if !force && !ShouldRefresh() {
		fillResultFromCache(&res, loadRemoteCache())
		return res, nil
	}

	if err := os.MkdirAll(userdirs.Cache(), 0o700); err != nil {
		return res, fmt.Errorf("create cache dir: %w", err)
	}
	path := remoteCachePath()
	release, err := flock.Acquire(path + ".lock")
	if err != nil {
		return res, fmt.Errorf("lock remote pricing cache: %w", err)
	}
	defer release()

	// Re-read under the lock. If another process refreshed while we waited for
	// the lock, its FetchedAt is now fresh and we skip the fetch (unless forced).
	// This is what keeps a burst of spawned workers to a single request.
	prev := loadRemoteCache()
	if !force && prev != nil && time.Since(prev.FetchedAt) < remoteRefreshInterval {
		fillResultFromCache(&res, prev)
		return res, nil
	}
	if prev == nil {
		prev = &RemoteCache{}
	}

	updated, outcome := fetchRemote(ctx, prev)
	if err := writeRemoteCache(path, updated); err != nil {
		return res, err
	}
	res.Outcome = outcome
	fillResultFromCache(&res, updated)
	return res, nil
}

// fillResultFromCache copies the reporting fields out of a cache snapshot.
func fillResultFromCache(res *RefreshResult, rc *RemoteCache) {
	if rc == nil {
		return
	}
	res.FetchedAt = rc.FetchedAt
	res.ETag = rc.ETag
	res.Entries = countValidEntries(rc.Doc)
}

// fetchRemote performs the conditional HTTP request and folds the outcome into a
// new RemoteCache derived from prev. Every outcome bumps FetchedAt. It never
// returns an error — the worst case is "keep prev's Doc+ETag".
func fetchRemote(ctx context.Context, prev *RemoteCache) (*RemoteCache, string) {
	url := remoteURL()
	out := &RemoteCache{
		FetchedAt: time.Now(),
		ETag:      prev.ETag,
		SourceURL: url,
		Doc:       prev.Doc,
	}

	ctx, cancel := context.WithTimeout(ctx, remoteFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, refreshOutcomeUnchanged
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", versioninfo.UserAgent())
	if prev.ETag != "" {
		req.Header.Set("If-None-Match", prev.ETag)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return out, refreshOutcomeUnchanged
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return out, refreshOutcomeNotModified
	}
	if resp.StatusCode != http.StatusOK {
		return out, refreshOutcomeUnchanged
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, remoteMaxBytes))
	if err != nil {
		return out, refreshOutcomeUnchanged
	}
	var doc fileSchema
	if err := json.Unmarshal(body, &doc); err != nil {
		return out, refreshOutcomeUnchanged
	}
	if !remoteDocUsable(&doc) {
		return out, refreshOutcomeUnchanged
	}
	out.Doc = &doc
	out.ETag = resp.Header.Get("ETag")
	return out, refreshOutcomeUpdated
}

// remoteDocUsable is the sanity gate a freshly-fetched doc must pass before it
// replaces the cached table: the supported schema_version and at least one entry
// that survives validateRate. This mirrors LoadRemoteEntries' read-time contract
// so a doc that would contribute zero usable rates is never stored.
func remoteDocUsable(doc *fileSchema) bool {
	return countValidEntries(doc) > 0
}

// countValidEntries counts the entries in doc that pass validateRate under the
// supported schema_version; a nil or wrong-version doc counts zero.
func countValidEntries(doc *fileSchema) int {
	if doc == nil || doc.SchemaVersion != 1 {
		return 0
	}
	n := 0
	for i := range doc.Models {
		if validateRate(doc.Models[i]) == nil {
			n++
		}
	}
	return n
}

// writeRemoteCache writes rc atomically (temp + rename) so a concurrent reader
// never observes a half-written file.
func writeRemoteCache(path string, rc *RemoteCache) error {
	data, err := json.MarshalIndent(rc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remote pricing cache: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write remote pricing cache tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename remote pricing cache: %w", err)
	}
	return nil
}
