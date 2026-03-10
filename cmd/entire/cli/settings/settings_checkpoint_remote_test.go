package settings

import (
	"os"
	"testing"
)

func TestGetCheckpointRemote_NotConfigured(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{}
	if got := s.GetCheckpointRemote(); got != "" {
		t.Errorf("GetCheckpointRemote() = %q, want empty string", got)
	}
}

func TestGetCheckpointRemote_EmptyStrategyOptions(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{},
	}
	if got := s.GetCheckpointRemote(); got != "" {
		t.Errorf("GetCheckpointRemote() = %q, want empty string", got)
	}
}

func TestGetCheckpointRemote_Configured(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": "private",
		},
	}
	if got := s.GetCheckpointRemote(); got != "private" {
		t.Errorf("GetCheckpointRemote() = %q, want %q", got, "private")
	}
}

func TestGetCheckpointRemote_EmptyString(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": "",
		},
	}
	if got := s.GetCheckpointRemote(); got != "" {
		t.Errorf("GetCheckpointRemote() = %q, want empty string", got)
	}
}

func TestGetCheckpointRemote_WrongType(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"checkpoint_remote": 42,
		},
	}
	if got := s.GetCheckpointRemote(); got != "" {
		t.Errorf("GetCheckpointRemote() = %q, want empty string for non-string type", got)
	}
}

func TestGetCheckpointRemote_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	settingsFile := tmpDir + "/settings.json"

	content := `{
  "enabled": true,
  "strategy_options": {
    "checkpoint_remote": "private-remote"
  }
}`
	if err := os.WriteFile(settingsFile, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write settings: %v", err)
	}

	s, err := LoadFromFile(settingsFile)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	if got := s.GetCheckpointRemote(); got != "private-remote" {
		t.Errorf("GetCheckpointRemote() after JSON load = %q, want %q", got, "private-remote")
	}
}

func TestGetCheckpointRemote_CoexistsWithPushSessions(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{
		StrategyOptions: map[string]any{
			"push_sessions":     false,
			"checkpoint_remote": "private",
		},
	}
	if got := s.GetCheckpointRemote(); got != "private" {
		t.Errorf("GetCheckpointRemote() = %q, want %q", got, "private")
	}
	if !s.IsPushSessionsDisabled() {
		t.Error("IsPushSessionsDisabled() = false, want true")
	}
}
