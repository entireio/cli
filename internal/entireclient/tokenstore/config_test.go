package tokenstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupConfig isolates config dir and env.
func setupConfig(t *testing.T, env, config string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	t.Setenv(BackendEnvVar, env)
	if env == "unset" {
		if err := os.Unsetenv(BackendEnvVar); err != nil {
			t.Fatal(err)
		}
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// config.json selects the backend.
func TestChooseBackend_ConfigFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		config  string
		file    bool
		wantErr bool
	}{
		{name: "no config", env: "unset"},
		{name: "config file", env: "unset", config: `{"token_store": "file"}`, file: true},
		{name: "config keyring", env: "unset", config: `{"token_store": "keyring"}`},
		{name: "config without key", env: "unset", config: `{}`},
		{name: "malformed config", env: "unset", config: `{`, wantErr: true},
		{name: "unknown value", env: "unset", config: `{"token_store": "fiel"}`, wantErr: true},
		{name: "unknown key", env: "unset", config: `{"tokenstore": "file"}`, wantErr: true},
		{name: "unknown key beside known", env: "unset", config: `{"token_store": "file", "x": 1}`, wantErr: true},
		{name: "key in wrong case", env: "unset", config: `{"Token_Store": "file"}`, wantErr: true},
		{name: "wrong value type", env: "unset", config: `{"token_store": 1}`, wantErr: true},
		{name: "not an object", env: "unset", config: `[]`, wantErr: true},
		{name: "unknown key fails despite env", env: "file", config: `{"tokenstore": "file"}`, file: false, wantErr: true},
		{name: "env wins", env: "keyring", config: `{"token_store": "file"}`},
		{name: "empty env wins", env: "", config: `{"token_store": "file"}`},
		{name: "bad config fails despite env", env: "file", config: `{`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupConfig(t, tc.env, tc.config)
			c := chooseBackend()
			if c.file != tc.file {
				t.Errorf("file = %v, want %v", c.file, tc.file)
			}
			if gotErr := c.err != nil; gotErr != tc.wantErr {
				t.Fatalf("err = %v, want error %v", c.err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(c.err, ErrBadConfig) {
				t.Errorf("err = %v, want ErrBadConfig", c.err)
			}
		})
	}
}

// A missing config dir means unset.
func TestChooseBackend_MissingConfigDir(t *testing.T) {
	setupConfig(t, "unset", "")
	t.Setenv("ENTIRE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing"))
	if c := chooseBackend(); c.file || c.err != nil {
		t.Fatalf("chooseBackend() = %+v, want keyring, no error", c)
	}
}

// A bad config fails every store call.
func TestStore_BadConfigFailsLoudly(t *testing.T) {
	dir := setupConfig(t, "unset", `{"token_store": "file",}`)
	resetBackendForTesting(t)
	_, err := Get("svc", "user")
	if !errors.Is(err, ErrBadConfig) {
		t.Fatalf("Get() err = %v, want ErrBadConfig", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, configFileName)) {
		t.Errorf("Get() err = %q, want it to name config.json", err)
	}
	if err := Set("svc", "user", "tok"); !errors.Is(err, ErrBadConfig) {
		t.Errorf("Set() err = %v, want ErrBadConfig", err)
	}
	if !strings.Contains(BackendDescription(), "fix or remove it") {
		t.Errorf("BackendDescription() = %q, want the config error", BackendDescription())
	}
}

// CheckConfig names the bad key and file.
func TestCheckConfig(t *testing.T) {
	dir := setupConfig(t, "unset", `{"tokenstore": "file"}`)
	err := CheckConfig()
	if !errors.Is(err, ErrBadConfig) {
		t.Fatalf("CheckConfig() err = %v, want ErrBadConfig", err)
	}
	for _, want := range []string{`unknown key "tokenstore"`, filepath.Join(dir, configFileName), "fix or remove it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckConfig() err = %q, want it to contain %q", err, want)
		}
	}
}

// A good or missing config passes.
func TestCheckConfig_OK(t *testing.T) {
	for _, config := range []string{"", `{}`, `{"token_store": "file"}`} {
		setupConfig(t, "unset", config)
		if err := CheckConfig(); err != nil {
			t.Errorf("CheckConfig() with %q = %v, want nil", config, err)
		}
	}
}
