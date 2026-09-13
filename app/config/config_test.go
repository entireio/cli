package config

import (
	"os"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg := LoadConfig()

	if cfg.ServerPort == "" {
		t.Errorf("expected non-empty ServerPort")
	}
	if cfg.Environment != "development" && cfg.Environment != os.Getenv("ENVIRONMENT") {
		t.Errorf("unexpected Environment: %s", cfg.Environment)
	}
}

func TestLoadConfigCustomEnv(t *testing.T) {
	os.Setenv("SERVER_PORT", "9999")
	defer os.Unsetenv("SERVER_PORT")

	cfg := LoadConfig()
	if cfg.ServerPort != "9999" {
		t.Errorf("expected ServerPort 9999, got %s", cfg.ServerPort)
	}
}
