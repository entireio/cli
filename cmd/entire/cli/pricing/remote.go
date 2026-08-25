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

// remoteModelRate is the WIRE shape of one model rate as entire-api serves it
// from GET /api/v1/pricing/models.json: camelCase, per that repo's CASING-001
// API rule.
//
// It is deliberately distinct from ModelRate, whose snake_case tags are a
// STORAGE format shared by two things that must not move: the embedded
// models/*.json build assets, and the user-facing `pricing.models` override
// block in .entire/settings.json. Re-casing ModelRate to match the wire would
// silently invalidate every existing user override file. Wire and storage are
// separate contracts; toModelRates is the seam between them.
type remoteModelRate struct {
	ID                  string   `json:"id"`
	Provider            string   `json:"provider"`
	Aliases             []string `json:"aliases"`
	InputPerMTok        float64  `json:"inputPerMTok"`
	OutputPerMTok       float64  `json:"outputPerMTok"`
	CacheReadPerMTok    *float64 `json:"cacheReadPerMTok"`
	CacheWritePerMTok   *float64 `json:"cacheWritePerMTok"`
	CacheWrite1hPerMTok *float64 `json:"cacheWrite1hPerMTok"`
	EffectiveDate       string   `json:"effectiveDate"`
}

// remoteCatalog is the wire envelope: {"schemaVersion": 1, "models": [...]}.
type remoteCatalog struct {
	SchemaVersion int               `json:"schemaVersion"`
	Models        []remoteModelRate `json:"models"`
}

// toFileSchema converts a fetched wire catalog into the internal (snake_case)
// shape the cache file and validateRate already use, so everything downstream
// of the fetch — LoadRemoteEntries, countValidEntries, the on-disk cache — is
// untouched by the wire's casing.
//
// A nil cache rate stays nil rather than collapsing to 0: Estimate's
// provider-aware fallback keys on absence to apply Anthropic's 0.1x/1.25x/2x
// multipliers, so a zeroed rate would price cached tokens as free.
func (c remoteCatalog) toFileSchema() fileSchema {
	models := make([]ModelRate, 0, len(c.Models))
	for _, m := range c.Models {
		models = append(models, ModelRate(m))
	}
	return fileSchema{SchemaVersion: c.SchemaVersion, Models: models}
}

// RemoteCache is the on-disk shape of the cached remote pricing table. It wraps
// the same fileSchema the embedded models/*.json files use — so validateRate and
// the schema_version contract are shared, not reimplemented — with the fetch
// bookkeeping the background refresh needs: FetchedAt drives the 24h refresh
// backoff, ETag drives conditional (If-None-Match) requests, SourceURL records
// where the table came from for diagnostics.
//
// The cache file stays snake_case even though the wire is camelCase: it is a
// local artifact, not an API, and keeping it aligned with the embedded files
// means an existing cache written by an earlier build still parses.
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

// RemoteURL is the default source for the remote pricing table: a single static
// file on the apex domain, proxied by entire.io from entire-api's canonical
// catalog. There is no version segment in the path — the document's own
// schemaVersion carries it (LoadRemoteEntries ignores anything but 1), so a
// future breaking change arrives as a different filename rather than as a
// path prefix whose meaning silently shifts.
//
// It is a var so tests can point it at an httptest server; the
// ENTIRE_PRICING_URL environment variable overrides it at fetch time for
// self-hosted setups.
var RemoteURL = "https://entire.io/model-pricing.json"

const (
	// remoteRefreshInterval is the minimum cache age before a refresh is
	// attempted. A response from the source — success, 304, non-200, or a
	// garbage body — resets FetchedAt to this full interval; a transport-level
	// failure (see transportRetryBackoff) resets it to a shorter floor instead.
	remoteRefreshInterval = 24 * time.Hour
	// remoteMaxBytes caps the response body read (1 MiB), mirroring versioncheck.
	remoteMaxBytes = 1 << 20
	// transportRetryBackoff is how soon a transport-level failure (timeout,
	// dial/connection error, or a connection dropped mid-read) is retried,
	// instead of waiting out the full remoteRefreshInterval. Such a failure
	// means we don't know whether the source is even reachable — unlike a
	// well-formed HTTP response (304, non-200, or a 200 with a garbage body),
	// which says the source IS reachable and just has nothing new, and so
	// keeps the full interval. Without this distinction, a single slow
	// request (e.g. a Worker cold start landing past remoteFetchTimeout)
	// resets the clock and blocks any retry for a full day.
	transportRetryBackoff = time.Hour
)

// remoteFetchTimeout bounds the whole refresh request (dial + read), matching
// the version check's 2s budget so a slow endpoint never stalls a background
// worker. It is a var so tests can shrink it to exercise the timeout path fast.
var remoteFetchTimeout = 2 * time.Second

