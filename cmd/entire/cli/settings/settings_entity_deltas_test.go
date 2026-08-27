package settings

import (
	"context"
	"testing"
)

// These tests use setupSettingsDir (t.Chdir) and t.Setenv, both process-global,
// so they cannot run in parallel.

func TestIsEntityDeltasEnabled_DefaultsFalse(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, "")
	if IsEntityDeltasEnabled(context.Background()) {
		t.Error("entity deltas should be off by default")
	}
}

func TestIsEntityDeltasEnabled_FileEnabled(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true, "entity_deltas": true}`, "")
	if !IsEntityDeltasEnabled(context.Background()) {
		t.Error("entity_deltas: true should enable entity deltas")
	}
}

func TestIsEntityDeltasEnabled_EnvOverride(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, "")
	t.Setenv("ENTIRE_ENTITY_DELTAS", "1")
	if !IsEntityDeltasEnabled(context.Background()) {
		t.Error("ENTIRE_ENTITY_DELTAS=1 should enable entity deltas regardless of settings")
	}
}

// The env override is tri-state: it must be able to force the feature OFF in a
// repo whose settings enable it, not just force it on.
func TestIsEntityDeltasEnabled_EnvForcesOffOverSettings(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "no", "off", " 0 "} {
		t.Run(v, func(t *testing.T) {
			setupSettingsDir(t, `{"enabled": true, "entity_deltas": true}`, "")
			t.Setenv("ENTIRE_ENTITY_DELTAS", v)
			if IsEntityDeltasEnabled(context.Background()) {
				t.Errorf("ENTIRE_ENTITY_DELTAS=%q must force entity deltas off", v)
			}
		})
	}
}

func TestIsEntityDeltasEnabled_EnvForcesOnOverSettings(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run(v, func(t *testing.T) {
			setupSettingsDir(t, `{"enabled": true, "entity_deltas": false}`, "")
			t.Setenv("ENTIRE_ENTITY_DELTAS", v)
			if !IsEntityDeltasEnabled(context.Background()) {
				t.Errorf("ENTIRE_ENTITY_DELTAS=%q must force entity deltas on", v)
			}
		})
	}
}

// An env var that is set-but-empty (or set to something we don't recognize)
// expresses no intent, so the settings file decides.
func TestIsEntityDeltasEnabled_EnvUnrecognizedFallsThroughToSettings(t *testing.T) {
	for _, v := range []string{"", "maybe"} {
		t.Run("enabled_"+v, func(t *testing.T) {
			setupSettingsDir(t, `{"enabled": true, "entity_deltas": true}`, "")
			t.Setenv("ENTIRE_ENTITY_DELTAS", v)
			if !IsEntityDeltasEnabled(context.Background()) {
				t.Errorf("ENTIRE_ENTITY_DELTAS=%q must fall through to the setting (true)", v)
			}
		})
		t.Run("disabled_"+v, func(t *testing.T) {
			setupSettingsDir(t, `{"enabled": true}`, "")
			t.Setenv("ENTIRE_ENTITY_DELTAS", v)
			if IsEntityDeltasEnabled(context.Background()) {
				t.Errorf("ENTIRE_ENTITY_DELTAS=%q must fall through to the setting (false)", v)
			}
		})
	}
}

func TestIsEntityDeltasEnabled_LocalFileEnables(t *testing.T) {
	// The gitignored settings.local.json is the natural place to opt into a
	// rollout feature; the merge path must honor it.
	setupSettingsDir(t, `{"enabled": true}`, `{"entity_deltas": true}`)
	if !IsEntityDeltasEnabled(context.Background()) {
		t.Error("entity_deltas in settings.local.json must enable entity deltas")
	}
}

func TestIsEntityDeltasEnabled_LocalFileDisablesBaseEnable(t *testing.T) {
	// A per-machine kill switch: local:false must override base:true.
	setupSettingsDir(t, `{"enabled": true, "entity_deltas": true}`, `{"entity_deltas": false}`)
	if IsEntityDeltasEnabled(context.Background()) {
		t.Error("local entity_deltas:false must override a base value of true")
	}
}

// TestEntireSettings_EntityDeltasJSONTag guards the JSON field name.
// LoadFromBytes uses DisallowUnknownFields, so a wrong tag fails to parse.
func TestEntireSettings_EntityDeltasJSONTag(t *testing.T) {
	t.Parallel()
	s, err := LoadFromBytes([]byte(`{"enabled": true, "entity_deltas": true}`))
	if err != nil {
		t.Fatalf("LoadFromBytes() error = %v", err)
	}
	if !s.EntityDeltas {
		t.Error("entity_deltas did not parse into EntireSettings.EntityDeltas")
	}
}
