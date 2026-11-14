package settings

import (
	"context"
	"testing"
)

// These tests use setupSettingsDir (t.Chdir), which is process-global, so they
// cannot run in parallel.

func TestIsImageDropEnabled(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		local string
		want  bool
	}{
		{"off by default", `{"enabled": true}`, "", false},
		{"redaction block without the key", `{"enabled": true, "redaction": {"pii": {"enabled": true}}}`, "", false},
		{"committed file enables", `{"enabled": true, "redaction": {"drop_images": true}}`, "", true},
		{"local file enables", `{"enabled": true}`, `{"redaction": {"drop_images": true}}`, true},
		{
			"local false cannot turn off a committed true",
			`{"enabled": true, "redaction": {"drop_images": true}}`,
			`{"redaction": {"drop_images": false}}`,
			true,
		},
		{
			"committed false does not block a local true",
			`{"enabled": true, "redaction": {"drop_images": false}}`,
			`{"redaction": {"drop_images": true}}`,
			true,
		},
		{"explicit false", `{"enabled": true, "redaction": {"drop_images": false}}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupSettingsDir(t, tt.base, tt.local)
			if got := IsImageDropEnabled(context.Background()); got != tt.want {
				t.Errorf("IsImageDropEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A settings file that cannot be loaded must not silently turn the drop off
// when it could have asked for it.
func TestIsImageDropEnabled_UnloadableSettings(t *testing.T) {
	t.Run("declares redaction", func(t *testing.T) {
		setupSettingsDir(t, `{"enabled": true, "redaction": {"drop_images": true}, "from_a_newer_cli": 1}`, "")
		if !IsImageDropEnabled(context.Background()) {
			t.Error("settings that declare redaction but fail to load must drop images")
		}
	})
	t.Run("no redaction block", func(t *testing.T) {
		setupSettingsDir(t, `{"enabled": true, "from_a_newer_cli": 1}`, "")
		if IsImageDropEnabled(context.Background()) {
			t.Error("settings that never mention redaction must not start dropping images")
		}
	})
}

// With both settings on, drop wins: nothing may be lifted into assets.
func TestIsImageExternalizationEnabled_OffWhenDropIsOn(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true, "redaction": {"drop_images": true, "externalize_images": true}}`, "")
	if IsImageExternalizationEnabled(context.Background()) {
		t.Error("externalization must be off while drop_images is on")
	}

	t.Run("env override", func(t *testing.T) {
		setupSettingsDir(t, `{"enabled": true, "redaction": {"drop_images": true}}`, "")
		t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")
		if IsImageExternalizationEnabled(context.Background()) {
			t.Error("ENTIRE_EXTERNALIZE_IMAGES must not override drop_images")
		}
	})
}

// LoadFromBytes uses DisallowUnknownFields, so a wrong tag fails to parse.
func TestRedactionSettings_DropImagesJSONTag(t *testing.T) {
	t.Parallel()
	s, err := LoadFromBytes([]byte(`{"enabled": true, "redaction": {"drop_images": true}}`))
	if err != nil {
		t.Fatalf("LoadFromBytes() error = %v", err)
	}
	if s.Redaction == nil || !s.Redaction.DropImages {
		t.Error("drop_images did not parse into RedactionSettings.DropImages")
	}
}
