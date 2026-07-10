package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// writeRemotePricingCache writes a raw remote pricing cache document to the
// isolated cache dir. Call t.Setenv("XDG_CACHE_HOME", ...) first so it lands in
// a throwaway location, never the developer's real ~/.cache/entire.
func writeRemotePricingCache(t *testing.T, jsonBody string) {
	t.Helper()
	dir := userdirs.Cache()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pricing_remote.json"), []byte(jsonBody), 0o644); err != nil {
		t.Fatalf("write remote pricing cache: %v", err)
	}
}

// remoteGPT55At999 is a well-formed remote cache doc that reprices gpt-5.5 far
// away from its embedded rate (5) so tests can tell which layer won.
const remoteGPT55At999 = `{"fetched_at":"2026-07-10T00:00:00Z","doc":{"schema_version":1,` +
	`"models":[{"id":"gpt-5.5","provider":"openai","input_per_mtok":999,"output_per_mtok":1000}]}}`

func lookupInput(t *testing.T, settingsJSON, cacheJSON string) float64 {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	setupSettingsDir(t, settingsJSON, "")
	if cacheJSON != "" {
		writeRemotePricingCache(t, cacheJSON)
	}
	table, _ := LoadPricingTable(context.Background())
	if table == nil {
		t.Fatal("LoadPricingTable returned nil table")
	}
	rate, ok := table.Lookup("gpt-5.5")
	if !ok {
		t.Fatal("gpt-5.5 did not resolve")
	}
	return rate.InputPerMTok
}

func TestLoadPricingTable_RemoteDisabled_EmbeddedOnly(t *testing.T) {
	// Remote setting absent: the cached doc must be ignored entirely.
	got := lookupInput(t, `{"enabled":true}`, remoteGPT55At999)
	if got != 5 {
		t.Errorf("gpt-5.5 input = %v, want 5 (embedded); remote must not merge when disabled", got)
	}
}

func TestLoadPricingTable_RemoteEnabled_RemoteOverridesEmbedded(t *testing.T) {
	got := lookupInput(t, `{"enabled":true,"pricing":{"remote":true}}`, remoteGPT55At999)
	if got != 999 {
		t.Errorf("gpt-5.5 input = %v, want 999 (remote overrides embedded)", got)
	}
}

func TestLoadPricingTable_RemoteEnabled_UserOverrideWins(t *testing.T) {
	settingsJSON := `{"enabled":true,"pricing":{"remote":true,` +
		`"models":[{"id":"gpt-5.5","provider":"openai","input_per_mtok":111,"output_per_mtok":222}]}}`
	got := lookupInput(t, settingsJSON, remoteGPT55At999)
	if got != 111 {
		t.Errorf("gpt-5.5 input = %v, want 111 (user override wins over remote)", got)
	}
}

func TestLoadPricingTable_RemoteEnabled_CorruptCache_EmbeddedOnly(t *testing.T) {
	got := lookupInput(t, `{"enabled":true,"pricing":{"remote":true}}`, `{not valid json`)
	if got != 5 {
		t.Errorf("gpt-5.5 input = %v, want 5 (corrupt cache falls back to embedded)", got)
	}
}

func TestLoadPricingTable_RemoteEnabled_SchemaVersion99Ignored(t *testing.T) {
	cache := `{"fetched_at":"2026-07-10T00:00:00Z","doc":{"schema_version":99,` +
		`"models":[{"id":"gpt-5.5","provider":"openai","input_per_mtok":999,"output_per_mtok":1000}]}}`
	got := lookupInput(t, `{"enabled":true,"pricing":{"remote":true}}`, cache)
	if got != 5 {
		t.Errorf("gpt-5.5 input = %v, want 5 (unsupported schema_version ignored)", got)
	}
}

func TestIsRemoteEnabled_Accessor(t *testing.T) {
	t.Parallel()

	var nilSettings *EntireSettings
	if nilSettings.IsRemoteEnabled() {
		t.Error("nil settings IsRemoteEnabled() = true, want false")
	}
	if (&EntireSettings{}).IsRemoteEnabled() {
		t.Error("empty settings IsRemoteEnabled() = true, want false")
	}
	if (&EntireSettings{Pricing: &PricingSettings{}}).IsRemoteEnabled() {
		t.Error("pricing without remote IsRemoteEnabled() = true, want false")
	}

	enabled := true
	if !(&EntireSettings{Pricing: &PricingSettings{Remote: &enabled}}).IsRemoteEnabled() {
		t.Error("remote=true IsRemoteEnabled() = false, want true")
	}
	disabled := false
	if (&EntireSettings{Pricing: &PricingSettings{Remote: &disabled}}).IsRemoteEnabled() {
		t.Error("remote=false IsRemoteEnabled() = true, want false")
	}
}