// Refresh outcomes reported by RefreshResult.Outcome.
const (
	refreshOutcomeUpdated     = "updated"      // 200 with a usable doc: Doc+ETag replaced.
	refreshOutcomeNotModified = "not-modified" // 304: prior Doc kept, FetchedAt bumped a full interval.
	refreshOutcomeUnchanged   = "unchanged"    // non-200/garbage: prior Doc kept, FetchedAt bumped a full interval; transport failure: prior Doc kept, FetchedAt bumped only transportRetryBackoff.
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

// cacheStillFresh reports whether rc is recent enough, AND from the currently
// configured source, to skip a refresh. A nil rc is never fresh.
//
// The source check matters because ENTIRE_PRICING_URL can change between
// processes (self-hosted setups, testing): a cache that is "fresh" by the 24h
// clock but was fetched from the OLD source must not block a refresh against
// the NEW one, or the cache would keep serving the wrong source's prices under
// the new URL's name for up to 24h with no way to tell. Every "skip, still
// fresh" decision in this file must go through this one function so the source
// check can't drift out of sync between them.
func cacheStillFresh(rc *RemoteCache) bool {
	if rc == nil {
		return false
	}
	if rc.SourceURL != "" && rc.SourceURL != remoteURL() {
		return false
	}
	return time.Since(rc.FetchedAt) < remoteRefreshInterval
}

// ShouldRefresh reports whether the remote pricing cache is stale (older than
// the 24h interval), absent, or was fetched from a different source than the
// one currently configured. Trigger sites call it to gate a background refresh
// spawn so a fresh, same-source cache skips the work — and the spawn — entirely.
func ShouldRefresh() bool {
	return !cacheStillFresh(loadRemoteCache())
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
// the previous Doc+ETag — so a broken endpoint degrades to "keep serving the
// last good table". A response from the source (304, non-200, garbage body)
// bumps FetchedAt a full remoteRefreshInterval, since the source is reachable
// and simply has nothing usable right now. A transport-level failure (network
// error, timeout, connection dropped mid-read) instead bumps FetchedAt only
// transportRetryBackoff, since it says nothing about the source itself — see
// fetchRemote. It returns an error only for a genuinely local failure
// (cache-dir create, lock, or write).
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
	// the lock, its FetchedAt is now fresh (and its source still matches) and we
	// skip the fetch (unless forced). This is what keeps a burst of spawned
	// workers to a single request.
	prev := loadRemoteCache()
	if !force && cacheStillFresh(prev) {
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
// new RemoteCache derived from prev. Every outcome bumps FetchedAt — to "now"
// for a response from the source, to a short retry floor (via transportFailure)
// for a transport-level failure. It never returns an error — the worst case is
// "keep prev's Doc+ETag", but only when prev is actually from the source about
// to be queried.
func fetchRemote(ctx context.Context, prev *RemoteCache) (*RemoteCache, string) {
	url := remoteURL()
	// prev may be from a DIFFERENT source (ENTIRE_PRICING_URL changed since it
	// was written — self-hosted setups, testing). Carrying its Doc/ETag over as
	// this fetch's fallback would, on any failure below, silently stamp the OLD
	// source's prices with the NEW source's URL and a fresh FetchedAt: a caller
	// reading the cache afterward has no way to tell it isn't actually serving
	// what SourceURL claims. Treat a source mismatch as "nothing to fall back
	// on" instead — a failure then correctly yields no usable remote data
	// (embedded defaults only) rather than mislabeled stale data.
	if prev.SourceURL != "" && prev.SourceURL != url {
		prev = &RemoteCache{}
	}
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
		// Timeout, dial failure, connection refused/reset — we don't know
		// whether the source is even reachable, so retry sooner.
		return transportFailure(out), refreshOutcomeUnchanged
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
		// Connection dropped mid-read: same "source reachability unknown" case
		// as the client.Do error above, not a well-formed-but-unusable response.
		return transportFailure(out), refreshOutcomeUnchanged
	}
	var catalog remoteCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return out, refreshOutcomeUnchanged
	}
	doc := catalog.toFileSchema()
	if !remoteDocUsable(&doc) {
		return out, refreshOutcomeUnchanged
	}
	out.Doc = &doc
	out.ETag = resp.Header.Get("ETag")
	return out, refreshOutcomeUpdated
}

// transportFailure backdates out.FetchedAt to a short retry floor instead of
// "now", for a transport-level failure — see remoteRefreshInterval and
// transportRetryBackoff.
func transportFailure(out *RemoteCache) *RemoteCache {
	out.FetchedAt = time.Now().Add(-(remoteRefreshInterval - transportRetryBackoff))
	return out
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
