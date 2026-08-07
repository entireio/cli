package shellhook

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolate points the userdirs config/cache resolution at throwaway dirs.
// t.Setenv forbids t.Parallel, so these tests are deliberately serial.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

func TestLoadPreferences_MissingFileIsOff(t *testing.T) {
	isolate(t)

	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences() error = %v, want nil", err)
	}
	if prefs.Mode != ModeOff {
		t.Errorf("Mode = %q, want %q", prefs.Mode, ModeOff)
	}
}

func TestPreferences_RoundTrip(t *testing.T) {
	isolate(t)

	want := &Preferences{
		Mode:                ModeAuto,
		DefaultAgents:       []string{"claude-code", "codex"},
		AutoEnableNoConfirm: true,
		WarnThrottleHours:   6,
	}
	if err := SavePreferences(want); err != nil {
		t.Fatalf("SavePreferences() error = %v", err)
	}

	got, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences() error = %v", err)
	}
	if got.Mode != want.Mode || got.AutoEnableNoConfirm != want.AutoEnableNoConfirm ||
		got.WarnThrottleHours != want.WarnThrottleHours {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if len(got.DefaultAgents) != 2 || got.DefaultAgents[0] != "claude-code" {
		t.Errorf("DefaultAgents = %v, want %v", got.DefaultAgents, want.DefaultAgents)
	}
	if got.Version != PreferencesVersion {
		t.Errorf("Version = %d, want %d", got.Version, PreferencesVersion)
	}
}

func TestSavePreferences_FileIsOwnerOnly(t *testing.T) {
	isolate(t)

	if err := SavePreferences(&Preferences{Mode: ModeWarn}); err != nil {
		t.Fatalf("SavePreferences() error = %v", err)
	}
	info, err := os.Stat(PreferencesPath())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("permissions = %o, want %o", perm, filePerm)
	}
}

func TestLoadPreferences_UnknownModeFallsBackToOff(t *testing.T) {
	isolate(t)

	path := PreferencesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"mode":"nonsense"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences() error = %v", err)
	}
	if prefs.Mode != ModeOff {
		t.Errorf("Mode = %q, want %q", prefs.Mode, ModeOff)
	}
}

func TestLoadPreferences_MalformedIsAnError(t *testing.T) {
	isolate(t)

	path := PreferencesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := LoadPreferences(); err == nil {
		t.Fatal("LoadPreferences() error = nil, want a parse error")
	}
}

func TestWarnThrottle_Defaults(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		prefs *Preferences
		want  time.Duration
	}{
		"nil":      {nil, DefaultWarnThrottleHours * time.Hour},
		"unset":    {&Preferences{}, DefaultWarnThrottleHours * time.Hour},
		"negative": {&Preferences{WarnThrottleHours: -1}, DefaultWarnThrottleHours * time.Hour},
		"custom":   {&Preferences{WarnThrottleHours: 3}, 3 * time.Hour},
	} {
		if got := tc.prefs.WarnThrottle(); got != tc.want {
			t.Errorf("%s: WarnThrottle() = %v, want %v", name, got, tc.want)
		}
	}
}
