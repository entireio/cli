// Command pricingsync mirrors the CLI's embedded pricing tables
// (cmd/entire/cli/pricing/models/*.json) from entire-api's canonical pricing
// catalog, so this repo stops hand-maintaining an independent copy of the
// same rates.
//
// Background: this repo, entire-api's internal/pricing, and entire.io's
// pricing route used to be three independently-updated copies of "the same"
// per-model rates — exactly how the CLI's 1-hour Anthropic cache-write
// premium went silently missing on one of them (a source supplying only the
// 5-minute rate looked identical to one deliberately omitting it). entire-api
// is now the source of truth (its own mise run pricing:check/update keeps it
// LiteLLM-drift-checked); this command is the release-time export that keeps
// the CLI's embedded snapshot in sync with it, the same way entire-api ported
// the CLI's engine once, except now automated and one-directional.
//
// Usage (via mise; both wrap this command):
//
//	mise run pricing:sync        # read-only: report what would change
//	mise run pricing:sync:write  # -write: apply it to models/*.json
//
// Read-only mode exits non-zero when drift is found (and 0 when the tables
// already match), so it doubles as a CI freshness gate — see
// .github/workflows/pricing-sync.yml, which runs it on a schedule and alerts
// on a non-zero exit rather than writing anything itself.
//
// Flags:
//
//	-source  catalog URL or local file path, in the canonical catalog's WIRE
//	         shape ({schemaVersion, models: [{inputPerMTok, ...}]}) — camelCase,
//	         which is what entire-api serves and what the embedded snake_case
//	         files are written FROM, not in. Defaults to the CLI's own production pricing
//	         source (https://entire.io/model-pricing.json — the same URL
//	         pricing/remote.go's opt-in runtime refresh already polls), so this
//	         command and the runtime refresh always agree on where "current" is
//	         defined.
//	-dir     directory of embedded model JSONs (default cmd/entire/cli/pricing/models).
//	-write   apply the sync to the JSON files (default off: report only).
//
// A model id present in an embedded file but absent from the fetched catalog
// is a "local-only" entry (e.g. a CLI-side manual addition the canonical
// source doesn't carry yet, or a stale one it dropped) — it is never deleted
// or guessed at; -write leaves it untouched and this command always reports
// it so a human can act on it. A provider present in the fetched catalog
// but with no existing embedded file gets one created.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultSource = "https://entire.io/model-pricing.json"

const defaultDir = "cmd/entire/cli/pricing/models"

// modelRate mirrors pricing.ModelRate's wire shape. Declared locally (not
// imported from the pricing package) so this command has no dependency on
// that package's internals and preserves the exact field order/shape the
// fetched catalog and the embedded files already share.
type modelRate struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Aliases       []string `json:"aliases"`
	InputPerMTok  float64  `json:"input_per_mtok"`
	OutputPerMTok float64  `json:"output_per_mtok"`
	// omitempty on these two (unlike pricing.ModelRate's own struct) matches
	// the embedded files' existing hand-authored convention of omitting a
	// null cache rate entirely rather than writing the key with a null value
	// — parses identically either way, so this is purely about not
	// introducing diff noise on every model that doesn't set one.
	CacheReadPerMTok    *float64 `json:"cache_read_per_mtok,omitempty"`
	CacheWritePerMTok   *float64 `json:"cache_write_per_mtok,omitempty"`
	CacheWrite1hPerMTok *float64 `json:"cache_write_1h_per_mtok,omitempty"`
	EffectiveDate       string   `json:"effective_date"`
}

type fileSchema struct {
	SchemaVersion int         `json:"schema_version"`
	Models        []modelRate `json:"models"`
}

