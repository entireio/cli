package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func floatPtr(f float64) *float64 { return &f }

func TestValidateCatalog(t *testing.T) {
	t.Parallel()

	valid := fileSchema{SchemaVersion: 1, Models: []modelRate{
		{ID: "a", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
	}}
	require.NoError(t, validateCatalog(valid))

	require.Error(t, validateCatalog(fileSchema{SchemaVersion: 2, Models: valid.Models}), "wrong schema version")
	require.Error(t, validateCatalog(fileSchema{SchemaVersion: 1}), "empty models")
	require.Error(t, validateCatalog(fileSchema{SchemaVersion: 1, Models: []modelRate{
		{ID: "", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
	}}), "empty id")
	require.Error(t, validateCatalog(fileSchema{SchemaVersion: 1, Models: []modelRate{
		{ID: "a", Provider: "", InputPerMTok: 1, OutputPerMTok: 1},
	}}), "empty provider")
	require.Error(t, validateCatalog(fileSchema{SchemaVersion: 1, Models: []modelRate{
		{ID: "a", Provider: "anthropic", InputPerMTok: 0, OutputPerMTok: 1},
	}}), "non-positive input rate")
}

func TestDiff_AddedChangedRemovedUnchanged(t *testing.T) {
	t.Parallel()

	current := map[string]modelRate{
		"unchanged-model": {ID: "unchanged-model", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
		"changed-model":   {ID: "changed-model", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
		"local-only":      {ID: "local-only", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
	}
	catalog := fileSchema{Models: []modelRate{
		{ID: "unchanged-model", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
		{ID: "changed-model", Provider: "anthropic", InputPerMTok: 2, OutputPerMTok: 1}, // rate moved
		{ID: "new-model", Provider: "openai", InputPerMTok: 1, OutputPerMTok: 1},
	}}

	report := diff(current, catalog)
	assert.Equal(t, []string{"new-model"}, report.added)
	assert.Equal(t, []string{"changed-model"}, report.changed)
	assert.Equal(t, []string{"local-only"}, report.removed)
	assert.Equal(t, 1, report.unchanged)
	assert.True(t, report.hasChanges())
}

func TestDiff_NoChangesIsNotHasChanges(t *testing.T) {
	t.Parallel()

	m := modelRate{ID: "a", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1}
	report := diff(map[string]modelRate{"a": m}, fileSchema{Models: []modelRate{m}})
	assert.False(t, report.hasChanges())
	assert.Empty(t, report.removed) // present in both -> not local-only either
}

func TestDiff_EffectiveDateIgnored(t *testing.T) {
	t.Parallel()

	// A bare re-stamp of effective_date with no rate change must not report
	// as "changed" — see ratesEqual's doc comment.
	current := map[string]modelRate{
		"a": {ID: "a", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1, EffectiveDate: "2024-01-01"},
	}
	catalog := fileSchema{Models: []modelRate{
		{ID: "a", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1, EffectiveDate: "2026-08-18"},
	}}
	report := diff(current, catalog)
	assert.False(t, report.hasChanges())
}

func TestDiff_CacheRatesCompared(t *testing.T) {
	t.Parallel()

	current := map[string]modelRate{
		"a": {ID: "a", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1, CacheWrite1hPerMTok: nil},
	}
	catalog := fileSchema{Models: []modelRate{
		{ID: "a", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1, CacheWrite1hPerMTok: floatPtr(2)},
	}}
	report := diff(current, catalog)
	assert.Equal(t, []string{"a"}, report.changed)
}

// TestApplyReport_PreservesOnDiskOrderAndAppendsNew is the regression for the
// embedded files' hand-curated (not alphabetical) ordering: applyReport must
// not resort an existing file, only patch changed entries in place and append
// new ones — see applyReport's doc comment.
func TestApplyReport_PreservesOnDiskOrderAndAppendsNew(t *testing.T) {
	dir := t.TempDir()
	seed := fileSchema{SchemaVersion: 1, Models: []modelRate{
		{ID: "z-first", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1, EffectiveDate: "2024-01-01"},
		{ID: "a-second", Provider: "anthropic", InputPerMTok: 2, OutputPerMTok: 2, EffectiveDate: "2024-01-01"},
	}}
	writeSchema(t, filepath.Join(dir, "anthropic.json"), seed)

	current, err := loadEmbedded(dir)
	require.NoError(t, err)

	catalog := fileSchema{Models: []modelRate{
		{ID: "z-first", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1, EffectiveDate: "2024-01-01"},
		{ID: "a-second", Provider: "anthropic", InputPerMTok: 99, OutputPerMTok: 2, EffectiveDate: "2026-08-18"}, // rate changed
		{ID: "brand-new", Provider: "anthropic", InputPerMTok: 3, OutputPerMTok: 3, EffectiveDate: "2026-08-18"},
	}}
	report := diff(current.byID, catalog)
	require.NoError(t, applyReport(dir, current, catalog, report))

	got := readSchema(t, filepath.Join(dir, "anthropic.json"))
	require.Len(t, got.Models, 3)
	// Original on-disk order preserved for the first two; the new one is
	// appended, not alphabetically inserted (would put it between them).
	assert.Equal(t, "z-first", got.Models[0].ID)
	assert.Equal(t, "a-second", got.Models[1].ID)
	assert.Equal(t, "brand-new", got.Models[2].ID)
	assert.InDelta(t, 99.0, got.Models[1].InputPerMTok, 1e-9, "changed rate must be applied")
	assert.Equal(t, "2024-01-01", got.Models[0].EffectiveDate, "untouched entry must be byte-identical, not re-stamped")
}

// TestApplyReport_LocalOnlyEntryNeverDeleted is the regression for the "never
// guessed at or erased" invariant on a model the canonical catalog doesn't
// (yet, or anymore) carry — see the package doc comment.
func TestApplyReport_LocalOnlyEntryNeverDeleted(t *testing.T) {
	dir := t.TempDir()
	seed := fileSchema{SchemaVersion: 1, Models: []modelRate{
		{ID: "local-internal-model", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1, EffectiveDate: "2024-01-01"},
	}}
	writeSchema(t, filepath.Join(dir, "anthropic.json"), seed)

	current, err := loadEmbedded(dir)
	require.NoError(t, err)

	catalog := fileSchema{Models: []modelRate{
		{ID: "unrelated-model", Provider: "openai", InputPerMTok: 1, OutputPerMTok: 1, EffectiveDate: "2026-08-18"},
	}}
	report := diff(current.byID, catalog)
	require.Equal(t, []string{"local-internal-model"}, report.removed)
	require.NoError(t, applyReport(dir, current, catalog, report))

	got := readSchema(t, filepath.Join(dir, "anthropic.json"))
	require.Len(t, got.Models, 1)
	assert.Equal(t, "local-internal-model", got.Models[0].ID, "local-only entry must survive -write untouched")

	// A brand new provider file must be created for the new entry.
	gotOpenAI := readSchema(t, filepath.Join(dir, "openai.json"))
	require.Len(t, gotOpenAI.Models, 1)
	assert.Equal(t, "unrelated-model", gotOpenAI.Models[0].ID)
}

func TestLoadEmbedded_TracksProviderFileOrder(t *testing.T) {
	dir := t.TempDir()
	writeSchema(t, filepath.Join(dir, "anthropic.json"), fileSchema{SchemaVersion: 1, Models: []modelRate{
		{ID: "z", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
		{ID: "a", Provider: "anthropic", InputPerMTok: 1, OutputPerMTok: 1},
	}})

	got, err := loadEmbedded(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"z", "a"}, got.byProviderOrdered["anthropic"], "must preserve file order, not sort")
	assert.Len(t, got.byID, 2)
}

func writeSchema(t *testing.T, path string, s fileSchema) {
	t.Helper()
	data, err := json.MarshalIndent(s, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func readSchema(t *testing.T, path string) fileSchema {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test fixture path under t.TempDir()
	require.NoError(t, err)
	var s fileSchema
	require.NoError(t, json.Unmarshal(data, &s))
	return s
}
