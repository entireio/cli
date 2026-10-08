package settings

import (
	"context"
	"testing"
)

// These tests use setupSettingsDir (t.Chdir) and t.Setenv, both process-global,
// so they cannot run in parallel.

func TestIsImageExternalizationEnabled_DefaultsFalse(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, "")
	if IsImageExternalizationEnabled(context.Background()) {
		t.Error("image externalization should be off by default")
	}
}

func TestIsImageExternalizationEnabled_FileEnabled(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true, "redaction": {"externalize_images": true}}`, "")
	if !IsImageExternalizationEnabled(context.Background()) {
		t.Error("redaction.externalize_images: true should enable externalization")
	}
}

func TestIsImageExternalizationEnabled_EnvOverride(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, "")
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")
	if !IsImageExternalizationEnabled(context.Background()) {
		t.Error("ENTIRE_EXTERNALIZE_IMAGES=1 should enable externalization regardless of settings")
	}
}

func TestIsImageExternalizationEnabled_LocalFileEnables(t *testing.T) {
	// The gitignored settings.local.json is the natural place to opt into a
	// rollout feature; the merge path must honor it.
	setupSettingsDir(t, `{"enabled": true}`, `{"redaction": {"externalize_images": true}}`)
	if !IsImageExternalizationEnabled(context.Background()) {
		t.Error("externalize_images in settings.local.json must enable externalization")
	}
}

func TestIsImageExternalizationEnabled_LocalFileDisablesBaseEnable(t *testing.T) {
	// A per-machine kill switch: local:false must override base:true.
	setupSettingsDir(t,
		`{"enabled": true, "redaction": {"externalize_images": true}}`,
		`{"redaction": {"externalize_images": false}}`)
	if IsImageExternalizationEnabled(context.Background()) {
		t.Error("local externalize_images:false must override a base value of true")
	}
}

// TestRedactionSettings_ExternalizeImagesJSONTag guards the JSON field name.
// LoadFromBytes uses DisallowUnknownFields, so a wrong tag fails to parse.
func TestRedactionSettings_ExternalizeImagesJSONTag(t *testing.T) {
	t.Parallel()
	s, err := LoadFromBytes([]byte(`{"enabled": true, "redaction": {"externalize_images": true}}`))
	if err != nil {
		t.Fatalf("LoadFromBytes() error = %v", err)
	}
	if s.Redaction == nil || !s.Redaction.ExternalizeImages {
		t.Errorf("externalize_images did not parse into RedactionSettings.ExternalizeImages")
	}
}