// catalogModelRate is the WIRE shape entire-api serves the canonical catalog
// in: camelCase, per that repo's CASING-001 API rule. It is deliberately
// separate from modelRate above, which is the snake_case shape of the embedded
// files this command WRITES — those are a storage format shared with the
// user-facing pricing.models override block in .entire/settings.json, so they
// must not be re-cased to follow the wire.
type catalogModelRate struct {
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

// catalogSchema is the wire envelope: {"schemaVersion": 1, "models": [...]}.
type catalogSchema struct {
	SchemaVersion int                `json:"schemaVersion"`
	Models        []catalogModelRate `json:"models"`
}

// toFileSchema converts the fetched wire catalog into the embedded files'
// shape, so every downstream step (validateCatalog, diff, applyReport) keeps
// working on one type. A nil cache rate stays nil rather than becoming 0 —
// the embedded files omit those keys deliberately so pricing.Estimate applies
// the provider's cache multipliers, and a zero would price cached tokens free.
func (c catalogSchema) toFileSchema() fileSchema {
	models := make([]modelRate, 0, len(c.Models))
	for _, m := range c.Models {
		models = append(models, modelRate(m))
	}
	return fileSchema{SchemaVersion: c.SchemaVersion, Models: models}
}

// errDriftFound is a sentinel returned by run in read-only mode when the
// embedded tables have drifted from the canonical catalog. main treats it as
// a distinct case from a genuine failure: the report (already printed to
// stdout) is the useful output, so it exits non-zero without an additional
// "pricingsync: <err>" line on stderr. This is what lets a CI job use the
// exit code as a freshness gate, the same way entire-api's own
// pricingcheck/mise run pricing:check already does.
var errDriftFound = errors.New("embedded pricing tables have drifted from the canonical catalog")

func main() {
	source := flag.String("source", defaultSource, "catalog URL or local file path")
	dir := flag.String("dir", defaultDir, "directory of embedded model JSONs")
	write := flag.Bool("write", false, "apply the sync to the JSON files (default: report only)")
	flag.Parse()

	err := run(*source, *dir, *write)
	if errors.Is(err, errDriftFound) {
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pricingsync:", err)
		os.Exit(1)
	}
}

func run(source, dir string, write bool) error {
	catalog, err := fetchCatalog(source)
	if err != nil {
		return fmt.Errorf("fetch catalog: %w", err)
	}
	if err := validateCatalog(catalog); err != nil {
		return fmt.Errorf("fetched catalog failed sanity check: %w", err)
	}

	current, err := loadEmbedded(dir)
	if err != nil {
		return fmt.Errorf("load embedded models: %w", err)
	}

	report := diff(current.byID, catalog)
	printReport(report)

	if !write {
		if report.hasChanges() {
			fmt.Println("\n(dry run: pass -write to apply)")
			return errDriftFound
		}
		return nil
	}
	if !report.hasChanges() {
		return nil
	}
	return applyReport(dir, current, catalog, report)
}

func fetchCatalog(source string) (fileSchema, error) {
	var body []byte
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return fileSchema{}, fmt.Errorf("build request: %w", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fileSchema{}, fmt.Errorf("fetch %s: %w", source, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fileSchema{}, fmt.Errorf("GET %s: status %d", source, resp.StatusCode)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB is generous; catches a runaway/garbage response
		if err != nil {
			return fileSchema{}, fmt.Errorf("read response body: %w", err)
		}
	} else {
		var err error
		body, err = os.ReadFile(source) // #nosec G304 -- operator-supplied path, short-lived CLI tool
		if err != nil {
			return fileSchema{}, fmt.Errorf("read %s: %w", source, err)
		}
	}

	var catalog catalogSchema
	if err := json.Unmarshal(body, &catalog); err != nil {
		return fileSchema{}, fmt.Errorf("parse catalog: %w", err)
	}
	return catalog.toFileSchema(), nil
}

// validateCatalog rejects a catalog too broken or too small to sync from —
// the same "don't stamp garbage over good data" posture as entire-api's
// remoteDocUsable. It does not repeat every field-level check the embedded
// table's own validateRate does; write-time diffing already surfaces
// per-model anomalies for review.
func validateCatalog(c fileSchema) error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d, want 1", c.SchemaVersion)
	}
	if len(c.Models) == 0 {
		return errors.New("catalog carries zero models")
	}
	for _, m := range c.Models {
		if strings.TrimSpace(m.ID) == "" {
			return errors.New("catalog contains a model with an empty id")
		}
		if strings.TrimSpace(m.Provider) == "" {
			return fmt.Errorf("model %q: empty provider", m.ID)
		}
		if m.InputPerMTok <= 0 || m.OutputPerMTok <= 0 {
			return fmt.Errorf("model %q: non-positive rate", m.ID)
		}
	}
	return nil
}

// providerFile is dir/<provider>.json — the CLI's existing one-file-per-
// provider layout, keyed by the provider field verbatim (matches
// models/{anthropic,openai,google,cursor}.json today).
func providerFile(dir, provider string) string {
	return filepath.Join(dir, provider+".json")
}

// embedded holds what's on disk today: byID for lookups/diffing, and
// byProviderOrdered preserving each file's existing on-disk order — the
// entries are hand-curated in roughly newest-first order per model family,
// not alphabetically, so applyReport must preserve it rather than impose its
// own sort (which would reorder every file on the first sync and bury real
// rate changes in position-only noise).
type embedded struct {
	byID              map[string]modelRate
	byProviderOrdered map[string][]string // provider -> ids, in file order
}

func loadEmbedded(dir string) (embedded, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return embedded{}, fmt.Errorf("read %s: %w", dir, err)
	}
	out := embedded{byID: map[string]modelRate{}, byProviderOrdered: map[string][]string{}}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- fixed embedded-models directory
		if err != nil {
			return embedded{}, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var schema fileSchema
		if err := json.Unmarshal(data, &schema); err != nil {
			return embedded{}, fmt.Errorf("%s: %w", e.Name(), err)
		}
		provider := strings.TrimSuffix(e.Name(), ".json")
		for _, m := range schema.Models {
			out.byID[m.ID] = m
			out.byProviderOrdered[provider] = append(out.byProviderOrdered[provider], m.ID)
		}
	}
	return out, nil
}

