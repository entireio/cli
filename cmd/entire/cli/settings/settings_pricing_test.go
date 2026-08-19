package settings

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/pricing"
)

func TestPricingAccessors_Defaults(t *testing.T) {
	t.Parallel()

	// nil receiver and empty settings both report the safe defaults.
	var nilSettings *EntireSettings
	if nilSettings.PricingOverrides() != nil {
		t.Error("nil settings PricingOverrides() != nil")
	}
	if nilSettings.IsEstimationDisabled() {
		t.Error("nil settings IsEstimationDisabled() = true, want false")
	}

	empty := &EntireSettings{}
	if empty.PricingOverrides() != nil {
		t.Error("empty settings PricingOverrides() != nil")
	}
	if empty.IsEstimationDisabled() {
		t.Error("empty settings IsEstimationDisabled() = true, want false")
	}
}

func TestPricingSettings_LoadFromBytes(t *testing.T) {
	t.Parallel()

	data := []byte(`{
      "enabled": true,
      "pricing": {
        "models": [
          {"id": "custom-x", "provider": "test", "input_per_mtok": 3, "output_per_mtok": 9}
        ],
        "disable_estimation": true
      }
    }`)

	s, err := LoadFromBytes(data)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	overrides := s.PricingOverrides()
	if len(overrides) != 1 {
		t.Fatalf("PricingOverrides() len = %d, want 1", len(overrides))
	}
	if overrides[0].ID != "custom-x" || overrides[0].InputPerMTok != 3 || overrides[0].OutputPerMTok != 9 {
		t.Errorf("override = %+v, want custom-x in=3 out=9", overrides[0])
	}
	if !s.IsEstimationDisabled() {
		t.Error("IsEstimationDisabled() = false, want true")
	}
}

func TestPricingSettings_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	disable := true
	orig := &EntireSettings{
		Enabled: true,
		Pricing: &PricingSettings{
			DisableEstimation: &disable,
			Models: []pricing.ModelRate{
				{ID: "rt-model", Provider: "test", InputPerMTok: 4, OutputPerMTok: 8},
			},
		},
	}

	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := LoadFromBytes(raw)
	if err != nil {
		t.Fatalf("LoadFromBytes(round-trip): %v", err)
	}
	if !got.IsEstimationDisabled() {
		t.Error("round-trip lost disable_estimation")
	}
	if len(got.PricingOverrides()) != 1 || got.PricingOverrides()[0].ID != "rt-model" {
		t.Errorf("round-trip overrides = %+v, want single rt-model", got.PricingOverrides())
	}
}

func TestPricingSettings_LocalOverrideMerges(t *testing.T) {
	base := `{
      "enabled": true,
      "pricing": {
        "models": [{"id": "base-model", "provider": "test", "input_per_mtok": 1, "output_per_mtok": 1}],
        "disable_estimation": false
      }
    }`
	local := `{"pricing": {"disable_estimation": true}}`
	setupSettingsDir(t, base, local)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Local override flips disable_estimation on; base models survive (models key
	// absent from the override, so it is not replaced).
	if !s.IsEstimationDisabled() {
		t.Error("local override did not enable disable_estimation")
	}
	if len(s.PricingOverrides()) != 1 || s.PricingOverrides()[0].ID != "base-model" {
		t.Errorf("overrides = %+v, want base-model preserved", s.PricingOverrides())
	}
}

func TestPricingSettings_LocalOverrideReplacesModels(t *testing.T) {
	base := `{
      "enabled": true,
      "pricing": {"models": [{"id": "base-model", "provider": "test", "input_per_mtok": 1, "output_per_mtok": 1}]}
    }`
	local := `{"pricing": {"models": [{"id": "local-model", "provider": "test", "input_per_mtok": 2, "output_per_mtok": 2}]}}`
	setupSettingsDir(t, base, local)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.PricingOverrides()) != 1 || s.PricingOverrides()[0].ID != "local-model" {
		t.Errorf("overrides = %+v, want models replaced by local-model", s.PricingOverrides())
	}
}