type syncReport struct {
	added     []string // present in catalog only
	removed   []string // present locally only (never auto-deleted — see file doc)
	changed   []string // present in both, at least one field differs
	unchanged int
}

func (r syncReport) hasChanges() bool {
	return len(r.added) > 0 || len(r.changed) > 0
}

func diff(current map[string]modelRate, catalog fileSchema) syncReport {
	var report syncReport
	seen := map[string]bool{}
	for _, m := range catalog.Models {
		seen[m.ID] = true
		prev, ok := current[m.ID]
		switch {
		case !ok:
			report.added = append(report.added, m.ID)
		case !ratesEqual(prev, m):
			report.changed = append(report.changed, m.ID)
		default:
			report.unchanged++
		}
	}
	for id := range current {
		if !seen[id] {
			report.removed = append(report.removed, id)
		}
	}
	sort.Strings(report.added)
	sort.Strings(report.removed)
	sort.Strings(report.changed)
	return report
}

// ratesEqual compares the priced fields only. EffectiveDate is deliberately
// excluded: it is provenance, not a priced field, and comparing it would
// report a "change" on every sync even when no rate moved.
func ratesEqual(a, b modelRate) bool {
	return a.Provider == b.Provider &&
		a.InputPerMTok == b.InputPerMTok &&
		a.OutputPerMTok == b.OutputPerMTok &&
		floatPtrEqual(a.CacheReadPerMTok, b.CacheReadPerMTok) &&
		floatPtrEqual(a.CacheWritePerMTok, b.CacheWritePerMTok) &&
		floatPtrEqual(a.CacheWrite1hPerMTok, b.CacheWrite1hPerMTok) &&
		sameAliases(a.Aliases, b.Aliases)
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameAliases(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func printReport(r syncReport) {
	fmt.Printf("added: %d, changed: %d, local-only: %d, unchanged: %d\n",
		len(r.added), len(r.changed), len(r.removed), r.unchanged)
	for _, id := range r.added {
		fmt.Printf("  + %s\n", id)
	}
	for _, id := range r.changed {
		fmt.Printf("  ~ %s\n", id)
	}
	for _, id := range r.removed {
		fmt.Printf("  local-only (left untouched): %s\n", id)
	}
}

// applyReport writes the synced set (current, with added/changed models from
// catalog layered on top — local-only ids are preserved verbatim) back to
// dir, one file per provider, matching the embedded models' existing
// formatting (2-space indent, trailing newline). Models are written in
// catalog order within a provider so a rerun against an unchanged catalog
// produces a byte-identical diff-free file.
func applyReport(dir string, current embedded, catalog fileSchema, report syncReport) error {
	changedOrAdded := map[string]bool{}
	for _, id := range report.added {
		changedOrAdded[id] = true
	}
	for _, id := range report.changed {
		changedOrAdded[id] = true
	}

	// final is every id's post-sync value: the catalog's for anything
	// added/changed, otherwise exactly what was already on disk (so a
	// re-marshal of an unchanged entry can't introduce incidental diffs, e.g.
	// from a field this tool doesn't compare).
	final := map[string]modelRate{}
	for id, m := range current.byID {
		final[id] = m
	}
	for _, m := range catalog.Models {
		if changedOrAdded[m.ID] {
			final[m.ID] = m
		}
	}

	byProvider := map[string][]modelRate{}
	placed := map[string]bool{}
	// Preserve each existing file's on-disk order first — these are
	// hand-curated (roughly newest-first per model family), not alphabetical,
	// so imposing a fresh sort here would reorder every file on the first
	// sync and bury real rate changes in position-only diff noise. A model
	// whose provider field itself changed is skipped here and picked up by
	// the appended pass below, filed under its new provider instead — a rare
	// enough event that losing its old position is an acceptable, visibly
	// reviewable diff.
	for provider, ids := range current.byProviderOrdered {
		for _, id := range ids {
			m := final[id]
			if m.Provider != provider {
				continue
			}
			byProvider[provider] = append(byProvider[provider], m)
			placed[id] = true
		}
	}
	// Append anything not already placed — genuinely new ids, and any model
	// whose provider changed — in catalog order, so a rerun against an
	// unchanged catalog is deterministic.
	for _, m := range catalog.Models {
		if placed[m.ID] {
			continue
		}
		placed[m.ID] = true
		byProvider[m.Provider] = append(byProvider[m.Provider], final[m.ID])
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	providers := make([]string, 0, len(byProvider))
	for p := range byProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		models := byProvider[provider]
		out := fileSchema{SchemaVersion: 1, Models: models}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal %s: %w", provider, err)
		}
		data = append(data, '\n')
		path := providerFile(dir, provider)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("wrote %s (%d models)\n", path, len(models))
	}
	return nil
}
